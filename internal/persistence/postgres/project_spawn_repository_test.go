package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

func TestProjectSpawn_Create_StampsID(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewProjectSpawnRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO project_spawns")).
		WithArgs(
			sqlmock.AnyArg(),
			"task-parent", "marketing", "step-spawn",
			"sales-q3-2026", "sales-campaign",
			sqlmock.AnyArg(),
		).WillReturnResult(sqlmock.NewResult(0, 1))

	s := &persistence.ProjectSpawn{
		ParentTaskID:   "task-parent",
		ParentProject:  "marketing",
		ParentStepID:   "step-spawn",
		SpawnedProject: "sales-q3-2026",
		TemplateSlug:   "sales-campaign",
		Params:         []byte(`{"name":"q3"}`),
	}
	if err := repo.Create(context.Background(), s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID == "" {
		t.Error("ID should be stamped")
	}
}

// TestProjectSpawn_CountForProjectSince_PowersRateLimit asserts
// the rate-limit query is shaped correctly. The maxSpawnsPerDay
// gate calls this with time.Now().Add(-24h).
func TestProjectSpawn_CountForProjectSince_PowersRateLimit(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewProjectSpawnRepository(db)

	since := time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM project_spawns")).
		WithArgs("marketing", since).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	n, err := repo.CountForProjectSince(context.Background(), "marketing", since)
	if err != nil {
		t.Fatalf("CountForProjectSince: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
}

// TestProjectSpawn_GetBySpawnedProject_NotFound asserts the
// executor's idempotence short-circuit can distinguish "no
// previous spawn" from a DB error.
func TestProjectSpawn_GetBySpawnedProject_NotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewProjectSpawnRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, parent_task_id")).
		WithArgs("sales-missing").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "parent_task_id", "parent_project", "parent_step_id",
			"spawned_project", "template_slug", "params", "created_at",
		}))

	_, err := repo.GetBySpawnedProject(context.Background(), "sales-missing")
	if err != persistence.ErrNotFound {
		t.Errorf("GetBySpawnedProject missing = %v, want ErrNotFound", err)
	}
}
