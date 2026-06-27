package workflowtelemetry

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectExecutionCounts queues the stage-1 (execution counts) query so
// downstream-stage tests can reach the populated-data branch without
// re-spelling it everywhere. Returns RunCount=total of the rows added.
func expectExecutionCounts(mock sqlmock.Sqlmock, wf string, completed, failed, cancelled int) {
	rows := sqlmock.NewRows([]string{"status", "count"})
	if completed > 0 {
		rows.AddRow("COMPLETED", completed)
	}
	if failed > 0 {
		rows.AddRow("FAILED", failed)
	}
	if cancelled > 0 {
		rows.AddRow("CANCELLED", cancelled)
	}
	mock.ExpectQuery(`SELECT status, count\(\*\)`).
		WithArgs(wf, sqlmock.AnyArg()).
		WillReturnRows(rows)
}

func expectAggregates(mock sqlmock.Sqlmock, wf string, totalCost, avgDur interface{}) {
	mock.ExpectQuery(`SUM\(u\.cost_usd\)`).
		WithArgs(wf, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"total"}).AddRow(totalCost))
	mock.ExpectQuery(`SELECT AVG\(EXTRACT\(EPOCH`).
		WithArgs(wf, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"avg"}).AddRow(avgDur))
}

// expectStepStages queues the three step-rollup queries (outcome dist,
// cost/duration, top error class) with empty results, plus the two
// remaining tail stages (failure classes, judge, rates) — the minimal
// scaffolding to let ForWorkflow complete after a stage under test.
func expectEmptyStepAndTail(mock sqlmock.Sqlmock, wf string) {
	mock.ExpectQuery(`GROUP BY so\.step_id, so\.role, so\.model, so\.outcome`).
		WithArgs(wf, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "outcome", "n", "first_seen"}))
	mock.ExpectQuery(`LEFT JOIN task_llm_usage`).
		WithArgs(wf, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "avg_seconds", "total_cost", "step_runs"}))
	mock.ExpectQuery(`DISTINCT ON \(so\.step_id, so\.role, so\.model\)`).
		WithArgs(wf, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "error_class"}))
	mock.ExpectQuery(`LIMIT 10`).
		WithArgs(wf, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"error_class", "count"}))
	mock.ExpectQuery(`task_judge_verdicts`).
		WithArgs(wf, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"verdict", "count"}))
	mock.ExpectQuery(`jsonb_array_length`).
		WithArgs(wf, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`message_kind = 'directive'`).
		WithArgs(wf, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
}

// TestForWorkflow_WindowBoundsAndUTC — the rollup must echo the window
// it queried: WindowStart is the supplied `since` normalised to UTC,
// WindowEnd is set to "now" and must be >= WindowStart. Exercised in
// the empty branch so no other stages are needed.
func TestForWorkflow_WindowBoundsAndUTC(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	// Non-UTC zone to prove normalisation.
	loc := time.FixedZone("UTC+5", 5*3600)
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, loc)
	mock.ExpectQuery(`SELECT status, count\(\*\)`).
		WithArgs("wf-w", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}))

	before := time.Now().UTC()
	r, err := svc.ForWorkflow(context.Background(), "wf-w", since)
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("ForWorkflow: %v", err)
	}
	if !r.WindowStart.Equal(since) {
		t.Errorf("WindowStart = %v, want equal to since %v", r.WindowStart, since)
	}
	if r.WindowStart.Location() != time.UTC {
		t.Errorf("WindowStart not UTC: %v", r.WindowStart.Location())
	}
	if r.WindowEnd.Before(before) || r.WindowEnd.After(after) {
		t.Errorf("WindowEnd %v not within [%v, %v]", r.WindowEnd, before, after)
	}
	if r.WorkflowID != "wf-w" {
		t.Errorf("WorkflowID = %q", r.WorkflowID)
	}
}

