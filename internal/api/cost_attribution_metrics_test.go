package api

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRecordCostAttribution_PerSource pins the counter shape
// the per-project-API-key migration KPI dashboard reads.
// One series per AttributionSource so operators can compute
// (key-bound / total) as the trustworthy-row fraction.
func TestRecordCostAttribution_PerSource(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewAPIMetrics(reg)

	m.RecordCostAttribution(AttributionFromDBKey)
	m.RecordCostAttribution(AttributionFromDBKey)
	m.RecordCostAttribution(AttributionFromDBKey)
	m.RecordCostAttribution(AttributionFromHeader)
	m.RecordCostAttribution(AttributionFromFallback)
	m.RecordCostAttribution(AttributionAnonymous)

	cases := []struct {
		source AttributionSource
		want   float64
	}{
		{AttributionFromDBKey, 3},
		{AttributionFromHeader, 1},
		{AttributionFromFallback, 1},
		{AttributionAnonymous, 1},
	}
	for _, tc := range cases {
		got := testutil.ToFloat64(m.CostAttributionTotal.WithLabelValues(string(tc.source)))
		if got != tc.want {
			t.Errorf("source=%q count=%v, want %v", tc.source, got, tc.want)
		}
	}
}

// TestRecordCostAttribution_NilMetricsSafe — call sites should
// stay simple (`s.apiMetrics.RecordCostAttribution(...)`)
// without nil-checking; the helper itself guards.
func TestRecordCostAttribution_NilMetricsSafe(t *testing.T) {
	var m *APIMetrics // nil
	m.RecordCostAttribution(AttributionFromDBKey)
	// Reaching here without panic is the assertion.
}
