// Package config provides configuration loading and hot reload capabilities.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Watcher watches configuration files for changes.
type Watcher struct {
	paths    []string
	interval time.Duration
	lastMod  map[string]time.Time
	onChange func(changed []string)
	logger   zerolog.Logger

	mu      sync.RWMutex
	running bool
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// WatcherOption configures the watcher.
type WatcherOption func(*Watcher)

// WithWatchInterval sets the polling interval.
func WithWatchInterval(d time.Duration) WatcherOption {
	return func(w *Watcher) {
		w.interval = d
	}
}

// WithWatchLogger sets the logger.
func WithWatchLogger(logger zerolog.Logger) WatcherOption {
	return func(w *Watcher) {
		w.logger = logger
	}
}

// NewWatcher creates a new configuration watcher.
func NewWatcher(paths []string, opts ...WatcherOption) *Watcher {
	w := &Watcher{
		paths:    paths,
		interval: 5 * time.Second,
		lastMod:  make(map[string]time.Time),
		stopCh:   make(chan struct{}),
		logger:   zerolog.Nop(),
	}

	for _, opt := range opts {
		opt(w)
	}

	return w
}

// OnChange sets the callback for when files change.
func (w *Watcher) OnChange(fn func(changed []string)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onChange = fn
}

// Start begins watching for changes.
func (w *Watcher) Start(ctx context.Context) error {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return nil
	}
	if w.stopCh == nil {
		w.stopCh = make(chan struct{})
	}
	stopCh := w.stopCh
	w.running = true
	w.mu.Unlock()

	// Initial scan to populate lastMod
	w.scan()

	w.wg.Add(1)
	go w.loop(ctx, stopCh)

	w.logger.Info().
		Strs("paths", w.paths).
		Dur("interval", w.interval).
		Msg("config watcher started")

	return nil
}

// Stop stops the watcher.
func (w *Watcher) Stop() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	w.running = false
	stopCh := w.stopCh
	w.stopCh = nil
	w.mu.Unlock()

	if stopCh != nil {
		close(stopCh)
	}
	w.wg.Wait()

	w.logger.Info().Msg("config watcher stopped")
}

func (w *Watcher) loop(ctx context.Context, stopCh <-chan struct{}) {
	defer w.wg.Done()

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-ticker.C:
			changed := w.scan()
			if len(changed) > 0 {
				w.logger.Debug().
					Strs("changed", changed).
					Msg("config files changed")
				w.mu.RLock()
				onChange := w.onChange
				w.mu.RUnlock()
				if onChange != nil {
					onChange(changed)
				}
			}
		}
	}
}

// scan checks all watched paths for modifications.
func (w *Watcher) scan() []string {
	var changed []string
	currentFiles := make(map[string]struct{})

	for _, path := range w.paths {
		info, err := os.Stat(path)
		if err != nil {
			if w.markMissing(path) {
				changed = append(changed, path)
			}
			continue
		}

		if info.IsDir() {
			if err := filepath.Walk(path, func(filePath string, fi os.FileInfo, err error) error {
				if err != nil || fi.IsDir() {
					return nil
				}
				ext := strings.ToLower(filepath.Ext(filePath))
				if ext != ".yaml" && ext != ".yml" {
					return nil
				}
				currentFiles[filePath] = struct{}{}
				if w.checkFile(filePath, fi) {
					changed = append(changed, filePath)
				}
				return nil
			}); err != nil {
				w.logger.Warn().
					Err(err).
					Str("path", path).
					Msg("failed to walk config directory")
			}
		} else {
			currentFiles[path] = struct{}{}
			if w.checkFile(path, info) {
				changed = append(changed, path)
			}
		}
	}

	changed = append(changed, w.pruneMissing(currentFiles)...)

	return changed
}

func (w *Watcher) checkFile(path string, info os.FileInfo) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	modTime := info.ModTime()
	lastMod, exists := w.lastMod[path]

	if !exists {
		w.lastMod[path] = modTime
		return false
	}

	if modTime.After(lastMod) {
		w.lastMod[path] = modTime
		return true
	}
	return false
}

func (w *Watcher) markMissing(path string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, exists := w.lastMod[path]; exists {
		delete(w.lastMod, path)
		return true
	}
	return false
}

func (w *Watcher) pruneMissing(currentFiles map[string]struct{}) []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	var changed []string
	for path := range w.lastMod {
		if _, exists := currentFiles[path]; exists {
			continue
		}
		// Only prune files that belong to currently watched paths.
		for _, watched := range w.paths {
			if path == watched || strings.HasPrefix(path, watched+string(filepath.Separator)) {
				delete(w.lastMod, path)
				changed = append(changed, path)
				break
			}
		}
	}
	return changed
}

// AddPath adds a path to watch.
func (w *Watcher) AddPath(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.paths = append(w.paths, path)
}

// RemovePath removes a path from watching.
func (w *Watcher) RemovePath(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for i, p := range w.paths {
		if p == path {
			w.paths = append(w.paths[:i], w.paths[i+1:]...)
			break
		}
	}
	delete(w.lastMod, path)
}

