package workflowtelemetry

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// newMockService wires a sqlmock-backed Service the tests can drive.
// Mirrors the newMockDBTX shape from internal/persistence/postgres so
// the assertions stay consistent across the codebase.
func newMockService(t *testing.T) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	cleanup := func() { _ = db.Close() }
	return NewService(db), mock, cleanup
}

// Helper: sqlmock with regex matcher for tests that don't want to
// pin exact SQL whitespace.
func newMockServiceRegex(t *testing.T) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	cleanup := func() { _ = db.Close() }
	return NewService(db), mock, cleanup
}

// TestForWorkflow_RejectsEmptyWorkflowID — guard before the SQL
// runs. An empty workflow_id would scan every row in the
// executions table; fail closed.
func TestForWorkflow_RejectsEmptyWorkflowID(t *testing.T) {
	svc, _, cleanup := newMockService(t)
	defer cleanup()
	_, err := svc.ForWorkflow(context.Background(), "", time.Now())
	if !errors.Is(err, ErrInvalidWorkflowID) {
		t.Fatalf("expected ErrInvalidWorkflowID, got %v", err)
	}
}

// TestForWorkflow_NoRunsReturnsEmptyRollup — zero-execution case
// short-circuits the rest of the queries. RunCount=0 and every
// downstream field stays at its zero value. Critical for the
// architect's "no data yet" branch.
func TestForWorkflow_NoRunsReturnsEmptyRollup(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	mock.ExpectQuery(`SELECT status, count\(\*\)`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}))
	rollup, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("ForWorkflow: %v", err)
	}
	if rollup.RunCount != 0 {
		t.Errorf("RunCount = %d, want 0", rollup.RunCount)
	}
	if len(rollup.Steps) != 0 {
		t.Errorf("expected no steps, got %d", len(rollup.Steps))
	}
	// Non-nil collections so JSON marshalling doesn't serialise
	// `null` to the architect prompt.
	if rollup.JudgeVerdictDist == nil {
		t.Error("JudgeVerdictDist must be non-nil")
	}
	if mock.ExpectationsWereMet() != nil {
		t.Errorf("unmet expectations: %v", mock.ExpectationsWereMet())
	}
}

// TestForWorkflow_AggregatesAcrossSixQueries pins the contract that
// every load-bearing query runs in the populated-data branch. Six
// queries today; if a future refactor reorders or skips one this
// test fails loudly.
func TestForWorkflow_AggregatesAcrossSixQueries(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()

	// 1. Execution counts.
	mock.ExpectQuery(`SELECT status, count\(\*\)`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).
			AddRow("COMPLETED", 7).
			AddRow("FAILED", 2).
			AddRow("CANCELLED", 1))
	// 2. Total cost.
	mock.ExpectQuery(`SUM\(u\.cost_usd\)`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"total"}).AddRow(2.50))
	// 2b. Avg duration.
	mock.ExpectQuery(`SELECT AVG\(EXTRACT\(EPOCH`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"avg"}).AddRow(45.5))
	// 3. Step outcome dist.
	mock.ExpectQuery(`GROUP BY so\.step_id, so\.role, so\.model, so\.outcome`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "outcome", "n", "first_seen"}).
			AddRow("review", "reviewer", "gpt-oss-20b", "ok", 5, time.Now()).
			AddRow("review", "reviewer", "gpt-oss-20b", "failed", 2, time.Now()))
	// 3b. Per-step cost + duration.
	mock.ExpectQuery(`LEFT JOIN task_llm_usage`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "avg_seconds", "total_cost", "step_runs"}).
			AddRow("review", "reviewer", "gpt-oss-20b", 12.3, 1.23, 7))
	// 3c. Per-step top error class.
	mock.ExpectQuery(`DISTINCT ON \(so\.step_id, so\.role, so\.model\)`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "error_class"}).
			AddRow("review", "reviewer", "gpt-oss-20b", "schema_violation"))
	// 4. Top failure classes.
	mock.ExpectQuery(`LIMIT 10`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"error_class", "count"}).
			AddRow("schema_violation", 2))
	// 5. Judge verdicts.
	mock.ExpectQuery(`task_judge_verdicts`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"verdict", "count"}).
			AddRow("pass", 6).AddRow("fail", 1).AddRow("abstain", 1))
	// 6a. Hallucination rate.
	mock.ExpectQuery(`jsonb_array_length`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// 6b. Operator intervention rate.
	mock.ExpectQuery(`message_kind = 'directive'`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	rollup, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("ForWorkflow: %v", err)
	}

	if rollup.RunCount != 10 {
		t.Errorf("RunCount = %d, want 10", rollup.RunCount)
	}
	if rollup.SuccessCount != 7 || rollup.FailureCount != 2 || rollup.CancelledCount != 1 {
		t.Errorf("counts off: %+v", rollup)
	}
	if rollup.AvgCostUSD != 0.25 {
		t.Errorf("AvgCostUSD = %v, want 0.25 (2.50/10)", rollup.AvgCostUSD)
	}
	if rollup.AvgDurationSeconds != 45.5 {
		t.Errorf("AvgDurationSeconds = %v, want 45.5", rollup.AvgDurationSeconds)
	}
	if len(rollup.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(rollup.Steps))
	}
	step := rollup.Steps[0]
	if step.StepID != "review" || step.Role != "reviewer" {
		t.Errorf("step shape wrong: %+v", step)
	}
	if step.OutcomeDist["ok"] != 5 || step.OutcomeDist["failed"] != 2 {
		t.Errorf("OutcomeDist wrong: %+v", step.OutcomeDist)
	}
	if step.TopErrorClass != "schema_violation" {
		t.Errorf("TopErrorClass = %q, want schema_violation", step.TopErrorClass)
	}
	if len(rollup.TopFailureClasses) != 1 || rollup.TopFailureClasses[0].ErrorClass != "schema_violation" {
		t.Errorf("TopFailureClasses wrong: %+v", rollup.TopFailureClasses)
	}
	if rollup.JudgeVerdictDist["pass"] != 6 {
		t.Errorf("JudgeVerdictDist[pass] = %d, want 6", rollup.JudgeVerdictDist["pass"])
	}
	// 1 hallucinated run / 10 runs.
	if rollup.HallucinationRate != 0.1 {
		t.Errorf("HallucinationRate = %v, want 0.1", rollup.HallucinationRate)
	}
	if rollup.OperatorInterventionRate != 0.2 {
		t.Errorf("OperatorInterventionRate = %v, want 0.2", rollup.OperatorInterventionRate)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestForWorkflow_PropagatesQueryErrors — first failing query
// aborts the rest. Defensive: a DB error mid-query shouldn't
// produce a partial-rollup that the architect treats as truth.
func TestForWorkflow_PropagatesQueryErrors(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	mock.ExpectQuery(`SELECT status, count`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnError(errors.New("connection refused"))
	_, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-time.Hour))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !regexp.MustCompile(`execution counts`).MatchString(err.Error()) {
		t.Errorf("error not wrapped with stage context: %v", err)
	}
}

// TestForWorkflow_NilService — defensive against partially-
// constructed values being passed around in tests.
func TestForWorkflow_NilService(t *testing.T) {
	var s *Service
	_, err := s.ForWorkflow(context.Background(), "wf-a", time.Now())
	if err == nil {
		t.Error("nil receiver must return error, not panic")
	}
}
