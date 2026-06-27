package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// retryFromStepCov_listErrRepo embeds the shared stub and overrides List to
// fail, so computeSupersedeCutoff's list-error branch is reachable without a
// real database.
type retryFromStepCov_listErrRepo struct {
	*stubStepOutcomeRepo
}

func (r *retryFromStepCov_listErrRepo) List(context.Context, persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return nil, errors.New("list failed")
}

// TestRetryFromStepCov_ComputeSupersedeCutoff_Guards exercises the early
// returns and the happy max-recorded-at computation.
func TestRetryFromStepCov_ComputeSupersedeCutoff_Guards(t *testing.T) {
	t.Run("nil repo returns zero time", func(t *testing.T) {
		e := &Executor{}
		got, err := e.computeSupersedeCutoff(context.Background(), "e1", []string{"plan"})
		require.NoError(t, err)
		assert.True(t, got.IsZero())
	})

	t.Run("empty survivors returns zero time", func(t *testing.T) {
		e := &Executor{outcomeRepo: newStubStepOutcomeRepo()}
		got, err := e.computeSupersedeCutoff(context.Background(), "e1", nil)
		require.NoError(t, err)
		assert.True(t, got.IsZero())
	})

	t.Run("list error propagates", func(t *testing.T) {
		e := &Executor{outcomeRepo: &retryFromStepCov_listErrRepo{newStubStepOutcomeRepo()}}
		_, err := e.computeSupersedeCutoff(context.Background(), "e1", []string{"plan"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "list outcomes for cutoff")
	})

	t.Run("max recorded_at among survivors", func(t *testing.T) {
		repo := newStubStepOutcomeRepo()
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		require.NoError(t, repo.Record(context.Background(), &persistence.ExecutionStepOutcome{ID: "a", ExecutionID: "e1", StepID: "plan", RecordedAt: t0}))
		require.NoError(t, repo.Record(context.Background(), &persistence.ExecutionStepOutcome{ID: "b", ExecutionID: "e1", StepID: "plan", RecordedAt: t0.Add(time.Hour)}))
		// A non-survivor row (later) must NOT raise the cutoff.
		require.NoError(t, repo.Record(context.Background(), &persistence.ExecutionStepOutcome{ID: "c", ExecutionID: "e1", StepID: "review", RecordedAt: t0.Add(2 * time.Hour)}))
		e := &Executor{outcomeRepo: repo}
		got, err := e.computeSupersedeCutoff(context.Background(), "e1", []string{"plan"})
		require.NoError(t, err)
		assert.Equal(t, t0.Add(time.Hour), got, "cutoff is the latest survivor recorded_at only")
	})
}

// TestRetryFromStepCov_ExecutionNotFound — a missing execution row surfaces a
// clear "not found" error (not the terminal-state sentinel).
func TestRetryFromStepCov_ExecutionNotFound(t *testing.T) {
	e, _, _, _, _ := setup()
	err := e.RetryFromStep(context.Background(), "missing-exec", "step")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestRetryFromStepCov_TaskNotFound — the execution is terminal and the step
// exists, but the task row is gone; the operator gets a precise error.
func TestRetryFromStepCov_TaskNotFound(t *testing.T) {
	e, _, er, _, _ := setup()
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t-gone",
		ProjectID:      "p1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan"},
	}
	require.NoError(t, er.Create(context.Background(), exec))

	err := e.RetryFromStep(context.Background(), "e1", "plan")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "task t-gone not found")
}
