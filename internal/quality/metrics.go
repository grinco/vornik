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

// ExecutionScoreMetrics expose publication completeness without per-execution
// labels. kind/status are a closed vocabulary, so label cardinality stays
// bounded as the execution ledger grows.
type ExecutionScoreMetrics struct {
	WritesTotal          *prometheus.CounterVec
	WriteFailuresTotal   prometheus.Counter
	PublicationPending   prometheus.Gauge
	OldestPendingSeconds prometheus.Gauge
}

// NewExecutionScoreMetrics registers bounded publication-health metrics.
func NewExecutionScoreMetrics(reg prometheus.Registerer) *ExecutionScoreMetrics {
	return &ExecutionScoreMetrics{
		WritesTotal: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "vornik_execution_quality_score_writes_total",
			Help: "Successful execution quality score upserts by scoring kind and verdict status.",
		}, []string{"kind", "status"}),
		WriteFailuresTotal: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "vornik_execution_quality_score_write_failures_total",
			Help: "Execution quality score upserts that failed and remain pending reconciliation.",
		}),
		PublicationPending: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Name: "vornik_execution_quality_score_publication_pending",
			Help: "Terminal executions that do not yet have a durable execution quality score row.",
		}),
		OldestPendingSeconds: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Name: "vornik_execution_quality_score_oldest_pending_seconds",
			Help: "Age in seconds of the oldest terminal execution still awaiting a quality score row.",
		}),
	}
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