// TestForWorkflow_UnknownStatusCountsTowardRunCountOnly — a status the
// switch doesn't recognise (e.g. RUNNING) still increments RunCount but
// none of the three classified buckets. Locks the "RunCount = total of
// every row" contract documented on fillExecutionCounts.
func TestForWorkflow_UnknownStatusCountsTowardRunCountOnly(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	mock.ExpectQuery(`SELECT status, count\(\*\)`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).
			AddRow("COMPLETED", 3).
			AddRow("RUNNING", 4).
			AddRow("FAILED", 1))
	expectAggregates(mock, "wf-a", 0.0, nil)
	expectEmptyStepAndTail(mock, "wf-a")

	r, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ForWorkflow: %v", err)
	}
	if r.RunCount != 8 {
		t.Errorf("RunCount = %d, want 8 (3+4+1)", r.RunCount)
	}
	if r.SuccessCount != 3 || r.FailureCount != 1 || r.CancelledCount != 0 {
		t.Errorf("classified counts wrong: success=%d failure=%d cancelled=%d",
			r.SuccessCount, r.FailureCount, r.CancelledCount)
	}
}

// TestForWorkflow_NullAggregatesYieldZero — NULL cost (COALESCE'd to 0
// at the SQL layer here returns NULL via sql.NullFloat64) and a NULL
// AVG duration (no completed runs) must leave the averages at 0 rather
// than panicking or producing NaN.
func TestForWorkflow_NullAggregatesYieldZero(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	expectExecutionCounts(mock, "wf-a", 5, 0, 0)
	// total cost NULL, avg duration NULL.
	expectAggregates(mock, "wf-a", nil, nil)
	expectEmptyStepAndTail(mock, "wf-a")

	r, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ForWorkflow: %v", err)
	}
	if r.AvgCostUSD != 0 {
		t.Errorf("AvgCostUSD = %v, want 0 on NULL cost", r.AvgCostUSD)
	}
	if r.AvgDurationSeconds != 0 {
		t.Errorf("AvgDurationSeconds = %v, want 0 on NULL avg", r.AvgDurationSeconds)
	}
}

// TestForWorkflow_AvgCostDividesByRunCount — AvgCostUSD is the summed
// workflow_step cost divided by RunCount (NOT by step count or row
// count). 3.00 over 4 runs = 0.75 exactly.
func TestForWorkflow_AvgCostDividesByRunCount(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	expectExecutionCounts(mock, "wf-a", 4, 0, 0)
	expectAggregates(mock, "wf-a", 3.00, 30.0)
	expectEmptyStepAndTail(mock, "wf-a")

	r, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ForWorkflow: %v", err)
	}
	if r.AvgCostUSD != 0.75 {
		t.Errorf("AvgCostUSD = %v, want 0.75 (3.00/4)", r.AvgCostUSD)
	}
	if r.AvgDurationSeconds != 30.0 {
		t.Errorf("AvgDurationSeconds = %v, want 30", r.AvgDurationSeconds)
	}
}

