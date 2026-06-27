package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

// PoolKey identifies a class of warm containers.
type PoolKey struct {
	ProjectID string
	Role      string
	Image     string
	// Network is the role's network policy. It is part of the key so a
	// warm container started under one policy is never reused for a role
	// with a different one (e.g. a daemon-only container must not serve
	// a host role, and vice-versa).
	Network NetworkMode
}

func (k PoolKey) String() string {
	return k.ProjectID + ":" + k.Role + ":" + k.Image + ":" + string(k.Network)
}

// PoolEntry tracks one warm container.
type PoolEntry struct {
	ContainerID  string
	Key          PoolKey
	InUse        bool
	LastUsedAt   time.Time
	ReuseCount   int
	InputDir     string // host-side bind mount for /app/input
	OutputDir    string // host-side bind mount for /app/output
	WorkspaceDir string // host-side bind mount for /app/workspace
	ProjectDir   string // host-side bind mount for /app/workspace/project (per-project shared dir)
}

// PoolConfig configures the warm pool.
type PoolConfig struct {
	IdleTimeout time.Duration
	MaxPerRole  int
}

// DefaultPoolConfig returns sensible defaults.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		IdleTimeout: 10 * time.Minute,
		MaxPerRole:  2,
	}
}

// WarmPool manages a pool of reusable agent containers.
type WarmPool struct {
	mu                   sync.Mutex
	started              bool
	stopped              bool
	manager              *Manager
	config               PoolConfig
	entries              map[string]*PoolEntry    // containerID -> entry
	byKey                map[PoolKey][]*PoolEntry // key -> entries
	envVars              map[string]string        // LLM env vars injected into warm containers
	projectWorkspacePath string                   // host path for per-project shared dirs
	logger               zerolog.Logger
	metrics              *PoolMetrics
	stopCh               chan struct{}
	wg                   sync.WaitGroup
}

// PoolOption configures the WarmPool.
type PoolOption func(*WarmPool)

// WithPoolLogger sets the logger.
func WithPoolLogger(l zerolog.Logger) PoolOption {
	return func(p *WarmPool) { p.logger = l }
}

// WithPoolEnvVars sets the env vars injected into warm containers.
func WithPoolEnvVars(env map[string]string) PoolOption {
	return func(p *WarmPool) { p.envVars = env }
}

// WithPoolProjectWorkspacePath sets the host path used for per-project
// persistent directories. Warm containers will receive the same
// /app/workspace/project/ mount as ephemeral containers.
func WithPoolProjectWorkspacePath(path string) PoolOption {
	return func(p *WarmPool) { p.projectWorkspacePath = path }
}

// WithPoolPrometheusRegistry creates pool metrics.
func WithPoolPrometheusRegistry(reg *prometheus.Registry) PoolOption {
	return func(p *WarmPool) {
		if reg != nil {
			p.metrics = NewPoolMetrics(reg)
		}
	}
}

