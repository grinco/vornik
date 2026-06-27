package sqlite_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestExecutionHintRepository_ListForExecution_UnionsScopes pins the
// §8.6 fix: the live-view hint history must surface BOTH execution-
// scoped hints AND task-scoped hints (which carry across retries),
// where ListByExecution returned only the execution-scoped ones.
func TestExecutionHintRepository_ListForExecution_UnionsScopes(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewExecutionHintRepository(db.DB)
	ctx := context.Background()

	// Execution-scoped hint for exec_1.
	mustInsertHint(t, repo, &persistence.ExecutionHint{
		ID: "h_exec", ExecutionID: "exec_1", Content: "exec scoped", CreatedBy: "op",
	})
	// Task-scoped hint for task_99 (carries across retries).
	mustInsertHint(t, repo, &persistence.ExecutionHint{
		ID: "h_task", TaskID: "task_99", Content: "task scoped", CreatedBy: "op",
	})
	// Noise: an unrelated execution's hint must NOT appear.
	mustInsertHint(t, repo, &persistence.ExecutionHint{
		ID: "h_other", ExecutionID: "exec_other", Content: "other", CreatedBy: "op",
	})

	// ListByExecution (legacy) sees only the execution-scoped hint.
	legacy, err := repo.ListByExecution(ctx, "exec_1")
	if err != nil {
		t.Fatalf("ListByExecution: %v", err)
	}
	if len(legacy) != 1 {
		t.Fatalf("ListByExecution returned %d, want 1 (regression guard)", len(legacy))
	}

	// ListForExecution sees BOTH the execution-scoped and the
	// task-scoped hint, but not the unrelated execution's hint.
	got, err := repo.ListForExecution(ctx, "exec_1", "task_99")
	if err != nil {
		t.Fatalf("ListForExecution: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListForExecution returned %d, want 2 (exec + task scoped)", len(got))
	}
	ids := map[string]bool{}
	for _, h := range got {
		ids[h.ID] = true
	}
	if !ids["h_exec"] || !ids["h_task"] {
		t.Errorf("missing expected hint IDs; got %v", ids)
	}
	if ids["h_other"] {
		t.Error("unrelated execution's hint leaked into the result")
	}
}

func mustInsertHint(t *testing.T, repo *sqlite.ExecutionHintRepository, h *persistence.ExecutionHint) {
	t.Helper()
	if err := repo.Insert(context.Background(), h); err != nil {
		t.Fatalf("Insert %s: %v", h.ID, err)
	}
}
