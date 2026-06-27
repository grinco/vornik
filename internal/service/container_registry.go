package service

// Project-registry wiring extracted from container.go as part of
// the 2026-05-16 service-package split. Owns:
//   - buildSecretsDetector       (secrets-config → detector + actions)
//   - initRegistry               (load + index project / swarm / workflow YAML)
//   - validateRegistryActivation (refuse a config reload that would orphan in-flight tasks)
//   - registryProjectIDsAdapter  (memory.ProjectLister adapter)
//   - resolveRegistryConfigDir + hasRegistryLayout (path resolution helpers)

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

// buildSecretsDetector translates daemon-config SecretsConfig into a
// secrets.Detector + per-checkpoint action map. Compiled patterns
// from the curated list combine with operator-supplied custom
// patterns, minus any names listed in patterns.disable. Allowlist
// extends (doesn't replace) the default. Action strings on the
// checkpoints map are validated; unknown values fall to detect-
// only with a warning so a config typo doesn't silently disable
// enforcement.
func buildSecretsDetector(cfg config.SecretsConfig) (secrets.Detector, map[string]secrets.Action, error) {
	patterns := secrets.DefaultPatterns()
	if len(cfg.Patterns.Disable) > 0 {
		disabled := make(map[string]struct{}, len(cfg.Patterns.Disable))
		for _, name := range cfg.Patterns.Disable {
			disabled[name] = struct{}{}
		}
		filtered := patterns[:0]
		for _, p := range patterns {
			if _, drop := disabled[p.Name]; !drop {
				filtered = append(filtered, p)
			}
		}
		patterns = filtered
	}
	for _, custom := range cfg.Patterns.Custom {
		patterns = append(patterns, secrets.Pattern{
			Name:        custom.Name,
			Regex:       custom.Regex,
			Description: custom.Description,
		})
	}

	allowlist := append(secrets.DefaultAllowlist(), cfg.Allowlist...)

	detector, err := secrets.NewMultiDetector(secrets.Config{
		Patterns:        patterns,
		Allowlist:       allowlist,
		EntropyDisabled: cfg.Entropy.Disabled,
		EntropyMinLen:   cfg.Entropy.MinLen,
		EntropyMinBits:  cfg.Entropy.MinBits,
	})
	if err != nil {
		return nil, nil, err
	}

	actions := make(map[string]secrets.Action, len(cfg.Checkpoints))
	for name, raw := range cfg.Checkpoints {
		a := secrets.Action(raw)
		if a.IsValid() {
			actions[name] = a
		}
		// Unknown action strings: skip silently here; the
		// caller logs the resolved-action map at startup so an
		// operator can spot a typo by comparing to what they
		// wrote in YAML.
	}
	return detector, actions, nil
}

