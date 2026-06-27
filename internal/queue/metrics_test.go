package queue

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/testutil/metricstest"
)

func TestNewMetrics(t *testing.T) {
	t.Run("creates metrics with default registerer", func(t *testing.T) {
		metricstest.IsolateDefaultRegistry(t) // count-safe nil-fallback coverage
		m := NewMetrics(nil)
		require.NotNil(t, m)
		assert.NotNil(t, m.Depth)
		assert.NotNil(t, m.EnqueuedTotal)
		assert.NotNil(t, m.DLQTotal)
	})

	t.Run("creates metrics with custom registry", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)
		require.NotNil(t, m)
		assert.NotNil(t, m.registry)
	})
}

func TestMetrics_EnqueuedTotal(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Increment enqueued counter
	m.EnqueuedTotal.WithLabelValues("project-1").Inc()
	m.EnqueuedTotal.WithLabelValues("project-2").Add(5)

	// Verify counter values
	count := testutil.ToFloat64(m.EnqueuedTotal.WithLabelValues("project-1"))
	assert.Equal(t, 1.0, count)

	count = testutil.ToFloat64(m.EnqueuedTotal.WithLabelValues("project-2"))
	assert.Equal(t, 5.0, count)
}

func TestMetrics_Depth(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Set depth gauge
	m.Depth.WithLabelValues("project-1").Set(10)

	// Verify gauge value
	value := testutil.ToFloat64(m.Depth.WithLabelValues("project-1"))
	assert.Equal(t, 10.0, value)

	// Update gauge
	m.Depth.WithLabelValues("project-1").Set(5)
	value = testutil.ToFloat64(m.Depth.WithLabelValues("project-1"))
	assert.Equal(t, 5.0, value)
}

func TestMetrics_DLQTotal(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Increment DLQ counter
	m.DLQTotal.WithLabelValues("project-1").Inc()

	// Verify counter value
	count := testutil.ToFloat64(m.DLQTotal.WithLabelValues("project-1"))
	assert.Equal(t, 1.0, count)
}

func TestWithPrometheusRegistry(t *testing.T) {
	registry := prometheus.NewRegistry()

	q := New(WithPrometheusRegistry(registry))

	// Metrics should be initialized
	require.NotNil(t, q.metrics)
}

func TestWithMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	q := New(WithMetrics(m))

	assert.Equal(t, m, q.metrics)
}

func TestQueue_Enqueue_WithMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	q := New(WithMetrics(m))

	err := q.Enqueue("task-1", "project-1", 10)
	require.NoError(t, err)

	// Verify enqueued counter incremented
	count := testutil.ToFloat64(m.EnqueuedTotal.WithLabelValues("project-1"))
	assert.Equal(t, 1.0, count)
}

func TestMetrics_RegisterWithoutPanic(t *testing.T) {
	// This test verifies that all metrics can be registered without panic
	registry := prometheus.NewRegistry()

	require.NotPanics(t, func() {
		NewMetrics(registry)
	})
}
