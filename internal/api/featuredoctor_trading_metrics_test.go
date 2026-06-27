package api

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestTradingSeriesMetrics_Set(t *testing.T) {
	// Use a private registry (not the promauto default) so the test is
	// count-safe under repeated runs.
	m := NewTradingSeriesMetrics(prometheus.NewRegistry())
	m.Set("trader", "stale", 2)
	m.Set("trader", "stale", 0) // recovery clears

	got := testutil.ToFloat64(m.anomalies.WithLabelValues("trader", "stale"))
	if got != 0 {
		t.Fatalf("after Set 0 the gauge should read 0, got %v", got)
	}
	m.Set("trader", "cadence_gap", 3)
	if v := testutil.ToFloat64(m.anomalies.WithLabelValues("trader", "cadence_gap")); v != 3 {
		t.Fatalf("gauge should read 3, got %v", v)
	}
}

func TestTradingSeriesMetrics_NilSafe(_ *testing.T) {
	var m *TradingSeriesMetrics
	m.Set("p", "c", 1) // must not panic
}
