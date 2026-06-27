package api

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestTradingEquityCheckMetrics_Set(t *testing.T) {
	// Private registry so the test is count-safe under repeated runs.
	m := NewTradingEquityCheckMetrics(prometheus.NewRegistry())

	// A drift finding: drift gauge carries the signed USD, the code's
	// anomaly gauge reads 1, and OTHER codes read 0.
	m.Set("trader", 1234.5, []string{"equity_drift"})
	if v := testutil.ToFloat64(m.drift.WithLabelValues("trader")); v != 1234.5 {
		t.Fatalf("drift gauge = %v, want 1234.5", v)
	}
	if v := testutil.ToFloat64(m.anomalies.WithLabelValues("trader", "equity_drift")); v != 1 {
		t.Fatalf("equity_drift gauge = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.anomalies.WithLabelValues("trader", "stale_snapshot")); v != 0 {
		t.Fatalf("stale_snapshot gauge = %v, want 0 (not firing)", v)
	}

	// Recovery: empty codes clears every code back to 0 and resets drift.
	m.Set("trader", 0, nil)
	if v := testutil.ToFloat64(m.drift.WithLabelValues("trader")); v != 0 {
		t.Fatalf("drift gauge after recovery = %v, want 0", v)
	}
	if v := testutil.ToFloat64(m.anomalies.WithLabelValues("trader", "equity_drift")); v != 0 {
		t.Fatalf("equity_drift after recovery = %v, want 0", v)
	}
}

func TestTradingEquityCheckMetrics_NilSafe(_ *testing.T) {
	var m *TradingEquityCheckMetrics
	m.Set("p", 1.0, []string{"equity_drift"}) // must not panic
}