// NewWarmPool creates a new warm container pool.
func NewWarmPool(manager *Manager, config PoolConfig, opts ...PoolOption) *WarmPool {
	p := &WarmPool{
		manager: manager,
		config:  config,
		entries: make(map[string]*PoolEntry),
		byKey:   make(map[PoolKey][]*PoolEntry),
		logger:  zerolog.Nop(),
		stopCh:  make(chan struct{}),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Start launches the idle-timeout reaper.
func (p *WarmPool) Start() {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	if p.stopCh == nil {
		p.stopCh = make(chan struct{})
	}
	stopCh := p.stopCh
	p.started = true
	p.stopped = false
	p.mu.Unlock()

	p.wg.Add(1)
	go p.reaperLoop(stopCh)
	p.logger.Info().
		Dur("idle_timeout", p.config.IdleTimeout).
		Int("max_per_role", p.config.MaxPerRole).
		Msg("warm container pool started")
}

// Stop shuts down all warm containers and the reaper.
func (p *WarmPool) Stop(ctx context.Context) {
	p.mu.Lock()
	if !p.started {
		p.stopped = true
		p.mu.Unlock()
		return
	}
	p.started = false
	p.stopped = true
	stopCh := p.stopCh
	p.stopCh = nil
	p.mu.Unlock()

	if stopCh != nil {
		close(stopCh)
	}
	if !waitGroupWithContext(ctx, &p.wg) {
		p.logger.Warn().Msg("warm container pool stop timed out while waiting for background work")
		return
	}

	p.mu.Lock()
	var toStop []*PoolEntry
	for _, entry := range p.entries {
		toStop = append(toStop, entry)
		p.recordPoolRemovalLocked(entry)
	}
	p.entries = make(map[string]*PoolEntry)
	p.byKey = make(map[PoolKey][]*PoolEntry)
	p.mu.Unlock()

	// Snapshot taken, lock released. Tear down in parallel.
	for _, entry := range toStop {
		entry := entry
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.teardownEntry(ctx, entry, true)
		}()
	}
	if !waitGroupWithContext(ctx, &p.wg) {
		p.logger.Warn().Msg("warm container pool stop timed out while tearing down containers")
		return
	}
	p.logger.Info().Msg("warm container pool stopped")
}

// Acquire returns an idle warm container for the given key, or nil.
func (p *WarmPool) Acquire(key PoolKey) *PoolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return nil
	}

	for _, entry := range p.byKey[key] {
		if !entry.InUse {
			entry.InUse = true
			entry.ReuseCount++
			if p.metrics != nil {
				p.metrics.WarmHits.WithLabelValues(key.ProjectID, key.Role).Inc()
			}
			p.logger.Debug().
				Str("container_id", entry.ContainerID).
				Str("key", key.String()).
				Int("reuse_count", entry.ReuseCount).
				Msg("acquired warm container")
			return entry
		}
	}
	if p.metrics != nil {
		p.metrics.ColdStarts.WithLabelValues(key.ProjectID, key.Role).Inc()
	}
	return nil
}

