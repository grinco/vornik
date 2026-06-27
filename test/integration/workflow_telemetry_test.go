//go:build integration
// +build integration

package integration_test

// Integration test for internal/workflowtelemetry against a real
// postgres. Validates the actual SQL semantics that sqlmock can't
// exercise: the joins across executions / step_outcomes /
// task_llm_usage / tasks / task_messages must produce a correctly
// aggregated rollup, NOT just a query that runs without error.
//
// Use case context: the architect agent (Slice 2) reads this rollup
// as its primary input. Wrong aggregates → wrong proposals →
// operator confusion. This test pins the contract that powers
// `vornikctl workflow-stats`.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/workflowtelemetry"
)

func TestWorkflowTelemetry_RollupAgainstRealDB(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	// Per-test unique project + workflow so concurrent CI runs
	// don't see each other's rows. Test cleans up afterwards.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	projectID := "wftel-" + suffix
	workflowID := "wftel-wf-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM execution_step_outcomes WHERE project_id = $1`, projectID)
		_, _ = db.Exec(`DELETE FROM task_llm_usage WHERE task_id IN (SELECT id FROM tasks WHERE project_id = $1)`, projectID)
		_, _ = db.Exec(`DELETE FROM task_messages WHERE task_id IN (SELECT id FROM tasks WHERE project_id = $1)`, projectID)
		_, _ = db.Exec(`DELETE FROM executions WHERE project_id = $1`, projectID)
		_, _ = db.Exec(`DELETE FROM tasks WHERE project_id = $1`, projectID)
	})

	// Per-test-unique IDs so concurrent / repeated runs don't
	// collide on the primary keys.
	t1 := "task-1-" + suffix
	t2 := "task-2-" + suffix
	t3 := "task-3-" + suffix
	tF := "task-foreign-" + suffix
	e1 := "exec-1-" + suffix
	e2 := "exec-2-" + suffix
	e3 := "exec-3-" + suffix
	eF := "exec-foreign-" + suffix
	o1 := "out-1-" + suffix
	o2 := "out-2-" + suffix
	o3 := "out-3-" + suffix
	oF := "out-foreign-" + suffix

	// Seed three executions: 2 COMPLETED, 1 FAILED. All in the
	// window. One foreign workflow_id under the same project — must
	// NOT leak into the rollup (workflow-scope discipline).
	mustInsertTaskWf(t, db, t1, projectID, workflowID)
	mustInsertTaskWf(t, db, t2, projectID, workflowID)
	mustInsertTaskWf(t, db, t3, projectID, workflowID)
	mustInsertTaskWf(t, db, tF, projectID, "other-wf")

	mustInsertExecution(t, db, e1, t1, projectID, workflowID, "COMPLETED")
	mustInsertExecution(t, db, e2, t2, projectID, workflowID, "COMPLETED")
	mustInsertExecution(t, db, e3, t3, projectID, workflowID, "FAILED")
	mustInsertExecution(t, db, eF, tF, projectID, "other-wf", "COMPLETED")

	// Two step outcomes per run, one with a failure class.
	mustInsertStepOutcome(t, db, o1, projectID, t1, e1, "review", "reviewer", "ok", "", 12000)
	mustInsertStepOutcome(t, db, o2, projectID, t2, e2, "review", "reviewer", "ok", "", 8000)
	mustInsertStepOutcome(t, db, o3, projectID, t3, e3, "review", "reviewer", "failed", "schema_violation", 22000)
	// Foreign-workflow outcome — must NOT count.
	mustInsertStepOutcome(t, db, oF, projectID, tF, eF, "review", "reviewer", "ok", "", 5000)

	// LLM usage rows: 1 USD across the 3 in-window runs (avg
	// 0.333). Foreign run has its own row that must NOT count.
	mustInsertLLMUsage(t, db, projectID, t1, e1, "review", "reviewer", 0.30)
	mustInsertLLMUsage(t, db, projectID, t2, e2, "review", "reviewer", 0.40)
	mustInsertLLMUsage(t, db, projectID, t3, e3, "review", "reviewer", 0.30)
	mustInsertLLMUsage(t, db, projectID, tF, eF, "review", "reviewer", 99.99)

	// Operator directive on one task — must lift the intervention
	// rate to 1/3 ≈ 0.333.
	mustInsertOperatorDirective(t, db, t2)

	svc := workflowtelemetry.NewService(db)
	since := time.Now().Add(-1 * time.Hour)

	rollup, err := svc.ForWorkflow(context.Background(), workflowID, since)
	require.NoError(t, err)

	require.Equal(t, 3, rollup.RunCount, "RunCount should count only the workflow's runs in window")
	require.Equal(t, 2, rollup.SuccessCount)
	require.Equal(t, 1, rollup.FailureCount)
	require.Equal(t, 0, rollup.CancelledCount)

	// Avg cost: (0.30 + 0.40 + 0.30) / 3 = 0.333. The foreign
	// 99.99 must be excluded.
	require.InDelta(t, 1.0/3.0, rollup.AvgCostUSD, 1e-6)

	// Step rollup must include only the in-workflow runs.
	require.Len(t, rollup.Steps, 1)
	step := rollup.Steps[0]
	require.Equal(t, "review", step.StepID)
	require.Equal(t, 2, step.OutcomeDist["ok"])
	require.Equal(t, 1, step.OutcomeDist["failed"])
	require.Equal(t, "schema_violation", step.TopErrorClass)
	// Avg duration: (12000 + 8000 + 22000) / 3 / 1000 = 14s.
	require.InDelta(t, 14.0, step.AvgDurationSeconds, 1e-6)

	// Top failure classes: one class with count 1 ("schema_violation").
	require.Len(t, rollup.TopFailureClasses, 1)
	require.Equal(t, "schema_violation", rollup.TopFailureClasses[0].ErrorClass)
	require.Equal(t, 1, rollup.TopFailureClasses[0].Count)

	// Operator intervention: 1 task out of 3.
	require.InDelta(t, 1.0/3.0, rollup.OperatorInterventionRate, 1e-6)

	// Window bounds populated.
	require.False(t, rollup.WindowStart.IsZero())
	require.False(t, rollup.WindowEnd.IsZero())
	require.Equal(t, workflowID, rollup.WorkflowID)
}

