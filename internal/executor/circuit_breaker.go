package executor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// AutonomyController is the narrow interface the circuit breaker
// uses to flip a project's autonomy off when its failure rate
// crosses the configured threshold. Production wires this to
// *registry.Registry; tests can stub it without dragging the full
// registry in.
type AutonomyController interface {
	// SetProjectAutonomyEnabled flips autonomy.enabled on the named
	// project both in memory and on disk. Returns an error if the
	// project doesn't exist or the on-disk write fails — the
	// breaker logs the error but doesn't propagate it; the in-memory
	// flip succeeds even when disk persistence fails, which is the
	// intent (the breaker should still pause autonomy in this
	// process even if the persisted value didn't update).
	SetProjectAutonomyEnabled(projectID string, enabled bool) error
}

// circuitBreaker is the per-project failure-rate guard. Constructed
// in container.go when autonomy.circuit_breaker.enabled is true. The
// executor calls Trip(...) from handleFailure after a task is
// marked FAILED with a class that should count toward the breaker.
//
// Concurrency: the breaker uses a per-project lock to serialize
// trip evaluations. Without this two near-simultaneous failures on
// the same project could both exceed the threshold and both fire
// the "pausing autonomy" notification, doubling the alert noise.
type circuitBreaker struct {
	taskRepo    persistence.TaskRepository
	autonomy    AutonomyController
	notifier    CompletionNotifier
	threshold   int
	window      time.Duration
	skipClasses map[string]struct{}
	logger      zerolog.Logger

	// trippedRecently dedupes alerts within a cooldown so a project
	// that's still failing post-trip doesn't generate one alert per
	// failure. Keyed by project ID, value is the time the trip
	// fired. The cooldown matches the rolling window — once the
	// window is past, the breaker is allowed to fire again (the
	// operator may have re-enabled autonomy in between).
	trippedRecently map[string]time.Time
	mu              sync.Mutex
}

// NewCircuitBreakerForExecutor is the exported constructor used by
// the service container; the unexported newCircuitBreaker is the
// internal entry point so tests in the same package can build one
// without going through the container's wiring.
func NewCircuitBreakerForExecutor(
	taskRepo persistence.TaskRepository,
	autonomy AutonomyController,
	notifier CompletionNotifier,
	threshold int,
	window time.Duration,
	skipClasses []string,
	logger zerolog.Logger,
) *circuitBreaker {
	return newCircuitBreaker(taskRepo, autonomy, notifier, threshold, window, skipClasses, logger)
}

// newCircuitBreaker constructs a breaker. Returns nil when the
// inputs don't satisfy the minimum required to function (no
// taskRepo, no autonomy controller, threshold ≤ 0) — callers
// should treat nil as "disabled" and skip Trip calls.
func newCircuitBreaker(
	taskRepo persistence.TaskRepository,
	autonomy AutonomyController,
	notifier CompletionNotifier,
	threshold int,
	window time.Duration,
	skipClasses []string,
	logger zerolog.Logger,
) *circuitBreaker {
	if taskRepo == nil || autonomy == nil || threshold <= 0 || window <= 0 {
		return nil
	}
	skip := make(map[string]struct{}, len(skipClasses))
	for _, c := range skipClasses {
		skip[c] = struct{}{}
	}
	return &circuitBreaker{
		taskRepo:        taskRepo,
		autonomy:        autonomy,
		notifier:        notifier,
		threshold:       threshold,
		window:          window,
		skipClasses:     skip,
		logger:          logger,
		trippedRecently: map[string]time.Time{},
	}
}

// Trip evaluates whether the project's recent failure count crosses
// the breaker's threshold and, if so, disables autonomy and notifies.
// errorClass is the failure class of the task that just failed —
// classes in skipClasses don't count toward the breaker (so
// CANCELLED tasks don't trip on operator-initiated stops).
//
// Idempotent: re-tripping while already in cooldown is a no-op and
// emits no extra alert. The cooldown matches the rolling window so
// an operator who re-enables autonomy mid-window can have the
// breaker fire again on a fresh wave of failures.
func (b *circuitBreaker) Trip(ctx context.Context, task *persistence.Task, errorClass string) {
	if b == nil || task == nil || task.ProjectID == "" {
		return
	}
	if _, skip := b.skipClasses[errorClass]; skip {
		return
	}
	if b.recentlyTripped(task.ProjectID) {
		return
	}

	since := time.Now().Add(-b.window)
	count, err := b.taskRepo.CountRecentFailures(ctx, task.ProjectID, nil, since)
	if err != nil {
		b.logger.Warn().
			Err(err).
			Str("project_id", task.ProjectID).
			Msg("circuit breaker: failed to count recent failures — skipping evaluation")
		return
	}
	if count < b.threshold {
		return
	}

	b.mu.Lock()
	if last, ok := b.trippedRecently[task.ProjectID]; ok && time.Since(last) < b.window {
		// Lost the race — another goroutine already tripped. Bail
		// without re-disabling or re-alerting.
		b.mu.Unlock()
		return
	}
	b.trippedRecently[task.ProjectID] = time.Now()
	b.mu.Unlock()

	if err := b.autonomy.SetProjectAutonomyEnabled(task.ProjectID, false); err != nil {
		// Disk write may have failed even though the in-memory flip
		// succeeded. Log loudly but proceed — autonomy IS paused in
		// this process, and the operator gets the alert below so
		// they can verify config state on the next reload.
		b.logger.Error().
			Err(err).
			Str("project_id", task.ProjectID).
			Msg("circuit breaker: SetProjectAutonomyEnabled failed (in-memory flip may have succeeded)")
	}

	b.logger.Warn().
		Str("project_id", task.ProjectID).
		Str("trigger_task_id", task.ID).
		Str("trigger_class", errorClass).
		Int("count", count).
		Int("threshold", b.threshold).
		Dur("window", b.window).
		Msg("circuit breaker tripped: autonomy paused on project")

	if b.notifier != nil {
		alertMsg := fmt.Sprintf(
			"Circuit breaker tripped on project %q.\n"+
				"Last %s saw %d task failures (threshold: %d). Autonomy is now paused.\n"+
				"Latest failure: task %s — %s\n"+
				"Re-enable with: vornikctl autonomy enable %s",
			task.ProjectID, b.window, count, b.threshold,
			task.ID, errorClass, task.ProjectID)
		// Reuse NotifyTaskCompleted with success=false; that's the
		// existing alert channel and the dedicated breaker channel
		// can come later if the message gets noisy.
		b.notifier.NotifyTaskCompleted(ctx, task, false, alertMsg)
	}
}

func (b *circuitBreaker) recentlyTripped(projectID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	last, ok := b.trippedRecently[projectID]
	if !ok {
		return false
	}
	return time.Since(last) < b.window
}
