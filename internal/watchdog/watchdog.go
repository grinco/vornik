// Package watchdog detects in-flight executions that have stopped
// making forward progress and either logs them or marks them failed
// so the dashboard surfaces the problem.
//
// "Forward progress" is defined as a write to executions.updated_at —
// the executor's saveCheckpoint between every workflow step bumps
// this column on every step boundary, so a fresh updated_at is
// strong evidence the executor is alive and still walking the
// workflow. An execution where status=RUNNING but updated_at is
// older than the configured stuck threshold is the watchdog's
// signal: the executor goroutine is hung, crashed silently, or
// trapped in a too-long step.
//
// Distinct from existing recovery layers:
//   - Context-deadline TIMEOUT fires from inside the executor when a
//     step exceeds its configured timeout. The watchdog catches the
//     case where no timeout was configured, or where the executor
//     itself is unable to fire its own timeout.
//   - LEASE_EXPIRED fires from the scheduler's recovery loop when the
//     task lease has elapsed. Lease durations are typically much
//     longer (hours) than the watchdog threshold (minutes) — the
//     watchdog is the faster signal.
//
// Action policy defaults to fail (was warn-only until 2026-05-13;
// flipped after a ghost-RUNNING incident on ibkr-trader where an
// operator cancelled live tasks thinking two were running in
// parallel because the prior retry's execution row was still
// RUNNING in the table). The "fail" action marks both the execution
// row and the task row as terminal with class STUCK_EXECUTION; it
// does NOT try to kill the in-flight goroutine (the executor's
// Cancel API is the right tool for that, but a goroutine genuinely
// hung in a syscall won't respond to it anyway). The eventual
// unsticking — daemon restart, OS sigkill, or the goroutine
// returning on its own — is out of scope for the watchdog itself.
// Operators with legitimately long-running steps can opt back to
// warn via WatchdogConfig in the daemon YAML.
package watchdog

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// Action is the policy applied when the watchdog finds a stuck
// execution. Two values: "warn" logs and increments a counter; "fail"
// additionally marks the row terminal so the operator dashboard
// stops showing it as in-flight.
type Action string

const (
	ActionWarn Action = "warn"
	ActionFail Action = "fail"
)

// IsValid reports whether the action string is one the watchdog
// understands. Unknown values are coerced to ActionWarn at startup
// so a config typo doesn't silently turn the watchdog off.
func (a Action) IsValid() bool {
	return a == ActionWarn || a == ActionFail
}

// ExecutionRepository is the narrow set of execution-row operations
// the watchdog uses. Defined locally so the watchdog package can be
// tested without dragging in the full persistence dependency graph.
type ExecutionRepository interface {
	List(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error)
	RecordFailure(ctx context.Context, id string, errorMessage, errorCode string) error
	// SupersedeOrphanPausedExecutions finalizes PAUSED executions whose
	// parent task is already terminal — the global backstop for the
	// orphan-PAUSED leak. Run once per watchdog scan.
	SupersedeOrphanPausedExecutions(ctx context.Context) (int64, error)
}

// TaskRepository is the watchdog's required surface on the task
// table — used only when Action=fail to flip the task row terminal
// alongside the execution row. The Get/Update pair mirrors what
// executor.handleFailure does.
type TaskRepository interface {
	Get(ctx context.Context, id string) (*persistence.Task, error)
	Update(ctx context.Context, task *persistence.Task) error
	// List + UpdateStatus back the approval-timeout sweep: find tasks
	// parked in AWAITING_APPROVAL, cancel the ones past the timeout.
	List(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error)
	UpdateStatus(ctx context.Context, id string, status persistence.TaskStatus) error
}

