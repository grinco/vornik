package quality

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics are the Prometheus gauges the quality read-model publishes. The
// `tier` label is "step" (A1) or "task" (A2); `key` is the role (A1) or
// workflow (A2). Label cardinality is bounded by swarm × role/workflow from
// the registry catalogue.
type Metrics struct {
	QualityScore        *prometheus.GaugeVec
	EffectiveCostTokens *prometheus.GaugeVec
	Sufficient          *prometheus.GaugeVec
}

// NewMetrics registers the gauges with reg (pass a fresh registry in tests).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	labels := []string{"tier", "swarm", "key"}
	return &Metrics{
		QualityScore: promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
			Name: "vornik_quality_score",
			// NOTE the tiers use different denominators: tier=step (A1) total =
			// all canonical steps with a role (incl. constrained exits); tier=task
			// (A2) total = terminal tasks only (COMPLETED/FAILED/CANCELLED). Do
			// not compare an A1 and A2 rate as the same bar.
			Help: "Composite quality rate (passing/total) per tier/swarm/role-or-workflow over the rolling window. A1(step) and A2(task) denominators differ — see godoc.",
		}, labels),
		EffectiveCostTokens: promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
			Name: "vornik_quality_effective_cost_tokens",
			Help: "Prompt tokens per quality-passing unit per tier/swarm/role-or-workflow.",
		}, labels),
		Sufficient: promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
			Name: "vornik_quality_sufficient",
			Help: "1 when the tier met its min-sample floor over the window, else 0 (score untrusted).",
		}, labels),
	}
}
