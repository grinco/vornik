package executor

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// stagingScenario builds a parent whose delegation children exercise all three
// completeness buckets at once: one child that stages an artifact, one MISSING
// (no COMPLETED execution), and one EMPTY (COMPLETED but zero artifacts — the
// T-06b5 recurrence class). The gate must emit staged=1, missing=1, empty=1.
// The children are seeded into a MockTaskRepo under the parent so the gate
// fetches them through e.taskRepo.GetChildren (the production path).
func stagingScenario() (parent *persistence.Task, tr *MockTaskRepo, er *gatherFakeExecRepo, ar *gatherFakeArtifactRepo) {
	parent = &persistence.Task{ID: "task_metricsparent"}
	staged := &persistence.Task{
		ID:             "task_staged000001",
		ParentTaskID:   strptr(parent.ID),
		DelegationMode: delegationMode(persistence.DelegationModeParallel),
		CreatedAt:      t0,
	}
	missing := &persistence.Task{
		ID:             "task_missing00001",
		ParentTaskID:   strptr(parent.ID),
		DelegationMode: delegationMode(persistence.DelegationModeParallel),
		CreatedAt:      t0,
	}
	empty := &persistence.Task{
		ID:             "task_empty0000001",
		ParentTaskID:   strptr(parent.ID),
		DelegationMode: delegationMode(persistence.DelegationModeParallel),
		CreatedAt:      t0,
	}
	tr = NewMockTaskRepo()
	tr.AddTask(parent)
	tr.AddTask(staged)
	tr.AddTask(missing)
	tr.AddTask(empty)
	er = &gatherFakeExecRepo{execs: []*persistence.Execution{
		completedExec("exec_staged", staged.ID, t0),
		// missing: intentionally NO completed execution.
		completedExec("exec_empty", empty.ID, t0),
	}}
	ar = &gatherFakeArtifactRepo{artifacts: []*persistence.Artifact{
		artifactFor("exec_staged", staged.ID, "findings.md", "/store/findings.md"),
		// exec_empty produces no artifacts.
	}}
	return parent, tr, er, ar
}

// TestStageResumeChildArtifacts_EmitsMetrics pins Task 4: the gate increments
// the staged / missing / empty counters by the right amounts, labelled by the
// resuming workflow id.
func TestStageResumeChildArtifacts_EmitsMetrics(t *testing.T) {
	parent, tr, er, ar := stagingScenario()
	e := newStageExecutor(er, ar, tr)
	reg := prometheus.NewRegistry()
	e.metrics = NewMetrics(reg)

	opts := &agentInputOpts{InputArtifacts: baselineArtifacts()}
	step := registry.WorkflowStep{Type: "agent", StageChildArtifacts: true}

	e.stageResumeChildArtifacts(context.Background(), parent, step, opts, "dev-pipeline")

	// Sanity: the summary the gate computed matches the buckets we built.
	require.NotNil(t, opts.InputArtifactsSummary)
	assert.Equal(t, 1, opts.InputArtifactsSummary.Staged)
	assert.Len(t, opts.InputArtifactsSummary.Missing, 1)
	assert.Len(t, opts.InputArtifactsSummary.Empty, 1)

	assert.Equal(t, float64(1), testutil.ToFloat64(
		e.metrics.ChildArtifactsStagedTotal.WithLabelValues("dev-pipeline")),
		"staged_total incremented by summary.Staged")
	assert.Equal(t, float64(1), testutil.ToFloat64(
		e.metrics.ChildArtifactsMissingTotal.WithLabelValues("dev-pipeline")),
		"missing_total incremented by len(summary.Missing)")
	assert.Equal(t, float64(1), testutil.ToFloat64(
		e.metrics.ChildArtifactsEmptyTotal.WithLabelValues("dev-pipeline")),
		"empty_total incremented by len(summary.Empty)")
}

// TestStageResumeChildArtifacts_NoMetricsWhenGateClosed proves the counters
// stay untouched when the gate does not stage (flag off / not a resume).
func TestStageResumeChildArtifacts_NoMetricsWhenGateClosed(t *testing.T) {
	parent, tr, er, ar := stagingScenario()
	e := newStageExecutor(er, ar, tr)
	reg := prometheus.NewRegistry()
	e.metrics = NewMetrics(reg)

	opts := &agentInputOpts{InputArtifacts: baselineArtifacts()}
	stepOff := registry.WorkflowStep{Type: "agent", StageChildArtifacts: false}

	e.stageResumeChildArtifacts(context.Background(), parent, stepOff, opts, "dev-pipeline")

	assert.Equal(t, float64(0), testutil.ToFloat64(
		e.metrics.ChildArtifactsStagedTotal.WithLabelValues("dev-pipeline")))
	assert.Equal(t, float64(0), testutil.ToFloat64(
		e.metrics.ChildArtifactsMissingTotal.WithLabelValues("dev-pipeline")))
	assert.Equal(t, float64(0), testutil.ToFloat64(
		e.metrics.ChildArtifactsEmptyTotal.WithLabelValues("dev-pipeline")))
}

// TestStageResumeChildArtifacts_NilMetricsSafe proves the gate is a no-op on a
// metrics-less deployment (e.metrics nil) — it must still stage without panic.
func TestStageResumeChildArtifacts_NilMetricsSafe(t *testing.T) {
	parent, tr, er, ar := stagingScenario()
	e := newStageExecutor(er, ar, tr) // no metrics wired

	opts := &agentInputOpts{InputArtifacts: baselineArtifacts()}
	step := registry.WorkflowStep{Type: "agent", StageChildArtifacts: true}

	assert.NotPanics(t, func() {
		e.stageResumeChildArtifacts(context.Background(), parent, step, opts, "dev-pipeline")
	})
	require.NotNil(t, opts.InputArtifactsSummary)
}

// TestRecordChildArtifactStaging_ZeroBucketsNoSeries proves per-metric
// guarding: a bucket with a zero count must not mint a series.
func TestRecordChildArtifactStaging_ZeroBucketsNoSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.RecordChildArtifactStaging("adaptive", 2, 0, 0)

	assert.Equal(t, float64(2), testutil.ToFloat64(
		m.ChildArtifactsStagedTotal.WithLabelValues("adaptive")))
	// missing/empty were zero → no Add, series stays at implicit zero.
	assert.Equal(t, 0, testutil.CollectAndCount(m.ChildArtifactsMissingTotal))
	assert.Equal(t, 0, testutil.CollectAndCount(m.ChildArtifactsEmptyTotal))
}

// TestRecordChildArtifactStaging_NilSafe is the nil-receiver pin.
func TestRecordChildArtifactStaging_NilSafe(t *testing.T) {
	var m *Metrics
	assert.NotPanics(t, func() { m.RecordChildArtifactStaging("x", 1, 1, 1) })
}