// Config holds watchdog tunables. Zero-value defaults are applied at
// New(), so the daemon can wire a partially-populated Config and
// still get a working watchdog.
type Config struct {
	// Enabled gates the periodic scan. When false, Start() is a
	// no-op so a daemon can ship the watchdog dark and turn it on
	// per-environment without code changes.
	Enabled bool
	// Interval is the period between scans. Default: 60s.
	// Shorter intervals catch stuck rows sooner at the cost of
	// repeated SELECTs against the executions table.
	Interval time.Duration
	// StuckThreshold is the maximum allowed gap between now and an
	// execution's updated_at before the watchdog fires. Default:
	// 30m. Should be longer than the slowest legitimate step in
	// any workflow plus a comfortable margin — false positives
	// here are noisier than missed detections.
	StuckThreshold time.Duration
	// Action is what to do when a stuck row is found. Default:
	// ActionWarn. Operators escalate to ActionFail only after they
	// trust the threshold isn't tripping on legitimate slow steps.
	Action Action
	// ApprovalTimeout cancels tasks parked in AWAITING_APPROVAL longer
	// than this — so the operator-action inbox can't accumulate stale
	// approvals forever. 0 disables (no expiry). Default wired from
	// autonomy.approval_timeout_hours (96h). Uses the same leader-gated
	// scan loop as the stuck-execution detector.
	ApprovalTimeout time.Duration
	// ReservationStaleAfter bounds how long a budget reservation
	// (trading-hardening §1) may stay unsettled before the sweep reaps it
	// regardless of task state — the backstop that guarantees a leaked
	// reservation (task row never created, or a settle that never ran)
	// can't block a project's hard cap forever. The sweep ALSO settles
	// reservations whose task already went terminal. 0 → defaultReservationStaleAfter.
	ReservationStaleAfter time.Duration
}

// DefaultConfig returns a watchdog configuration with the action set
// to fail. The original default was warn-only (ship-dark posture
// while operators learned the threshold), but warn-only left ghost
// RUNNING rows in the executions table that confused operators into
// cancelling tasks they thought had multiple concurrent agents
// running (observed 2026-05-13: ibkr-trader execution row stayed
// RUNNING for >12 min while the scheduler had already moved on to
// the next retry attempt — operator saw "two RUNNING" in the
// dashboard and cancelled, when in fact only the latest was a live
// goroutine). The 30-minute threshold is much longer than any
// legitimate single step in the workflows we ship, so flipping the
// default to fail is safe: a real stuck row gets cleaned up, the
// dashboard reflects reality, and operators with workloads that
// legitimately need >30m can opt back to warn via WatchdogConfig.
func DefaultConfig() Config {
	return Config{
		Enabled:        true,
		Interval:       60 * time.Second,
		StuckThreshold: 30 * time.Minute,
		Action:         ActionFail,
	}
}

// Metrics groups the Prometheus counters the watchdog publishes.
// detected fires once per (execution_id, observed updated_at) pair —
// not per tick — so a long-stuck row contributes 1 to the counter,
// not N for the number of times we observed it. failed fires only
// when Action=fail and the row was successfully marked terminal.
type Metrics struct {
	detected prometheus.Counter
	failed   prometheus.Counter
	// orphansSwept counts PAUSED executions finalized by the
	// orphan-PAUSED reconcile backstop (parent task already terminal).
	orphansSwept prometheus.Counter
	// approvalsExpired counts tasks cancelled by the approval-timeout
	// sweep (parked in AWAITING_APPROVAL past the configured timeout).
	approvalsExpired prometheus.Counter
	// reservationsSwept counts budget reservations settled by the
	// terminal-and-stale sweep (terminal task, or older than the stale bound).
	reservationsSwept prometheus.Counter
}

// NewMetrics registers watchdog counters on the provided registry.
// Returns nil when the registry is nil so tests can opt out.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		return nil
	}
	m := &Metrics{
		detected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vornik_watchdog_stuck_executions_detected_total",
			Help: "Number of distinct stuck-execution events the watchdog has observed (deduped per execution_id + observed updated_at).",
		}),
		failed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vornik_watchdog_stuck_executions_failed_total",
			Help: "Number of stuck executions the watchdog has marked terminal (fires only when action=fail).",
		}),
		orphansSwept: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vornik_watchdog_orphan_paused_swept_total",
			Help: "Number of orphan PAUSED executions (parent task already terminal) finalized by the reconcile backstop.",
		}),
		approvalsExpired: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vornik_watchdog_approvals_expired_total",
			Help: "Number of tasks cancelled by the approval-timeout sweep (parked in AWAITING_APPROVAL past the timeout).",
		}),
		reservationsSwept: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vornik_watchdog_budget_reservations_swept_total",
			Help: "Number of budget reservations settled by the terminal-and-stale sweep (terminal task or older than the stale bound).",
		}),
	}
	reg.MustRegister(m.detected, m.failed, m.orphansSwept, m.approvalsExpired, m.reservationsSwept)
	return m
}