// ConfigReloader coordinates config reload operations.
type ConfigReloader struct {
	watcher   *Watcher
	loader    func() error
	validator func() error
	activator func() error
	logger    zerolog.Logger

	// reloadMu serializes entire Reload() cycles. Reload has many
	// concurrent triggers — SIGHUP, POST /config/reload, the 5s file
	// watcher, retryPendingLoop, the LISTEN/NOTIFY peer broadcast, the
	// workflow applier, the project wizard — and the loader/validator/
	// activator trio mutates the Registry's single staged slot. With
	// the phases unserialized, reload B's Stage() could overwrite the
	// set reload A had just validated, and A's ActivateStaged() then
	// promoted B's NOT-yet-validated config (the in-flight-task
	// conflict gate was also computed against the wrong set) —
	// bug-sweep follow-up 2026-06-04. r.mu below stays a short-hold
	// lock for the status fields so Status() readers never block
	// behind a running reload.
	reloadMu sync.Mutex

	mu                sync.RWMutex
	reloadErrors      []string
	reloadWarnings    []string
	lastReload        time.Time
	lastAttempt       time.Time
	pendingActivation bool
	blocked           bool
	blockedReason     string
	// metrics observes each Reload() cycle on Prometheus (audit R7).
	// Nil-safe: unset on SQLite / test rigs that never call SetMetrics.
	metrics *Metrics
	// postReloadHook fires after every successful Reload(). Used
	// by multi-instance deployments to broadcast the reload over
	// postgres LISTEN/NOTIFY so peer replicas refresh their
	// in-process caches too. nil = single-process behaviour
	// (only the receiving instance reloads).
	postReloadHook func()
}

// ActivationBlockedError indicates a staged config is valid but cannot be activated yet.
type ActivationBlockedError struct {
	Reason string
}

func (e *ActivationBlockedError) Error() string {
	if e == nil || e.Reason == "" {
		return "activation blocked"
	}
	return e.Reason
}

// NewConfigReloader creates a new reloader.
func NewConfigReloader(watcher *Watcher, logger zerolog.Logger) *ConfigReloader {
	return &ConfigReloader{
		watcher: watcher,
		logger:  logger,
	}
}

// SetLoader sets the config loading function.
func (r *ConfigReloader) SetLoader(fn func() error) {
	r.loader = fn
}

// SetValidator sets the config validation function.
func (r *ConfigReloader) SetValidator(fn func() error) {
	r.validator = fn
}

// SetActivator sets the config activation function.
func (r *ConfigReloader) SetActivator(fn func() error) {
	r.activator = fn
}

// SetMetrics wires the Prometheus reload collectors so every Reload()
// cycle bumps the outcome counter and refreshes the validation-error /
// last-reload-timestamp / staged-pending gauges. Nil-safe.
func (r *ConfigReloader) SetMetrics(m *Metrics) {
	r.metrics = m
}

// SetPostReloadHook installs a hook invoked exactly once after
// every successful Reload(). Multi-instance deployments use this
// to fire a postgres NOTIFY so peer replicas refresh their
// in-process caches; single-process deployments leave it nil.
//
// The hook MUST be cheap + non-blocking (it runs on the reload
// hot path). Any error is the hook's to handle — Reload doesn't
// propagate it back, because a successful reload on the local
// instance shouldn't be reported as a failure just because the
// broadcast didn't reach peers (the next reload event catches
// up; this is at-most-once + best-effort).
func (r *ConfigReloader) SetPostReloadHook(fn func()) {
	r.postReloadHook = fn
}

// Start begins watching and auto-reloading.
func (r *ConfigReloader) Start(ctx context.Context) error {
	r.watcher.OnChange(func(changed []string) {
		r.logger.Info().Strs("files", changed).Msg("config change detected")
		if err := r.Reload(); err != nil {
			r.logger.Error().Err(err).Msg("config reload failed")
		}
	})
	if err := r.watcher.Start(ctx); err != nil {
		return err
	}

	go r.retryPendingLoop(ctx)
	return nil
}

// Stop stops the reloader.
func (r *ConfigReloader) Stop() {
	r.watcher.Stop()
}

// RecordReloadWarning appends a non-fatal warning to the current
// reload cycle's warning list. Visible via Status().Warnings and
// echoed in the Reload HTTP response. Used by the validator wiring
// to surface partial-success conditions — most importantly,
// projects stripped from the staged set because their referenced
// workflows/swarms didn't resolve.
//
// Pre-2026-05-27 these warnings only landed in the daemon's WARN
// log; operators hit "reload succeeded but my project is missing"
// and had no programmatic signal to diagnose. The warning surface
// closes that gap without changing the success/failure contract
// (a strip is still a successful reload — it just didn't activate
// everything the operator dropped on disk).
func (r *ConfigReloader) RecordReloadWarning(msg string) {
	if msg == "" {
		return
	}
	r.mu.Lock()
	r.reloadWarnings = append(r.reloadWarnings, msg)
	r.mu.Unlock()
}

