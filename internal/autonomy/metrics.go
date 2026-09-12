package autonomy

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds Prometheus metrics for the autonomy manager.
type Metrics struct {
	EvaluationsTotal *prometheus.CounterVec
	TasksCreated     *prometheus.CounterVec
	NoActionTotal    *prometheus.CounterVec
	ErrorsTotal      *prometheus.CounterVec
	EvalDuration     *prometheus.HistogramVec
	// EvaluationOutcomesTotal counts ticks by the outcome written to
	// autonomy_evaluations. Separate series from EvaluationsTotal on
	// purpose: adding an `outcome` label to a live series changes its
	// identity and breaks existing recording rules. The two divide
	// cleanly — EvaluationsTotal answers "is the loop ticking",
	// this answers "what are the ticks deciding". A project stuck at
	// one outcome for a whole window is the monotony class of design
	// §2, and was invisible before this series existed.
	EvaluationOutcomesTotal *prometheus.CounterVec
	// FeedLagSeconds is how long ago each declared feed last ran.
	// Emitted only for projects declaring autonomy.feeds, so
	// cardinality is bounded by declared feeds and a project that
	// declares none adds no series at all.
	FeedLagSeconds *prometheus.GaugeVec
	// FeedCadenceBreachTotal counts cadence violations by direction:
	// "slow" (overdue) or "fast" (re-run inside half the cadence).
	// Both directions matter — the 2026-09-10 window ran one feed 4.9x
	// slow and another 14x fast, and only one of those looks like
	// neglect.
	FeedCadenceBreachTotal *prometheus.CounterVec
}

// NewMetrics creates and registers autonomy metrics.
func NewMetrics(reg *prometheus.Registry) *Metrics {
	m := &Metrics{
		EvaluationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "evaluations_total",
			Help:      "Total autonomous evaluations run.",
		}, []string{"project_id"}),
		TasksCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "tasks_created_total",
			Help:      "Total tasks created by autonomous lead.",
		}, []string{"project_id"}),
		NoActionTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "no_action_total",
			Help:      "Total evaluations where the lead decided no action was needed.",
		}, []string{"project_id"}),
		ErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "errors_total",
			Help:      "Total autonomous evaluation errors.",
		}, []string{"project_id"}),
		EvalDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "evaluation_duration_seconds",
			Help:      "Duration of each autonomous evaluation.",
			Buckets:   []float64{1, 5, 10, 30, 60, 120, 300},
		}, []string{"project_id"}),
		EvaluationOutcomesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "evaluation_outcomes_total",
			Help:      "Autonomy ticks by recorded outcome (CREATED, NO_ACTION, RATE_LIMITED, ...).",
		}, []string{"project_id", "outcome"}),
		FeedLagSeconds: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "feed_lag_seconds",
			Help:      "Seconds since a declared autonomy feed last ran.",
		}, []string{"project_id", "slug"}),
		FeedCadenceBreachTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "feed_cadence_breach_total",
			Help:      "Declared-feed cadence violations, by direction (slow|fast).",
		}, []string{"project_id", "slug", "direction"}),
	}
	reg.MustRegister(m.EvaluationsTotal, m.TasksCreated, m.NoActionTotal, m.ErrorsTotal, m.EvalDuration,
		m.EvaluationOutcomesTotal, m.FeedLagSeconds, m.FeedCadenceBreachTotal)
	return m
}
