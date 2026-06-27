package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// fakeAutonomyController records SetProjectAutonomyEnabled calls so
// tests can assert the breaker actually flipped the flag.
type fakeAutonomyController struct {
	mu    sync.Mutex
	calls []autonomyCall
	err   error
}

type autonomyCall struct {
	projectID string
	enabled   bool
}

func (f *fakeAutonomyController) SetProjectAutonomyEnabled(projectID string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, autonomyCall{projectID: projectID, enabled: enabled})
	return f.err
}

func (f *fakeAutonomyController) callsSnapshot() []autonomyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]autonomyCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// cbRecordingNotifier captures the alert payload so tests can assert
// the breaker emitted the expected human-readable message.
type cbRecordingNotifier struct {
	mu       sync.Mutex
	messages []string
}

func (r *cbRecordingNotifier) NotifyTaskCompleted(_ context.Context, _ *persistence.Task, success bool, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !success {
		r.messages = append(r.messages, message)
	}
}

func (r *cbRecordingNotifier) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.messages))
	copy(out, r.messages)
	return out
}

func TestCircuitBreaker_TripsAtThreshold(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CountRecentFailuresFunc: func(_ context.Context, _ string, _ []string, _ time.Time) (int, error) {
			return 5, nil
		},
	}
	autonomy := &fakeAutonomyController{}
	notifier := &cbRecordingNotifier{}
	cb := newCircuitBreaker(repo, autonomy, notifier, 5, 2*time.Hour, nil, zerolog.Nop())
	require.NotNil(t, cb)

	cb.Trip(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, persistence.TaskFailureClassToolError)
	calls := autonomy.callsSnapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "proj", calls[0].projectID)
	assert.False(t, calls[0].enabled, "breaker must disable autonomy")

	msgs := notifier.snapshot()
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0], "Circuit breaker tripped")
	assert.Contains(t, msgs[0], "proj")
	assert.Contains(t, msgs[0], "vornikctl autonomy enable")
}

func TestCircuitBreaker_BelowThresholdNoTrip(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CountRecentFailuresFunc: func(_ context.Context, _ string, _ []string, _ time.Time) (int, error) {
			return 4, nil
		},
	}
	autonomy := &fakeAutonomyController{}
	notifier := &cbRecordingNotifier{}
	cb := newCircuitBreaker(repo, autonomy, notifier, 5, 2*time.Hour, nil, zerolog.Nop())

	cb.Trip(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, persistence.TaskFailureClassToolError)
	assert.Empty(t, autonomy.callsSnapshot(), "breaker must not fire below threshold")
	assert.Empty(t, notifier.snapshot())
}

func TestCircuitBreaker_SkipsConfiguredClasses(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CountRecentFailuresFunc: func(_ context.Context, _ string, _ []string, _ time.Time) (int, error) {
			t.Fatal("CountRecentFailures should not be called for skipped classes")
			return 0, nil
		},
	}
	autonomy := &fakeAutonomyController{}
	cb := newCircuitBreaker(repo, autonomy, nil, 5, 2*time.Hour,
		[]string{persistence.TaskFailureClassCancelled, persistence.TaskFailureClassBudgetBlocked},
		zerolog.Nop())

	cb.Trip(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, persistence.TaskFailureClassCancelled)
	cb.Trip(context.Background(), &persistence.Task{ID: "t2", ProjectID: "proj"}, persistence.TaskFailureClassBudgetBlocked)
	assert.Empty(t, autonomy.callsSnapshot())
}

func TestCircuitBreaker_CooldownPreventsAlertSpam(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CountRecentFailuresFunc: func(_ context.Context, _ string, _ []string, _ time.Time) (int, error) {
			return 5, nil
		},
	}
	autonomy := &fakeAutonomyController{}
	notifier := &cbRecordingNotifier{}
	cb := newCircuitBreaker(repo, autonomy, notifier, 5, 2*time.Hour, nil, zerolog.Nop())

	// First trip fires; subsequent trips within cooldown are no-ops
	// even though the count is still above threshold.
	cb.Trip(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, persistence.TaskFailureClassToolError)
	cb.Trip(context.Background(), &persistence.Task{ID: "t2", ProjectID: "proj"}, persistence.TaskFailureClassToolError)
	cb.Trip(context.Background(), &persistence.Task{ID: "t3", ProjectID: "proj"}, persistence.TaskFailureClassToolError)

	assert.Len(t, autonomy.callsSnapshot(), 1, "cooldown must dedupe SetProjectAutonomyEnabled calls")
	assert.Len(t, notifier.snapshot(), 1, "cooldown must dedupe operator alerts")
}

func TestCircuitBreaker_DisabledWhenInputsMissing(t *testing.T) {
	// Returns nil when any required dependency is missing — the
	// executor's Trip call site checks for nil and skips, so this
	// is the hatch for "feature off in this deployment".
	assert.Nil(t, newCircuitBreaker(nil, &fakeAutonomyController{}, nil, 5, time.Hour, nil, zerolog.Nop()))
	assert.Nil(t, newCircuitBreaker(&mocks.MockTaskRepository{}, nil, nil, 5, time.Hour, nil, zerolog.Nop()))
	assert.Nil(t, newCircuitBreaker(&mocks.MockTaskRepository{}, &fakeAutonomyController{}, nil, 0, time.Hour, nil, zerolog.Nop()))
	assert.Nil(t, newCircuitBreaker(&mocks.MockTaskRepository{}, &fakeAutonomyController{}, nil, 5, 0, nil, zerolog.Nop()))
}
