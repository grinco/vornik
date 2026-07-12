package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

// TestTaskCredentialRepository_Upsert pins the INSERT ... ON CONFLICT column
// order and that ID/CreatedAt default when left zero.
func TestTaskCredentialRepository_Upsert(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTaskCredentialRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO task_credentials")).
		WithArgs(
			sqlmock.AnyArg(), // id defaulted
			"task-x", "exec-1", "mcp__pagedrop__pagedrop_publish",
			"viewing password", "pw-1", "https://v/p/1",
			sqlmock.AnyArg(), // created_at defaulted
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	cred := &persistence.TaskCredential{
		TaskID: "task-x", ExecutionID: "exec-1", Tool: "mcp__pagedrop__pagedrop_publish",
		Label: "viewing password", Value: "pw-1", ArtifactURL: "https://v/p/1",
	}
	if err := repo.Upsert(context.Background(), cred); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if cred.ID == "" {
		t.Error("Upsert should populate ID")
	}
	if cred.CreatedAt.IsZero() {
		t.Error("Upsert should default CreatedAt")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestTaskCredentialRepository_ListByTaskLatestExecution pins the query and
// row scan.
func TestTaskCredentialRepository_ListByTaskLatestExecution(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTaskCredentialRepository(db)

	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"id", "task_id", "execution_id", "tool", "label", "value", "artifact_url", "created_at"}).
		AddRow("taskcred-1", "task-x", "exec-2", "mcp__pagedrop__pagedrop_publish", "viewing password", "pw-2", "https://v/p/2", now)
	mock.ExpectQuery(regexp.QuoteMeta("FROM task_credentials")).
		WithArgs("task-x").
		WillReturnRows(rows)

	got, err := repo.ListByTaskLatestExecution(context.Background(), "task-x")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Value != "pw-2" || got[0].ExecutionID != "exec-2" {
		t.Fatalf("got %+v, want one exec-2/pw-2 row", got)
	}

	// Empty task short-circuits without a query.
	none, err := repo.ListByTaskLatestExecution(context.Background(), "")
	if err != nil || none != nil {
		t.Errorf("empty taskID: got (%v, %v), want (nil, nil)", none, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
