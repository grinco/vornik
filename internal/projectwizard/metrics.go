package projectwizard

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics is the Phase C operator surface for the project-setup
// wizard. Counters cover the three outcomes operators care
// about: turns served, proposals committed, sessions abandoned; the
// composer (task 1.1b) extends the family with bundle-validation and
// guardrail counters (design §5.8).
type Metrics struct {
	// TurnsTotal counts every Converse call by tier + outcome. Tier
	// labels: "1"/"2"/"3" (the final, post-arbitration tier actually
	// persisted/returned — a tier-3 emission downgraded by the anchor
	// gate is labelled by whatever tier it was downgraded TO) or "n/a"
	// for a turn rejected before an envelope was ever parsed
	// (turn-cap, session-cap, LLM transport error). Outcome labels:
	//   - "assistant_reply" — LLM responded; envelope persisted
	//   - "validation_error" — proposal didn't pass validator
	//     (reply still returned with the validation note appended)
	//   - "llm_error" — provider failure / parse error / etc
	//   - "rejected" — turn-cap / committed-session / bad-input
	//     refusals before the LLM was called
	//   - "fallback" — the tier-3 consecutive-validation-failure
	//     circuit breaker tripped (task 1.3, design §7 row 1): the
	//     composer stopped retrying tier-3 and downgraded to a tier-2
	//     nearest-template suggestion. Deliberately folded into this
	//     existing metric (rather than a new vornik_composer_*_total
	//     counter) since it's a turn outcome like the others above —
	//     operators can chart fallback-rate the same way they already
	//     chart validation_error-rate.
	// Operators watch the ratio of validation_error to
	// assistant_reply — a high ratio means the LLM is producing
	// shapes the registry doesn't accept, which signals a prompt
	// or model regression; the tier split additionally surfaces how
	// often (and how expensively, via tier=3) that's happening.
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

	// BundlesValidatedTotal counts every tier-3 staged-registry
	// validation run, by result ("valid" / "invalid"). Design §5.8.
	BundlesValidatedTotal *prometheus.CounterVec

	// GuardrailHitsTotal counts every guardrail violation the
	// composer's deterministic pass finds, by rule (the
	// guardrailRule* constants in guardrail.go). Design §5.8 — "every
	// guardrail correction and validation bounce is visible", this is
	// the aggregate operator-facing counterpart of that transcript
	// visibility.
	GuardrailHitsTotal *prometheus.CounterVec

	// ComposerCommitsTotal counts every attempt to activate a tier-3
	// bundle through the journaled commit path (task 1.2b, design
	// §5.6/§5.8: vornik_composer_commits_total{tier}), by tier ("3" —
	// the only tier this path handles today; labelled rather than
	// hard-coded so a future tier-2-composed commit could share the
	// counter) and result ("created"/"failed") — the composer-specific
	// counterpart of the pre-existing project_wizard_commits_total,
	// which the legacy/composition commit paths keep using unchanged.
	ComposerCommitsTotal *prometheus.CounterVec
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
				Help:      "Wizard converse turns, by tier and outcome.",
			},
			[]string{"tier", "outcome"},
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
		BundlesValidatedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "composer",
				Name:      "bundles_validated_total",
				Help:      "Tier-3 composed bundles run through staged registry validation, by result.",
			},
			[]string{"result"},
		),
		GuardrailHitsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "composer",
				Name:      "guardrail_hits_total",
				Help:      "Composer guardrail violations found during the deterministic guardrail pass, by rule.",
			},
			[]string{"rule"},
		),
		ComposerCommitsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "composer",
				Name:      "commits_total",
				Help:      "Tier-3 composed bundle activations through the journaled commit path, by tier and result.",
			},
			[]string{"tier", "result"},
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
	// turnOutcomeFallback: the consecutive-validation-failure circuit
	// breaker tripped (task 1.3, design §7 row 1) — see the
	// TurnsTotal doc comment above.
	turnOutcomeFallback = "fallback"
)

const (
	commitOutcomeCreated   = "created"
	commitOutcomeFailed    = "failed"
	commitOutcomeCancelled = "cancelled"
)

// recordTurn bumps the turn counter for the given tier + outcome.
// Nil-safe so production paths that don't wire metrics stay quiet.
func (m *Metrics) recordTurn(tier, outcome string) {
	if m == nil || m.TurnsTotal == nil {
		return
	}
	if tier == "" {
		tier = tierLabelUnknown
	}
	m.TurnsTotal.WithLabelValues(tier, outcome).Inc()
}

// bundleValidationOutcome labels — vornik_composer_bundles_validated_total{result}.
const (
	bundleValidationResultValid   = "valid"
	bundleValidationResultInvalid = "invalid"
)

// recordBundleValidated bumps the staged-validation counter. Nil-safe.
func (m *Metrics) recordBundleValidated(result string) {
	if m == nil || m.BundlesValidatedTotal == nil {
		return
	}
	m.BundlesValidatedTotal.WithLabelValues(result).Inc()
}

// recordGuardrailHit bumps the guardrail-violation counter once per
// rule name. Nil-safe.
func (m *Metrics) recordGuardrailHit(rule string) {
	if m == nil || m.GuardrailHitsTotal == nil {
		return
	}
	m.GuardrailHitsTotal.WithLabelValues(rule).Inc()
}

// recordCommit bumps the commit counter. Nil-safe.
func (m *Metrics) recordCommit(outcome string) {
	if m == nil || m.CommitsTotal == nil {
		return
	}
	m.CommitsTotal.WithLabelValues(outcome).Inc()
}

// composerCommitResult labels — vornik_composer_commits_total{tier,result}.
const (
	composerCommitResultCreated = "created"
	composerCommitResultFailed  = "failed"
)

// composerCommitTier3 is the only tier this metric's caller labels
// today (bundle_commit.go's journaled path handles tier-3 bundles
// exclusively).
const composerCommitTier3 = "3"

// recordComposerCommit bumps vornik_composer_commits_total{tier,result}.
// Nil-safe.
//
// journaled path only ever handles tier-3 bundles) but the metric's
// contract is {tier,result} per design §5.8 — kept as a real parameter
// rather than hard-coded so a future tier-2-composed commit can share
// this counter without an API change.
//
//nolint:unparam // tier is always composerCommitTier3 today (the
func (m *Metrics) recordComposerCommit(tier, result string) {
	if m == nil || m.ComposerCommitsTotal == nil {
		return
	}
	m.ComposerCommitsTotal.WithLabelValues(tier, result).Inc()
}