// StartWarm starts a new warm container and adds it to the pool.
// envOverrides are merged on top of the pool-level defaults so that
// per-role settings (e.g. VORNIK_LLM_MODEL) reach the container.
func (p *WarmPool) StartWarm(ctx context.Context, key PoolKey, envOverrides map[string]string) (*PoolEntry, error) {
	// Reserve a slot under lock to prevent TOCTOU race where two
	// goroutines both pass the capacity check before either registers.
	placeholder := &PoolEntry{Key: key, InUse: true}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil, fmt.Errorf("pool is stopped")
	}
	if len(p.byKey[key]) >= p.config.MaxPerRole {
		p.mu.Unlock()
		return nil, fmt.Errorf("pool limit reached for %s (max %d)", key.String(), p.config.MaxPerRole)
	}
	p.byKey[key] = append(p.byKey[key], placeholder)
	p.mu.Unlock()

	// On failure, remove the reserved slot.
	removePlaceholder := func() {
		p.mu.Lock()
		entries := p.byKey[key]
		for i, e := range entries {
			if e == placeholder {
				p.byKey[key] = append(entries[:i], entries[i+1:]...)
				break
			}
		}
		p.mu.Unlock()
	}

	// Create persistent host directories
	root, err := os.MkdirTemp("", "vornik-warm-*")
	if err != nil {
		removePlaceholder()
		return nil, fmt.Errorf("failed to create warm dirs: %w", err)
	}
	inputDir := filepath.Join(root, "input")
	outputDir := filepath.Join(root, "output")
	workspaceDir := filepath.Join(root, "workspace")
	for _, dir := range []string{inputDir, outputDir, workspaceDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			_ = os.RemoveAll(root)
			removePlaceholder()
			return nil, fmt.Errorf("failed to create warm dir %s: %w", dir, err)
		}
	}

	// Create per-project shared dir if a workspace path is configured.
	var projectDir string
	if p.projectWorkspacePath != "" {
		projectDir = filepath.Join(p.projectWorkspacePath, key.ProjectID)
		if err := os.MkdirAll(projectDir, 0o755); err != nil {
			p.logger.Warn().Err(err).Str("project_dir", projectDir).Msg("failed to create project dir for warm container")
			projectDir = ""
		}
	}

	// Merge env vars: pool defaults → role overrides → warm mode flag.
	envVars := make(map[string]string, len(p.envVars)+len(envOverrides)+1)
	for k, v := range p.envVars {
		envVars[k] = v
	}
	for k, v := range envOverrides {
		envVars[k] = v
	}
	envVars["VORNIK_WARM_MODE"] = "1"

	config := &ContainerConfig{
		Image:           key.Image,
		ProjectID:       key.ProjectID,
		Role:            key.Role,
		TaskID:          fmt.Sprintf("warm-%d", time.Now().UnixNano()),
		LifecyclePolicy: PolicyWarm,
		EnvVars:         envVars,
		InputDir:        inputDir,
		OutputDir:       outputDir,
		WorkspaceDir:    workspaceDir,
		ProjectDir:      projectDir,
		// Per-role network policy, carried on the pool key so warm
		// containers honor runtime.network exactly like ephemeral ones
		// (Step B; mitigation plan §7.1). The Manager applies the
		// daemon-only socket mount + env rewrite from this field.
		Network: key.Network,
	}

	containerID, err := p.manager.StartContainer(ctx, config)
	if err != nil {
		_ = os.RemoveAll(root)
		removePlaceholder()
		return nil, fmt.Errorf("failed to start warm container: %w", err)
	}

	entry := &PoolEntry{
		ContainerID:  containerID,
		Key:          key,
		InUse:        true,
		LastUsedAt:   time.Now(),
		ReuseCount:   1,
		InputDir:     inputDir,
		OutputDir:    outputDir,
		WorkspaceDir: workspaceDir,
		ProjectDir:   projectDir,
	}

	// Replace the placeholder with the real entry.
	p.mu.Lock()
	entries := p.byKey[key]
	for i, e := range entries {
		if e == placeholder {
			entries[i] = entry
			break
		}
	}
	p.entries[containerID] = entry
	p.mu.Unlock()

	if p.metrics != nil {
		p.metrics.PoolSize.WithLabelValues(key.ProjectID, key.Role).Inc()
	}

	p.logger.Info().
		Str("container_id", containerID).
		Str("key", key.String()).
		Msg("started new warm container")
	return entry, nil
}

// InjectTask writes task.json and signals the warm agent to process it.
func (p *WarmPool) InjectTask(entry *PoolEntry, inputData []byte) error {
	// Clear previous output
	_ = os.Remove(filepath.Join(entry.OutputDir, "result.json"))
	_ = os.Remove(filepath.Join(entry.OutputDir, ".done"))

	// Write task.json — 0o600 because the payload can contain
	// inline secrets pulled from project config or prior steps;
	// the container reads it as the same UID, no need for world
	// read.
	if err := os.WriteFile(filepath.Join(entry.InputDir, "task.json"), inputData, 0o600); err != nil {
		return fmt.Errorf("failed to write task.json: %w", err)
	}

	// Signal ready. The sentinel itself carries no secrets but
	// its presence/timestamp is a timing oracle for any local
	// reader; 0o600 keeps both content and metadata private.
	if err := os.WriteFile(filepath.Join(entry.InputDir, ".ready"), []byte("go"), 0o600); err != nil {
		return fmt.Errorf("failed to write .ready sentinel: %w", err)
	}
	return nil
}