func TestWorkflowTelemetry_NoRunsReturnsEmpty(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	svc := workflowtelemetry.NewService(db)
	rollup, err := svc.ForWorkflow(context.Background(),
		"nonexistent-workflow-"+fmt.Sprintf("%d", time.Now().UnixNano()),
		time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, rollup.RunCount)
	require.NotNil(t, rollup.JudgeVerdictDist, "must be non-nil for JSON marshalling")
	require.Empty(t, rollup.Steps)
}

// --- Helpers ---

func mustInsertTaskWf(t *testing.T, db *sql.DB, id, projectID, workflowID string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO tasks (id, project_id, workflow_id, status, priority, creation_source, attempt, max_attempts, created_at, updated_at)
		VALUES ($1, $2, $3, 'COMPLETED', 50, 'USER', 1, 3, NOW(), NOW())`,
		id, projectID, workflowID)
	require.NoError(t, err, "insert task %s", id)
}

func mustInsertExecution(t *testing.T, db *sql.DB, id, taskID, projectID, workflowID, status string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO executions (id, task_id, project_id, workflow_id, workflow_revision, status, completed_steps, started_at, completed_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'v1', $5::execution_status, ARRAY[]::text[], NOW() - INTERVAL '10 minutes', NOW() - INTERVAL '9 minutes', NOW() - INTERVAL '10 minutes', NOW())`,
		id, taskID, projectID, workflowID, status)
	require.NoError(t, err, "insert execution %s", id)
}

func mustInsertStepOutcome(t *testing.T, db *sql.DB, id, projectID, taskID, execID, stepID, role, outcome, errClass string, durationMs int) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO execution_step_outcomes (
		    id, project_id, task_id, execution_id, step_id, role, model,
		    outcome, error_class, error_detail, duration_ms, recorded_at
		) VALUES ($1, $2, $3, $4, $5, $6, 'gpt-oss-20b', $7, $8, '', $9, NOW())`,
		id, projectID, taskID, execID, stepID, role, outcome, errClass, durationMs)
	require.NoError(t, err, "insert step outcome %s", id)
}

func mustInsertLLMUsage(t *testing.T, db *sql.DB, projectID, taskID, execID, stepID, role string, costUSD float64) {
	t.Helper()
	id := "usg-" + taskID + "-" + execID
	_, err := db.Exec(`
		INSERT INTO task_llm_usage (
		    id, project_id, task_id, execution_id, step_id, role, model, source,
		    prompt_tokens, completion_tokens, cost_usd, recorded_at
		) VALUES ($1, $2, $3, $4, $5, $6, 'gpt-oss-20b', 'workflow_step',
		          1000, 500, $7, NOW())`,
		id, projectID, taskID, execID, stepID, role, costUSD)
	require.NoError(t, err, "insert llm usage")
}

func mustInsertOperatorDirective(t *testing.T, db *sql.DB, taskID string) {
	t.Helper()
	id := "msg-" + taskID
	_, err := db.Exec(`
		INSERT INTO task_messages (
		    id, task_id, message_kind, author_kind, content, metadata, created_at
		) VALUES ($1, $2, 'directive', 'operator', 'change approach', '{}', NOW())`,
		id, taskID)
	require.NoError(t, err, "insert task message")
}