// Watchdog runs the periodic scan as a single background goroutine.
// Lifecycle mirrors scheduler.Scheduler: Start() spawns the loop,
// Stop() cancels and waits for it to drain.
type Watchdog struct {
	cfg      Config
	execRepo ExecutionRepository
	taskRepo TaskRepository // optional; required only when Action=fail
	logger   zerolog.Logger
	metrics  *Metrics
	now      func() time.Time
	// leaderGate gates each scan on the elected leader so two
	// daemons don't both detect + act on the same stuck row.
	// Especially important when cfg.Action=fail — duplicate
	// detection would result in two FailExecution calls and
	// confusing audit chatter. Nil-safe.
	leaderGate LeaderGate

	// reservationSweeper settles budget reservations whose task went
	// terminal or that have gone stale. Optional — nil disables the sweep
	// (the prompt settle in the executor still runs). Wired via
	// SetReservationSweeper so the constructor signature stays stable.
	reservationSweeper ReservationSweeper

	mu      sync.Mutex
	started bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// seenLock guards the dedupe table. The map keys executions by
	// ID; the value is the updated_at the watchdog last observed for
	// that row. A re-observation with the same updated_at is silenced;
	// a fresh updated_at clears the entry (the executor unstuck and
	// resumed checkpointing). When a row leaves RUNNING the entry
	// also clears.
	seenLock sync.Mutex
	seen     map[string]time.Time
}

// New constructs a Watchdog. Zero-valued Config fields fall back to
// DefaultConfig() values; an unknown Action is coerced to ActionWarn
// (a typo in the daemon YAML must not silently disable the
// watchdog). Returns nil when execRepo is nil — the watchdog has
// nothing to scan without it.
func New(cfg Config, execRepo ExecutionRepository, taskRepo TaskRepository, logger zerolog.Logger, metrics *Metrics) *Watchdog {
	if execRepo == nil {
		return nil
	}
	def := DefaultConfig()
	if cfg.Interval <= 0 {
		cfg.Interval = def.Interval
	}
	if cfg.StuckThreshold <= 0 {
		cfg.StuckThreshold = def.StuckThreshold
	}
	if !cfg.Action.IsValid() {
		cfg.Action = def.Action
	}
	return &Watchdog{
		cfg:      cfg,
		execRepo: execRepo,
		taskRepo: taskRepo,
		logger:   logger,
		metrics:  metrics,
		now:      func() time.Time { return time.Now().UTC() },
		seen:     make(map[string]time.Time),
	}
}

// Enabled reports whether the watchdog will actually scan when
// Start() is called. Exposed for tests + diagnostics so callers
// who built the watchdog via initWatchdog wiring can verify the
// effective config (e.g. that a missing `watchdog:` block in
// the operator YAML still leaves the safety net armed).
func (w *Watchdog) Enabled() bool {
	if w == nil {
		return false
	}
	return w.cfg.Enabled
}

// Start begins the periodic scan in a background goroutine. Returns
// nil immediately when Enabled=false so the daemon can call Start()
// unconditionally and the operator's config knob does the gating.
// Calling Start() twice returns an error.
func (w *Watchdog) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.cfg.Enabled {
		w.logger.Info().Msg("watchdog disabled by config — periodic stuck-execution scan will not run")
		return nil
	}
	if w.started {
		return fmt.Errorf("watchdog already started")
	}
	w.ctx, w.cancel = context.WithCancel(context.Background())
	w.started = true
	w.wg.Add(1)
	go w.runLoop()
	w.logger.Info().
		Dur("interval", w.cfg.Interval).
		Dur("stuck_threshold", w.cfg.StuckThreshold).
		Str("action", string(w.cfg.Action)).
		Msg("watchdog started")
	return nil
}

// Stop signals the loop to exit and waits for it to drain. Safe to
// call multiple times; subsequent calls after the first are no-ops.
func (w *Watchdog) Stop() error {
	w.mu.Lock()
	if !w.started {
		w.mu.Unlock()
		return nil
	}
	w.cancel()
	w.started = false
	w.mu.Unlock()
	w.wg.Wait()
	return nil
}

