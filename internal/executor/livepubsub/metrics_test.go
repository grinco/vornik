package livepubsub

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublisher_PublishedAndDroppedMetrics(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	require.NotNil(t, m)
	p := New(10, WithMetrics(m))

	// Subscribe but never drain → the bounded channel (cap 64) fills and
	// further publishes drop, exercising both counters.
	_, cancel, err := p.Subscribe("exec-1", 0)
	require.NoError(t, err)
	defer cancel()

	const n = 80
	for i := 0; i < n; i++ {
		p.Publish(context.Background(), "exec-1", "step", nil)
	}

	assert.Equal(t, float64(n), testutil.ToFloat64(m.PublishedTotal.WithLabelValues("step")),
		"every publish counted")
	assert.Greater(t, testutil.ToFloat64(m.DroppedTotal.WithLabelValues("subscriber_slow")), float64(0),
		"undrained slow subscriber drops once its 64-deep buffer fills")
}

func TestPublisher_NilMetricsIsNoOp(t *testing.T) {
	p := New(10) // no metrics option
	// Must not panic when metrics are absent.
	seq := p.Publish(context.Background(), "exec-1", "step", nil)
	assert.GreaterOrEqual(t, seq, int64(0))
}

func TestNewMetrics_NilRegistryReturnsNil(t *testing.T) {
	assert.Nil(t, NewMetrics(nil), "nil registry → nil metrics (no phantom registration)")
}
