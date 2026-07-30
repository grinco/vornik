package scheduler

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// INCIDENT 2026-07-30, customer deployment. A task failed after exhausting three attempts
// with `last_error = "task left executor in non-terminal status RUNNING"` and
// **`last_error_class` EMPTY**. The watchdog had stamped STUCK_EXECUTION on the EXECUTION
// row nine seconds later, but the task — the object an operator actually inspects — carried
// no class at all.
//
// Consequence: `vornikctl task explain <id>` had no class to explain and
// `vornikctl playbook show <CLASS>` had nothing to look up. The playbook corpus is
// described in the troubleshooting guide as "the highest-value and least-known path in the
// product", and it was unreachable from the failure it was written for.
//
// Cause: Scheduler.TaskCompleted builds ReleaseOptions with `Error` and never `ErrorClass`.
// ReleaseOptions.ErrorClass has existed all along, and its own comment says empty
// "preserves the existing column when a caller hasn't (yet) been updated to classify its
// failures — enables progressive rollout". The scheduler was one of the callers never
// updated.
func TestSchedulerFailureClass_NeverEmptyOnTerminalFailure(t *testing.T) {
	got := classifySchedulerFailure(
		&persistence.Task{ID: "t1"},
		"task left executor in non-terminal status RUNNING",
	)
	if got == "" {
		t.Fatal("a terminal failure must carry SOME class — an empty column is what made " +
			"playbook show and task explain dead ends")
	}
	if !persistence.IsKnownTaskFailureClass(got) {
		t.Fatalf("class %q is not a known class, so `playbook show %s` would fail too — "+
			"stamping an unrecognised class just moves the dead end", got, got)
	}
}

// The empty-preserves contract is load-bearing. When the executor has already stamped a
// precise class (TOOL_ITERATION_LIMIT, SECRET_LEAK, WORKFLOW_DRIFT), the scheduler's later
// release must NOT overwrite it with a generic one — that would actively destroy
// information and make the report worse than before this change.
func TestSchedulerFailureClass_PreservesAPreciseClassAlreadySet(t *testing.T) {
	for _, existing := range []string{
		persistence.TaskFailureClassToolIterationLimit,
		persistence.TaskFailureClassSecretLeak,
		persistence.TaskFailureClassWorkflowDrift,
		persistence.TaskFailureClassStuckExecution,
	} {
		t.Run(existing, func(t *testing.T) {
			cls := existing
			task := &persistence.Task{ID: "t1", LastErrorClass: &cls}
			if got := classifySchedulerFailure(task, "some generic scheduler message"); got != "" {
				t.Errorf("class = %q, want empty so the existing %q is preserved",
					got, existing)
			}
		})
	}
}

// An UNKNOWN already on the row is not worth preserving — it carries no information, so a
// later pass may replace it.
func TestSchedulerFailureClass_ReplacesAnExistingUnknown(t *testing.T) {
	cls := persistence.TaskFailureClassUnknown
	task := &persistence.Task{ID: "t1", LastErrorClass: &cls}
	if got := classifySchedulerFailure(task, "anything"); got == "" {
		t.Error("an existing UNKNOWN should not block a later classification attempt")
	}
}

// A nil task (lookup failed) must still yield a class rather than panicking: that path
// exists precisely because the scheduler could not read the row.
func TestSchedulerFailureClass_NilTaskIsSafe(t *testing.T) {
	if got := classifySchedulerFailure(nil, "boom"); got == "" {
		t.Error("a failure we cannot look up still deserves a class")
	}
}

// End to end through TaskCompleted: the terminal release must carry a class into
// ReleaseOptions, because that is the field the operator-facing column is written from.
func TestTaskCompleted_StampsAClassOnTerminalFailure(t *testing.T) {
	repo := NewMockTaskRepository()
	lease := "lease-1"
	repo.tasks["t1"] = &persistence.Task{
		ID:          "t1",
		Status:      persistence.TaskStatusRunning,
		Attempt:     3,
		MaxAttempts: 3,
		LeaseID:     &lease,
	}
	s := &Scheduler{repo: repo}

	if err := s.TaskCompleted("t1", lease, false,
		"task left executor in non-terminal status RUNNING"); err != nil {
		t.Fatalf("TaskCompleted: %v", err)
	}

	task := repo.tasks["t1"]
	if task.Status != persistence.TaskStatusFailed {
		t.Fatalf("status = %s, want FAILED", task.Status)
	}
	if task.LastErrorClass == nil || *task.LastErrorClass == "" {
		t.Fatal("last_error_class is still empty after a terminal failure — this is the " +
			"exact 2026-07-30 defect")
	}
	if !persistence.IsKnownTaskFailureClass(*task.LastErrorClass) {
		t.Errorf("class = %q, not a known class", *task.LastErrorClass)
	}
}

// A retry is not terminal. Stamping a generic class mid-flight would overwrite whatever the
// next attempt learns, so a retry release must leave the column alone.
func TestTaskCompleted_RetryDoesNotStampAGenericClass(t *testing.T) {
	repo := NewMockTaskRepository()
	lease := "lease-1"
	repo.tasks["t1"] = &persistence.Task{
		ID:          "t1",
		Status:      persistence.TaskStatusRunning,
		Attempt:     1,
		MaxAttempts: 3,
		LeaseID:     &lease,
	}
	s := &Scheduler{repo: repo}

	if err := s.TaskCompleted("t1", lease, false, "transient boom"); err != nil {
		t.Fatalf("TaskCompleted: %v", err)
	}
	task := repo.tasks["t1"]
	if task.Status != persistence.TaskStatusQueued {
		t.Fatalf("status = %s, want QUEUED for a retry", task.Status)
	}
	if task.LastErrorClass != nil && *task.LastErrorClass == persistence.TaskFailureClassUnknown {
		t.Error("a retry must not stamp UNKNOWN — the next attempt may classify precisely")
	}
}

// Success must never write a class.
func TestTaskCompleted_SuccessCarriesNoClass(t *testing.T) {
	repo := NewMockTaskRepository()
	lease := "lease-1"
	repo.tasks["t1"] = &persistence.Task{
		ID: "t1", Status: persistence.TaskStatusRunning, LeaseID: &lease,
	}
	s := &Scheduler{repo: repo}

	if err := s.TaskCompleted("t1", lease, true, ""); err != nil {
		t.Fatalf("TaskCompleted: %v", err)
	}
	if task := repo.tasks["t1"]; task.LastErrorClass != nil {
		t.Errorf("class = %v on a successful task, want none", *task.LastErrorClass)
	}
}

// Guard the registry itself: every class the scheduler can stamp must be one the playbook
// corpus knows, or the fix reintroduces the dead end it exists to remove.
func TestIsKnownTaskFailureClass(t *testing.T) {
	if !persistence.IsKnownTaskFailureClass(persistence.TaskFailureClassUnknown) {
		t.Error("UNKNOWN must be a known class — it is the documented fallback")
	}
	if persistence.IsKnownTaskFailureClass("NOT_A_REAL_CLASS") {
		t.Error("an invented class must not validate")
	}
	if persistence.IsKnownTaskFailureClass("") {
		t.Error("empty must not validate as a class")
	}
}
