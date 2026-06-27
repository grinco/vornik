package chat

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewMetrics_RegistersCacheSeriesTwiceWithoutPanic — each NewMetrics
// against a fresh registry must succeed (no duplicate-registration
// panic). Guards the repeated-construction / repeated-test-run path.
func TestNewMetrics_RegistersCacheSeriesTwiceWithoutPanic(t *testing.T) {
	assert.NotPanics(t, func() {
		_ = NewMetrics(prometheus.NewRegistry())
		_ = NewMetrics(prometheus.NewRegistry())
	})
}

// TestMetrics_ObserveCacheUsage_CountsTokensRatioAndDollars exercises the
// N8 observation path: the creation/read counters take the token counts
// (labelled model/role/source), the dollars-saved counter accumulates,
// and the hit-ratio gauge recomputes read/(read+creation).
func TestMetrics_ObserveCacheUsage_CountsTokensRatioAndDollars(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	require.NotNil(t, m.CacheCreationTokensTotal)
	require.NotNil(t, m.CacheReadTokensTotal)
	require.NotNil(t, m.CacheHitRatio)
	require.NotNil(t, m.CacheDollarsSavedTotal)

	// First call: 100 creation, 300 read → ratio 0.75.
	m.ObserveCacheUsage("claude", "external_api", "external_api", 100, 300, 0.42)
	// Second call same labels: +0 creation, +100 read → totals 100/400 → 0.8.
	m.ObserveCacheUsage("claude", "external_api", "external_api", 0, 100, 0.10)

	assert.Equal(t, 100.0, testutil.ToFloat64(
		m.CacheCreationTokensTotal.WithLabelValues("claude", "external_api", "external_api")))
	assert.Equal(t, 400.0, testutil.ToFloat64(
		m.CacheReadTokensTotal.WithLabelValues("claude", "external_api", "external_api")))
	assert.InDelta(t, 0.52, testutil.ToFloat64(
		m.CacheDollarsSavedTotal.WithLabelValues("claude", "external_api")), 1e-9)
	assert.InDelta(t, 0.8, testutil.ToFloat64(
		m.CacheHitRatio.WithLabelValues("claude", "external_api")), 1e-9)
}

// TestMetrics_ObserveCacheUsage_EmptyLabelsAndZeroTokens — empty labels
// fall back to "unknown"; a fully-zero call records nothing (no series
// created) so cache-less providers don't pollute the metric.
func TestMetrics_ObserveCacheUsage_EmptyLabelsAndZeroTokens(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())

	m.ObserveCacheUsage("", "", "", 50, 0, 0)
	assert.Equal(t, 50.0, testutil.ToFloat64(
		m.CacheCreationTokensTotal.WithLabelValues("unknown", "unknown", "unknown")))

	// All-zero: nothing observed.
	before := testutil.CollectAndCount(m.CacheReadTokensTotal)
	m.ObserveCacheUsage("m", "r", "s", 0, 0, 0)
	assert.Equal(t, before, testutil.CollectAndCount(m.CacheReadTokensTotal))
}

// TestMetrics_ObserveCacheUsage_NilReceiverSafe — the api handlers call
// this on every recorded usage row including when chat metrics were
// never wired.
func TestMetrics_ObserveCacheUsage_NilReceiverSafe(t *testing.T) {
	var m *Metrics
	assert.NotPanics(t, func() {
		m.ObserveCacheUsage("m", "r", "s", 10, 20, 0.1)
	})
}