// runLoop is the periodic scan. Each tick lists RUNNING executions,
// filters those whose updated_at is older than now-StuckThreshold,
// and dispatches each through the configured action.
func (w *Watchdog) runLoop() {
	defer w.wg.Done()

	// Fire one scan immediately so a stuck row caught at startup
	// doesn't have to wait an interval before being surfaced.
	w.scanOnce()

	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			if w.leaderGate != nil && !w.leaderGate.IsLeader() {
				continue
			}
			w.scanOnce()
		}
	}
}

// LeaderGate is the narrow contract Watchdog uses to skip scans
// on non-leader daemons. Satisfied by *leaderelection.Elector;
// defined locally to keep the watchdog package independent.
type LeaderGate interface {
	IsLeader() bool
}

// SetLeaderGate wires the elected-leader gate. Called by the
// service container after the Watchdog is constructed. nil
// clears (single-process default).
func (w *Watchdog) SetLeaderGate(g LeaderGate) {
	if w == nil {
		return
	}
	w.leaderGate = g
}

// scanOnce performs a single watchdog scan. Exported test surface so
// unit tests can drive the loop deterministically without sleeping
// for an interval.
func (w *Watchdog) scanOnce() {
	parent := w.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	running := persistence.ExecutionStatusRunning
	rows, err := w.execRepo.List(ctx, persistence.ExecutionFilter{
		Status:   &running,
		PageSize: 500, // bound the scan; a deployment with 500+ concurrently-RUNNING is itself a problem
	})
	if err != nil {
		w.logger.Warn().Err(err).Msg("watchdog: failed to list RUNNING executions for scan")
		return
	}

	now := w.now()
	threshold := now.Add(-w.cfg.StuckThreshold)
	currentRunning := make(map[string]struct{}, len(rows))

	for _, exec := range rows {
		if exec == nil {
			continue
		}
		currentRunning[exec.ID] = struct{}{}
		if !exec.UpdatedAt.Before(threshold) {
			// Fresh checkpoint — clear any prior dedupe so this
			// execution can be reported again if it re-sticks.
			w.clearSeen(exec.ID)
			continue
		}
		w.handleStuck(ctx, exec, now)
	}

	w.pruneSeen(currentRunning)

	// Orphan-PAUSED reconcile backstop. Independent of the stuck-RUNNING
	// scan above: finalize PAUSED executions whose parent task already went
	// terminal (the adaptive-route leak the per-task cascade misses on
	// CLOSED / odd-cancel paths). Best-effort — a failure is logged, never
	// fatal; the next tick retries.
	w.reconcileOrphanPaused(ctx)

	// Approval-timeout sweep: cancel tasks parked in AWAITING_APPROVAL past
	// the configured timeout so the operator-action inbox doesn't accrue
	// stale approvals. Best-effort; no-op when disabled / no task repo.
	w.sweepExpiredApprovals(ctx)

	// Budget-reservation sweep: settle reservations whose task went terminal
	// (prompt settle missed, e.g. a crash) or that have gone stale. The
	// backstop that keeps a leaked reservation from blocking a hard cap
	// forever. Best-effort; no-op when no sweeper is wired.
	w.sweepBudgetReservations(ctx)
}

// defaultReservationStaleAfter bounds an unsettled reservation's lifetime
// when Config.ReservationStaleAfter is unset. Comfortably longer than any
// legitimate task run (the executor settles promptly on terminal), so the
// stale path only ever reaps genuinely-leaked rows.
const defaultReservationStaleAfter = 6 * time.Hour

// ReservationSweeper is the narrow contract the watchdog needs to reap
// budget reservations. Satisfied by persistence.BudgetReservationRepository.
type ReservationSweeper interface {
	SweepTerminalAndStale(ctx context.Context, staleCutoff, now time.Time) (int64, error)
}

// SetReservationSweeper wires the budget-reservation sweeper. Called by the
// service container after construction. nil disables the sweep.
func (w *Watchdog) SetReservationSweeper(s ReservationSweeper) {
	if w == nil {
		return
	}
	w.reservationSweeper = s
}

