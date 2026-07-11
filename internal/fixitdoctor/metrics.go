package fixitdoctor

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics is the operator surface for the Fix-It Doctor repair chat
// (fix-it-doctor-design.md §5.2/§8). Mirrors projectwizard.Metrics'
// shape and nil-safety conventions.
type Metrics struct {
	// SessionsTotal counts session lifecycle events, by failure_kind +
	// outcome. Outcomes: "opened" (a new session was created),
	// "rejected" (creation refused — session cap / budget), "resolved"
	// (a Resolved:true turn was observed), "closed" (the session was
	// closed — operator-driven or cascade-on-missing-object).
	SessionsTotal *prometheus.CounterVec

	// GuardrailHitsTotal counts every proposed action the server
	// dropped from an envelope before rendering it, by reason
	// (GuardrailReason* — unknown_kind / params_invalid). Operators
	// watch this to catch a model regression that starts proposing
	// out-of-vocabulary or malformed actions.
	GuardrailHitsTotal *prometheus.CounterVec

	// ActionsTotal counts every dispatcher Apply call, by kind + result
	// (ActionResult* — applied/rejected/failed, fix-it-doctor-design.md
	// §5.6/§8, task 3.3). This is the action-DISPATCH counter, distinct
	// from GuardrailHitsTotal which counts proposals dropped before the
	// user ever sees them; ActionsTotal counts actions the user actually
	// clicked Apply on.
	ActionsTotal *prometheus.CounterVec
}

// Session outcome labels — vornik_fixit_sessions_total{outcome=...}.
const (
	SessionOutcomeOpened   = "opened"
	SessionOutcomeRejected = "rejected"
	SessionOutcomeResolved = "resolved"
	SessionOutcomeClosed   = "closed"
)

// NewMetrics constructs the Fix-It Doctor metrics surface, registering
// vornik_fixit_sessions_total, vornik_fixit_guardrail_hits_total, and
// vornik_fixit_actions_total (task 3.3's action-dispatcher metric).
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &Metrics{
		SessionsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "fixit",
				Name:      "sessions_total",
				Help:      "Fix-It Doctor repair-chat sessions, by failure kind and lifecycle outcome.",
			},
			[]string{"failure_kind", "outcome"},
		),
		GuardrailHitsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "fixit",
				Name:      "guardrail_hits_total",
				Help:      "Fix-It Doctor proposed actions dropped by the server guardrail, by reason.",
			},
			[]string{"reason"},
		),
		ActionsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "fixit",
				Name:      "actions_total",
				Help:      "Fix-It Doctor dispatched actions, by action kind and result (applied|rejected|failed).",
			},
			[]string{"kind", "result"},
		),
	}
}

// recordSession bumps the session counter. Nil-safe.
func (m *Metrics) recordSession(failureKind, outcome string) {
	if m == nil || m.SessionsTotal == nil {
		return
	}
	m.SessionsTotal.WithLabelValues(failureKind, outcome).Inc()
}

// recordGuardrailHit bumps the guardrail counter. Nil-safe.
func (m *Metrics) recordGuardrailHit(reason string) {
	if m == nil || m.GuardrailHitsTotal == nil {
		return
	}
	m.GuardrailHitsTotal.WithLabelValues(reason).Inc()
}

// recordAction bumps the action-dispatch counter. Nil-safe.
func (m *Metrics) recordAction(kind, result string) {
	if m == nil || m.ActionsTotal == nil {
		return
	}
	m.ActionsTotal.WithLabelValues(kind, result).Inc()
}
