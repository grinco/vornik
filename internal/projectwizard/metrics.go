package projectwizard

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics is the Phase C operator surface for the project-setup
// wizard. Counters cover the three outcomes operators care
// about: turns served, proposals committed, sessions abandoned.
type Metrics struct {
	// TurnsTotal counts every Converse call by outcome. Labels:
	//   - "assistant_reply" — LLM responded; envelope persisted
	//   - "validation_error" — proposal didn't pass validator
	//     (reply still returned with the validation note appended)
	//   - "llm_error" — provider failure / parse error / etc
	//   - "rejected" — turn-cap / committed-session / bad-input
	//     refusals before the LLM was called
	// Operators watch the ratio of validation_error to
	// assistant_reply — a high ratio means the LLM is producing
	// shapes the registry doesn't accept, which signals a prompt
	// or model regression.
	TurnsTotal *prometheus.CounterVec

	// CommitsTotal counts successful project commits. One label
	// today: "outcome" (created / failed). Total across all
	// operators; per-operator drill-down lives on the
	// project_wizard_sessions table.
	CommitsTotal *prometheus.CounterVec

	// AbandonedTotal increments when a session is detected as
	// abandoned (no turn in N days, no commit). The retention
	// sweeper does the detection; the counter ticks per row it
	// purges so operators can chart "how many drafts did we
	// throw away last week".
	AbandonedTotal prometheus.Counter
}

// NewMetrics constructs the wizard metrics surface.
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &Metrics{
		TurnsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "project_wizard",
				Name:      "turns_total",
				Help:      "Wizard converse turns, by outcome.",
			},
			[]string{"outcome"},
		),
		CommitsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "project_wizard",
				Name:      "commits_total",
				Help:      "Wizard commits, by outcome.",
			},
			[]string{"outcome"},
		),
		AbandonedTotal: promauto.With(registerer).NewCounter(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "project_wizard",
				Name:      "abandoned_total",
				Help:      "Wizard sessions purged by the retention sweep without ever being committed.",
			},
		),
	}
}

// Turn outcome labels — the named-const pattern keeps the label
// set auditable in one place.
const (
	turnOutcomeAssistantReply  = "assistant_reply"
	turnOutcomeValidationError = "validation_error"
	turnOutcomeLLMError        = "llm_error"
	turnOutcomeRejected        = "rejected"
)

const (
	commitOutcomeCreated   = "created"
	commitOutcomeFailed    = "failed"
	commitOutcomeCancelled = "cancelled"
)

// recordTurn bumps the turn counter for the given outcome. Nil-
// safe so production paths that don't wire metrics stay quiet.
func (m *Metrics) recordTurn(outcome string) {
	if m == nil || m.TurnsTotal == nil {
		return
	}
	m.TurnsTotal.WithLabelValues(outcome).Inc()
}

// recordCommit bumps the commit counter. Nil-safe.
func (m *Metrics) recordCommit(outcome string) {
	if m == nil || m.CommitsTotal == nil {
		return
	}
	m.CommitsTotal.WithLabelValues(outcome).Inc()
}
