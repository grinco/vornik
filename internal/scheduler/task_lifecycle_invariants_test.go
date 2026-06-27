package scheduler

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Cross-cutting task-lifecycle invariants. These tests assert the
// scheduler.TaskCompleted / completeTask state machine never lands
// a task in a contradictory state (e.g. RUNNING with a cleared
// lease, FAILED with retry budget remaining, etc.).
//
// Operator-facing: every UI screen, every dashboard tile, every
// CLI subcommand reads task.Status. If the invariants below are
// violated, those surfaces show inconsistent / stale information.
// Reproduced 2026-05-07: a task showed RUNNING in the UI for 3+
// minutes after its execution had definitively failed, because
// the recovered-execution path didn't release the lease.

// invariantCheck is the predicate every lifecycle test runs after
// a state transition. Captures the contracts every UI/CLI surface
// implicitly relies on.
func invariantCheck(t *testing.T, label string, task *persistence.Task) {
	t.Helper()
	switch task.Status {
	case persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled:
		// Terminal: lease MUST be cleared (a stuck lease on a
		// terminal task confuses the scheduler's recovery loop +
		// inflates concurrency counts).
		if task.LeaseID != nil && *task.LeaseID != "" {
			t.Errorf("%s: terminal status %s but LeaseID still set %q", label, task.Status, *task.LeaseID)
		}
		if task.LeasedBy != nil && *task.LeasedBy != "" {
			t.Errorf("%s: terminal status %s but LeasedBy still set %q", label, task.Status, *task.LeasedBy)
		}
	case persistence.TaskStatusQueued,
		persistence.TaskStatusPending:
		// Idle: lease should also be cleared so the scheduler can
		// re-lease cleanly.
		if task.LeaseID != nil && *task.LeaseID != "" {
			t.Errorf("%s: idle status %s but LeaseID still set %q", label, task.Status, *task.LeaseID)
		}
	}
	// Attempt sanity. MaxAttempts==0 is legacy/unconfigured;
	// otherwise Attempt MUST be in [1, MaxAttempts].
	if task.MaxAttempts > 0 {
		if task.Attempt < 1 {
			t.Errorf("%s: attempt %d below floor 1", label, task.Attempt)
		}
		if task.Attempt > task.MaxAttempts {
			t.Errorf("%s: attempt %d above max %d", label, task.Attempt, task.MaxAttempts)
		}
		// FAILED with budget remaining is a contradiction — the
		// retry path should have flipped to QUEUED instead.
		if task.Status == persistence.TaskStatusFailed && task.Attempt < task.MaxAttempts {
			t.Errorf("%s: FAILED with retry budget remaining (attempt %d / max %d)", label, task.Attempt, task.MaxAttempts)
		}
	}
}

// TestLifecycle_SuccessfulCompletion — the happy path: task runs,
// completes, status flips to COMPLETED, lease clears.
func TestLifecycle_SuccessfulCompletion(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "scheduler-1-123"
	leaseBy := "scheduler-host-1"
	repo.AddTask(&persistence.Task{
		ID:          "task_lifecycle_success",
		Status:      persistence.TaskStatusRunning,
		Attempt:     1,
		MaxAttempts: 3,
		LeaseID:     &leaseID,
		LeasedBy:    &leaseBy,
	})
	s := New(repo, DefaultConfig())

	if err := s.TaskCompleted("task_lifecycle_success", leaseID, true, ""); err != nil {
		t.Fatalf("TaskCompleted success: %v", err)
	}
	final, _ := repo.Get(context.Background(), "task_lifecycle_success")
	if final.Status != persistence.TaskStatusCompleted {
		t.Errorf("status: got %s, want COMPLETED", final.Status)
	}
	invariantCheck(t, "successful completion", final)
}

// TestLifecycle_FailureWithRetryBudget — failure with budget
// remaining MUST land QUEUED (not FAILED) and bump Attempt.
func TestLifecycle_FailureWithRetryBudget(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "scheduler-1-124"
	leaseBy := "scheduler-host-1"
	repo.AddTask(&persistence.Task{
		ID:          "task_lifecycle_retry",
		Status:      persistence.TaskStatusRunning,
		Attempt:     1,
		MaxAttempts: 3,
		LeaseID:     &leaseID,
		LeasedBy:    &leaseBy,
	})
	s := New(repo, DefaultConfig())

	if err := s.TaskCompleted("task_lifecycle_retry", leaseID, false, "transient"); err != nil {
		t.Fatalf("TaskCompleted failure: %v", err)
	}
	final, _ := repo.Get(context.Background(), "task_lifecycle_retry")
	if final.Status != persistence.TaskStatusQueued {
		t.Errorf("status: got %s, want QUEUED (retry budget remained)", final.Status)
	}
	if final.Attempt != 2 {
		t.Errorf("attempt: got %d, want 2 (incremented from 1)", final.Attempt)
	}
	invariantCheck(t, "failure with retry budget", final)
}

