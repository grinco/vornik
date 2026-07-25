package controlplane

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// CanaryMetrics are the Prometheus counters the cost/quality canary guard
// publishes (design 2026-07-24 §I8, §6). Nil-safe: a nil *CanaryMetrics (tests /
// no observability registry) makes every Inc a no-op, matching the other
// controlplane/quality metrics' nil-guard pattern.
type CanaryMetrics struct {
	// Outcomes counts terminal (and opened) canary transitions by outcome:
	// opened|passed|regressed|insufficient_data|superseded.
	Outcomes *prometheus.CounterVec
	// CoverageGap counts APPLIED cost-tuning proposals that yielded NO canary
	// because their ProposedBy/Evidence.change.kind didn't match — a silent
	// stringly-typed-contract drift made visible (design §I8).
	CoverageGap prometheus.Counter
}

// NewCanaryMetrics registers the canary counters with reg (pass a fresh registry
// in tests). Returns nil when reg is nil so callers can pass it straight through.
func NewCanaryMetrics(reg prometheus.Registerer) *CanaryMetrics {
	if reg == nil {
		return nil
	}
	// Namespace/Subsystem/Name so the exposed names are
	// vornik_cost_tuning_canary_total and
	// vornik_cost_tuning_canary_coverage_gap_total (design §6/§I8).
	return &CanaryMetrics{
		Outcomes: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "cost_tuning",
			Name: "canary_total",
			Help: "Cost/quality canary transitions by outcome (opened|passed|regressed|insufficient_data|superseded).",
		}, []string{"outcome"}),
		CoverageGap: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "cost_tuning",
			Name: "canary_coverage_gap_total",
			Help: "APPLIED cost-tuning proposals that opened no canary (unmatched ProposedBy/Evidence.change.kind).",
		}),
	}
}

// incOutcome bumps the outcome counter, nil-safe.
func (m *CanaryMetrics) incOutcome(outcome string) {
	if m == nil || m.Outcomes == nil {
		return
	}
	m.Outcomes.WithLabelValues(outcome).Inc()
}

// incCoverageGap bumps the coverage-gap counter, nil-safe.
func (m *CanaryMetrics) incCoverageGap() {
	if m == nil || m.CoverageGap == nil {
		return
	}
	m.CoverageGap.Inc()
}