// sweepBudgetReservations settles terminal-task + stale reservations. The
// stale bound falls back to defaultReservationStaleAfter when unset.
func (w *Watchdog) sweepBudgetReservations(ctx context.Context) {
	if w.reservationSweeper == nil {
		return
	}
	stale := w.cfg.ReservationStaleAfter
	if stale <= 0 {
		stale = defaultReservationStaleAfter
	}
	now := w.now()
	n, err := w.reservationSweeper.SweepTerminalAndStale(ctx, now.Add(-stale), now)
	if err != nil {
		w.logger.Warn().Err(err).Msg("watchdog: budget-reservation sweep failed; retry next tick")
		return
	}
	if n > 0 {
		if w.metrics != nil {
			w.metrics.reservationsSwept.Add(float64(n))
		}
		w.logger.Info().Int64("settled", n).Msg("watchdog: settled terminal/stale budget reservations")
	}
}

// sweepExpiredApprovals cancels tasks that have sat in AWAITING_APPROVAL
// longer than cfg.ApprovalTimeout. The task's updated_at is the entry time
// (status changes touch it; a task awaiting approval sees no other writes),
// so it's the age proxy. Best-effort + idempotent: a cancelled task leaves
// the AWAITING_APPROVAL set, so a re-run finds nothing.
func (w *Watchdog) sweepExpiredApprovals(ctx context.Context) {
	if w.cfg.ApprovalTimeout <= 0 || w.taskRepo == nil {
		return
	}
	status := persistence.TaskStatusAwaitingApproval
	tasks, err := w.taskRepo.List(ctx, persistence.TaskFilter{Status: &status, PageSize: 500})
	if err != nil {
		w.logger.Warn().Err(err).Msg("watchdog: approval-timeout list failed; retry next tick")
		return
	}
	cutoff := w.now().Add(-w.cfg.ApprovalTimeout)
	expired := 0
	for _, t := range tasks {
		if t == nil || !t.UpdatedAt.Before(cutoff) {
			continue
		}
		if err := w.taskRepo.UpdateStatus(ctx, t.ID, persistence.TaskStatusCancelled); err != nil {
			w.logger.Warn().Err(err).Str("task_id", t.ID).Msg("watchdog: approval-timeout cancel failed")
			continue
		}
		expired++
		w.logger.Info().
			Str("task_id", t.ID).
			Str("project_id", t.ProjectID).
			Dur("approval_age", w.now().Sub(t.UpdatedAt)).
			Dur("timeout", w.cfg.ApprovalTimeout).
			Msg("watchdog: cancelled task — approval timed out (no operator decision within the window)")
	}
	if expired > 0 && w.metrics != nil && w.metrics.approvalsExpired != nil {
		w.metrics.approvalsExpired.Add(float64(expired))
	}
}

// reconcileOrphanPaused finalizes orphan PAUSED executions whose parent task
// is terminal. Best-effort + idempotent (a no-op once none remain).
func (w *Watchdog) reconcileOrphanPaused(ctx context.Context) {
	n, err := w.execRepo.SupersedeOrphanPausedExecutions(ctx)
	if err != nil {
		w.logger.Warn().Err(err).Msg("watchdog: orphan-PAUSED reconcile failed; orphans linger until next tick")
		return
	}
	if n > 0 {
		if w.metrics != nil && w.metrics.orphansSwept != nil {
			w.metrics.orphansSwept.Add(float64(n))
		}
		w.logger.Info().Int64("orphans_swept", n).
			Msg("watchdog: finalized orphan PAUSED executions (parent task already terminal)")
	}
}

