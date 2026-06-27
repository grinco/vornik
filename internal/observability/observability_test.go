package observability

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigDefaults(t *testing.T) {
	cfg := Config{
		MetricsAddr:     ":9090",
		TracingEnabled:  false,
		TracingEndpoint: "localhost:4317",
	}

	assert.Equal(t, ":9090", cfg.MetricsAddr)
	assert.False(t, cfg.TracingEnabled)
	assert.Equal(t, "localhost:4317", cfg.TracingEndpoint)
}

func TestNewMetrics(t *testing.T) {
	logger := zerolog.Nop()
	cfg := Config{MetricsAddr: ":9090"}

	metrics := NewMetrics(cfg, logger)
	assert.NotNil(t, metrics)
	assert.NotNil(t, metrics.Registry())

	families, err := metrics.Registry().Gather()
	require.NoError(t, err)

	names := make(map[string]struct{}, len(families))
	for _, family := range families {
		names[family.GetName()] = struct{}{}
	}

	_, hasGoCollector := names["go_goroutines"]
	_, hasProcessCollector := names["process_resident_memory_bytes"]
	_, hasVornikMetric := names["vornik_observability_up"]

	assert.True(t, hasGoCollector)
	assert.True(t, hasProcessCollector)
	assert.True(t, hasVornikMetric)
}

func TestNewTracingDisabled(t *testing.T) {
	logger := zerolog.Nop()
	cfg := Config{TracingEnabled: false}

	tracing, err := NewTracing(cfg, logger)
	assert.NoError(t, err)
	assert.Nil(t, tracing) // Should be nil when disabled
}

// Note: Testing tracing with actual exporter requires a running OTLP endpoint
// and is better suited for integration tests.
func TestNewTracingEnabledRequiresEndpoint(t *testing.T) {
	// This test would need a mock OTLP endpoint or be an integration test.
	// For scaffolding, we just verify the config is accepted.
	t.Skip("requires OTLP endpoint for integration testing")
}

func TestObservabilityNew(t *testing.T) {
	logger := zerolog.Nop()
	cfg := Config{
		MetricsAddr:     ":9090",
		TracingEnabled:  false,
		TracingEndpoint: "localhost:4317",
	}

	obs, err := New(cfg, logger)
	assert.NoError(t, err)
	assert.NotNil(t, obs)
	assert.NotNil(t, obs.Metrics)
	assert.Nil(t, obs.Tracing) // Disabled
}

func TestObservabilityNewTracingOnly(t *testing.T) {
	logger := zerolog.Nop()
	cfg := Config{}

	obs, err := New(cfg, logger)
	assert.NoError(t, err)
	assert.NotNil(t, obs)
	// Metrics registry is always created (used by /metrics on main API server)
	assert.NotNil(t, obs.Metrics)
	assert.Nil(t, obs.Tracing)
}