// WaitForTaskDone polls for the .done sentinel and returns result.json.
//
// The wait is bounded by two independent conditions:
//  1. the ticker polls .done every 250ms — fast loop because the agent
//     writes the sentinel the instant it finishes;
//  2. a slower heartbeat (every 2s) checks that the container is still
//     running. If the container died without writing .done (entrypoint
//     crashed, curl killed, OOM, operator force-removed the container),
//     we fail immediately rather than waiting out the full timeout.
func (p *WarmPool) WaitForTaskDone(ctx context.Context, entry *PoolEntry, timeout time.Duration) ([]byte, error) {
	ctx, cancel := contextWithOptionalTimeout(ctx, timeout)
	defer cancel()

	doneFile := filepath.Join(entry.OutputDir, ".done")
	resultFile := filepath.Join(entry.OutputDir, "result.json")

	doneTicker := time.NewTicker(250 * time.Millisecond)
	defer doneTicker.Stop()
	livenessTicker := time.NewTicker(2 * time.Second)
	defer livenessTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("warm container timed out waiting for task completion")
		case <-doneTicker.C:
			if _, err := os.Stat(doneFile); err == nil {
				data, err := os.ReadFile(resultFile)
				if err != nil {
					return nil, fmt.Errorf("task done but result.json missing: %w", err)
				}
				return data, nil
			}
		case <-livenessTicker.C:
			// Defensive: the manager may be nil in unit tests that
			// instantiate a bare WarmPool. Skip the probe in that case;
			// the .done watch still enforces the overall timeout.
			if p.manager == nil {
				continue
			}
			info, err := p.manager.InspectContainer(ctx, entry.ContainerID)
			if err != nil {
				var notFound *ContainerNotFoundError
				if errors.As(err, &notFound) {
					// Container is gone entirely (operator removed it,
					// crashed out of the registry). Treat as death.
					return nil, fmt.Errorf("warm container %s disappeared before emitting .done: %w",
						entry.ContainerID, err)
				}
				// Transient podman failure (daemon restart, load, etc).
				// Log and continue waiting; the timeout still bounds us.
				p.logger.Debug().Err(err).Str("container_id", entry.ContainerID).Msg("transient inspect failure during liveness check")
				continue
			}
			// Race: the container could exit cleanly and write .done in
			// the 2s window between liveness checks. Do one final .done
			// poll before declaring death so we don't drop a valid
			// result on the floor.
			if info != nil && info.Status != StatusRunning {
				if _, err := os.Stat(doneFile); err == nil {
					data, readErr := os.ReadFile(resultFile)
					if readErr != nil {
						return nil, fmt.Errorf("container exited and task done but result.json missing: %w", readErr)
					}
					return data, nil
				}
				return nil, fmt.Errorf("warm container %s exited (%s) without writing .done — entrypoint likely crashed",
					entry.ContainerID, info.Status)
			}
		}
	}
}

// Release marks a container as idle or removes it if unhealthy.
//
// When Release runs DURING Stop (race window between Stop's two
// wg.Waits), pre-2026-05-29 it could spawn an unhealthy-teardown
// goroutine after Stop's first wg.Wait had already drained — the
// goroutine outlived Stop with no bound. Now: when p.stopped is
// set, Release marks the entry idle and leaves it in the map for
// Stop's bulk iteration to find. Stop is the single owner of
// teardown during drain.
func (p *WarmPool) Release(entry *PoolEntry, healthy bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		// Pool is draining — surrender the entry to Stop's bulk
		// teardown path. Marking InUse=false makes the entry
		// eligible for Stop's iteration (which doesn't filter on
		// InUse — every entry gets torn down). Skipping
		// removeEntryLocked + the goroutine spawn closes the race
		// the audit-agent flagged.
		if entry != nil {
			entry.InUse = false
		}
		return
	}

	if !healthy {
		p.removeEntryLocked(entry)
		// Track the cleanup goroutine in p.wg so Stop() waits for it and we
		// don't leak pending podman rm calls across shutdown.
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.teardownEntry(context.Background(), entry, false)
		}()
		return
	}

	entry.InUse = false
	entry.LastUsedAt = time.Now()
	p.logger.Debug().
		Str("container_id", entry.ContainerID).
		Str("key", entry.Key.String()).
		Msg("released warm container back to pool")
}

