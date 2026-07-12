package sqlite_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestTransitionToCancelled_AllowsPaused — an operator-paused task can be
// cancelled (not only resumed). Regression for the UI gap where PAUSED tasks
// offered no Cancel action; the SQL guard now includes PAUSED.
func TestTransitionToCancelled_AllowsPaused(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()

	task := &persistence.Task{ID: "tp1", ProjectID: "p1", Status: persistence.TaskStatusPaused}
	if err := repo.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cancelled, err := repo.TransitionToCancelled(ctx, "tp1")
	if err != nil || !cancelled {
		t.Fatalf("PAUSED → CANCELLED must succeed; got cancelled=%v err=%v", cancelled, err)
	}
	got, _ := repo.Get(ctx, "tp1")
	if got == nil || got.Status != persistence.TaskStatusCancelled {
		t.Fatalf("status = %v, want CANCELLED", got.Status)
	}
}
