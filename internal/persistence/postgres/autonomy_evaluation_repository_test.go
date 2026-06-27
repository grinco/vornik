package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

func newAutonomyEvaluationRepo(t *testing.T) (*AutonomyEvaluationRepository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewAutonomyEvaluationRepository(db), mock, func() { _ = db.Close() }
}

func autonomyEvaluationRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "project_id", "outcome", "reason", "task_id",
		"task_type", "workflow_id", "prompt_hash", "duration_ms", "created_at",
	})
}

func TestAutonomyEvaluationRepositoryRecord(t *testing.T) {
	repo, mock, cleanup := newAutonomyEvaluationRepo(t)
	defer cleanup()

	if err := repo.Record(context.Background(), nil); err == nil {
		t.Fatal("expected nil evaluation error")
	}

	taskID := "task-1"
	evaluation := &persistence.AutonomyEvaluation{
		ID:         "eval-1",
		ProjectID:  "proj-a",
		Outcome:    "created_task",
		Reason:     "high confidence",
		TaskID:     &taskID,
		TaskType:   "bugfix",
		WorkflowID: "wf-1",
		PromptHash: "hash-1",
		DurationMs: 123,
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO autonomy_evaluations")).
		WithArgs(
			evaluation.ID, evaluation.ProjectID, evaluation.Outcome, evaluation.Reason, sqlmock.AnyArg(),
			evaluation.TaskType, evaluation.WorkflowID, evaluation.PromptHash, evaluation.DurationMs, sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), evaluation); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if evaluation.CreatedAt.IsZero() {
		t.Fatal("Record() did not set CreatedAt")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestAutonomyEvaluationRepositoryListFiltersAndScans(t *testing.T) {
	repo, mock, cleanup := newAutonomyEvaluationRepo(t)
	defer cleanup()

	projectID := "proj-a"
	outcome := "created_task"
	created := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, outcome, reason, task_id")).
		WithArgs(projectID, outcome, 5, 10).
		WillReturnRows(autonomyEvaluationRows().AddRow(
			"eval-1", projectID, outcome, "reason", "task-1",
			"bugfix", "wf-1", "hash-1", int64(123), created,
		))

	got, err := repo.List(context.Background(), persistence.AutonomyEvaluationFilter{
		ProjectID: &projectID,
		Outcome:   &outcome,
		PageSize:  5,
		Offset:    10,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "eval-1" || got[0].TaskID == nil || *got[0].TaskID != "task-1" {
		t.Fatalf("List() = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestAutonomyEvaluationRepositoryCountByOutcome(t *testing.T) {
	repo, mock, cleanup := newAutonomyEvaluationRepo(t)
	defer cleanup()

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT outcome, COUNT(*)")).
		WithArgs("proj-a", since, until).
		WillReturnRows(sqlmock.NewRows([]string{"outcome", "count"}).
			AddRow("created_task", int64(3)).
			AddRow("skipped", int64(7)))

	got, err := repo.CountByOutcome(context.Background(), "proj-a", since, until)
	if err != nil {
		t.Fatalf("CountByOutcome() error = %v", err)
	}
	if got["created_task"] != 3 || got["skipped"] != 7 {
		t.Fatalf("CountByOutcome() = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
