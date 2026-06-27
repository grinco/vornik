package executor

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestShapeRetry_RescuesMissingKey is the headline integration test
// for item 10 of https://docs.vornik.io It
// proves that a regular agent step whose first attempt fails
// outputSchema validation (one required key omitted) gets a corrective
// retry, and that the retry's valid output rescues the task.
//
// Pre-item-10 behaviour: the lead path had a corrective retry but
// every other agent role hard-failed on the same class of error.
// Post-item-10: the shape-retry generalisation reclaims those losses.
//
// Test scenario:
//   - role "writer" declares requiredOutputKeys=[message, writing]
//   - first agent invocation produces {"status":"COMPLETED","message":"hi"}
//     (missing the "writing" key → schema violation fires)
//   - second invocation produces a valid result with both keys
//   - asserts: task COMPLETED, runtime invoked twice, the new
//     shape_retry_by_outcome_total counter incremented for
//     {role=writer, outcome=attempted} AND {role=writer, outcome=recovered}.
//
// This is the regression test that catches a future change which
// silently dropped the retry for non-lead roles — without the rescue
// the task would FAIL instead of completing.
func TestShapeRetry_RescuesMissingKey(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		// Attempt 1: missing the required "writing" key. Triggers
		// "schema violation: ... is missing required keys: [writing]"
		// in container.go's validateRequiredOutputKeys path.
		`{"status":"COMPLETED","message":"first attempt — forgot the writing object"}`,
		// Attempt 2 (the corrective retry): both keys present.
		`{"status":"COMPLETED","message":"second attempt","writing":{"written":true,"path":"/out/note.md"}}`,
	}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()

	// Wire a metrics registry so we can assert on the new
	// shape_retry_by_outcome_total counter — the rescue-rate
	// visibility surface item 10 promises operators.
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	e := NewWithOptions(rt, er, ar, tr, nil)
	e.metrics = metrics
	e.config.RetryDelay = 0

	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "single"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{
				Name:               "writer",
				Runtime:            registry.SwarmRoleRuntime{Image: "fake-agent:latest"},
				RequiredOutputKeys: []string{"message", "writing"},
			}}},
		},
		workflows: map[string]*registry.Workflow{
			"single": {
				ID:         "single",
				Entrypoint: "go",
				Steps: map[string]registry.WorkflowStep{
					"go": {Type: "agent", Role: "writer", OnSuccess: "done", OnFail: "failed"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done":   {Status: "COMPLETED"},
					"failed": {Status: "FAILED"},
				},
			},
		},
	})

	taskID := "t-writer-retry"
	tr.AddTask(&persistence.Task{
		ID:          taskID,
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"taskType":"write","context":{"prompt":"write a note"}}`),
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute(taskID))

	// Wait for terminal state — completion arrives only when the
	// corrective retry's output passes validation.
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), taskID)
		return task != nil && task.Status == persistence.TaskStatusCompleted
	}, 3*time.Second, 10*time.Millisecond,
		"task must finalize COMPLETED after shape retry rescues missing key — "+
			"without item 10's generalised retry the writer would hard-fail")

	assert.Equal(t, 2, rt.startCalls,
		"runtime must be invoked exactly twice: original attempt (missing key) + shape retry (recovered)")

	// Item 10's new metric: per-role outcome counter must show
	// attempted=1 and recovered=1 for the writer role.
	gotAttempted := testutil.ToFloat64(metrics.ShapeRetryByOutcomeTotal.WithLabelValues("writer", "attempted"))
	assert.Equal(t, float64(1), gotAttempted,
		"shape_retry_by_outcome_total{role=writer,outcome=attempted} must increment once per retry fire")

	gotRecovered := testutil.ToFloat64(metrics.ShapeRetryByOutcomeTotal.WithLabelValues("writer", "recovered"))
	assert.Equal(t, float64(1), gotRecovered,
		"shape_retry_by_outcome_total{role=writer,outcome=recovered} must increment when retry produces valid output")

	gotFailed := testutil.ToFloat64(metrics.ShapeRetryByOutcomeTotal.WithLabelValues("writer", "failed"))
	assert.Equal(t, float64(0), gotFailed,
		"failed counter must stay at zero on a successful rescue")
}

// TestShapeRetry_RecordsFailedOutcomeOnBothAttemptsBad — the negative
// half of the rescue-rate metric. When both the original attempt
// and the corrective retry fail validation, the executor must
// surface a FAILED task AND record outcome=failed so dashboards can
// distinguish "rescue worked" from "rescue didn't work either".
func TestShapeRetry_RecordsFailedOutcomeOnBothAttemptsBad(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		// Both attempts omit the required "writing" key.
		`{"status":"COMPLETED","message":"attempt 1 — missing writing"}`,
		`{"status":"COMPLETED","message":"attempt 2 — also missing writing"}`,
	}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()

	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	e := NewWithOptions(rt, er, ar, tr, nil)
	e.metrics = metrics
	e.config.RetryDelay = 0

	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "single"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{
				Name:               "writer",
				Runtime:            registry.SwarmRoleRuntime{Image: "fake-agent:latest"},
				RequiredOutputKeys: []string{"message", "writing"},
			}}},
		},
		workflows: map[string]*registry.Workflow{
			"single": {
				ID:         "single",
				Entrypoint: "go",
				Steps: map[string]registry.WorkflowStep{
					"go": {Type: "agent", Role: "writer", OnSuccess: "done", OnFail: "failed"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done":   {Status: "COMPLETED"},
					"failed": {Status: "FAILED"},
				},
			},
		},
	})

	taskID := "t-writer-double-fail"
	tr.AddTask(&persistence.Task{
		ID:          taskID,
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"taskType":"write","context":{"prompt":"write a note"}}`),
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute(taskID))

	// Both attempts produce invalid output → the workflow's on_fail
	// terminal lands. Status is FAILED (not COMPLETED).
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), taskID)
		return task != nil &&
			(task.Status == persistence.TaskStatusFailed ||
				task.Status == persistence.TaskStatusCompleted)
	}, 3*time.Second, 10*time.Millisecond,
		"task must reach terminal state after both attempts fail validation")

	task, _ := tr.Get(context.Background(), taskID)
	assert.NotEqual(t, persistence.TaskStatusCompleted, task.Status,
		"both-attempts-bad must not surface as COMPLETED")

	// Item 10's metric: attempted=1, failed=1, recovered=0.
	gotAttempted := testutil.ToFloat64(metrics.ShapeRetryByOutcomeTotal.WithLabelValues("writer", "attempted"))
	assert.Equal(t, float64(1), gotAttempted, "attempted must increment even when the retry fails")

	gotFailed := testutil.ToFloat64(metrics.ShapeRetryByOutcomeTotal.WithLabelValues("writer", "failed"))
	assert.Equal(t, float64(1), gotFailed, "failed must increment when the retry's own output also fails validation")

	gotRecovered := testutil.ToFloat64(metrics.ShapeRetryByOutcomeTotal.WithLabelValues("writer", "recovered"))
	assert.Equal(t, float64(0), gotRecovered, "recovered must stay at zero when both attempts fail")
}