// TestLifecycle_FailureExhaustsRetries — after the last attempt
// fails, status MUST be FAILED. Pre-fix the scheduler could leave
// it RUNNING when the executor's goroutine returned without the
// scheduler dispatch loop watching (recovered-exec path).
func TestLifecycle_FailureExhaustsRetries(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "scheduler-1-125"
	leaseBy := "scheduler-host-1"
	repo.AddTask(&persistence.Task{
		ID:          "task_lifecycle_terminal",
		Status:      persistence.TaskStatusRunning,
		Attempt:     3,
		MaxAttempts: 3,
		LeaseID:     &leaseID,
		LeasedBy:    &leaseBy,
	})
	s := New(repo, DefaultConfig())

	if err := s.TaskCompleted("task_lifecycle_terminal", leaseID, false, "exhausted"); err != nil {
		t.Fatalf("TaskCompleted terminal: %v", err)
	}
	final, _ := repo.Get(context.Background(), "task_lifecycle_terminal")
	if final.Status != persistence.TaskStatusFailed {
		t.Errorf("status: got %s, want FAILED", final.Status)
	}
	invariantCheck(t, "terminal failure", final)
}

// TestLifecycle_TaskCompletedAcrossAllStates — exhaustive table
// drive: every (input status, success, attempt/max) tuple lands
// the right output. Catches a future scheduler refactor that
// flips the state machine for one of the rarer combinations.
func TestLifecycle_TaskCompletedAcrossAllStates(t *testing.T) {
	cases := []struct {
		name    string
		attempt int
		max     int
		success bool
		want    persistence.TaskStatus
	}{
		{"first_attempt_success", 1, 3, true, persistence.TaskStatusCompleted},
		{"first_attempt_fail_can_retry", 1, 3, false, persistence.TaskStatusQueued},
		{"middle_attempt_success", 2, 3, true, persistence.TaskStatusCompleted},
		{"middle_attempt_fail_can_retry", 2, 3, false, persistence.TaskStatusQueued},
		{"last_attempt_success", 3, 3, true, persistence.TaskStatusCompleted},
		{"last_attempt_fail_terminal", 3, 3, false, persistence.TaskStatusFailed},
		{"max_attempts_zero_legacy_success", 1, 0, true, persistence.TaskStatusCompleted},
		{"max_attempts_zero_legacy_failure_terminal", 1, 0, false, persistence.TaskStatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := NewMockTaskRepository()
			leaseID := "scheduler-l-" + tc.name
			leaseBy := "h"
			repo.AddTask(&persistence.Task{
				ID:          "task_" + tc.name,
				Status:      persistence.TaskStatusRunning,
				Attempt:     tc.attempt,
				MaxAttempts: tc.max,
				LeaseID:     &leaseID,
				LeasedBy:    &leaseBy,
			})
			s := New(repo, DefaultConfig())
			err := s.TaskCompleted("task_"+tc.name, leaseID, tc.success, "")
			if err != nil {
				t.Fatalf("TaskCompleted: %v", err)
			}
			final, _ := repo.Get(context.Background(), "task_"+tc.name)
			if final.Status != tc.want {
				t.Errorf("status: got %s, want %s", final.Status, tc.want)
			}
			invariantCheck(t, tc.name, final)
		})
	}
}

// TestLifecycle_NeverFailedWithBudgetRemaining — the strongest
// invariant: across every successful + failure path, a task MUST
// NEVER be FAILED while it still has retry budget. This catches
// a class of bugs where a later code change accidentally flips
// the terminal predicate (e.g. `<=` instead of `<`).
func TestLifecycle_NeverFailedWithBudgetRemaining(t *testing.T) {
	for attempt := 1; attempt <= 5; attempt++ {
		for max := 1; max <= 5; max++ {
			if attempt > max {
				continue
			}
			repo := NewMockTaskRepository()
			leaseID := "lease-x"
			leaseBy := "h"
			repo.AddTask(&persistence.Task{
				ID:          "task_invariant",
				Status:      persistence.TaskStatusRunning,
				Attempt:     attempt,
				MaxAttempts: max,
				LeaseID:     &leaseID,
				LeasedBy:    &leaseBy,
			})
			s := New(repo, DefaultConfig())
			if err := s.TaskCompleted("task_invariant", leaseID, false, ""); err != nil {
				t.Fatalf("attempt=%d max=%d: %v", attempt, max, err)
			}
			final, _ := repo.Get(context.Background(), "task_invariant")
			if final.Status == persistence.TaskStatusFailed && attempt < max {
				t.Errorf("attempt=%d max=%d: terminal FAILED with budget remaining", attempt, max)
			}
			invariantCheck(t, "fail-with-budget-check", final)
		}
	}
}
