package api

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// equityCheckCodes is the closed set of finding codes the cross-check can
// emit (mirrors internal/trading/equitycheck). Listed here so Set can clear
// a code's gauge to 0 once it stops firing — without the full set we could
// only ever set 1s and a recovered series would keep a stale 1.
var equityCheckCodes = []string{"no_baseline", "stale_snapshot", "equity_drift"}

// TradingEquityCheckMetrics holds the gauges the equity cross-checker sets
// on each tick:
//   - vornik_trading_equity_drift_usd{project}: signed (live - recorded)
//     equity in USD, set every check so a flat line confirms agreement.
//   - vornik_trading_equity_crosscheck_anomalies{project,code}: 1 while a
//     finding fires, 0 once it clears.
type TradingEquityCheckMetrics struct {
	drift     *prometheus.GaugeVec
	anomalies *prometheus.GaugeVec
}

// NewTradingEquityCheckMetrics registers the gauges on registerer. Build it
// ONLY on the served (observability) registry — never 1's DefaultRegisterer
// — or the gauges vanish when /metrics switches registries (the two-pass
// trap documented in container_http.go).
func NewTradingEquityCheckMetrics(registerer prometheus.Registerer) *TradingEquityCheckMetrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &TradingEquityCheckMetrics{
		drift: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Name:      "trading_equity_drift_usd",
				Help:      "Signed (live broker - latest recorded snapshot) equity in USD, per project. Set by the trading equity cross-checker each tick (near 0 = in agreement).",
			},
			[]string{"project"},
		),
		anomalies: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Name:      "trading_equity_crosscheck_anomalies",
				Help:      "Equity cross-check finding state by project and code (1 = firing, 0 = clear). Codes: no_baseline, stale_snapshot, equity_drift.",
			},
			[]string{"project", "code"},
		),
	}
}

// Set records one project's cross-check outcome: the signed drift and the
// firing/clear state of every known code. Nil-safe.
func (m *TradingEquityCheckMetrics) Set(projectID string, driftUSD float64, codes []string) {
	if m == nil {
		return
	}
	if m.drift != nil {
		m.drift.WithLabelValues(projectID).Set(driftUSD)
	}
	if m.anomalies == nil {
		return
	}
	firing := make(map[string]bool, len(codes))
	for _, c := range codes {
		firing[c] = true
	}
	for _, code := range equityCheckCodes {
		v := 0.0
		if firing[code] {
			v = 1.0
		}
		m.anomalies.WithLabelValues(projectID, code).Set(v)
	}
}
