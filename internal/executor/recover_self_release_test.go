package executor

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// Regression coverage for the recovered-execution self-release
// path. Reproduces the 2026-05-07 bug where task
// task_20260507204558_88382830f8da1aaf stayed in RUNNING for 3+
// minutes after its post-restart execution failed because the
// recovery codepath had no scheduler.dispatchViaExecutor goroutine
// watching it — the task only flipped to terminal when the
// operator cancelled it manually.
//
// The fix lives in cleanupExecution + releaseRecoveredTask
// (executor.go). These tests exercise the public surface
// (cleanupExecution called via defer in runExecution) plus the
// helper directly, with a stripped-down Executor that wires only
// the taskRepo + logger fields needed.

// minimalExecutorWithRepo builds an Executor populated only
// enough for the self-release path to run. The real executor
// constructor pulls in a lot more wiring (runtime, exec repo,
// metrics, workflow resolver) that's irrelevant here.
func minimalExecutorWithRepo(repo TaskRepository) *Executor {
	return &Executor{
		taskRepo:         repo,
		activeExecutions: make(map[string]*executionHandle),
		logger:           zerolog.Nop(),
	}
}

// TestCleanupExecution_RecoveredExec_RetryBudgetRemaining — the
// headline case: a recovered exec finishes with the task at
// attempt 2/3, status RUNNING. cleanupExecution must flip the
// task to QUEUED + bump Attempt to 3 + clear the lease, with NO
// scheduler involvement. Pre-fix the task stayed RUNNING.
func TestCleanupExecution_RecoveredExec_RetryBudgetRemaining(t *testing.T) {
	repo := NewMockTaskRepo()
	leaseID := "scheduler-old-1"
	leaseBy := "old-daemon"
	priorErr := "agent reported FAILED status: degenerate loop"
	repo.AddTask(&persistence.Task{
		ID:          "task_recovered_retry",
		Status:      persistence.TaskStatusRunning,
		Attempt:     2,
		MaxAttempts: 3,
		LeaseID:     &leaseID,
		LeasedBy:    &leaseBy,
		LastError:   &priorErr,
	})
	exec := minimalExecutorWithRepo(repo)

	// Simulate recoverExecution registering the handle …
	exec.activeExecutions["task_recovered_retry"] = &executionHandle{
		taskID:    "task_recovered_retry",
		recovered: true,
	}
	// … then runExecution returning, which fires the deferred
	// cleanupExecution call.
	exec.cleanupExecution("task_recovered_retry")

	final, _ := repo.Get(context.Background(), "task_recovered_retry")
	if final.Status != persistence.TaskStatusQueued {
		t.Errorf("status: got %s, want QUEUED (retry budget remained)", final.Status)
	}
	if final.Attempt != 3 {
		t.Errorf("attempt: got %d, want 3 (incremented from 2)", final.Attempt)
	}
	if final.LeaseID != nil || final.LeasedBy != nil {
		t.Errorf("lease should be cleared on release, got LeaseID=%v LeasedBy=%v", final.LeaseID, final.LeasedBy)
	}
	if final.LastError == nil || *final.LastError != priorErr {
		t.Errorf("LastError: got %v, want preserved %q", final.LastError, priorErr)
	}
}

// TestCleanupExecution_RecoveredExec_RetryBudgetExhausted — when
// attempt == MaxAttempts the task should land FAILED, not QUEUED.
// Mirrors scheduler.completeTask's terminal branch.
func TestCleanupExecution_RecoveredExec_RetryBudgetExhausted(t *testing.T) {
	repo := NewMockTaskRepo()
	terminalErr := "validation exception: too many tool args"
	repo.AddTask(&persistence.Task{
		ID:          "task_recovered_terminal",
		Status:      persistence.TaskStatusRunning,
		Attempt:     3,
		MaxAttempts: 3,
		LastError:   &terminalErr,
	})
	exec := minimalExecutorWithRepo(repo)
	exec.activeExecutions["task_recovered_terminal"] = &executionHandle{
		taskID:    "task_recovered_terminal",
		recovered: true,
	}
	exec.cleanupExecution("task_recovered_terminal")

	final, _ := repo.Get(context.Background(), "task_recovered_terminal")
	if final.Status != persistence.TaskStatusFailed {
		t.Errorf("status: got %s, want FAILED (retry budget exhausted)", final.Status)
	}
	if final.Attempt != 3 {
		t.Errorf("attempt: got %d, want preserved 3", final.Attempt)
	}
}

