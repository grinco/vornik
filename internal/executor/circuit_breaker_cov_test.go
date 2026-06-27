package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestCircuitBreakerCov_TripNilGuards — Trip must be a no-op (never
// touch the repo or autonomy) for a nil breaker, a nil task, or a
// task with an empty ProjectID. These are the defensive guards at the
// top of Trip; the executor's call site can pass any of these.
func TestCircuitBreakerCov_TripNilGuards(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CountRecentFailuresFunc: func(_ context.Context, _ string, _ []string, _ time.Time) (int, error) {
			t.Fatal("CountRecentFailures must not run when the guards short-circuit")
			return 0, nil
		},
	}
	autonomy := &fakeAutonomyController{}

	// nil receiver
	var nilBreaker *circuitBreaker
	nilBreaker.Trip(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, persistence.TaskFailureClassToolError)

	cb := newCircuitBreaker(repo, autonomy, nil, 5, time.Hour, nil, zerolog.Nop())
	require.NotNil(t, cb)

	// nil task
	cb.Trip(context.Background(), nil, persistence.TaskFailureClassToolError)
	// empty project ID
	cb.Trip(context.Background(), &persistence.Task{ID: "t1", ProjectID: ""}, persistence.TaskFailureClassToolError)

	assert.Empty(t, autonomy.callsSnapshot())
}

// TestCircuitBreakerCov_CountFailuresErrorSkipsEvaluation — when the
// repo's CountRecentFailures errors, the breaker logs a warning and
// bails WITHOUT flipping autonomy. A DB hiccup must not silently trip
// (or silently un-trip) the breaker.
func TestCircuitBreakerCov_CountFailuresErrorSkipsEvaluation(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CountRecentFailuresFunc: func(_ context.Context, _ string, _ []string, _ time.Time) (int, error) {
			return 0, errors.New("db transient")
		},
	}
	autonomy := &fakeAutonomyController{}
	notifier := &cbRecordingNotifier{}
	cb := newCircuitBreaker(repo, autonomy, notifier, 5, time.Hour, nil, zerolog.Nop())

	cb.Trip(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, persistence.TaskFailureClassToolError)
	assert.Empty(t, autonomy.callsSnapshot(), "count error must skip the evaluation entirely")
	assert.Empty(t, notifier.snapshot())
}

// TestCircuitBreakerCov_AutonomyDiskErrorStillAlerts — the in-memory
// autonomy flip may succeed even when the on-disk write returns an
// error. The breaker must log the error but STILL emit the operator
// alert (autonomy is paused in-process regardless). This drives the
// SetProjectAutonomyEnabled error branch.
func TestCircuitBreakerCov_AutonomyDiskErrorStillAlerts(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CountRecentFailuresFunc: func(_ context.Context, _ string, _ []string, _ time.Time) (int, error) {
			return 9, nil
		},
	}
	autonomy := &fakeAutonomyController{err: errors.New("disk write failed")}
	notifier := &cbRecordingNotifier{}
	cb := newCircuitBreaker(repo, autonomy, notifier, 5, time.Hour, nil, zerolog.Nop())

	cb.Trip(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, persistence.TaskFailureClassToolError)
	require.Len(t, autonomy.callsSnapshot(), 1, "flip is attempted even when it errors")
	require.Len(t, notifier.snapshot(), 1, "operator alert must still fire after a disk-write error")
}

// TestCircuitBreakerCov_TripWithNilNotifier — when no notifier is
// wired the breaker still flips autonomy; the alert branch is simply
// skipped (the notifier-nil guard). No panic.
func TestCircuitBreakerCov_TripWithNilNotifier(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CountRecentFailuresFunc: func(_ context.Context, _ string, _ []string, _ time.Time) (int, error) {
			return 5, nil
		},
	}
	autonomy := &fakeAutonomyController{}
	cb := newCircuitBreaker(repo, autonomy, nil, 5, time.Hour, nil, zerolog.Nop())

	cb.Trip(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, persistence.TaskFailureClassToolError)
	require.Len(t, autonomy.callsSnapshot(), 1, "autonomy still flips with a nil notifier")
}

// TestCircuitBreakerCov_RecentlyTrippedFalseWhenNeverTripped — the
// recentlyTripped fast-path returns false when there's no record for
// the project. Pinned independently so the not-ok branch is covered
// without relying on a full Trip.
func TestCircuitBreakerCov_RecentlyTrippedFalseWhenNeverTripped(t *testing.T) {
	repo := &mocks.MockTaskRepository{}
	cb := newCircuitBreaker(repo, &fakeAutonomyController{}, nil, 5, time.Hour, nil, zerolog.Nop())
	assert.False(t, cb.recentlyTripped("never-seen"))
}
