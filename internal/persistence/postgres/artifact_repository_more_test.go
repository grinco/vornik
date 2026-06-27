package postgres

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

func newArtifactRepo(t *testing.T) (*ArtifactRepository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewArtifactRepository(db), mock, func() { _ = db.Close() }
}

func artifactRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "project_id", "execution_id", "task_id", "name", "artifact_class",
		"storage_path", "size_bytes", "content_hash_sha256", "mime_type", "created_at", "origin",
	})
}

func TestArtifactRepositoryGetAndGetByHash(t *testing.T) {
	repo, mock, cleanup := newArtifactRepo(t)
	defer cleanup()

	created := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	rows := artifactRows().AddRow(
		"art-1", "proj-a", "exec-1", "task-1", "output.json", persistence.ArtifactClassOutput,
		"/tmp/output.json", int64(42), "sha256", "application/json", created, "task_output",
	)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, execution_id, task_id, name, artifact_class")).
		WithArgs("art-1").
		WillReturnRows(rows)

	got, err := repo.Get(context.Background(), "art-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != "art-1" || got.ProjectID != "proj-a" || got.ExecutionID == nil || *got.ExecutionID != "exec-1" {
		t.Fatalf("Get() = %#v", got)
	}
	if got.TaskID == nil || *got.TaskID != "task-1" || got.SizeBytes == nil || *got.SizeBytes != 42 {
		t.Fatalf("Get() nullable fields = %#v", got)
	}

	rows = artifactRows().AddRow(
		"art-2", "proj-a", nil, nil, "log.txt", persistence.ArtifactClassLog,
		"/tmp/log.txt", nil, nil, nil, created, "unknown",
	)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, execution_id, task_id, name, artifact_class")).
		WithArgs("hash-2").
		WillReturnRows(rows)

	got, err = repo.GetByHash(context.Background(), "hash-2")
	if err != nil {
		t.Fatalf("GetByHash() error = %v", err)
	}
	if got.ID != "art-2" || got.ExecutionID != nil || got.TaskID != nil || got.SizeBytes != nil {
		t.Fatalf("GetByHash() nullable fields = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestArtifactRepositoryGetMapsNoRowsToNotFound(t *testing.T) {
	repo, mock, cleanup := newArtifactRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, execution_id, task_id, name, artifact_class")).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)

	_, err := repo.Get(context.Background(), "missing")
	if err != persistence.ErrNotFound {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestArtifactRepositoryListFiltersAndPagination(t *testing.T) {
	repo, mock, cleanup := newArtifactRepo(t)
	defer cleanup()

	projectID := "proj-a"
	executionID := "exec-1"
	taskID := "task-1"
	created := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, execution_id, task_id, name, artifact_class")).
		WithArgs(projectID, executionID, taskID, 10, 20).
		WillReturnRows(artifactRows().AddRow(
			"art-1", projectID, executionID, taskID, "output.json", persistence.ArtifactClassOutput,
			"/tmp/output.json", int64(42), "sha256", "application/json", created, "task_output",
		))

	got, err := repo.List(context.Background(), persistence.ArtifactFilter{
		ProjectID:   &projectID,
		ExecutionID: &executionID,
		TaskID:      &taskID,
		PageSize:    10,
		Offset:      20,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "art-1" {
		t.Fatalf("List() = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestArtifactRepositoryUpdateTaskIDValidationAndDeleteByExecution(t *testing.T) {
	repo, mock, cleanup := newArtifactRepo(t)
	defer cleanup()

	if err := repo.UpdateTaskID(context.Background(), "", "task-1"); err == nil {
		t.Fatal("expected empty artifact id error")
	}
	if err := repo.UpdateTaskID(context.Background(), "art-1", ""); err == nil {
		t.Fatal("expected empty task id error")
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE artifacts SET task_id = $1 WHERE id = $2")).
		WithArgs("task-1", "art-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM artifacts WHERE execution_id = $1")).
		WithArgs("exec-1").
		WillReturnResult(sqlmock.NewResult(0, 2))

	if err := repo.UpdateTaskID(context.Background(), "art-1", "task-1"); err != nil {
		t.Fatalf("UpdateTaskID() error = %v", err)
	}
	if err := repo.DeleteByExecutionID(context.Background(), "exec-1"); err != nil {
		t.Fatalf("DeleteByExecutionID() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
