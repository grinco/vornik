package sqlite_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestTaskRepository_AwaitingApprovalStatusAccepted is the DB-level
// guard for the autonomy manual-approval gate
// (https://docs.vornik.io). The
// tasks.status CHECK constraint must admit AWAITING_APPROVAL — without
// it, the autonomy manager's Create would fail and approval-gated tasks
// could never be persisted. Mirrors the Postgres enum value added in
// migration v93.
func TestTaskRepository_AwaitingApprovalStatusAccepted(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskRepository(db.DB)

	task := &persistence.Task{
		ID:        "task-awaiting-approval",
		ProjectID: "p",
		Status:    persistence.TaskStatusAwaitingApproval,
		Payload:   []byte(`{}`),
	}
	if err := repo.Create(ctx, task); err != nil {
		t.Fatalf("Create with AWAITING_APPROVAL status: %v", err)
	}

	got, err := repo.Get(ctx, "task-awaiting-approval")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != persistence.TaskStatusAwaitingApproval {
		t.Errorf("status round-trip: want AWAITING_APPROVAL, got %s", got.Status)
	}
}
