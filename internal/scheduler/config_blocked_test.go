package scheduler

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// configBlockedExec records whether it was ever asked to run anything.
type configBlockedExec struct{ executed []string }

func (e *configBlockedExec) ExecuteWithContext(_ context.Context, taskID string) error {
	e.executed = append(e.executed, taskID)
	return nil
}
func (e *configBlockedExec) IsExecuting(string) bool { return false }
func (e *configBlockedExec) Cancel(string) error     { return nil }

// Regression: re-audit 2026-09-15 CA-20 — "unresolved journal recovery does
// not block affected execution".
//
// ApplyEngine.BlockedProjects existed, was tested, and had NO PRODUCTION
// CALLER. Startup recovery logged "affected projects may be mid-apply" and
// then went straight on to Scheduler.Start, so a project whose config was
// half-applied or externally drifted executed tasks against it after a
// restart — against config nobody had resolved.
//
// THE SEAM, per the design's §1.4 ("EXTERNAL DRIFT ... blocks affected
// execution and waits for an operator"): computing a blocked set is not
// blocking anything. This asserts the gate at the point execution actually
// happens, and the container test asserts that production supplies it.
func TestScheduler_DoesNotDispatchABlockedProject(t *testing.T) {
	exec := &configBlockedExec{}
	repo := NewMockTaskRepository()
	task := w2LeasedTask("task_1")
	task.ProjectID = "digest"
	task.Status = persistence.TaskStatusRunning
	repo.AddTask(task)

	s := NewWithOptions(repo, &Config{LeaseDurationSeconds: 60, PollInterval: time.Minute},
		WithExecutor(exec),
		WithConfigBlocked(func(projectID string) (bool, string) {
			return projectID == "digest", "journal row unresolved"
		}),
	)
	s.runningCount = 1
	s.dispatchWg.Add(1)
	s.dispatchTask(task)

	if len(exec.executed) != 0 {
		t.Fatalf("a blocked project's task was executed: %v", exec.executed)
	}
	repo.mu.Lock()
	got := repo.tasks["task_1"].Status
	repo.mu.Unlock()
	if got != persistence.TaskStatusPending {
		t.Fatalf("a blocked task must go back to PENDING for a later tick, got %s", got)
	}
}

// An UNBLOCKED project must still run, or the gate would be an outage rather
// than a guard — the shape CA-19 just taught us to check for.
func TestScheduler_DispatchesWhenNotBlocked(t *testing.T) {
	exec := &configBlockedExec{}
	repo := NewMockTaskRepository()
	task := w2LeasedTask("task_2")
	task.ProjectID = "other"
	task.Status = persistence.TaskStatusRunning
	repo.AddTask(task)

	s := NewWithOptions(repo, &Config{LeaseDurationSeconds: 60, PollInterval: time.Minute},
		WithExecutor(exec),
		WithConfigBlocked(func(projectID string) (bool, string) {
			return projectID == "digest", "journal row unresolved"
		}),
	)
	s.runningCount = 1
	s.dispatchWg.Add(1)
	s.dispatchTask(task)

	if len(exec.executed) != 1 {
		t.Fatalf("an unblocked project must still execute, got %v", exec.executed)
	}
}
