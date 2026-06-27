package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
)

// failingOutcomeRepo wraps stubStepOutcomeRepo to inject sweep
// errors. Other methods passthrough to inner.
type failingOutcomeRepo struct {
	inner    *stubStepOutcomeRepo
	sweepErr error
}

func (f *failingOutcomeRepo) Record(ctx context.Context, o *persistence.ExecutionStepOutcome) error {
	return f.inner.Record(ctx, o)
}
func (f *failingOutcomeRepo) Finalize(ctx context.Context, id, outcome, errorClass, errorDetail string, attr *string) error {
	return f.inner.Finalize(ctx, id, outcome, errorClass, errorDetail, attr)
}
func (f *failingOutcomeRepo) FinalizePending(ctx context.Context, executionID, stepID, outcome, errorClass, errorDetail string, attr *string) (string, string, error) {
	return f.inner.FinalizePending(ctx, executionID, stepID, outcome, errorClass, errorDetail, attr)
}
func (f *failingOutcomeRepo) SweepPending(_ context.Context, _, _ string) ([]persistence.SweepResult, error) {
	return nil, f.sweepErr
}
func (f *failingOutcomeRepo) List(ctx context.Context, filter persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return f.inner.List(ctx, filter)
}
func (f *failingOutcomeRepo) SupersedeAfter(ctx context.Context, executionID string, cutoff time.Time) (int64, error) {
	return f.inner.SupersedeAfter(ctx, executionID, cutoff)
}
func (f *failingOutcomeRepo) CountByRoleModelOutcome(ctx context.Context, outcome string, since, until time.Time, projectID string) ([]persistence.RoleModelOutcomeCount, error) {
	return f.inner.CountByRoleModelOutcome(ctx, outcome, since, until, projectID)
}

// TestSweepPendingOutcomes_NilRepo — when the outcome repo isn't
// wired the helper short-circuits. Pin the production default
// for deployments that don't enable per-step outcome tracking.
func TestSweepPendingOutcomes_NilRepoShortCircuit(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	require.NotPanics(t, func() {
		e.sweepPendingOutcomes(context.Background(), "exec-x", "ok")
	})
}

// TestSweepPendingOutcomes_RepoError — repo blip is logged and
// swallowed; the helper does not propagate (the caller is
// finalizing a task and shouldn't abort on a metrics blip).
func TestSweepPendingOutcomes_RepoError(t *testing.T) {
	repo := &failingOutcomeRepo{
		inner:    newStubStepOutcomeRepo(),
		sweepErr: errors.New("db unreachable"),
	}
	e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}
	require.NotPanics(t, func() {
		e.sweepPendingOutcomes(context.Background(), "exec-x", "ok")
	})
}

// TestSweepPendingOutcomes_MetricsEmittedPerSweptRow — when the
// sweep returns multiple rows, the helper emits one
// RecordFinalOutcome per row so the quality gauges credit each
// role+model independently. Pinning the per-row emission
// because the alternative (one batched call) would lose the
// per-role/model labels.
func TestSweepPendingOutcomes_MetricsPerRow(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	repo := newStubStepOutcomeRepo()
	e := &Executor{outcomeRepo: repo, metrics: m, logger: zerolog.Nop()}

	task := &persistence.Task{ID: "t", ProjectID: "p"}
	exec := &persistence.Execution{ID: "exec-y"}
	// Two pending rows in different (role, model) buckets so we can
	// assert each got its own counter increment.
	e.recordStepOutcome(context.Background(), task, exec, "s1", "researcher", "model-a",
		string(stepoutcome.PendingValidation), "", "", nil, nil)
	e.recordStepOutcome(context.Background(), task, exec, "s2", "writer", "model-b",
		string(stepoutcome.PendingValidation), "", "", nil, nil)

	e.sweepPendingOutcomes(context.Background(), "exec-y", string(stepoutcome.OK))

	// Each (role, model) pair must have its outcome counter
	// incremented once. RecordFinalOutcome uses outcome label
	// "ok" so we can read the totals back via testutil.
	got := testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("researcher", "model-a", string(stepoutcome.OK)))
	assert.Equal(t, 1.0, got, "per-(role,model) outcome counter must be 1 after sweep")
	got2 := testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("writer", "model-b", string(stepoutcome.OK)))
	assert.Equal(t, 1.0, got2)
}

// TestSweepPendingOutcomes_NoPendingRowsSilent — when no
// pending rows exist for the execution the sweep returns an
// empty slice; the helper must NOT emit any debug log or
// metrics. Pinned because an emit-anyway would spam the
// debug log for every clean execution.
func TestSweepPendingOutcomes_NoPendingRows(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	repo := newStubStepOutcomeRepo()
	e := &Executor{outcomeRepo: repo, metrics: m, logger: zerolog.Nop()}

	require.NotPanics(t, func() {
		e.sweepPendingOutcomes(context.Background(), "exec-empty", "ok")
	})
	// No counters should have been touched.
	got := testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("any", "any", "ok"))
	assert.Equal(t, 0.0, got)
}

// TestFinalizePendingOutcome_NilRepo — without the outcome repo
// wired, the helper returns silently. Used in test executors.
func TestFinalizePendingOutcome_NilRepoShortCircuit(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	require.NotPanics(t, func() {
		e.finalizePendingOutcome(context.Background(),
			"exec-x", "step-y", "ok", "", "", nil)
	})
}

// TestFinalizePendingOutcome_ErrNotFoundSwallowed — the helper
// treats ErrNotFound as a benign "no pending row exists"
// signal: deferring to the consumer's recordStepOutcome means
// the row may already have been finalized OR may never have
// been written (pre-feature execution). Either way, no warn.
func TestFinalizePendingOutcome_ErrNotFoundSwallowed(t *testing.T) {
	buf := &writableBuffer{}
	repo := newStubStepOutcomeRepo()
	e := &Executor{outcomeRepo: repo, logger: zerolog.New(buf)}

	e.finalizePendingOutcome(context.Background(),
		"no-such-exec", "no-such-step", "ok", "", "", nil)
	assert.NotContains(t, buf.String(), "failed to finalize pending row",
		"ErrNotFound must NOT log a warn — it's a benign case")
}

// TestFinalizePendingOutcome_HappyPathEmitsMetric — when a
// pending row exists, the finalize flips it AND emits a
// RecordFinalOutcome counter increment so the dashboards see
// the post-validation outcome.
func TestFinalizePendingOutcome_HappyPathEmitsMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	repo := newStubStepOutcomeRepo()
	e := &Executor{outcomeRepo: repo, metrics: m, logger: zerolog.Nop()}

	task := &persistence.Task{ID: "t", ProjectID: "p"}
	exec := &persistence.Execution{ID: "exec-fz"}
	e.recordStepOutcome(context.Background(), task, exec, "step-fz", "coder", "model-z",
		string(stepoutcome.PendingValidation), "", "", nil, nil)

	e.finalizePendingOutcome(context.Background(),
		"exec-fz", "step-fz", string(stepoutcome.OK), "", "", nil)

	got := testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("coder", "model-z", string(stepoutcome.OK)))
	assert.Equal(t, 1.0, got, "finalizePendingOutcome must emit one RecordFinalOutcome with the actual role+model")
}