// TestForWorkflow_MultiStepOrderingAndComposition — two distinct steps,
// each split across multiple outcome rows, must collapse into one
// StepRollup per (step_id,role,model) keyed composite, preserving the
// first-seen ordering returned by the query. Also verifies the
// cost/duration second pass joins by the composite key and the
// per-step cost divides by step_runs.
func TestForWorkflow_MultiStepOrderingAndComposition(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	expectExecutionCounts(mock, "wf-a", 10, 0, 0)
	expectAggregates(mock, "wf-a", 0.0, 0.0)

	t0 := time.Now()
	mock.ExpectQuery(`GROUP BY so\.step_id, so\.role, so\.model, so\.outcome`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "outcome", "n", "first_seen"}).
			// "plan" appears first → index 0.
			AddRow("plan", "planner", "m1", "ok", 9, t0).
			AddRow("plan", "planner", "m1", "failed", 1, t0.Add(time.Second)).
			// "review" appears later → index 1.
			AddRow("review", "reviewer", "m2", "ok", 6, t0.Add(2*time.Second)).
			AddRow("review", "reviewer", "m2", "failed", 4, t0.Add(3*time.Second)))
	// Cost/duration: plan has 4 step_runs / 2.00 cost → 0.50 avg;
	// review has 0 step_runs (gate-only) → cost stays 0.
	mock.ExpectQuery(`LEFT JOIN task_llm_usage`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "avg_seconds", "total_cost", "step_runs"}).
			AddRow("plan", "planner", "m1", 8.0, 2.00, 4).
			AddRow("review", "reviewer", "m2", 5.0, 9.99, 0))
	mock.ExpectQuery(`DISTINCT ON \(so\.step_id, so\.role, so\.model\)`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "error_class"}).
			AddRow("review", "reviewer", "m2", "timeout"))
	mock.ExpectQuery(`LIMIT 10`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"error_class", "count"}))
	mock.ExpectQuery(`task_judge_verdicts`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"verdict", "count"}))
	mock.ExpectQuery(`jsonb_array_length`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`message_kind = 'directive'`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	r, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ForWorkflow: %v", err)
	}
	if len(r.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d: %+v", len(r.Steps), r.Steps)
	}
	if r.Steps[0].StepID != "plan" || r.Steps[1].StepID != "review" {
		t.Errorf("step order wrong: %q then %q", r.Steps[0].StepID, r.Steps[1].StepID)
	}
	if r.Steps[0].OutcomeDist["ok"] != 9 || r.Steps[0].OutcomeDist["failed"] != 1 {
		t.Errorf("plan OutcomeDist wrong: %+v", r.Steps[0].OutcomeDist)
	}
	if r.Steps[0].AvgCostUSD != 0.50 {
		t.Errorf("plan AvgCostUSD = %v, want 0.50 (2.00/4)", r.Steps[0].AvgCostUSD)
	}
	if r.Steps[0].AvgDurationSeconds != 8.0 {
		t.Errorf("plan AvgDurationSeconds = %v, want 8", r.Steps[0].AvgDurationSeconds)
	}
	// review has step_runs=0 → cost untouched at 0 despite total_cost 9.99.
	if r.Steps[1].AvgCostUSD != 0 {
		t.Errorf("review AvgCostUSD = %v, want 0 (step_runs=0)", r.Steps[1].AvgCostUSD)
	}
	if r.Steps[1].TopErrorClass != "timeout" {
		t.Errorf("review TopErrorClass = %q, want timeout", r.Steps[1].TopErrorClass)
	}
	// plan had no error row → TopErrorClass stays empty.
	if r.Steps[0].TopErrorClass != "" {
		t.Errorf("plan TopErrorClass = %q, want empty", r.Steps[0].TopErrorClass)
	}
}

// TestForWorkflow_StepCostRowWithoutMatchingStep — the cost/duration
// second pass may return a (step_id,role,model) tuple that the first
// pass never produced (e.g. a race where outcome rows lag usage rows).
// Such rows must be silently skipped, not appended as a phantom step.
func TestForWorkflow_StepCostRowWithoutMatchingStep(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	expectExecutionCounts(mock, "wf-a", 3, 0, 0)
	expectAggregates(mock, "wf-a", 0.0, 0.0)

	mock.ExpectQuery(`GROUP BY so\.step_id, so\.role, so\.model, so\.outcome`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "outcome", "n", "first_seen"}).
			AddRow("known", "r", "m", "ok", 3, time.Now()))
	mock.ExpectQuery(`LEFT JOIN task_llm_usage`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "avg_seconds", "total_cost", "step_runs"}).
			AddRow("known", "r", "m", 1.0, 0.30, 3).
			// phantom tuple — no matching first-pass step.
			AddRow("ghost", "x", "z", 9.0, 9.0, 9))
	// error-class pass also references the phantom; must be ignored.
	mock.ExpectQuery(`DISTINCT ON \(so\.step_id, so\.role, so\.model\)`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "error_class"}).
			AddRow("ghost", "x", "z", "phantom_err"))
	mock.ExpectQuery(`LIMIT 10`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"error_class", "count"}))
	mock.ExpectQuery(`task_judge_verdicts`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"verdict", "count"}))
	mock.ExpectQuery(`jsonb_array_length`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`message_kind = 'directive'`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	r, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ForWorkflow: %v", err)
	}
	if len(r.Steps) != 1 {
		t.Fatalf("phantom step leaked: %+v", r.Steps)
	}
	if r.Steps[0].StepID != "known" || r.Steps[0].AvgCostUSD < 0.0999 || r.Steps[0].AvgCostUSD > 0.1001 {
		t.Errorf("known step wrong: %+v (want cost ~0.10)", r.Steps[0])
	}
	if r.Steps[0].TopErrorClass != "" {
		t.Errorf("phantom error class leaked onto known step: %q", r.Steps[0].TopErrorClass)
	}
}