func (c *Container) initRegistry() error {
	configDir := resolveRegistryConfigDir(c.ConfigPath)
	if configDir == "" {
		return fmt.Errorf("no registry config directory found")
	}

	reg := registry.New()
	if err := reg.Load(configDir); err != nil {
		var ve *registry.ValidationError
		if !errors.As(err, &ve) {
			return fmt.Errorf("failed to load registry from %s: %w", configDir, err)
		}
		c.Logger.Warn().Err(err).Str("config_dir", configDir).Msg("registry loaded with validation warnings; invalid projects skipped")
	}

	c.Registry = reg
	taskRepo := c.repos.Tasks
	execRepo := c.repos.Executions
	// Watch the registry dir AND config.yaml itself, so an edit to either
	// triggers a reload. config.yaml lives outside configDir (typically its
	// parent), so it needs to be in the watch set explicitly; the watcher
	// supports a plain file path alongside directories.
	watchPaths := []string{configDir}
	if c.ConfigPath != "" {
		watchPaths = append(watchPaths, c.ConfigPath)
	}
	watcher := config.NewWatcher(watchPaths,
		config.WithWatchLogger(c.Logger),
	)
	reloader := config.NewConfigReloader(watcher, c.Logger)
	reloader.SetLoader(func() error {
		if err := c.Registry.Stage(configDir); err != nil {
			return err
		}
		// Re-parse config.yaml for the keys that are safe to hot-apply
		// (memory gate knobs). Parse/validate failure aborts the whole
		// reload before activation — fail closed, leaving the running
		// config untouched. Most config.yaml keys (DB, runtime, auth, the
		// embed worker pool) are boot-only and are NOT applied live; see
		// applyHotConfig.
		if c.ConfigPath != "" {
			staged, err := config.LoadFromPath(c.ConfigPath)
			if err != nil {
				return fmt.Errorf("reload: re-parse config.yaml: %w", err)
			}
			c.stagedConfig = staged
		}
		return nil
	})
	reloader.SetValidator(func() error {
		if warn := c.Registry.StripInvalidFromStaged(); warn != nil {
			// Log AND surface to the reloader's warning channel so
			// the HTTP response + /reload-status echo the strip
			// details. Pre-2026-05-27 these landed only in journald,
			// which is why "reload succeeded but my project is
			// missing" was a debug-by-grep affair. warn is already
			// *ValidationError; iterate its Errors slice so each
			// stripped project appears as its own warning entry.
			c.Logger.Warn().Err(warn).Msg("registry reload: invalid projects stripped")
			for _, e := range warn.Errors {
				reloader.RecordReloadWarning(e.Error())
			}
		}
		return c.validateRegistryActivation(taskRepo, execRepo)
	})
	reloader.SetActivator(func() error {
		if err := c.Registry.ActivateStaged(); err != nil {
			return err
		}
		// Re-wire autonomy manager so new/changed projects get loops.
		if c.autonomyManager != nil {
			c.autonomyManager.Reload()
		}
		// Reconcile MCP servers to the newly-activated config.
		// initMCP routes through Manager.SyncProjects (build new
		// clients, swap, close displaced) — the previous
		// Close()-then-reinit here failed every in-flight tool call
		// for the duration of the reconnects on every reload
		// (bug-sweep follow-up 2026-06-04).
		c.initMCP()
		// Apply the hot-reloadable config.yaml keys (memory gate knobs) to
		// the live subsystems. Done last, after the registry activates, so a
		// registry-activation failure doesn't leave config half-applied.
		c.applyHotConfig()
		return nil
	})
	c.ConfigReloader = reloader
	if c.Executor != nil {
		c.Executor.SetWorkflowResolver(c.Registry)
	}

	c.Logger.Info().Str("config_dir", configDir).Msg("registry loaded")
	return nil
}

// applyHotConfig pushes the safe-to-hot-apply config.yaml keys from the freshly
// re-parsed staged config onto the live subsystems, then clears the staging
// slot. Currently scoped to the memory ingest-gate knobs
// (prompt_injection_scan, claim_audit_disabled_projects, deny_patterns), which
// are read per-ingest, and the logship scope allowlist
// (logging.forward.scopes), which the router reads via an atomic snapshot per
// Offer (logship LLD §7 — v1 hot-reloads scopes only; sink config is
// boot-time). Both swap atomically without a restart. The staged config is
// parsed + validated before staging, so this step fails closed — a bad
// config.yaml edit never reaches the live subsystems, which keep their
// last-good snapshot. The
// rest of config.yaml (DB, runtime, embed worker pool, auth chain, …) is
// boot-only and deliberately NOT applied here — changing it still requires a
// daemon restart. The memory pipeline holds the authoritative live value for
// these two knobs after a hot-reload; c.Config is intentionally left untouched
// to avoid racing the many goroutines that hold its pointer.
func (c *Container) applyHotConfig() {
	staged := c.stagedConfig
	c.stagedConfig = nil
	if staged == nil {
		return
	}
	if c.memoryPipeline != nil {
		c.memoryPipeline.UpdateGates(
			staged.Memory.PromptInjectionScan,
			staged.Memory.ClaimAuditDisabledProjects,
			staged.Memory.DenyPatterns,
		)
		c.Logger.Info().
			Str("prompt_injection_scan", staged.Memory.PromptInjectionScan).
			Int("claim_audit_disabled_projects", len(staged.Memory.ClaimAuditDisabledProjects)).
			Int("deny_patterns", len(staged.Memory.DenyPatterns)).
			Msg("hot-reload: applied memory ingest-gate knobs from config.yaml without restart")
	}
	// logship scope allowlist: the live router reads the allowlist via an
	// atomic snapshot per Offer, so a logging.forward.scopes edit hot-swaps
	// without a restart (logship LLD §7 — v1 hot-reloads scopes only; sink
	// config remains boot-time). Other logging.forward keys (sinks, queue,
	// batch, flush) are deliberately NOT applied here.
	if c.logshipForwarder != nil {
		switch {
		case !staged.Logging.Forward.Enabled:
			// The router + sinks are boot-time; an enabled:false edit cannot
			// tear forwarding down live. Warn so the operator knows forwarding
			// CONTINUES until restart — and do NOT touch the scope allowlist,
			// so a disable edit can't covertly widen what ships.
			c.Logger.Warn().
				Msg("hot-reload: logging.forward.enabled=false cannot take effect until restart; forwarding continues with the last-good scope allowlist")
		case len(staged.Logging.Forward.Scopes) == 0:
			// Empty scopes means ship-ALL. Refuse to widen the live allowlist
			// on hot-reload (e.g. an accidentally-deleted scopes key) — that
			// would covertly expand what's forwarded to the collector. A
			// deliberate ship-all requires a restart.
			c.Logger.Warn().
				Msg("hot-reload: empty logging.forward.scopes (ship-all) refused live to avoid covert expansion; restart to apply a ship-all allowlist")
		default:
			c.logshipForwarder.SetScopes(staged.Logging.Forward.Scopes)
			c.Logger.Info().
				Int("scopes", len(staged.Logging.Forward.Scopes)).
				Msg("hot-reload: applied logship scope allowlist from config.yaml without restart")
		}
	}
}

