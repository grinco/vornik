package sqlite_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestSecretRedactionAuditRepository_RecordAndCount — the secret-leak
// Phase 3 badge data path: Record a batch of redaction events, then
// CountByTask sums per finding type. Backlog item 2.
func TestSecretRedactionAuditRepository_RecordAndCount(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewSecretRedactionAuditRepository(db.DB)
	ctx := context.Background()

	// Empty batch is a no-op.
	if err := repo.Record(ctx, nil); err != nil {
		t.Fatalf("Record(nil): %v", err)
	}

	events := []persistence.SecretRedactionEvent{
		{ProjectID: "p1", TaskID: "task-x", ExecutionID: "exec-1", Checkpoint: "result_json", FindingType: "openai_key", Count: 2, Source: "live"},
		{ProjectID: "p1", TaskID: "task-x", ExecutionID: "exec-1", Checkpoint: "result_json", FindingType: "entropy", Count: 1, Source: "live"},
		// A second event for the same task+type must SUM in the count.
		{ProjectID: "p1", TaskID: "task-x", Checkpoint: "tool_audit", FindingType: "openai_key", Count: 3, Source: "live"},
		// A different task must not bleed into task-x's badge.
		{ProjectID: "p1", TaskID: "task-y", Checkpoint: "result_json", FindingType: "github_pat", Count: 5, Source: "scan"},
	}
	if err := repo.Record(ctx, events); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// IDs must be populated.
	for i, e := range events {
		if e.ID == "" {
			t.Errorf("event %d: Record should populate ID", i)
		}
	}

	byType, total, err := repo.CountByTask(ctx, "task-x")
	if err != nil {
		t.Fatalf("CountByTask: %v", err)
	}
	if byType["openai_key"] != 5 { // 2 + 3
		t.Errorf("openai_key = %d, want 5", byType["openai_key"])
	}
	if byType["entropy"] != 1 {
		t.Errorf("entropy = %d, want 1", byType["entropy"])
	}
	if _, ok := byType["github_pat"]; ok {
		t.Errorf("task-y's github_pat must not appear on task-x: %+v", byType)
	}
	if total != 6 { // 5 + 1
		t.Errorf("total = %d, want 6", total)
	}

	// A task with no redactions returns an empty map / zero total.
	empty, t0, err := repo.CountByTask(ctx, "task-none")
	if err != nil {
		t.Fatalf("CountByTask(none): %v", err)
	}
	if len(empty) != 0 || t0 != 0 {
		t.Errorf("no-redaction task should be empty; got %+v total=%d", empty, t0)
	}

	// Empty taskID short-circuits without a query.
	if _, t1, err := repo.CountByTask(ctx, ""); err != nil || t1 != 0 {
		t.Errorf("empty taskID = (%d, %v), want (0, nil)", t1, err)
	}
}
