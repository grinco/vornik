package runtime

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/testutil/metricstest"
)

func TestNewMetrics_Runtime(t *testing.T) {
	t.Run("creates metrics with default registerer", func(t *testing.T) {
		metricstest.IsolateDefaultRegistry(t) // count-safe nil-fallback coverage
		m := NewMetrics(nil)
		require.NotNil(t, m)
		assert.NotNil(t, m.ContainersStartedTotal)
		assert.NotNil(t, m.ContainersStoppedTotal)
		assert.NotNil(t, m.ContainersRunning)
		assert.NotNil(t, m.ContainerStartLatencySeconds)
		assert.NotNil(t, m.ContainerStopLatencySeconds)
		assert.NotNil(t, m.PodmanErrorsTotal)
	})

	t.Run("creates metrics with custom registry", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)
		require.NotNil(t, m)
		assert.NotNil(t, m.registry)
	})
}

func TestMetrics_RecordContainerStarted(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordContainerStarted("project-1", 1.5)

	// Verify counter incremented
	count := testutil.ToFloat64(m.ContainersStartedTotal.WithLabelValues("project-1"))
	assert.Equal(t, 1.0, count)

	// Verify gauge incremented
	running := testutil.ToFloat64(m.ContainersRunning.WithLabelValues("project-1"))
	assert.Equal(t, 1.0, running)

	// Start another
	m.RecordContainerStarted("project-1", 2.0)
	running = testutil.ToFloat64(m.ContainersRunning.WithLabelValues("project-1"))
	assert.Equal(t, 2.0, running)
}

func TestMetrics_RecordContainerStopped(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// First start some containers
	m.RecordContainerStarted("project-1", 1.0)
	m.RecordContainerStarted("project-1", 1.0)

	// Then stop one
	m.RecordContainerStopped("project-1", 0.5)

	// Verify counter incremented
	count := testutil.ToFloat64(m.ContainersStoppedTotal.WithLabelValues("project-1"))
	assert.Equal(t, 1.0, count)

	// Verify gauge decremented
	running := testutil.ToFloat64(m.ContainersRunning.WithLabelValues("project-1"))
	assert.Equal(t, 1.0, running)
}

func TestMetrics_RecordPodmanError(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Record errors for different operations
	m.RecordPodmanError("start")
	m.RecordPodmanError("start")
	m.RecordPodmanError("stop")
	m.RecordPodmanError("inspect")

	count := testutil.ToFloat64(m.PodmanErrorsTotal.WithLabelValues("start"))
	assert.Equal(t, 2.0, count)

	count = testutil.ToFloat64(m.PodmanErrorsTotal.WithLabelValues("stop"))
	assert.Equal(t, 1.0, count)

	count = testutil.ToFloat64(m.PodmanErrorsTotal.WithLabelValues("inspect"))
	assert.Equal(t, 1.0, count)
}

func TestMetrics_SetContainersRunning(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Set specific values
	m.SetContainersRunning("project-1", 5.0)

	running := testutil.ToFloat64(m.ContainersRunning.WithLabelValues("project-1"))
	assert.Equal(t, 5.0, running)

	// Update value
	m.SetContainersRunning("project-1", 3.0)
	running = testutil.ToFloat64(m.ContainersRunning.WithLabelValues("project-1"))
	assert.Equal(t, 3.0, running)
}

func TestMetrics_LatencyHistograms(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Record latencies
	m.RecordContainerStarted("project-1", 1.5)
	m.RecordContainerStopped("project-1", 0.5)

	// Verify histograms are functional (doesn't panic)
	assert.NotNil(t, m.ContainerStartLatencySeconds)
	assert.NotNil(t, m.ContainerStopLatencySeconds)
}

func TestMetrics_NilSafety_Runtime(t *testing.T) {
	var m *Metrics

	// All methods should be safe with nil receiver
	assert.NotPanics(t, func() { m.RecordContainerStarted("project-1", 1.0) })
	assert.NotPanics(t, func() { m.RecordContainerStopped("project-1", 1.0) })
	assert.NotPanics(t, func() { m.RecordPodmanError("start") })
	assert.NotPanics(t, func() { m.SetContainersRunning("project-1", 5.0) })
}

func TestMetrics_RegisterWithoutPanic_Runtime(t *testing.T) {
	// This test verifies that all metrics can be registered without panic
	registry := prometheus.NewRegistry()

	require.NotPanics(t, func() {
		NewMetrics(registry)
	})
}

func TestWithMetrics_Runtime(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Note: We can't create a Manager with custom metrics directly in tests
	// without podman available, so we test the option function
	opt := WithMetrics(m)
	assert.NotNil(t, opt)
}

func TestWithPrometheusRegistry_Runtime(t *testing.T) {
	registry := prometheus.NewRegistry()

	opt := WithPrometheusRegistry(registry)
	assert.NotNil(t, opt)
}