// TestCleanupExecution_LiveDispatch_DoesNotSelfRelease — the
// flip-side guarantee: cleanupExecution called for a NON-recovered
// (live-dispatch) execution MUST NOT touch the task status. The
// scheduler.TaskCompleted path handles those — double-writing
// would race.
func TestCleanupExecution_LiveDispatch_DoesNotSelfRelease(t *testing.T) {
	repo := NewMockTaskRepo()
	repo.AddTask(&persistence.Task{
		ID:          "task_live_dispatch",
		Status:      persistence.TaskStatusRunning,
		Attempt:     1,
		MaxAttempts: 3,
	})
	exec := minimalExecutorWithRepo(repo)
	// Recovered=false (the default for primary dispatch).
	exec.activeExecutions["task_live_dispatch"] = &executionHandle{
		taskID:    "task_live_dispatch",
		recovered: false,
	}
	exec.cleanupExecution("task_live_dispatch")

	final, _ := repo.Get(context.Background(), "task_live_dispatch")
	if final.Status != persistence.TaskStatusRunning {
		t.Errorf("live-dispatch path mutated status: got %s, want RUNNING (scheduler handles release)", final.Status)
	}
	if final.Attempt != 1 {
		t.Errorf("live-dispatch path mutated attempt: got %d, want 1", final.Attempt)
	}
}

// TestCleanupExecution_RecoveredExec_AlreadyTerminal — when the
// task is already terminal (e.g. operator cancelled mid-recovery,
// or a sibling code path already released), cleanupExecution
// must be a no-op. Without this an externally-CANCELLED task
// would get overwritten as FAILED on cleanup.
func TestCleanupExecution_RecoveredExec_AlreadyTerminal(t *testing.T) {
	for _, terminal := range []persistence.TaskStatus{
		persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
	} {
		t.Run(string(terminal), func(t *testing.T) {
			repo := NewMockTaskRepo()
			repo.AddTask(&persistence.Task{
				ID:          "task_already_terminal",
				Status:      terminal,
				Attempt:     2,
				MaxAttempts: 3,
			})
			exec := minimalExecutorWithRepo(repo)
			exec.activeExecutions["task_already_terminal"] = &executionHandle{
				taskID:    "task_already_terminal",
				recovered: true,
			}
			exec.cleanupExecution("task_already_terminal")

			final, _ := repo.Get(context.Background(), "task_already_terminal")
			if final.Status != terminal {
				t.Errorf("terminal status %s overwritten to %s", terminal, final.Status)
			}
		})
	}
}

// TestCleanupExecution_RecoveredExec_NoRepoSafe — defensive: if
// the executor was constructed without a taskRepo (rare; some
// test paths), the self-release path should no-op rather than
// nil-deref.
func TestCleanupExecution_RecoveredExec_NoRepoSafe(t *testing.T) {
	exec := &Executor{
		activeExecutions: make(map[string]*executionHandle),
		logger:           zerolog.Nop(),
	}
	exec.activeExecutions["task_no_repo"] = &executionHandle{
		taskID:    "task_no_repo",
		recovered: true,
	}
	// Must not panic.
	exec.cleanupExecution("task_no_repo")
}

// TestCleanupExecution_RecoveredExec_NoLeaseID_RetryBudgetRemaining
// is the regression test for the 2026-05-16 stuck-PENDING bug.
// retry-from-step initiated executions don't carry a lease (the
// scheduler wasn't involved in the dispatch), so the task arrives
// at cleanupExecution with task.LeaseID == nil. Pre-fix this hit
// ReleaseLease's "leaseID required" guard, the warn log fired,
// and the task was abandoned in its non-terminal status — only
// recoverable by a manual UI Retry click. Operator-observed on
// task_20260516015931_2c9658cb6a103380.
//
// Post-fix the leaseless path uses TransitionConditional with the
// non-terminal status set as the WHERE gate; attempt and
// max_attempts persist on the same UPDATE.
func TestCleanupExecution_RecoveredExec_NoLeaseID_RetryBudgetRemaining(t *testing.T) {
	repo := NewMockTaskRepo()
	priorErr := "agent reported FAILED status: missing prerequisite cv.pdf"
	repo.AddTask(&persistence.Task{
		ID:          "task_no_lease_retry",
		Status:      persistence.TaskStatusRunning,
		Attempt:     2,
		MaxAttempts: 6,
		LeaseID:     nil, // retry-from-step path — never leased by scheduler
		LastError:   &priorErr,
	})
	exec := minimalExecutorWithRepo(repo)
	exec.activeExecutions["task_no_lease_retry"] = &executionHandle{
		taskID:    "task_no_lease_retry",
		recovered: true,
	}
	exec.cleanupExecution("task_no_lease_retry")

	final, _ := repo.Get(context.Background(), "task_no_lease_retry")
	if final.Status != persistence.TaskStatusQueued {
		t.Errorf("status: got %s, want QUEUED — leaseless retry-from-step path must reach the QUEUED transition via TransitionConditional fallback (pre-fix this stayed RUNNING forever)", final.Status)
	}
	if final.Attempt != 3 {
		t.Errorf("attempt: got %d, want 3 (incremented from 2)", final.Attempt)
	}
	if final.MaxAttempts != 6 {
		t.Errorf("max_attempts: got %d, want preserved 6", final.MaxAttempts)
	}
}

