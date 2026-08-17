package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

func TestExecutionQualityScoreRepository_UpsertPinsExecutionIdentity(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionQualityScoreRepository(db)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	score := &persistence.ExecutionQualityScore{
		ProjectID: "p1", TaskID: "t1", ExecutionID: "e1", WorkflowID: "dev-pipeline",
		WorkflowRevision: "ignored-caller-value", ScorerVersion: "1", ScoringPolicySHA: "sha",
		Kind: "pinned_case_validation", Status: "scored", Score: pgFloatPtr(0.5),
		PassedCaseCount: 1, PinnedCaseCount: 2, RecordedAt: now,
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO execution_quality_scores")).
		WithArgs("1", "sha", "pinned_case_validation", "scored", score.Score, 1, 2, "", sqlmock.AnyArg(), now,
			"e1", "p1", "t1", "dev-pipeline").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Upsert(context.Background(), score); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO execution_quality_scores")).
		WithArgs("1", "sha", "pinned_case_validation", "scored", score.Score, 1, 2, "", sqlmock.AnyArg(), now,
			"e1", "p1", "t1", "dev-pipeline").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.Upsert(context.Background(), score); err == nil {
		t.Fatal("zero-row identity-select must reject a cross-project/missing execution")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionQualityScoreRepository_ListPendingTerminalIncludesSnapshot(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionQualityScoreRepository(db)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	query := "(?s)" + regexp.QuoteMeta("LEFT JOIN execution_quality_scores") +
		".*" + regexp.QuoteMeta("e.status IN ('COMPLETED','FAILED','CANCELLED')") +
		".*" + regexp.QuoteMeta("s.execution_id IS NULL") +
		".*" + regexp.QuoteMeta("ORDER BY e.created_at ASC, e.id ASC")
	mock.ExpectQuery(query).WithArgs(25).WillReturnRows(sqlmock.NewRows([]string{
		"id", "task_id", "project_id", "workflow_id", "workflow_revision", "workflow_snapshot",
		"status", "current_step_id", "completed_steps", "state_snapshot", "result", "error_message",
		"error_code", "started_at", "completed_at", "created_at", "updated_at",
		"parent_execution_id", "forked_from_step_id", "forked_prompt_override",
	}).AddRow("e1", "t1", "p1", "dev-pipeline", "r1", []byte(`{"QualityScoring":{}}`),
		persistence.ExecutionStatusCompleted, nil, "{}", []byte(`{"stepResults":{}}`), nil, nil, nil,
		now, now, now, now, nil, nil, nil))

	got, err := repo.ListPendingTerminal(context.Background(), 25)
	if err != nil {
		t.Fatalf("ListPendingTerminal: %v", err)
	}
	if len(got) != 1 || got[0].ID != "e1" || len(got[0].WorkflowSnapshot) == 0 || len(got[0].StateSnapshot) == 0 {
		t.Fatalf("pending terminal row lost scoring snapshots: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionQualityScoreRepository_PendingStatsAreProjectScoped(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionQualityScoreRepository(db)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	query := "(?s)" + regexp.QuoteMeta("LEFT JOIN execution_quality_scores") +
		".*" + regexp.QuoteMeta("s.execution_id IS NULL") +
		".*" + regexp.QuoteMeta("e.project_id = ANY($1)")
	mock.ExpectQuery(query).WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count", "oldest_at"}).AddRow(int64(2), now))
	got, err := repo.PendingTerminalStats(context.Background(), []string{"p1", "p2"})
	if err != nil || got.Count != 2 || got.OldestAt == nil || !got.OldestAt.Equal(now) {
		t.Fatalf("PendingTerminalStats = %+v, %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func pgFloatPtr(v float64) *float64 { return &v }
