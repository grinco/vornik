package hallucination

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds Prometheus metrics for the hallucination subsystem
// (Phase 1 detectors + Phase 3 LLM-as-judge). Centralised here so
// both the Detector and JudgeRunner share the same registration
// path — the service container hands one Metrics value to both.
type Metrics struct {
	// JudgeVerdictsTotal counts every persisted judge verdict by
	// outcome. Powers the dashboard tile that visualises
	// pass/fail/abstain rate over a rolling window.
	JudgeVerdictsTotal *prometheus.CounterVec
	// JudgeConfidence buckets the verdict confidence so operators
	// can spot a judge that's persistently low-confidence (often a
	// signal that the rubric / model is mismatched to the task
	// type).
	JudgeConfidence *prometheus.HistogramVec
	// JudgeCostUSDTotal accumulates judge-LLM spend on the same
	// axes used elsewhere (project / role / model) so judge cost
	// can be aliased against worker + dispatcher cost.
	JudgeCostUSDTotal *prometheus.CounterVec
	// JudgeEvaluationsTotal counts every Evaluate() call attempt,
	// including failures — pairs with JudgeVerdictsTotal so the
	// gap (attempts minus persisted verdicts) surfaces evaluation
	// errors at metric-time without crawling logs.
	JudgeEvaluationsTotal *prometheus.CounterVec
	// SignalsTotal counts Phase 1 detector emissions by severity
	// and rule name. A spike on a specific detector identifies
	// which class of failure is suddenly more frequent.
	SignalsTotal *prometheus.CounterVec
}

// NewMetrics builds and registers all hallucination-subsystem
// metrics on the provided registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		JudgeVerdictsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "judge",
			Name:      "verdicts_total",
			Help:      "Total judge verdicts recorded, labelled by outcome.",
		}, []string{"project_id", "role", "verdict"}),
		JudgeConfidence: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "vornik",
			Subsystem: "judge",
			Name:      "confidence",
			Help:      "Confidence score of judge verdicts (0..1).",
			Buckets:   []float64{0.1, 0.25, 0.5, 0.7, 0.8, 0.9, 0.95, 1.0},
		}, []string{"project_id", "role", "verdict"}),
		JudgeCostUSDTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "judge",
			Name:      "cost_usd_total",
			Help:      "Cumulative USD spend on judge LLM calls.",
		}, []string{"project_id", "role", "model"}),
		JudgeEvaluationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "judge",
			Name:      "evaluations_total",
			Help:      "Total judge evaluation attempts, labelled by outcome (ok|error|abstain_no_config|skipped_existing).",
		}, []string{"project_id", "outcome"}),
		SignalsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "hallucination",
			Name:      "signals_total",
			Help:      "Total hallucination Phase-1 detector signals emitted.",
		}, []string{"project_id", "severity", "detector"}),
	}
	if reg != nil {
		reg.MustRegister(
			m.JudgeVerdictsTotal,
			m.JudgeConfidence,
			m.JudgeCostUSDTotal,
			m.JudgeEvaluationsTotal,
			m.SignalsTotal,
		)
	}
	return m
}

// ObserveSignals increments SignalsTotal for each entry in the
// detector's emitted slice. Nil-safe — when m is nil the call is
// a no-op, so callers can blindly invoke it without a guard.
func (m *Metrics) ObserveSignals(projectID string, signals []Signal) {
	if m == nil || len(signals) == 0 {
		return
	}
	for _, s := range signals {
		m.SignalsTotal.WithLabelValues(projectID, string(s.Severity), s.Detector).Inc()
	}
}
