package api

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// TradingSeriesMetrics holds the gauge the trading-series feature-doctor probe
// sets on each run. vornik_trading_series_anomalies{project,code} carries the
// per-project anomaly count for each finding code; the probe sets 0 for codes
// that aren't firing so a recovered series clears its prior value.
type TradingSeriesMetrics struct {
	anomalies *prometheus.GaugeVec
}

// NewTradingSeriesMetrics registers the anomaly gauge on registerer. Build it
// ONLY on the served (observability) registry — never pass 1's
// DefaultRegisterer — or the gauge becomes invisible once /metrics switches
// registries (the two-pass trap documented in container_http.go).
func NewTradingSeriesMetrics(registerer prometheus.Registerer) *TradingSeriesMetrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &TradingSeriesMetrics{
		anomalies: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Name:      "trading_series_anomalies",
				Help:      "Trading equity time-series anomaly count by project and finding code (0 = healthy). Set by the trading-series feature-doctor probe.",
			},
			[]string{"project", "code"},
		),
	}
}

// Set records the anomaly count for one (project, code). Nil-safe.
func (m *TradingSeriesMetrics) Set(projectID, code string, count int) {
	if m == nil || m.anomalies == nil {
		return
	}
	m.anomalies.WithLabelValues(projectID, code).Set(float64(count))
}