// TestForWorkflow_TopFailureClassesOrderPreserved — the DB returns
// failure classes pre-sorted desc by count; the rollup must preserve
// that slice order verbatim (it's appended in iteration order).
func TestForWorkflow_TopFailureClassesOrderPreserved(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	expectExecutionCounts(mock, "wf-a", 20, 0, 0)
	expectAggregates(mock, "wf-a", 0.0, 0.0)
	mock.ExpectQuery(`GROUP BY so\.step_id, so\.role, so\.model, so\.outcome`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "outcome", "n", "first_seen"}))
	mock.ExpectQuery(`LEFT JOIN task_llm_usage`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "avg_seconds", "total_cost", "step_runs"}))
	mock.ExpectQuery(`DISTINCT ON \(so\.step_id, so\.role, so\.model\)`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "error_class"}))
	mock.ExpectQuery(`LIMIT 10`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"error_class", "count"}).
			AddRow("schema_violation", 11).
			AddRow("timeout", 5).
			AddRow("rate_limit", 2))
	mock.ExpectQuery(`task_judge_verdicts`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"verdict", "count"}))
	mock.ExpectQuery(`jsonb_array_length`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`message_kind = 'directive'`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	r, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ForWorkflow: %v", err)
	}
	want := []FailureClassCount{
		{"schema_violation", 11}, {"timeout", 5}, {"rate_limit", 2},
	}
	if len(r.TopFailureClasses) != 3 {
		t.Fatalf("got %d classes, want 3: %+v", len(r.TopFailureClasses), r.TopFailureClasses)
	}
	for i, w := range want {
		if r.TopFailureClasses[i] != w {
			t.Errorf("TopFailureClasses[%d] = %+v, want %+v", i, r.TopFailureClasses[i], w)
		}
	}
}

// TestForWorkflow_QualityRatesAreFractionsOfRunCount — hallucination
// and intervention counts are divided by RunCount, not by each other.
// 2 hallucinated / 8 runs = 0.25; 6 intervention / 8 = 0.75. Boundary:
// a count equal to RunCount yields exactly 1.0.
func TestForWorkflow_QualityRatesAreFractionsOfRunCount(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	expectExecutionCounts(mock, "wf-a", 8, 0, 0)
	expectAggregates(mock, "wf-a", 0.0, 0.0)
	mock.ExpectQuery(`GROUP BY so\.step_id, so\.role, so\.model, so\.outcome`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "outcome", "n", "first_seen"}))
	mock.ExpectQuery(`LEFT JOIN task_llm_usage`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "avg_seconds", "total_cost", "step_runs"}))
	mock.ExpectQuery(`DISTINCT ON \(so\.step_id, so\.role, so\.model\)`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "error_class"}))
	mock.ExpectQuery(`LIMIT 10`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"error_class", "count"}))
	mock.ExpectQuery(`task_judge_verdicts`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"verdict", "count"}))
	mock.ExpectQuery(`jsonb_array_length`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`message_kind = 'directive'`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(6))

	r, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ForWorkflow: %v", err)
	}
	if r.HallucinationRate != 0.25 {
		t.Errorf("HallucinationRate = %v, want 0.25 (2/8)", r.HallucinationRate)
	}
	if r.OperatorInterventionRate != 0.75 {
		t.Errorf("OperatorInterventionRate = %v, want 0.75 (6/8)", r.OperatorInterventionRate)
	}
}

// TestForWorkflow_AggregatesStageError — a failure in stage 2 (cost
// query) aborts and is wrapped with the "aggregates" stage label. Hits
// the error-return path that the happy-path test never executes.
func TestForWorkflow_AggregatesStageError(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	expectExecutionCounts(mock, "wf-a", 5, 0, 0)
	mock.ExpectQuery(`SUM\(u\.cost_usd\)`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnError(errors.New("boom"))
	_, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-time.Hour))
	if err == nil || !regexp.MustCompile(`aggregates`).MatchString(err.Error()) {
		t.Fatalf("want error wrapped 'aggregates', got %v", err)
	}
}

