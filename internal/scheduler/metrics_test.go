package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/testutil/metricstest"
)

func TestNewMetrics_Scheduler(t *testing.T) {
	t.Run("creates metrics with default registerer", func(t *testing.T) {
		metricstest.IsolateDefaultRegistry(t) // count-safe nil-fallback coverage
		m := NewMetrics(nil)
		require.NotNil(t, m)
		assert.NotNil(t, m.LeasesAcquiredTotal)
		assert.NotNil(t, m.LeasesExpiredTotal)
		assert.NotNil(t, m.RecoveriesTotal)
		assert.NotNil(t, m.LeaseDurationSeconds)
		assert.NotNil(t, m.QueueWaitSeconds)
		assert.NotNil(t, m.TasksScheduledTotal)
		assert.NotNil(t, m.LoopsTotal)
	})

	t.Run("creates metrics with custom registry", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)
		require.NotNil(t, m)
		assert.NotNil(t, m.registry)
	})
}

// TestMetrics_RecordLoop — the loop-iteration counter (audit R5) bumps
// once per call and is nil-safe on an unwired scheduler.
func TestMetrics_RecordLoop(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordLoop()
	m.RecordLoop()
	assert.Equal(t, 2.0, testutil.ToFloat64(m.LoopsTotal))

	var nilM *Metrics
	assert.NotPanics(t, func() { nilM.RecordLoop() })
}

func TestMetrics_RecordLeaseAcquired(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordLeaseAcquired("project-1")

	// Verify counter incremented
	count := testutil.ToFloat64(m.LeasesAcquiredTotal.WithLabelValues("project-1"))
	assert.Equal(t, 1.0, count)

	// Record another
	m.RecordLeaseAcquired("project-1")
	count = testutil.ToFloat64(m.LeasesAcquiredTotal.WithLabelValues("project-1"))
	assert.Equal(t, 2.0, count)

	// Different project
	m.RecordLeaseAcquired("project-2")
	count = testutil.ToFloat64(m.LeasesAcquiredTotal.WithLabelValues("project-2"))
	assert.Equal(t, 1.0, count)
}

func TestMetrics_RecordLeaseExpired(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordLeaseExpired("project-1")
	m.RecordLeaseExpired("project-1")
	m.RecordLeaseExpired("project-2")

	count := testutil.ToFloat64(m.LeasesExpiredTotal.WithLabelValues("project-1"))
	assert.Equal(t, 2.0, count)

	count = testutil.ToFloat64(m.LeasesExpiredTotal.WithLabelValues("project-2"))
	assert.Equal(t, 1.0, count)
}

func TestMetrics_RecordRecovery(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordRecovery("project-1")
	m.RecordRecovery("project-1")
	m.RecordRecovery("project-2")

	count := testutil.ToFloat64(m.RecoveriesTotal.WithLabelValues("project-1"))
	assert.Equal(t, 2.0, count)

	count = testutil.ToFloat64(m.RecoveriesTotal.WithLabelValues("project-2"))
	assert.Equal(t, 1.0, count)
}

func TestMetrics_RecordLeaseDuration(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Record durations
	m.RecordLeaseDuration("project-1", 0.5)
	m.RecordLeaseDuration("project-1", 1.5)
	m.RecordLeaseDuration("project-2", 2.0)

	// Verify histogram is functional (doesn't panic)
	assert.NotNil(t, m.LeaseDurationSeconds)
}

// TestMetrics_RecordQueueWait — queue residency (creation→lease) is now owned
// by the scheduler, not the dead queue.Lease() path. Records on the histogram
// and is nil-safe.
func TestMetrics_RecordQueueWait(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordQueueWait("project-1", 0.5)
	m.RecordQueueWait("project-1", 2.0)

	// One label set with two observations.
	require.Equal(t, 1, testutil.CollectAndCount(m.QueueWaitSeconds))

	var nilM *Metrics
	assert.NotPanics(t, func() { nilM.RecordQueueWait("project-1", 1.0) })
}

func TestMetrics_RecordTaskScheduled(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordTaskScheduled("project-1")
	m.RecordTaskScheduled("project-1")
	m.RecordTaskScheduled("project-2")

	count := testutil.ToFloat64(m.TasksScheduledTotal.WithLabelValues("project-1"))
	assert.Equal(t, 2.0, count)

	count = testutil.ToFloat64(m.TasksScheduledTotal.WithLabelValues("project-2"))
	assert.Equal(t, 1.0, count)
}

func TestMetrics_NilSafety_Scheduler(t *testing.T) {
	var m *Metrics

	// All methods should be safe with nil receiver
	assert.NotPanics(t, func() { m.RecordLeaseAcquired("project-1") })
	assert.NotPanics(t, func() { m.RecordLeaseExpired("project-1") })
	assert.NotPanics(t, func() { m.RecordRecovery("project-1") })
	assert.NotPanics(t, func() { m.RecordLeaseDuration("project-1", 1.0) })
	assert.NotPanics(t, func() { m.RecordQueueWait("project-1", 1.0) })
	assert.NotPanics(t, func() { m.RecordTaskScheduled("project-1") })
}

func TestWithMetrics_Scheduler(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	repo := &mockTaskRepo{}
	s := NewWithOptions(repo, nil, WithMetrics(m))

	assert.Equal(t, m, s.metrics)
}

func TestWithPrometheusRegistry_Scheduler(t *testing.T) {
	registry := prometheus.NewRegistry()

	repo := &mockTaskRepo{}
	s := NewWithOptions(repo, nil, WithPrometheusRegistry(registry))

	// Metrics should be initialized
	require.NotNil(t, s.metrics)
}

func TestMetrics_RegisterWithoutPanic_Scheduler(t *testing.T) {
	// This test verifies that all metrics can be registered without panic
	registry := prometheus.NewRegistry()

	require.NotPanics(t, func() {
		NewMetrics(registry)
	})
}

// mockTaskRepo is a minimal mock for TaskRepository
type mockTaskRepo struct{}

func (m *mockTaskRepo) Get(ctx context.Context, id string) (*persistence.Task, error) {
	return nil, persistence.ErrNotFound
}

func (m *mockTaskRepo) LeaseTask(ctx context.Context, opts persistence.LeaseOptions) (*persistence.Task, error) {
	return nil, nil
}

func (m *mockTaskRepo) RenewLease(ctx context.Context, taskID, leaseID string, extendBySeconds int) error {
	return nil
}

func (m *mockTaskRepo) ReleaseLease(ctx context.Context, taskID, leaseID string, newStatus persistence.TaskStatus, opts persistence.ReleaseOptions) error {
	return nil
}

func (m *mockTaskRepo) FindExpiredLeases(ctx context.Context, limit int) ([]*persistence.Task, error) {
	return nil, nil
}

func (m *mockTaskRepo) CountByStatus(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
	return nil, nil
}

func (m *mockTaskRepo) CountRecentFailures(ctx context.Context, projectID string, errorClasses []string, since time.Time) (int, error) {
	return 0, nil
}

func (m *mockTaskRepo) Ping(ctx context.Context) error {
	return nil
}
