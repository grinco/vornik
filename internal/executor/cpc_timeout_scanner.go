package executor

import (
	"context"
	"sync"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// cpcTimeoutScannerInterval is how often the goroutine wakes
// up to sweep expired CPC rows. 30s matches the broker's
// reconcile cadence and the LLD §11 mention. Operators can
// override via VORNIK_CPC_TIMEOUT_SCAN_INTERVAL — same env
// pattern Feature #3's hint scanner uses.
const cpcTimeoutScannerInterval = 30 * time.Second

// cpcTimeoutScannerBatchSize caps per-tick work so a backlog
// (e.g. operator deleted a receiving project, dozens of
// pending rows expire at once) doesn't lock the table for
// minutes.
const cpcTimeoutScannerBatchSize = 100

// CPCTimeoutScanner is the Phase D hardening loop. It sweeps
// cross_project_calls rows past their timeout_at into
// status=timed_out and wakes the blocked caller tasks. LLD §8.1
// + §11.
//
// The scanner does NOT auto-cancel the callee task — operator
// decides via v1.1 config (cancelOnTimeout). This matches the
// in-project delegation semantics: timeout on the caller side
// doesn't stop the callee's in-progress work.
//
// Best-effort: every per-row error is logged and skipped; one
// bad row never wedges the loop. Stops cleanly on context
// cancel.
type CPCTimeoutScanner struct {
	executor *Executor
	interval time.Duration
	batch    int

	// leaderGate gates the scan on the elected leader in
	// multi-instance deployments. nil → run every tick
	// (single-process default). See
	// https://docs.vornik.io §3.
	leaderGate CPCLeaderGate
}

// CPCLeaderGate is the narrow contract the scanner consults
// before each tick. Local interface so the executor package
// doesn't pull internal/leaderelection;
// *leaderelection.Elector satisfies structurally.
type CPCLeaderGate interface {
	IsLeader() bool
}

// SetLeaderGate attaches the gate. Safe to call before Run.
func (s *CPCTimeoutScanner) SetLeaderGate(g CPCLeaderGate) {
	if s == nil {
		return
	}
	s.leaderGate = g
}

// NewCPCTimeoutScanner wires the scanner against an Executor.
// Reads the cpcRepo + execRepo + adminAuditRepo + metrics +
// livePub from the executor — same dependencies the
// resolve hook uses to wake the caller and write observability
// rows. Returns nil when the executor doesn't have a CPC repo
// wired (the feature is off; no scanner work to do).
func NewCPCTimeoutScanner(e *Executor) *CPCTimeoutScanner {
	if e == nil || e.cpcRepo == nil {
		return nil
	}
	return &CPCTimeoutScanner{
		executor: e,
		interval: cpcTimeoutScannerInterval,
		batch:    cpcTimeoutScannerBatchSize,
	}
}

// Run blocks until ctx is cancelled. Polls the repo every
// `interval`, sweeping any timed-out rows. Returns ctx.Err()
// on shutdown so callers can distinguish clean stop from
// failure.
//
// Safe to call from a goroutine; safe to call once (no internal
// run-once guard — caller manages the lifecycle).
func (s *CPCTimeoutScanner) Run(ctx context.Context) error {
	if s == nil {
		return nil
	}
	// Tick once on startup so a daemon that boots into a
	// backlog of timed-out rows doesn't wait `interval` before
	// the first sweep.
	s.tick(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick performs one sweep. Public-ish (lower-cased; package
// tests call it directly) so tests can drive the scanner
// without spinning the goroutine.
func (s *CPCTimeoutScanner) tick(ctx context.Context) {
	if s == nil || s.executor == nil || s.executor.cpcRepo == nil {
		return
	}
	if s.leaderGate != nil && !s.leaderGate.IsLeader() {
		return
	}
	rows, err := s.executor.cpcRepo.ClaimTimedOut(ctx, time.Now(), s.batch)
	if err != nil {
		s.executor.logger.Warn().Err(err).
			Msg("cpc timeout scanner: ClaimTimedOut failed; retrying on next tick")
		return
	}
	if len(rows) == 0 {
		return
	}
	for _, cpc := range rows {
		s.processOne(ctx, cpc)
	}
}

// processOne handles the post-claim work for a single row:
// emit live events on the caller's stream, bump metrics, write
// the audit row, and wake the caller task so its on_fail
// branch fires.
func (s *CPCTimeoutScanner) processOne(ctx context.Context, cpc *persistence.CrossProjectCall) {
	if cpc == nil {
		return
	}
	e := s.executor
	dur := time.Duration(0)
	if !cpc.CreatedAt.IsZero() {
		dur = time.Since(cpc.CreatedAt)
	}

	// Metrics.
	if e.metrics != nil {
		e.metrics.RecordCrossProjectCallResolved(cpc.CallerProject, cpc.CalleeProject, string(persistence.CPCStatusTimedOut), dur.Seconds())
	}

	// Live event on caller's stream.
	if callerExecID := e.lookupExecutionIDForTask(ctx, cpc.CallerTaskID); callerExecID != "" {
		errMsg := ""
		if cpc.ErrorMessage != nil {
			errMsg = *cpc.ErrorMessage
		}
		e.emitLive(ctx, callerExecID, livepubsub.KindCrossProjectCallResolved, livepubsub.CrossProjectCallResolvedPayload{
			CPCId:        cpc.ID,
			Status:       string(persistence.CPCStatusTimedOut),
			Summary:      "timeout elapsed",
			ErrorMessage: errMsg,
			DurationMs:   dur.Milliseconds(),
		})
	}

	// Audit row.
	errMsg := ""
	if cpc.ErrorMessage != nil {
		errMsg = *cpc.ErrorMessage
	}
	e.recordCPCAuditResolve(ctx, cpc, string(persistence.CPCStatusTimedOut), errMsg)

	// Wake the caller task. The caller is paused at
	// WAITING_FOR_CHILDREN — same shape as in-project
	// delegation, so the existing checkParentUnblock-style
	// resume primitive handles it. We re-queue the caller
	// task directly here so the timeout doesn't depend on the
	// callee actually terminating (which it never will in the
	// timeout case).
	s.wakeCallerForTimeout(ctx, cpc)

	// Optional cascade-cancel: when the call_project step set
	// cancel_on_timeout=true the callee task is killed too.
	// LLD §8.1 — opt-in because the default is to let the
	// callee's work continue (the operator may still want it).
	if cpc.CancelOnTimeout && cpc.CalleeTaskID != nil && *cpc.CalleeTaskID != "" {
		s.cascadeCancelCallee(ctx, cpc)
	}

	e.logger.Info().
		Str("cpc_id", cpc.ID).
		Str("caller_task_id", cpc.CallerTaskID).
		Str("callee_project", cpc.CalleeProject).
		Bool("cancel_on_timeout", cpc.CancelOnTimeout).
		Dur("elapsed", dur).
		Msg("cpc timeout scanner: row timed out, caller re-queued")
}

// cascadeCancelCallee transitions the callee task to CANCELLED
// when its CPC times out and the workflow opted into the
// cascade via cancel_on_timeout. The existing task-cancel
// primitive handles the rest of the cleanup (executor
// shutdown signal, container teardown). Best-effort — failure
// here logs but doesn't fail the scanner.
func (s *CPCTimeoutScanner) cascadeCancelCallee(ctx context.Context, cpc *persistence.CrossProjectCall) {
	if cpc == nil || cpc.CalleeTaskID == nil || s.executor == nil || s.executor.taskRepo == nil {
		return
	}
	taskID := *cpc.CalleeTaskID
	task, err := s.executor.taskRepo.Get(ctx, taskID)
	if err != nil || task == nil {
		return
	}
	// Skip if already terminal — idempotent.
	switch task.Status {
	case persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled:
		return
	}
	if err := s.executor.taskRepo.UpdateStatus(ctx, taskID, persistence.TaskStatusCancelled); err != nil {
		s.executor.logger.Warn().Err(err).
			Str("cpc_id", cpc.ID).
			Str("callee_task_id", taskID).
			Msg("cpc timeout scanner: cancel_on_timeout cascade failed; callee task still in flight")
		return
	}
	s.executor.logger.Info().
		Str("cpc_id", cpc.ID).
		Str("callee_task_id", taskID).
		Msg("cpc timeout scanner: cascaded cancel to callee task")
}

// wakeCallerForTimeout transitions the caller task from
// WAITING_FOR_CHILDREN back to QUEUED so the scheduler picks
// it up and re-enters the workflow at the step AFTER the
// call_project step (the executor's checkpoint advanced
// state.CurrentStepID to step.OnFail before pausing).
//
// Best-effort: a repo error is logged; the next scheduler
// recovery pass picks up the orphan eventually.
func (s *CPCTimeoutScanner) wakeCallerForTimeout(ctx context.Context, cpc *persistence.CrossProjectCall) {
	if cpc == nil || s.executor == nil || s.executor.taskRepo == nil {
		return
	}
	task, err := s.executor.taskRepo.Get(ctx, cpc.CallerTaskID)
	if err != nil || task == nil {
		return
	}
	// Only re-queue if the task is still WAITING_FOR_CHILDREN.
	// Other states (CANCELLED, FAILED, COMPLETED) shouldn't
	// be touched — the operator already moved the task on.
	if task.Status != persistence.TaskStatusWaitingForChildren {
		return
	}
	if err := s.executor.taskRepo.UpdateStatus(ctx, cpc.CallerTaskID, persistence.TaskStatusQueued); err != nil {
		s.executor.logger.Warn().Err(err).
			Str("caller_task_id", cpc.CallerTaskID).
			Msg("cpc timeout scanner: failed to re-queue caller task; scheduler recovery will pick it up")
	}
}

// liveCallReceivedTracker captures the in-process set of
// callee executions we've already emitted
// cross_project_call_received for. Without this guard, a
// retry / scheduler recovery that re-leases the callee task
// would emit the event multiple times. Per-process state is
// fine — the event is informational, not a correctness
// dependency; a daemon restart re-emitting once is acceptable.
type liveCallReceivedTracker struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func newLiveCallReceivedTracker() *liveCallReceivedTracker {
	return &liveCallReceivedTracker{seen: map[string]struct{}{}}
}

// markEmitted reports whether this is the first sighting for
// the executionID. Subsequent calls return false (already
// emitted in this process).
func (t *liveCallReceivedTracker) markEmitted(executionID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.seen[executionID]; ok {
		return false
	}
	t.seen[executionID] = struct{}{}
	return true
}
