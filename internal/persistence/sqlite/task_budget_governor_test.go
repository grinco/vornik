package sqlite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// seedTask inserts a minimal task row for the per-task budget governor tests.
func seedTask(t *testing.T, repo *sqlite.TaskRepository, id, projectID string, status persistence.TaskStatus, budget *float64) {
	t.Helper()
	err := repo.Create(context.Background(), &persistence.Task{
		ID:        id,
		ProjectID: projectID,
		Status:    status,
		BudgetUSD: budget,
	})
	if err != nil {
		t.Fatalf("seed task %s: %v", id, err)
	}
}

func seedUsage(t *testing.T, repo *sqlite.TaskLLMUsageRepository, id, taskID, execID string, cost float64) {
	t.Helper()
	tid, eid := taskID, execID
	err := repo.Record(context.Background(), &persistence.TaskLLMUsage{
		ID:          id,
		ProjectID:   "p1",
		TaskID:      &tid,
		ExecutionID: &eid,
		StepID:      "step",
		Role:        "coder",
		Model:       "m",
		CostUSD:     cost,
		Source:      persistence.TaskLLMUsageSourceWorkflowStep,
		RecordedAt:  time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed usage %s: %v", id, err)
	}
}

// TestSumCostByTask_UnknownIsZero — an unknown task_id sums to 0 (COALESCE).
func TestSumCostByTask_UnknownIsZero(t *testing.T) {
	db := newTestDB(t)
	usage := sqlite.NewTaskLLMUsageRepository(db.DB)
	got, err := usage.SumCostByTask(context.Background(), "nope")
	if err != nil {
		t.Fatalf("SumCostByTask: %v", err)
	}
	if got != 0 {
		t.Fatalf("unknown task → 0, got %v", got)
	}
}

// TestSumCostByTask_CumulativeAcrossExecutions — the core §3.2/F1 accounting:
// SumCostByTask sums usage rows across MULTIPLE executions of the same task_id
// (a parked-resume mints a fresh execution), and sums ONLY the given task.
func TestSumCostByTask_CumulativeAcrossExecutions(t *testing.T) {
	db := newTestDB(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	usage := sqlite.NewTaskLLMUsageRepository(db.DB)

	seedTask(t, tasks, "t1", "p1", persistence.TaskStatusRunning, nil)
	seedTask(t, tasks, "t2", "p1", persistence.TaskStatusRunning, nil)

	// t1 execution #1
	seedUsage(t, usage, "u1", "t1", "exec1", 1.25)
	seedUsage(t, usage, "u2", "t1", "exec1", 0.75)
	// t1 execution #2 (the resume re-run — different execution_id, same task)
	seedUsage(t, usage, "u3", "t1", "exec2", 2.00)
	// t2 must not leak into t1's total
	seedUsage(t, usage, "u4", "t2", "execX", 5.00)

	got, err := usage.SumCostByTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("SumCostByTask: %v", err)
	}
	if got != 4.00 {
		t.Fatalf("cumulative across executions = 1.25+0.75+2.00 = 4.00, got %v", got)
	}
}

// TestSumCostByTask_ReparkAfterTopupIsCumulative — the core F1 case at the
// data layer: a task hard-parks (execution #1 spend), an operator tops up the
// budget (RaiseTaskBudget resume), the resume RE-RUNS the task minting
// execution #2, and the second hard evaluation must see the CUMULATIVE lifetime
// spend across BOTH executions — not a fresh-from-zero count. This proves the
// governor's cumulative accounting across resume re-runs.
func TestSumCostByTask_ReparkAfterTopupIsCumulative(t *testing.T) {
	db := newTestDB(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	usage := sqlite.NewTaskLLMUsageRepository(db.DB)

	b := 3.0
	seedTask(t, tasks, "t1", "p1", persistence.TaskStatusAwaitingInput, &b)

	// Execution #1 spend that tripped the original $3 budget → parked.
	seedUsage(t, usage, "e1a", "t1", "exec1", 2.0)
	seedUsage(t, usage, "e1b", "t1", "exec1", 1.5) // lifetime = 3.5

	// Operator tops up to $6 and resumes (AWAITING_INPUT→QUEUED).
	ok, err := tasks.RaiseTaskBudget(context.Background(), "t1", 6.0, true)
	if err != nil || !ok {
		t.Fatalf("top-up resume must apply, ok=%v err=%v", ok, err)
	}

	// Resume re-runs → execution #2 adds more spend.
	seedUsage(t, usage, "e2a", "t1", "exec2", 3.0) // lifetime = 6.5

	lifetime, err := usage.SumCostByTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("SumCostByTask: %v", err)
	}
	if lifetime != 6.5 {
		t.Fatalf("cumulative lifetime across both executions = 3.5+3.0 = 6.5, got %v", lifetime)
	}
	// The governor would see 6.5 >= raised budget 6.0 ⇒ re-park (cumulative,
	// not fresh-from-zero which would be only 3.0 < 6.0 and wrongly proceed).
	got, _ := tasks.Get(context.Background(), "t1")
	if got.BudgetUSD == nil || *got.BudgetUSD != 6.0 {
		t.Fatalf("raised budget should be 6.0, got %v", got.BudgetUSD)
	}
	if lifetime < *got.BudgetUSD {
		t.Fatalf("F1 invariant: cumulative %v must be >= raised budget %v to re-park", lifetime, *got.BudgetUSD)
	}
}

// TestCreate_RejectsStoredZeroBudget — the write layer refuses a stored 0/neg
// budget so "off" is always NULL (§3.1).
func TestCreate_RejectsStoredZeroBudget(t *testing.T) {
	db := newTestDB(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	zero := 0.0
	err := tasks.Create(context.Background(), &persistence.Task{ID: "z", ProjectID: "p", Status: persistence.TaskStatusQueued, BudgetUSD: &zero})
	if !errors.Is(err, persistence.ErrInvalidTaskBudget) {
		t.Fatalf("stored 0 must be rejected with ErrInvalidTaskBudget, got %v", err)
	}
	neg := -1.0
	err = tasks.Create(context.Background(), &persistence.Task{ID: "n", ProjectID: "p", Status: persistence.TaskStatusQueued, BudgetUSD: &neg})
	if !errors.Is(err, persistence.ErrInvalidTaskBudget) {
		t.Fatalf("stored negative must be rejected, got %v", err)
	}
}

// TestBudgetRoundTrip — a positive budget persists and reads back; NULL reads
// back as nil.
func TestBudgetRoundTrip(t *testing.T) {
	db := newTestDB(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	b := 3.5
	seedTask(t, tasks, "with", "p", persistence.TaskStatusQueued, &b)
	seedTask(t, tasks, "without", "p", persistence.TaskStatusQueued, nil)

	got, _ := tasks.Get(context.Background(), "with")
	if got.BudgetUSD == nil || *got.BudgetUSD != 3.5 {
		t.Fatalf("budget round-trip: got %v", got.BudgetUSD)
	}
	got2, _ := tasks.Get(context.Background(), "without")
	if got2.BudgetUSD != nil {
		t.Fatalf("nil budget must read back nil, got %v", *got2.BudgetUSD)
	}
}

// TestRaiseTaskBudget_StrictIncrease — a raise only applies when strictly
// greater than the current value; a decrease/equal is a 0-row no-op.
func TestRaiseTaskBudget_StrictIncrease(t *testing.T) {
	db := newTestDB(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	b := 5.0
	seedTask(t, tasks, "t", "p", persistence.TaskStatusAwaitingInput, &b)

	// Decrease → no-op.
	ok, err := tasks.RaiseTaskBudget(context.Background(), "t", 3.0, false)
	if err != nil || ok {
		t.Fatalf("decrease must be 0-row no-op, ok=%v err=%v", ok, err)
	}
	// Equal → no-op.
	ok, _ = tasks.RaiseTaskBudget(context.Background(), "t", 5.0, false)
	if ok {
		t.Fatalf("equal must be 0-row no-op")
	}
	// Increase → applies.
	ok, err = tasks.RaiseTaskBudget(context.Background(), "t", 8.0, false)
	if err != nil || !ok {
		t.Fatalf("increase must apply, ok=%v err=%v", ok, err)
	}
	got, _ := tasks.Get(context.Background(), "t")
	if got.BudgetUSD == nil || *got.BudgetUSD != 8.0 {
		t.Fatalf("budget should be 8.0, got %v", got.BudgetUSD)
	}
	// Non-positive rejected outright.
	if _, err := tasks.RaiseTaskBudget(context.Background(), "t", 0, false); !errors.Is(err, persistence.ErrInvalidTaskBudget) {
		t.Fatalf("non-positive raise must be ErrInvalidTaskBudget, got %v", err)
	}
}

// TestRaiseTaskBudget_ResumeGuardedTransition — resume mode raises the budget
// AND transitions AWAITING_INPUT→QUEUED, but ONLY when the task is
// AWAITING_INPUT (guarded conditional update).
func TestRaiseTaskBudget_ResumeGuardedTransition(t *testing.T) {
	db := newTestDB(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	b := 5.0
	seedTask(t, tasks, "parked", "p", persistence.TaskStatusAwaitingInput, &b)

	ok, err := tasks.RaiseTaskBudget(context.Background(), "parked", 10.0, true)
	if err != nil || !ok {
		t.Fatalf("resume raise on AWAITING_INPUT must apply, ok=%v err=%v", ok, err)
	}
	got, _ := tasks.Get(context.Background(), "parked")
	if got.Status != persistence.TaskStatusQueued {
		t.Fatalf("resume must transition to QUEUED, got %v", got.Status)
	}
	if got.BudgetUSD == nil || *got.BudgetUSD != 10.0 {
		t.Fatalf("resume must raise budget to 10, got %v", got.BudgetUSD)
	}

	// A task NOT in AWAITING_INPUT is a 0-row no-op in resume mode.
	seedTask(t, tasks, "running", "p", persistence.TaskStatusRunning, &b)
	ok, _ = tasks.RaiseTaskBudget(context.Background(), "running", 20.0, true)
	if ok {
		t.Fatalf("resume on non-AWAITING_INPUT must be 0-row no-op")
	}
	got2, _ := tasks.Get(context.Background(), "running")
	if got2.Status != persistence.TaskStatusRunning || got2.BudgetUSD == nil || *got2.BudgetUSD != 5.0 {
		t.Fatalf("no-op must leave status+budget untouched, got %v %v", got2.Status, got2.BudgetUSD)
	}
}

// TestRaiseTaskBudget_CancelVsTopupRace — F2: with the task AWAITING_INPUT, a
// cancel (guarded conditional to CANCELLED) and a top-up-resume are mutually
// exclusive. Whichever runs first wins; the loser matches 0 rows.
func TestRaiseTaskBudget_CancelVsTopupRace(t *testing.T) {
	db := newTestDB(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	b := 5.0

	// Case A: cancel wins first → top-up-resume loses (0 rows), no budget write,
	// task stays CANCELLED.
	seedTask(t, tasks, "a", "p", persistence.TaskStatusAwaitingInput, &b)
	cancelled, err := tasks.TransitionConditional(context.Background(), "a",
		[]persistence.TaskStatus{persistence.TaskStatusAwaitingInput},
		persistence.TaskStatusCancelled, persistence.TransitionOpts{})
	if err != nil || !cancelled {
		t.Fatalf("cancel should win, cancelled=%v err=%v", cancelled, err)
	}
	ok, _ := tasks.RaiseTaskBudget(context.Background(), "a", 10.0, true)
	if ok {
		t.Fatalf("top-up-resume must lose after cancel (0-row no-op)")
	}
	gotA, _ := tasks.Get(context.Background(), "a")
	if gotA.Status != persistence.TaskStatusCancelled {
		t.Fatalf("cancelled task must stay CANCELLED, got %v", gotA.Status)
	}
	if gotA.BudgetUSD == nil || *gotA.BudgetUSD != 5.0 {
		t.Fatalf("no budget write on a cancelled task, got %v", gotA.BudgetUSD)
	}

	// Case B: top-up-resume wins first → cancel loses (0 rows), task is QUEUED.
	seedTask(t, tasks, "bb", "p", persistence.TaskStatusAwaitingInput, &b)
	ok, err = tasks.RaiseTaskBudget(context.Background(), "bb", 10.0, true)
	if err != nil || !ok {
		t.Fatalf("top-up-resume should win, ok=%v err=%v", ok, err)
	}
	cancelled, _ = tasks.TransitionConditional(context.Background(), "bb",
		[]persistence.TaskStatus{persistence.TaskStatusAwaitingInput},
		persistence.TaskStatusCancelled, persistence.TransitionOpts{})
	if cancelled {
		t.Fatalf("cancel must lose after resume (0-row no-op)")
	}
	gotB, _ := tasks.Get(context.Background(), "bb")
	if gotB.Status != persistence.TaskStatusQueued {
		t.Fatalf("resumed task must be QUEUED, got %v", gotB.Status)
	}
}
