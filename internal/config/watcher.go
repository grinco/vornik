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

// watchedConfigExts are the file extensions the watcher tracks for hot reload.
// projects/*.yaml are the classic case; swarms/workflows are SWARM.md/
// WORKFLOW.md and the role library is *.md, so .md must be tracked too (before
// 2026-07-17 only YAML was, so a swarm/workflow .md edit didn't reload — see
// commit ce804dd7 / backlog 2026-07-08).
//
// Safe because a re-stage is idempotent, coalesced (one onChange per scan tick)
// and reloadMu-serialised. Two edge notes: (a) a stray non-config .md under
// swarms//workflows/ would fail to parse — the loaders skip README.md and
// otherwise fail closed (keep-last-good), which is intended; (b) role-library
// is read FRESH at every eval (rolelibrary.Load, no in-memory cache), so a
// role-library .md edit needs no reload to take effect — tracking it just fires
// a harmless no-op re-stage (the registry re-stage doesn't read role-library).
var watchedConfigExts = map[string]struct{}{
	".yaml": {},
	".yml":  {},
	".md":   {},
}

// isWatchedConfigExt reports whether the file's extension is one the watcher
// tracks for hot reload.
func isWatchedConfigExt(path string) bool {
	_, ok := watchedConfigExts[strings.ToLower(filepath.Ext(path))]
	return ok
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
				if !isWatchedConfigExt(filePath) {
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

	// reloadBound caps how long a watcher/retry-triggered reload may hold the
	// scan loop before it returns ReloadDeferred (the cycle then finishes in
	// the background). Defaults to watchReloadBound; overridable in tests.
	reloadBound time.Duration

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
		watcher:     watcher,
		logger:      logger,
		reloadBound: watchReloadBound,
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

// watchReloadBound caps how long a watcher-triggered reload may hold the scan
// loop. A healthy reload completes well within this; a slow or wedged reload
// (e.g. an offline MCP server stalling initMCP inside the activator) returns
// ReloadDeferred and keeps running in a background goroutine — the scan loop
// stays live and keeps detecting further edits instead of wedging.
//
// This is the fix for the 2026-07-08 watcher wedge: an offline MCP server hung
// initMCP, the unbounded Reload() called synchronously here never returned, and
// the scan loop stalled for ~35 min (no live config change applied). The
// bounded TryReload primitive existed (built for the 2026-07-06 config-save
// wedge) but was never wired into the watcher's own trigger — only the UI save
// path. See https://docs.vornik.io
const watchReloadBound = 10 * time.Second

// Start begins watching and auto-reloading.
func (r *ConfigReloader) Start(ctx context.Context) error {
	r.watcher.OnChange(r.handleWatchedChange)
	if err := r.watcher.Start(ctx); err != nil {
		return err
	}

	go r.retryPendingLoop(ctx)
	return nil
}

// handleWatchedChange is the watcher's OnChange callback. It runs ON the scan-
// loop goroutine, so it MUST NOT block: a synchronous unbounded Reload() here
// blocks the loop — permanently if the cycle never returns (2026-07-08 wedge:
// an offline MCP server hung initMCP inside the activator). It uses the bounded
// TryReload instead; on deferral the cycle finishes (and applies) in a
// background goroutine while the loop keeps scanning for further edits.
func (r *ConfigReloader) handleWatchedChange(changed []string) {
	r.logger.Info().Strs("files", changed).Msg("config change detected")
	bound := r.reloadBound
	if bound <= 0 {
		bound = watchReloadBound
	}
	switch outcome, err := r.TryReload(bound); outcome {
	case ReloadApplied:
		// finishReloadSuccess already logged "config reloaded successfully".
	case ReloadDeferred:
		r.logger.Warn().Dur("bound", bound).
			Msg("config reload deferred (slow/busy reloader — e.g. an offline MCP server); applying in background, scan loop stays live")
	case ReloadBlocked:
		r.logger.Info().Err(err).
			Msg("config reload activation blocked (e.g. in-flight tasks); retry loop will re-attempt")
	case ReloadFailed:
		r.logger.Error().Err(err).Msg("config reload failed")
	}
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
	return r.runCycle()
}

// runCycle runs one loader → validator → activator cycle and updates the
// status fields. The caller MUST hold reloadMu: Reload() takes it directly;
// TryReload()'s watchdog goroutine takes it before calling runCycle.
func (r *ConfigReloader) runCycle() error {
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

	r.finishReloadSuccess(start)
	return nil
}

// finishReloadSuccess records a clean reload cycle: it clears the
// error/pending/blocked status, observes success metrics, logs, and fires
// the best-effort post-reload hook. Extracted from runCycle to keep that
// function within the funlen budget. Caller holds reloadMu.
func (r *ConfigReloader) finishReloadSuccess(start time.Time) {
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
}

// ReloadOutcome classifies the result of a bounded TryReload attempt.
type ReloadOutcome int

const (
	// ReloadApplied — the cycle completed cleanly within the bound.
	ReloadApplied ReloadOutcome = iota
	// ReloadDeferred — the reloader was busy/wedged (lock held) or the
	// cycle exceeded the bound. The edit is on disk; a daemon restart is
	// required to apply it.
	ReloadDeferred
	// ReloadBlocked — activation is gated (e.g. in-flight tasks). The edit
	// is on disk and will apply on a later reload or a restart.
	ReloadBlocked
	// ReloadFailed — validation/activation returned a hard error.
	ReloadFailed
)

// TryReload attempts a reload bounded by d. It never blocks the caller
// longer than d. See
// https://docs.vornik.io
//
//   - If reloadMu is already held (a reload in progress, or wedged like the
//     2026-07-06 incident), it returns ReloadDeferred immediately — no block.
//   - Otherwise it runs the cycle in a watchdog goroutine and waits up to d.
//     Completing in time yields ReloadApplied / ReloadBlocked / ReloadFailed.
//     Exceeding d yields ReloadDeferred; the goroutine keeps ownership of
//     reloadMu and Unlocks when the cycle returns. If the cycle wedges
//     forever, later TryReload calls take the TryLock fast-path and also
//     return ReloadDeferred — the system degrades to restart-required
//     everywhere, never a hang.
func (r *ConfigReloader) TryReload(d time.Duration) (ReloadOutcome, error) {
	if !r.reloadMu.TryLock() {
		return ReloadDeferred, nil
	}
	done := make(chan error, 1)
	go func() {
		defer r.reloadMu.Unlock()
		done <- r.runCycle()
	}()
	select {
	case err := <-done:
		switch {
		case err == nil:
			return ReloadApplied, nil
		case isActivationBlocked(err):
			return ReloadBlocked, err
		default:
			return ReloadFailed, err
		}
	case <-time.After(d):
		return ReloadDeferred, nil
	}
}

// isActivationBlocked reports whether err is (or wraps) an
// *ActivationBlockedError — the in-flight-task activation gate.
func isActivationBlocked(err error) bool {
	var blocked *ActivationBlockedError
	return errors.As(err, &blocked)
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
			// Bounded, like the watcher trigger: this retry goroutine must not
			// wedge on a stalled reload cycle either.
			bound := r.reloadBound
			if bound <= 0 {
				bound = watchReloadBound
			}
			if outcome, err := r.TryReload(bound); outcome != ReloadApplied {
				r.logger.Debug().Err(err).Msg("blocked config activation still pending")
			}
		}
	}
}