// handleStuck applies the configured action to a single stuck
// execution row. Dedupe by (id, updated_at) so a long-stuck row
// only fires the action on first detection plus any subsequent
// re-stick — not on every tick.
func (w *Watchdog) handleStuck(ctx context.Context, exec *persistence.Execution, now time.Time) {
	if w.hasSeen(exec.ID, exec.UpdatedAt) {
		return // already reported at this updated_at — silenced
	}
	age := now.Sub(exec.UpdatedAt)
	logEvent := w.logger.Warn().
		Str("execution_id", exec.ID).
		Str("task_id", exec.TaskID).
		Str("project_id", exec.ProjectID).
		Dur("checkpoint_age", age).
		Time("last_updated_at", exec.UpdatedAt).
		Str("action", string(w.cfg.Action))

	if w.cfg.Action == ActionFail {
		if err := w.markFailed(ctx, exec, age); err != nil {
			logEvent.Err(err).Msg("watchdog: stuck execution detected — fail action errored, row left as-is")
			return
		}
		w.markSeen(exec.ID, exec.UpdatedAt)
		if w.metrics != nil {
			w.metrics.detected.Inc()
		}
		if w.metrics != nil {
			w.metrics.failed.Inc()
		}
		logEvent.Msg("watchdog: stuck execution detected — marked FAILED with class STUCK_EXECUTION")
		return
	}
	w.markSeen(exec.ID, exec.UpdatedAt)
	if w.metrics != nil {
		w.metrics.detected.Inc()
	}
	logEvent.Msg("watchdog: stuck execution detected — warn-only, row left RUNNING")
}

// markFailed flips both the execution and (when wired) the task row
// to terminal with class STUCK_EXECUTION. Either side failing is
// surfaced to the caller so the metric increment + the success-log
// happen only on the all-or-nothing path.
func (w *Watchdog) markFailed(ctx context.Context, exec *persistence.Execution, age time.Duration) error {
	msg := fmt.Sprintf("watchdog: execution stuck — no checkpoint advance in %s (last update %s)",
		age.Truncate(time.Second), exec.UpdatedAt.UTC().Format(time.RFC3339))
	if err := w.execRepo.RecordFailure(ctx, exec.ID, msg, persistence.TaskFailureClassStuckExecution); err != nil {
		return fmt.Errorf("execution RecordFailure: %w", err)
	}
	if w.taskRepo == nil || exec.TaskID == "" {
		return nil
	}
	task, err := w.taskRepo.Get(ctx, exec.TaskID)
	if err != nil {
		// Best-effort task-side update; an execution row marked
		// FAILED with a still-RUNNING task row will surface as a
		// discrepancy in the UI which an operator can investigate.
		return fmt.Errorf("task Get: %w", err)
	}
	if task == nil {
		return nil
	}
	// Defensive: if the task is already in a terminal state, don't
	// regress it. Watchdog only runs on RUNNING executions, but a
	// race with the executor's own handleFailure could land us
	// here just after the task transitioned.
	switch task.Status {
	case persistence.TaskStatusCompleted, persistence.TaskStatusFailed, persistence.TaskStatusCancelled:
		return nil
	}
	task.Status = persistence.TaskStatusFailed
	errClass := persistence.TaskFailureClassStuckExecution
	task.LastError = &msg
	task.LastErrorClass = &errClass
	if err := w.taskRepo.Update(ctx, task); err != nil {
		return fmt.Errorf("task Update: %w", err)
	}
	return nil
}

// markSeen records (id, updatedAt) and returns true when this is the
// first observation at this updated_at — i.e. the caller should
// fire the action. Returns false on a repeat observation.
func (w *Watchdog) markSeen(id string, updatedAt time.Time) bool {
	w.seenLock.Lock()
	defer w.seenLock.Unlock()
	if prev, ok := w.seen[id]; ok && prev.Equal(updatedAt) {
		return false
	}
	w.seen[id] = updatedAt
	return true
}

func (w *Watchdog) hasSeen(id string, updatedAt time.Time) bool {
	w.seenLock.Lock()
	defer w.seenLock.Unlock()
	prev, ok := w.seen[id]
	return ok && prev.Equal(updatedAt)
}

// clearSeen removes the dedupe entry for an execution that has
// freshened its checkpoint. Idempotent.
func (w *Watchdog) clearSeen(id string) {
	w.seenLock.Lock()
	defer w.seenLock.Unlock()
	delete(w.seen, id)
}

// pruneSeen drops dedupe entries for executions that are no longer
// RUNNING (completed, failed, cancelled) so the map doesn't grow
// without bound across the daemon's lifetime.
func (w *Watchdog) pruneSeen(stillRunning map[string]struct{}) {
	w.seenLock.Lock()
	defer w.seenLock.Unlock()
	for id := range w.seen {
		if _, ok := stillRunning[id]; !ok {
			delete(w.seen, id)
		}
	}
}
