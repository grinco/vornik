package sqlite_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestTaskRepository_OrphanChildren covers the retry-path child-detach.
// A delegating (resume_after_children) parent records its subtasks as
// children hanging off parent_task_id; on retry the resume guard fast-
// fails the entrypoint while those prior-attempt children remain, so the
// retry burns its budget without ever re-decomposing. OrphanChildren
// detaches them (parent_task_id → NULL) so GetChildren returns empty and
// decompose re-runs, while the child rows survive as standalone history.
// Regression for backlog item 1.
func TestTaskRepository_OrphanChildren(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()

	parentID := "orphan-parent"
	if err := repo.Create(ctx, &persistence.Task{ID: parentID, ProjectID: "p1"}); err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	for _, id := range []string{"kid-1", "kid-2"} {
		if err := repo.Create(ctx, &persistence.Task{ID: id, ProjectID: "p1", ParentTaskID: &parentID}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}

	n, err := repo.OrphanChildren(ctx, parentID)
	if err != nil {
		t.Fatalf("OrphanChildren: %v", err)
	}
	if n != 2 {
		t.Errorf("OrphanChildren affected = %d, want 2", n)
	}

	// The resume guard reads GetChildren; it must now be empty so
	// decompose re-runs fresh.
	kids, err := repo.GetChildren(ctx, parentID)
	if err != nil {
		t.Fatalf("GetChildren: %v", err)
	}
	if len(kids) != 0 {
		t.Errorf("GetChildren after orphan = %d, want 0", len(kids))
	}

	// The child rows survive (audit trail) with a nil parent.
	for _, id := range []string{"kid-1", "kid-2"} {
		got, gErr := repo.Get(ctx, id)
		if gErr != nil {
			t.Fatalf("Get %s: %v", id, gErr)
		}
		if got == nil {
			t.Fatalf("child %s was deleted; orphaning must preserve the row", id)
		}
		if got.ParentTaskID != nil {
			t.Errorf("child %s still has parent_task_id %v, want NULL", id, *got.ParentTaskID)
		}
	}

	// Idempotent: a second call detaches nothing.
	if n2, err2 := repo.OrphanChildren(ctx, parentID); err2 != nil || n2 != 0 {
		t.Errorf("second OrphanChildren = (%d, %v), want (0, nil)", n2, err2)
	}
}
