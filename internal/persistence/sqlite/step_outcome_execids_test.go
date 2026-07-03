package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestExecutionStepOutcome_ListByExecutionIDs — the ExecutionIDs filter (E5,
// audit 2026-07-03) returns rows for the whole run set in one query and
// excludes executions not in the set, so the healing-evidence path drops its
// per-execution N+1.
func TestExecutionStepOutcome_ListByExecutionIDs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewExecutionStepOutcomeRepository(db.DB)

	// Three executions, one row each.
	for i, exec := range []string{"e1", "e2", "e3"} {
		if err := repo.Record(ctx, &persistence.ExecutionStepOutcome{
			ID:          "oc-" + exec,
			ProjectID:   "p",
			TaskID:      "t",
			ExecutionID: exec,
			StepID:      "s1",
			Role:        "worker",
			Model:       "m",
			Outcome:     "ok",
			RecordedAt:  time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("Record %s: %v", exec, err)
		}
	}

	got, err := repo.List(ctx, persistence.ExecutionStepOutcomeFilter{
		ExecutionIDs: []string{"e1", "e3"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows for {e1,e3}, got %d", len(got))
	}
	for _, r := range got {
		if r.ExecutionID != "e1" && r.ExecutionID != "e3" {
			t.Errorf("unexpected execution %q in result", r.ExecutionID)
		}
	}
}
