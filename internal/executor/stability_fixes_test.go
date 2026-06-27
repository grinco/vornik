package executor

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// TestCleanupExecution_CallsHandleCancel — pre-fix the per-execution
// context.WithCancel child of e.ctx leaked on every normal completion
// because cleanupExecution never invoked handle.cancel(). Captured
// here by registering a handle with a counted cancel func and
// verifying it fires.
func TestCleanupExecution_CallsHandleCancel(t *testing.T) {
	exec := &Executor{
		taskRepo:         NewMockTaskRepo(),
		activeExecutions: make(map[string]*executionHandle),
		logger:           zerolog.Nop(),
	}
	var canceled atomic.Bool
	exec.activeExecutions["task_cancel_leak"] = &executionHandle{
		taskID: "task_cancel_leak",
		cancel: func() { canceled.Store(true) },
	}
	exec.cleanupExecution("task_cancel_leak")

	if !canceled.Load() {
		t.Errorf("cleanupExecution did not invoke handle.cancel — context leak")
	}
}

// TestCleanupExecution_CallsHandleCancelEvenForLiveDispatch — the
// cancel must fire for non-recovered (live-dispatch) executions
// too — this is the common case and where the leak rate is highest.
func TestCleanupExecution_CallsHandleCancelEvenForLiveDispatch(t *testing.T) {
	exec := &Executor{
		taskRepo:         NewMockTaskRepo(),
		activeExecutions: make(map[string]*executionHandle),
		logger:           zerolog.Nop(),
	}
	var canceled atomic.Bool
	exec.activeExecutions["task_live"] = &executionHandle{
		taskID:    "task_live",
		recovered: false, // live dispatch
		cancel:    func() { canceled.Store(true) },
	}
	exec.cleanupExecution("task_live")

	if !canceled.Load() {
		t.Errorf("cleanupExecution did not invoke handle.cancel for live dispatch")
	}
}

// TestReleaseRecoveredTask_ClearsLeaseOnTerminalCompleted — pre-fix,
// handleSuccess used UpdateStatus(Completed) which leaves
// lease_id/leased_by populated. releaseRecoveredTask then early-
// returned on COMPLETED without clearing the lease. Result: a
// terminal-status row with stale lease columns, violating the
// invariant the lifecycle tests document. Now releaseRecoveredTask
// must call ReleaseLease(Completed) to atomically clear the lease.
func TestReleaseRecoveredTask_ClearsLeaseOnTerminalCompleted(t *testing.T) {
	repo := NewMockTaskRepo()
	staleLeaseID := "scheduler-old-lease"
	staleLeaseBy := "old-daemon"
	repo.AddTask(&persistence.Task{
		ID:       "task_terminal_stale_lease",
		Status:   persistence.TaskStatusCompleted, // handleSuccess already wrote this
		LeaseID:  &staleLeaseID,                   // but lease columns still set
		LeasedBy: &staleLeaseBy,
	})
	exec := &Executor{
		taskRepo:         repo,
		activeExecutions: make(map[string]*executionHandle),
		logger:           zerolog.Nop(),
	}
	exec.activeExecutions["task_terminal_stale_lease"] = &executionHandle{
		taskID:    "task_terminal_stale_lease",
		recovered: true,
	}
	exec.cleanupExecution("task_terminal_stale_lease")

	final, _ := repo.Get(context.Background(), "task_terminal_stale_lease")
	if final.Status != persistence.TaskStatusCompleted {
		t.Errorf("status: got %s, want COMPLETED (terminal status preserved)", final.Status)
	}
	if final.LeaseID != nil || final.LeasedBy != nil {
		t.Errorf("lease must be cleared on terminal cleanup, got LeaseID=%v LeasedBy=%v", final.LeaseID, final.LeasedBy)
	}
}

// TestReleaseRecoveredTask_ClearsLeaseOnTerminalFailed — same
// guarantee as the COMPLETED case but for handleFailure's terminal
// path which uses Update(task) (only writes status not lease cols).
func TestReleaseRecoveredTask_ClearsLeaseOnTerminalFailed(t *testing.T) {
	repo := NewMockTaskRepo()
	staleLeaseID := "scheduler-old-lease-2"
	repo.AddTask(&persistence.Task{
		ID:      "task_terminal_failed_stale",
		Status:  persistence.TaskStatusFailed,
		LeaseID: &staleLeaseID,
	})
	exec := &Executor{
		taskRepo:         repo,
		activeExecutions: make(map[string]*executionHandle),
		logger:           zerolog.Nop(),
	}
	exec.activeExecutions["task_terminal_failed_stale"] = &executionHandle{
		taskID:    "task_terminal_failed_stale",
		recovered: true,
	}
	exec.cleanupExecution("task_terminal_failed_stale")

	final, _ := repo.Get(context.Background(), "task_terminal_failed_stale")
	if final.LeaseID != nil {
		t.Errorf("lease must be cleared on terminal cleanup, got LeaseID=%v", final.LeaseID)
	}
}

// TestStop_ResetsContext — pre-fix, Stop() called e.cancel() but did
// not reset e.ctx/e.cancel to nil. The next ExecuteWithContext or
// recoverExecution call found e.ctx != nil and derived a child of
// the already-cancelled context, making the new execution fail
// instantly with context.Canceled. Verify Stop() now resets the
// fields so a subsequent caller gets a fresh context.
func TestStop_ResetsContext(t *testing.T) {
	exec := &Executor{
		activeExecutions: make(map[string]*executionHandle),
		logger:           zerolog.Nop(),
	}
	// Initialize as ExecuteWithContext would.
	exec.ctx, exec.cancel = context.WithCancel(context.Background())
	originalCtx := exec.ctx

	if err := exec.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if exec.ctx != nil {
		t.Errorf("Stop must reset e.ctx to nil; got %v", exec.ctx)
	}
	if exec.cancel != nil {
		t.Errorf("Stop must reset e.cancel to nil; got %v", exec.cancel)
	}
	if originalCtx.Err() == nil {
		t.Errorf("Stop did not cancel the original context")
	}
}
