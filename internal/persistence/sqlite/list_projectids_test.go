package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestExecutionList_ByProjectIDs / TestTaskList_ByProjectIDs — the ProjectIDs
// filter (E3, audit 2026-07-03) selects rows across several projects in one
// query (project_id IN (...)) and excludes projects not in the set, so the
// scoped list handlers merge/sort/limit in the DB instead of per-project.
func TestExecutionList_ByProjectIDs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewExecutionRepository(db.DB)

	for i, p := range []string{"pa", "pb", "pc"} {
		if err := repo.Create(ctx, &persistence.Execution{
			ID:        "e-" + p,
			TaskID:    "t-" + p,
			ProjectID: p,
			Status:    persistence.ExecutionStatusRunning,
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("Create %s: %v", p, err)
		}
	}

	got, err := repo.List(ctx, persistence.ExecutionFilter{ProjectIDs: []string{"pa", "pc"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 execs for {pa,pc}, got %d", len(got))
	}
	for _, e := range got {
		if e.ProjectID != "pa" && e.ProjectID != "pc" {
			t.Errorf("unexpected project %q", e.ProjectID)
		}
	}
}

func TestTaskList_ByProjectIDs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskRepository(db.DB)

	for i, p := range []string{"pa", "pb", "pc"} {
		if err := repo.Create(ctx, &persistence.Task{
			ID:        "t-" + p,
			ProjectID: p,
			Status:    persistence.TaskStatusQueued,
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("Create %s: %v", p, err)
		}
	}

	got, err := repo.List(ctx, persistence.TaskFilter{ProjectIDs: []string{"pa", "pc"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 tasks for {pa,pc}, got %d", len(got))
	}
	for _, tk := range got {
		if tk.ProjectID != "pa" && tk.ProjectID != "pc" {
			t.Errorf("unexpected project %q", tk.ProjectID)
		}
	}
}