// Reload performs a full reload cycle.
func (r *ConfigReloader) Reload() error {
	// One reload cycle at a time — see the reloadMu field doc.
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	r.mu.Lock()
	r.lastAttempt = time.Now()
	r.reloadErrors = nil
	r.reloadWarnings = nil
	r.pendingActivation = false
	r.blocked = false
	r.blockedReason = ""
	r.mu.Unlock()

	start := time.Now()

	if r.loader != nil {
		if err := r.loader(); err != nil {
			r.mu.Lock()
			r.reloadErrors = append(r.reloadErrors, "load: "+err.Error())
			r.pendingActivation = false
			nErr := len(r.reloadErrors)
			r.mu.Unlock()
			r.metrics.observeReload(false, nErr, false, time.Now())
			return fmt.Errorf("load failed: %w", err)
		}
		r.mu.Lock()
		r.pendingActivation = true
		r.mu.Unlock()
	}

	if r.validator != nil {
		if err := r.validator(); err != nil {
			r.mu.Lock()
			r.reloadErrors = append(r.reloadErrors, "validate: "+err.Error())
			var blockedErr *ActivationBlockedError
			if errors.As(err, &blockedErr) {
				r.blocked = true
				r.blockedReason = blockedErr.Error()
				r.pendingActivation = true
			} else {
				r.pendingActivation = false
			}
			nErr := len(r.reloadErrors)
			pending := r.pendingActivation
			r.mu.Unlock()
			r.metrics.observeReload(false, nErr, pending, time.Now())
			return fmt.Errorf("validation failed: %w", err)
		}
	}

	if r.activator != nil {
		if err := r.activator(); err != nil {
			r.mu.Lock()
			r.reloadErrors = append(r.reloadErrors, "activate: "+err.Error())
			var blockedErr *ActivationBlockedError
			if errors.As(err, &blockedErr) {
				r.blocked = true
				r.blockedReason = blockedErr.Error()
				r.pendingActivation = true
			} else {
				r.pendingActivation = false
			}
			nErr := len(r.reloadErrors)
			pending := r.pendingActivation
			r.mu.Unlock()
			r.metrics.observeReload(false, nErr, pending, time.Now())
			return fmt.Errorf("activation failed: %w", err)
		}
	}

	r.mu.Lock()
	r.lastReload = time.Now()
	r.pendingActivation = false
	r.blocked = false
	r.blockedReason = ""
	successAt := r.lastReload
	hook := r.postReloadHook
	r.mu.Unlock()
	// Successful cycle: 0 validation errors, no staged-pending.
	r.metrics.observeReload(true, 0, false, successAt)
	r.logger.Info().Dur("duration", time.Since(start)).Msg("config reloaded successfully")
	// Fire the post-reload hook AFTER the success log so the
	// success line lands first in the operator's tail — the hook
	// may emit its own log line (e.g. "config reload broadcast
	// to peers"). Best-effort: a panicking hook would otherwise
	// poison the return path, so isolate it in a recover.
	if hook != nil {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					r.logger.Warn().
						Interface("panic", rec).
						Msg("config reload: postReloadHook panicked; ignoring")
				}
			}()
			hook()
		}()
	}
	return nil
}

// Status returns the current reload status.
func (r *ConfigReloader) Status() ReloadStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return ReloadStatus{
		LastReload:        r.lastReload,
		LastAttempt:       r.lastAttempt,
		Errors:            append([]string(nil), r.reloadErrors...),
		HasErrors:         len(r.reloadErrors) > 0,
		Warnings:          append([]string(nil), r.reloadWarnings...),
		HasWarnings:       len(r.reloadWarnings) > 0,
		PendingActivation: r.pendingActivation,
		Blocked:           r.blocked,
		BlockedReason:     r.blockedReason,
	}
}

// ReloadStatus contains information about the reload state.
type ReloadStatus struct {
	LastReload  time.Time `json:"last_reload"`
	LastAttempt time.Time `json:"last_attempt"`
	Errors      []string  `json:"errors,omitempty"`
	HasErrors   bool      `json:"has_errors"`
	// Warnings captures non-fatal conditions from the last reload
	// — most commonly, projects stripped from the staged set
	// because their referenced workflows/swarms didn't resolve.
	// A reload with warnings still counts as a SUCCESS (status
	// 200, success: true) — the warnings flag that the active
	// config doesn't reflect everything on disk.
	Warnings          []string `json:"warnings,omitempty"`
	HasWarnings       bool     `json:"has_warnings"`
	PendingActivation bool     `json:"pending_activation"`
	Blocked           bool     `json:"blocked"`
	BlockedReason     string   `json:"blocked_reason,omitempty"`
}

func (r *ConfigReloader) retryPendingLoop(ctx context.Context) {
	interval := 5 * time.Second
	if r.watcher != nil && r.watcher.interval > 0 {
		interval = r.watcher.interval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status := r.Status()
			if !status.PendingActivation || !status.Blocked {
				continue
			}
			r.logger.Info().Str("reason", status.BlockedReason).Msg("retrying blocked config activation")
			if err := r.Reload(); err != nil {
				r.logger.Debug().Err(err).Msg("blocked config activation still pending")
			}
		}
	}
}