// TestForWorkflow_StepRollupsScanError — a row whose column types don't
// match the Scan targets surfaces as a "step rollups" wrapped error,
// covering the scan-error branch in fillStepRollups' first pass.
func TestForWorkflow_StepRollupsScanError(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	expectExecutionCounts(mock, "wf-a", 5, 0, 0)
	expectAggregates(mock, "wf-a", 0.0, 0.0)
	// "n" should be an int; feeding a non-numeric string forces a
	// Scan failure into the int destination.
	mock.ExpectQuery(`GROUP BY so\.step_id, so\.role, so\.model, so\.outcome`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "outcome", "n", "first_seen"}).
			AddRow("s", "r", "m", "ok", "not-an-int", time.Now()))
	_, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-time.Hour))
	if err == nil || !regexp.MustCompile(`step rollups`).MatchString(err.Error()) {
		t.Fatalf("want error wrapped 'step rollups', got %v", err)
	}
}

// TestForWorkflow_JudgeVerdictsStageError — judge-verdict query failure
// wraps with "judge verdicts" and aborts before the quality-rate stage.
func TestForWorkflow_JudgeVerdictsStageError(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	expectExecutionCounts(mock, "wf-a", 5, 0, 0)
	expectAggregates(mock, "wf-a", 0.0, 0.0)
	mock.ExpectQuery(`GROUP BY so\.step_id, so\.role, so\.model, so\.outcome`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "outcome", "n", "first_seen"}))
	mock.ExpectQuery(`LEFT JOIN task_llm_usage`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "avg_seconds", "total_cost", "step_runs"}))
	mock.ExpectQuery(`DISTINCT ON \(so\.step_id, so\.role, so\.model\)`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "error_class"}))
	mock.ExpectQuery(`LIMIT 10`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"error_class", "count"}))
	mock.ExpectQuery(`task_judge_verdicts`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnError(errors.New("relation does not exist"))
	_, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-time.Hour))
	if err == nil || !regexp.MustCompile(`judge verdicts`).MatchString(err.Error()) {
		t.Fatalf("want error wrapped 'judge verdicts', got %v", err)
	}
}

// TestForWorkflow_QualityRatesStageError — failure on the hallucination
// query (stage 6) wraps with "quality rates", covering the final stage's
// error path and confirming a mid-rate DB error doesn't yield a
// half-populated rollup to the caller.
func TestForWorkflow_QualityRatesStageError(t *testing.T) {
	svc, mock, cleanup := newMockServiceRegex(t)
	defer cleanup()
	expectExecutionCounts(mock, "wf-a", 5, 0, 0)
	expectAggregates(mock, "wf-a", 0.0, 0.0)
	mock.ExpectQuery(`GROUP BY so\.step_id, so\.role, so\.model, so\.outcome`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "outcome", "n", "first_seen"}))
	mock.ExpectQuery(`LEFT JOIN task_llm_usage`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "avg_seconds", "total_cost", "step_runs"}))
	mock.ExpectQuery(`DISTINCT ON \(so\.step_id, so\.role, so\.model\)`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "role", "model", "error_class"}))
	mock.ExpectQuery(`LIMIT 10`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"error_class", "count"}))
	mock.ExpectQuery(`task_judge_verdicts`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"verdict", "count"}))
	mock.ExpectQuery(`jsonb_array_length`).
		WithArgs("wf-a", sqlmock.AnyArg()).
		WillReturnError(errors.New("boom"))
	_, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now().Add(-time.Hour))
	if err == nil || !regexp.MustCompile(`quality rates`).MatchString(err.Error()) {
		t.Fatalf("want error wrapped 'quality rates', got %v", err)
	}
}

// TestNewService_NilDBStillConstructsButForWorkflowGuards — NewService
// with a nil DBTX returns a non-nil Service, but ForWorkflow fails
// closed with "not configured" rather than dereferencing nil.
func TestNewService_NilDBStillConstructsButForWorkflowGuards(t *testing.T) {
	svc := NewService(nil)
	if svc == nil {
		t.Fatal("NewService(nil) returned nil")
	}
	_, err := svc.ForWorkflow(context.Background(), "wf-a", time.Now())
	if err == nil || !regexp.MustCompile(`not configured`).MatchString(err.Error()) {
		t.Fatalf("want 'not configured' error, got %v", err)
	}
}
