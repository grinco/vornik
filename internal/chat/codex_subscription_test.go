package chat

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexSubscriptionRecordMetricsUsesChatMetricCardinality(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	c := NewCodexSubscriptionClient("gpt-5.4-mini")
	c.SetMetrics(metrics)

	c.recordMetrics(time.Now().Add(-time.Second), "ok")
	c.recordMetrics(time.Now().Add(-time.Second), "error")

	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.RequestsTotal.WithLabelValues("gpt-5.4-mini", "ok")))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.RequestsTotal.WithLabelValues("gpt-5.4-mini", "error")))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ErrorsTotal.WithLabelValues("gpt-5.4-mini", "error")))

	// RequestDuration has only the "model" label. This assertion would
	// panic before the codex-subscription provider matched the shared
	// chat metrics schema.
	histogram, ok := metrics.RequestDuration.WithLabelValues("gpt-5.4-mini").(prometheus.Metric)
	require.True(t, ok)
	var metric dto.Metric
	require.NoError(t, histogram.Write(&metric))
	require.NotNil(t, metric.Histogram)
	assert.Equal(t, uint64(2), metric.Histogram.GetSampleCount())
}
