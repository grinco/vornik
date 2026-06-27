package executor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestRecordStepOutcome_StampsContextSource confirms that an
// outcome row written for an execution whose workspace prep
// resolved a canonical-context layout carries the layout name
// in ContextSource. Drives the operator-facing query the LLD
// Phase B promises.
func TestRecordStepOutcome_StampsContextSource(t *testing.T) {
	repo := newStubStepOutcomeRepo()
	e := &Executor{outcomeRepo: repo}
	e.contextSourceByExecution.Store("exec-with-source", "dot_autonomy")

	task := &persistence.Task{ID: "t1", ProjectID: "assistant"}
	exec := &persistence.Execution{ID: "exec-with-source"}
	e.recordStepOutcome(context.Background(), task, exec, "step-1", "lead", "m",
		"ok", "", "", nil, nil)

	if len(repo.rows) != 1 {
		t.Fatalf("rows count: %d", len(repo.rows))
	}
	if got := repo.rows[0].ContextSource; got != "dot_autonomy" {
		t.Errorf("ContextSource = %q, want dot_autonomy", got)
	}
}

// TestRecordStepOutcome_EmptyContextSourceWhenAbsent confirms
// the field stays empty for executions whose workspace prep
// didn't populate the cache (e.g. projects that don't use the
// .autonomy/ convention).
func TestRecordStepOutcome_EmptyContextSourceWhenAbsent(t *testing.T) {
	repo := newStubStepOutcomeRepo()
	e := &Executor{outcomeRepo: repo}
	// Nothing stashed for exec-no-source.

	task := &persistence.Task{ID: "t1", ProjectID: "assistant"}
	exec := &persistence.Execution{ID: "exec-no-source"}
	e.recordStepOutcome(context.Background(), task, exec, "step-1", "lead", "m",
		"ok", "", "", nil, nil)

	if got := repo.rows[0].ContextSource; got != "" {
		t.Errorf("ContextSource should be empty when no cache entry; got %q", got)
	}
}
