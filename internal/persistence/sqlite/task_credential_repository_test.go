package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestTaskCredentialRepository_UpsertAndLatestExecution exercises the
// credential-carryover store: upsert is idempotent within an execution
// (a re-publish overwrites, never duplicates), and surfacing returns only
// the task's most-recently-capturing execution so a retry's stale credential
// is hidden.
func TestTaskCredentialRepository_UpsertAndLatestExecution(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTaskCredentialRepository(db.DB)
	ctx := context.Background()

	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

	// exec-1 captures a viewing password.
	c1 := &persistence.TaskCredential{
		TaskID: "task-x", ExecutionID: "exec-1", Tool: "mcp__pagedrop__pagedrop_publish",
		Label: "viewing password", Value: "pw-old", ArtifactURL: "https://v/p/1", CreatedAt: base,
	}
	if err := repo.Upsert(ctx, c1); err != nil {
		t.Fatalf("Upsert c1: %v", err)
	}
	if c1.ID == "" {
		t.Fatal("Upsert should populate ID")
	}

	// A re-publish within exec-1 (same task/exec/tool/url) overwrites in place.
	c1b := &persistence.TaskCredential{
		TaskID: "task-x", ExecutionID: "exec-1", Tool: "mcp__pagedrop__pagedrop_publish",
		Label: "viewing password", Value: "pw-new", ArtifactURL: "https://v/p/1", CreatedAt: base.Add(time.Second),
	}
	if err := repo.Upsert(ctx, c1b); err != nil {
		t.Fatalf("Upsert c1b: %v", err)
	}

	got, err := repo.ListByTaskLatestExecution(ctx, "task-x")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("after re-publish want 1 row (overwrite), got %d", len(got))
	}
	if got[0].Value != "pw-new" {
		t.Errorf("value = %q, want overwritten pw-new", got[0].Value)
	}

	// A retry (exec-2, later) captures a fresh password. Surfacing must show
	// ONLY exec-2's credential, not exec-1's stale one.
	c2 := &persistence.TaskCredential{
		TaskID: "task-x", ExecutionID: "exec-2", Tool: "mcp__pagedrop__pagedrop_publish",
		Label: "viewing password", Value: "pw-retry", ArtifactURL: "https://v/p/2", CreatedAt: base.Add(time.Hour),
	}
	if err := repo.Upsert(ctx, c2); err != nil {
		t.Fatalf("Upsert c2: %v", err)
	}
	got, err = repo.ListByTaskLatestExecution(ctx, "task-x")
	if err != nil {
		t.Fatalf("List after retry: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("latest-execution filter want 1 row, got %d", len(got))
	}
	if got[0].ExecutionID != "exec-2" || got[0].Value != "pw-retry" {
		t.Errorf("got %s/%q, want exec-2/pw-retry (stale exec-1 must be hidden)", got[0].ExecutionID, got[0].Value)
	}

	// Unknown task → empty.
	none, err := repo.ListByTaskLatestExecution(ctx, "task-none")
	if err != nil {
		t.Fatalf("List none: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("unknown task want 0 rows, got %d", len(none))
	}
}