// NewContainerWithObservability creates a container with observability enabled.

func resolveRegistryConfigDir(configPath string) string {
	// Explicit env override wins over all discovery heuristics.
	if envDir := os.Getenv("VORNIK_CONFIGS_DIR"); envDir != "" {
		if hasRegistryLayout(envDir) {
			return envDir
		}
	}

	candidates := make([]string, 0, 3)

	if configPath != "" {
		baseDir := filepath.Dir(configPath)
		candidates = append(candidates,
			filepath.Join(baseDir, "configs"),
			baseDir,
		)
	}

	candidates = append(candidates, "configs")

	for _, candidate := range candidates {
		if hasRegistryLayout(candidate) {
			return candidate
		}
	}

	return ""
}

func hasRegistryLayout(dir string) bool {
	required := []string{"projects", "swarms", "workflows"}
	for _, name := range required {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

func (c *Container) validateRegistryActivation(taskRepo persistence.TaskRepository, execRepo persistence.ExecutionRepository) error {
	if c.Registry == nil {
		return nil
	}

	diff, err := c.Registry.DiffStaged()
	if err != nil {
		return err
	}
	if !diff.HasChanges() {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	taskConflicts, err := collectTaskConflicts(ctx, taskRepo, diff)
	if err != nil {
		return err
	}
	execConflicts, err := collectExecutionConflicts(ctx, execRepo, taskRepo, diff)
	if err != nil {
		return err
	}

	conflicts := append(taskConflicts, execConflicts...)
	if len(conflicts) == 0 {
		return nil
	}

	return &config.ActivationBlockedError{
		Reason: fmt.Sprintf("staged config conflicts with in-flight work: %s", strings.Join(conflicts, "; ")),
	}
}

func collectTaskConflicts(ctx context.Context, repo persistence.TaskRepository, diff registry.ConfigDiff) ([]string, error) {
	statuses := []persistence.TaskStatus{
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
		persistence.TaskStatusWaitingForChildren,
	}

	projectChanges := toSet(append(append([]string{}, diff.ChangedProjects...), diff.DeletedProjects...))
	workflowChanges := toSet(append(append([]string{}, diff.ChangedWorkflows...), diff.DeletedWorkflows...))

	seen := make(map[string]struct{})
	var conflicts []string

	for _, status := range statuses {
		status := status
		tasks, err := repo.List(ctx, persistence.TaskFilter{Status: &status, PageSize: 1000})
		if err != nil {
			return nil, fmt.Errorf("failed to list in-flight tasks: %w", err)
		}
		for _, task := range tasks {
			if task == nil {
				continue
			}
			if _, ok := projectChanges[task.ProjectID]; ok {
				msg := fmt.Sprintf("task %s references changed project %s", task.ID, task.ProjectID)
				if _, exists := seen[msg]; !exists {
					seen[msg] = struct{}{}
					conflicts = append(conflicts, msg)
				}
			}
			if task.WorkflowID != nil {
				if _, ok := workflowChanges[*task.WorkflowID]; ok {
					msg := fmt.Sprintf("task %s references changed workflow %s", task.ID, *task.WorkflowID)
					if _, exists := seen[msg]; !exists {
						seen[msg] = struct{}{}
						conflicts = append(conflicts, msg)
					}
				}
			}
		}
	}

	return conflicts, nil
}

func collectExecutionConflicts(ctx context.Context, execRepo persistence.ExecutionRepository, taskRepo persistence.TaskRepository, diff registry.ConfigDiff) ([]string, error) {
	statuses := []persistence.ExecutionStatus{
		persistence.ExecutionStatusPending,
		persistence.ExecutionStatusRunning,
		persistence.ExecutionStatusPaused,
	}

	projectChanges := toSet(append(append([]string{}, diff.ChangedProjects...), diff.DeletedProjects...))
	workflowChanges := toSet(append(append([]string{}, diff.ChangedWorkflows...), diff.DeletedWorkflows...))

	seen := make(map[string]struct{})
	var conflicts []string

	// Cache task statuses we've already looked up. A single
	// config-reload pass often sees the same task referenced by
	// multiple executions (parent + child + retry), so this
	// trims the N+1 cost noticeably.
	taskStatusCache := make(map[string]persistence.TaskStatus)
	taskTerminalForExec := func(taskID string) bool {
		if taskID == "" || taskRepo == nil {
			return false
		}
		if status, ok := taskStatusCache[taskID]; ok {
			return isTerminalTaskStatus(status)
		}
		task, err := taskRepo.Get(ctx, taskID)
		if err != nil || task == nil {
			// Treat lookup failure as "not terminal" (conservative
			// — the conflict gets raised, operator sees the
			// detail in the response).
			taskStatusCache[taskID] = persistence.TaskStatus("unknown")
			return false
		}
		taskStatusCache[taskID] = task.Status
		return isTerminalTaskStatus(task.Status)
	}

	for _, status := range statuses {
		status := status
		executions, err := execRepo.List(ctx, persistence.ExecutionFilter{Status: &status, PageSize: 1000})
		if err != nil {
			return nil, fmt.Errorf("failed to list in-flight executions: %w", err)
		}
		for _, exec := range executions {
			if exec == nil {
				continue
			}
			// Skip orphan executions: PAUSED rows whose task is
			// already terminal. Architecture quirk — the
			// adaptive-route flow creates a fresh continuation
			// execution after a child unblocks rather than
			// resuming the original PAUSED row, leaving the
			// original as a dead pointer. Until the orphan-
			// finalize bug lands (ROADMAP — see "Phase A —
			// orphan PAUSED execution cleanup"), the safety
			// check has to ignore them or every config reload
			// fails.
			if status == persistence.ExecutionStatusPaused && taskTerminalForExec(exec.TaskID) {
				continue
			}
			if _, ok := projectChanges[exec.ProjectID]; ok {
				msg := fmt.Sprintf("execution %s references changed project %s", exec.ID, exec.ProjectID)
				if _, exists := seen[msg]; !exists {
					seen[msg] = struct{}{}
					conflicts = append(conflicts, msg)
				}
			}
			if _, ok := workflowChanges[exec.WorkflowID]; ok {
				msg := fmt.Sprintf("execution %s references changed workflow %s", exec.ID, exec.WorkflowID)
				if _, exists := seen[msg]; !exists {
					seen[msg] = struct{}{}
					conflicts = append(conflicts, msg)
				}
			}
		}
	}

	return conflicts, nil
}

// isTerminalTaskStatus mirrors the terminal-state predicate the
// task state machine uses internally. Centralised here so the
// orphan-skip rule stays in sync with what the rest of the daemon
// considers "done".
func isTerminalTaskStatus(s persistence.TaskStatus) bool {
	switch s {
	case persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled:
		return true
	}
	return false
}

func toSet(ids []string) map[string]struct{} {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

type registryProjectIDsAdapter struct {
	registry *registry.Registry
}

func (a *registryProjectIDsAdapter) ListProjectIDs() []string {
	if a == nil || a.registry == nil {
		return nil
	}
	projects := a.registry.ListProjects()
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		if p != nil && p.ID != "" {
			out = append(out, p.ID)
		}
	}
	return out
}