// TestCleanupExecution_RecoveredExec_NoLeaseID_RetryBudgetExhausted
// mirrors the above but at attempt == max_attempts: the leaseless
// path must still terminate to FAILED, not leave the task hanging.
func TestCleanupExecution_RecoveredExec_NoLeaseID_RetryBudgetExhausted(t *testing.T) {
	repo := NewMockTaskRepo()
	repo.AddTask(&persistence.Task{
		ID:          "task_no_lease_terminal",
		Status:      persistence.TaskStatusRunning,
		Attempt:     6,
		MaxAttempts: 6,
		LeaseID:     nil,
	})
	exec := minimalExecutorWithRepo(repo)
	exec.activeExecutions["task_no_lease_terminal"] = &executionHandle{
		taskID:    "task_no_lease_terminal",
		recovered: true,
	}
	exec.cleanupExecution("task_no_lease_terminal")

	final, _ := repo.Get(context.Background(), "task_no_lease_terminal")
	if final.Status != persistence.TaskStatusFailed {
		t.Errorf("status: got %s, want FAILED (no retry budget left)", final.Status)
	}
}

// TestCleanupExecution_RecoveredExec_NoLeaseID_PendingSource —
// PENDING is a legitimate source status for the leaseless
// transition: a paused execution that was set to PENDING on
// release. The fallback's WHERE gate must accept PENDING so a
// post-restart self-release fires on it, not just on RUNNING.
func TestCleanupExecution_RecoveredExec_NoLeaseID_PendingSource(t *testing.T) {
	repo := NewMockTaskRepo()
	repo.AddTask(&persistence.Task{
		ID:          "task_pending_no_lease",
		Status:      persistence.TaskStatusPending,
		Attempt:     2,
		MaxAttempts: 5,
		LeaseID:     nil,
	})
	exec := minimalExecutorWithRepo(repo)
	exec.activeExecutions["task_pending_no_lease"] = &executionHandle{
		taskID:    "task_pending_no_lease",
		recovered: true,
	}
	exec.cleanupExecution("task_pending_no_lease")

	final, _ := repo.Get(context.Background(), "task_pending_no_lease")
	if final.Status != persistence.TaskStatusQueued {
		t.Errorf("PENDING source: got %s, want QUEUED via leaseless fallback", final.Status)
	}
	if final.Attempt != 3 {
		t.Errorf("attempt: got %d, want 3", final.Attempt)
	}
}

// TestCleanupExecution_RecoveredExec_NoLeaseID_PersistsErrors —
// LastError + LastErrorClass set by handleFailure must survive
// the leaseless transition so the UI / CLI listings show the
// real failure reason instead of a blank field.
func TestCleanupExecution_RecoveredExec_NoLeaseID_PersistsErrors(t *testing.T) {
	repo := NewMockTaskRepo()
	priorErr := "agent reported FAILED: write step needed research.md which didn't exist"
	priorClass := "WORKFLOW_ROLE_MISSING"
	repo.AddTask(&persistence.Task{
		ID:             "task_no_lease_errors",
		Status:         persistence.TaskStatusRunning,
		Attempt:        2,
		MaxAttempts:    6,
		LastError:      &priorErr,
		LastErrorClass: &priorClass,
	})
	exec := minimalExecutorWithRepo(repo)
	exec.activeExecutions["task_no_lease_errors"] = &executionHandle{
		taskID:    "task_no_lease_errors",
		recovered: true,
	}
	exec.cleanupExecution("task_no_lease_errors")

	final, _ := repo.Get(context.Background(), "task_no_lease_errors")
	if final.LastError == nil || *final.LastError != priorErr {
		t.Errorf("LastError lost in leaseless transition: got %v, want %q", final.LastError, priorErr)
	}
	if final.LastErrorClass == nil || *final.LastErrorClass != priorClass {
		t.Errorf("LastErrorClass lost in leaseless transition: got %v, want %q", final.LastErrorClass, priorClass)
	}
}

// TestCleanupExecution_RecoveredExec_ZeroMaxAttempts — when the
// task has MaxAttempts=0 (an unconstrained or legacy task), treat
// it as 1 — this single attempt counts, so cleanup goes to
// terminal FAILED. Mirrors scheduler.completeTask's same guard.
func TestCleanupExecution_RecoveredExec_ZeroMaxAttempts(t *testing.T) {
	repo := NewMockTaskRepo()
	repo.AddTask(&persistence.Task{
		ID:          "task_zero_max",
		Status:      persistence.TaskStatusRunning,
		Attempt:     1,
		MaxAttempts: 0, // legacy / unconfigured
	})
	exec := minimalExecutorWithRepo(repo)
	exec.activeExecutions["task_zero_max"] = &executionHandle{
		taskID:    "task_zero_max",
		recovered: true,
	}
	exec.cleanupExecution("task_zero_max")

	final, _ := repo.Get(context.Background(), "task_zero_max")
	if final.Status != persistence.TaskStatusFailed {
		t.Errorf("zero-max-attempts: got %s, want FAILED", final.Status)
	}
}