// Size returns the number of entries for a key.
func (p *WarmPool) Size(key PoolKey) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byKey[key])
}

// reaperLoop evicts idle containers periodically.
func (p *WarmPool) reaperLoop(stopCh <-chan struct{}) {
	defer p.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			p.evictIdle()
		}
	}
}

func (p *WarmPool) evictIdle() {
	p.mu.Lock()
	var toEvict []*PoolEntry
	now := time.Now()
	for _, entry := range p.entries {
		if !entry.InUse && now.Sub(entry.LastUsedAt) > p.config.IdleTimeout {
			toEvict = append(toEvict, entry)
		}
	}
	for _, entry := range toEvict {
		p.removeEntryLocked(entry)
	}
	p.mu.Unlock()

	// Run each teardown in its own tracked goroutine so the reaper tick stays
	// short and multiple idle containers evict in parallel. p.wg.Wait in
	// Stop() will still drain them before returning.
	for _, entry := range toEvict {
		entry := entry // shadow for closure
		idle := now.Sub(entry.LastUsedAt)
		p.logger.Info().
			Str("container_id", entry.ContainerID).
			Str("key", entry.Key.String()).
			Dur("idle", idle).
			Msg("evicting idle warm container")
		if p.metrics != nil {
			p.metrics.IdleEvictions.WithLabelValues(entry.Key.ProjectID, entry.Key.Role).Inc()
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.teardownEntry(context.Background(), entry, true)
		}()
	}
}

// teardownEntry stops and removes a single warm container and cleans up its
// per-task scratch dir. signalShutdown=true writes a .shutdown sentinel and
// waits briefly to let the agent exit cleanly (used for idle evictions);
// unhealthy containers skip the sentinel because they may be unresponsive.
func (p *WarmPool) teardownEntry(parent context.Context, entry *PoolEntry, signalShutdown bool) {
	ctx, cancel := poolTeardownContext(parent)
	defer cancel()
	if signalShutdown {
		_ = os.WriteFile(filepath.Join(entry.InputDir, ".shutdown"), []byte("bye"), 0o600)
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	if p.manager != nil {
		_ = p.manager.StopContainer(ctx, entry.ContainerID, true)
		_ = p.manager.RemoveContainer(ctx, entry.ContainerID, true)
	}
	_ = os.RemoveAll(filepath.Dir(entry.InputDir))
}

func (p *WarmPool) removeEntryLocked(entry *PoolEntry) {
	if entry == nil {
		return
	}
	p.recordPoolRemovalLocked(entry)
	if entry.ContainerID != "" {
		delete(p.entries, entry.ContainerID)
	}
	entries := p.byKey[entry.Key]
	for i, e := range entries {
		if e == entry || (entry.ContainerID != "" && e.ContainerID == entry.ContainerID) {
			p.byKey[entry.Key] = append(entries[:i], entries[i+1:]...)
			break
		}
	}
}

func (p *WarmPool) recordPoolRemovalLocked(entry *PoolEntry) {
	if p.metrics == nil || entry == nil || entry.ContainerID == "" {
		return
	}
	if _, ok := p.entries[entry.ContainerID]; ok {
		p.metrics.PoolSize.WithLabelValues(entry.Key.ProjectID, entry.Key.Role).Dec()
	}
}

func waitGroupWithContext(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	if ctx == nil {
		<-done
		return true
	}
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func poolTeardownContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		return context.WithTimeout(context.Background(), 10*time.Second)
	}
	if _, ok := parent.Deadline(); ok {
		return parent, func() {}
	}
	return context.WithTimeout(parent, 10*time.Second)
}
