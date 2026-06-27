// Package service provides service-level coordination for vornik.
package service

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// DependencyStatus represents the health of a critical dependency.
type DependencyStatus struct {
	Name    string    // e.g., "podman", "postgresql"
	Healthy bool      // true if the dependency is available
	Message string    // human-readable status message
	Since   time.Time // when the status last changed
}

// DegradedModeTracker tracks dependency health and degraded mode state.
type DegradedModeTracker struct {
	mu            sync.RWMutex
	deps          map[string]DependencyStatus
	degraded      bool
	degradedSince time.Time
	logger        zerolog.Logger

	onEnterDegraded func()
	onExitDegraded  func()
}

// NewDegradedModeTracker creates a new tracker.
func NewDegradedModeTracker(logger zerolog.Logger) *DegradedModeTracker {
	return &DegradedModeTracker{
		deps:   make(map[string]DependencyStatus),
		logger: logger,
	}
}

// UpdateDependency updates the status of a dependency.
func (t *DegradedModeTracker) UpdateDependency(name string, healthy bool, message string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	oldStatus, exists := t.deps[name]
	wasHealthy := !exists || oldStatus.Healthy

	t.deps[name] = DependencyStatus{
		Name:    name,
		Healthy: healthy,
		Message: message,
		Since:   now,
	}

	// Check if we're entering or exiting degraded mode
	allHealthy := true
	for _, dep := range t.deps {
		if !dep.Healthy {
			allHealthy = false
			break
		}
	}

	if allHealthy && t.degraded {
		// Exiting degraded mode
		t.degraded = false
		t.logger.Info().
			Str("degraded_duration", time.Since(t.degradedSince).String()).
			Msg("service recovered from degraded mode")
		if t.onExitDegraded != nil {
			t.onExitDegraded()
		}
	} else if !allHealthy && !t.degraded {
		// Entering degraded mode
		t.degraded = true
		t.degradedSince = now
		t.logger.Warn().
			Strs("unhealthy_deps", t.getUnhealthyDepsLocked()).
			Msg("service entering degraded mode")
		if t.onEnterDegraded != nil {
			t.onEnterDegraded()
		}
	}

	// Log individual dependency status changes
	if wasHealthy != healthy {
		if healthy {
			t.logger.Info().Str("dependency", name).Msg("dependency recovered")
		} else {
			t.logger.Warn().
				Str("dependency", name).
				Str("message", message).
				Msg("dependency unhealthy")
		}
	}
}

// IsDegraded returns true if the service is in degraded mode.
func (t *DegradedModeTracker) IsDegraded() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.degraded
}

// GetStatus returns the current status of all dependencies.
func (t *DegradedModeTracker) GetStatus() map[string]DependencyStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make(map[string]DependencyStatus, len(t.deps))
	for k, v := range t.deps {
		result[k] = v
	}
	return result
}

// GetUnhealthyDeps returns names of currently unhealthy dependencies.
func (t *DegradedModeTracker) GetUnhealthyDeps() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.getUnhealthyDepsLocked()
}

func (t *DegradedModeTracker) getUnhealthyDepsLocked() []string {
	var unhealthy []string
	for name, dep := range t.deps {
		if !dep.Healthy {
			unhealthy = append(unhealthy, name)
		}
	}
	return unhealthy
}

// OnEnterDegraded sets a callback for entering degraded mode.
func (t *DegradedModeTracker) OnEnterDegraded(fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onEnterDegraded = fn
}

// OnExitDegraded sets a callback for exiting degraded mode.
func (t *DegradedModeTracker) OnExitDegraded(fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onExitDegraded = fn
}

// DegradedModeHandler provides request handling for degraded mode.
type DegradedModeHandler struct {
	tracker *DegradedModeTracker
}

// NewDegradedModeHandler creates a new handler.
func NewDegradedModeHandler(tracker *DegradedModeTracker) *DegradedModeHandler {
	return &DegradedModeHandler{tracker: tracker}
}

// CheckReturnsError returns an error if the service is in degraded mode
// and the request cannot be served.
func (h *DegradedModeHandler) CheckReturnsError(operation string) error {
	if !h.tracker.IsDegraded() {
		return nil
	}

	unhealthy := h.tracker.GetUnhealthyDeps()
	return &DegradedModeError{
		Operation:     operation,
		UnhealthyDeps: unhealthy,
	}
}

// CanServe returns true if the operation can be served in degraded mode.
// Some operations (like task listing) can still work without the scheduler.
func (h *DegradedModeHandler) CanServe(operation string) bool {
	if !h.tracker.IsDegraded() {
		return true
	}

	// Operations that can be served in degraded mode
	// (e.g., read-only operations that only need the database)
	readOnlyOps := map[string]bool{
		"list_tasks":      true,
		"get_task":        true,
		"list_executions": true,
		"get_execution":   true,
		"get_status":      true,
	}

	if readOnlyOps[operation] {
		// Check if only the scheduler/runtime is unhealthy (DB is fine)
		status := h.tracker.GetStatus()
		if dbStatus, ok := status["postgresql"]; ok && dbStatus.Healthy {
			return true
		}
	}

	return false
}

// DegradedModeError is returned when an operation cannot be served
// due to degraded mode.
type DegradedModeError struct {
	Operation     string
	UnhealthyDeps []string
}

func (e *DegradedModeError) Error() string {
	return "cannot perform " + e.Operation + " in degraded mode (unhealthy: " + joinDeps(e.UnhealthyDeps) + ")"
}

func joinDeps(deps []string) string {
	if len(deps) == 0 {
		return "none"
	}
	result := deps[0]
	for i := 1; i < len(deps); i++ {
		result += ", " + deps[i]
	}
	return result
}

// DependencyHealthChecker periodically checks dependency health.
type DependencyHealthChecker struct {
	tracker  *DegradedModeTracker
	checkers map[string]func() (bool, string)
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewDependencyHealthChecker creates a new health checker.
func NewDependencyHealthChecker(tracker *DegradedModeTracker, interval time.Duration) *DependencyHealthChecker {
	return &DependencyHealthChecker{
		tracker:  tracker,
		checkers: make(map[string]func() (bool, string)),
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// RegisterChecker registers a health check function for a dependency.
func (c *DependencyHealthChecker) RegisterChecker(name string, check func() (bool, string)) {
	c.checkers[name] = check
}

// Start begins periodic health checking.
func (c *DependencyHealthChecker) Start(ctx context.Context) {
	c.wg.Add(1)
	go c.loop(ctx)
}

// Stop stops the health checker.
func (c *DependencyHealthChecker) Stop() {
	close(c.stopCh)
	c.wg.Wait()
}

func (c *DependencyHealthChecker) loop(ctx context.Context) {
	defer c.wg.Done()

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	// Initial check
	c.checkAll()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.checkAll()
		}
	}
}

func (c *DependencyHealthChecker) checkAll() {
	for name, check := range c.checkers {
		healthy, message := check()
		c.tracker.UpdateDependency(name, healthy, message)
	}
}
