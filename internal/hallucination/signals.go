// Package hallucination implements claim-grounding detection for
// agent and dispatcher outputs. It cross-references assertive
// claims (URLs visited, files written, tasks/projects/artifacts
// referenced, numeric outcomes) against a step-scoped
// "grounding context" assembled from tool_audit_log + the
// artifact catalogue. Unsupported claims surface as Signals,
// which the executor / dispatcher use to fail-and-retry the
// producing step.
//
// Layered on top of `executor.verifyClaimedFiles` which handles
// the file-level "claimed but not on disk" case. This package
// catches the textual claims a model makes about the world
// outside the filesystem — URLs it says it fetched, IDs it
// says it created, counts it says it found.
package hallucination

import "time"

// Severity grades how serious a finding is. The executor blocks
// only on `High`; lower severities surface in the UI but don't
// fail the step. The threshold is deliberate: false positives at
// `High` cause retry storms, so the rules tuned to High-level
// output should be the ones with very low false-positive rates
// (URL claimed but no web_fetch in audit; task ID claimed but
// no task in DB).
type Severity string

const (
	// SeverityInfo — observation worth recording but no action.
	// Used for soft signals like "agent text mentioned a URL,
	// the URL was fetched, nothing further to flag" — the
	// detector publishes these so the UI can correlate prose
	// with audit entries even when nothing's wrong.
	SeverityInfo Severity = "info"

	// SeverityWarn — likely-but-not-certain claim drift. UI
	// surfaces it; executor doesn't block. Numeric mismatches
	// ("agent said 12 listings, 9 in artifacts") sit here
	// because tokenization noise can make that ambiguous.
	SeverityWarn Severity = "warn"

	// SeverityHigh — confident hallucination. Step fails so
	// the scheduler's existing retry path picks it up. Reserved
	// for grounded-by-construction claims: a URL the model
	// quoted that doesn't appear in any tool input/output, a
	// task_id that doesn't exist, a project_id outside the
	// registry. False positives here trigger expensive retries,
	// so rules graduate to High only after their soak window.
	SeverityHigh Severity = "high"
)

// Block reports whether this severity should fail the producing
// step. Centralised so the executor and dispatcher share the
// definition (and a future severity bump only changes one line).
func (s Severity) Block() bool { return s == SeverityHigh }

// Signal is one detector finding. Persisted JSONB on the step
// outcome row (executor path) and emitted as a warn-level log +
// audit event on the dispatcher path. The shape is intentionally
// small so the DB column doesn't bloat — the full sentence the
// claim came from is retained for UI context but truncated.
type Signal struct {
	// Detector names the rule that fired. Used for per-rule
	// metrics ("rule:url_not_fetched fired 14× this week"). One
	// detector can produce multiple signals on one output.
	Detector string `json:"detector"`

	// Severity is one of Info/Warn/High; see Severity.
	Severity Severity `json:"severity"`

	// ClaimType is a coarse category — "url", "path", "task_id",
	// "project_id", "artifact_name", "numeric". Lets the UI
	// group findings without re-parsing Detail.
	ClaimType string `json:"claim_type"`

	// ClaimValue is the literal value the model stated (the
	// URL, path, ID, etc.). Truncated to 256 bytes — these are
	// short by construction, but we cap to keep the JSONB row
	// small.
	ClaimValue string `json:"claim_value"`

	// Sentence is the surrounding ~200-char context so the UI
	// can show the model's actual prose. Truncated server-side.
	// Storing it makes the signal self-contained for the audit
	// view; readers don't have to fetch result.json to see why
	// the rule fired.
	Sentence string `json:"sentence,omitempty"`

	// EvidenceSearched describes the grounding sources the
	// detector consulted before emitting the signal. Helps an
	// operator decide whether the rule's negative space was
	// actually exhaustive — e.g. "tool_audit (4 entries),
	// artifacts (12)" is much more credible than
	// "tool_audit (0)" which usually means the audit lookup
	// failed and the signal is noise.
	EvidenceSearched string `json:"evidence_searched,omitempty"`

	// Detail is the human-readable reason. Free text; UI
	// renders it as the headline of the signal card.
	Detail string `json:"detail"`

	// RecordedAt timestamps the detection. Useful when the
	// detector is run async (Phase 3 judge); for synchronous
	// Phase 1 it's effectively step-end time.
	RecordedAt time.Time `json:"recorded_at"`
}

// HighestSeverity returns the strongest severity present in
// signals, or empty string if signals is empty. Centralised so
// callers needing "should I block?" logic share semantics.
func HighestSeverity(signals []Signal) Severity {
	rank := map[Severity]int{SeverityInfo: 1, SeverityWarn: 2, SeverityHigh: 3}
	var best Severity
	bestRank := 0
	for _, s := range signals {
		if r := rank[s.Severity]; r > bestRank {
			bestRank = r
			best = s.Severity
		}
	}
	return best
}

// truncate is a small helper for keeping ClaimValue / Sentence
// bounded when they get persisted. Exported indirectly via
// constructors below.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// MaxClaimValueBytes / MaxSentenceBytes bound the JSONB blob.
// One signal stays under ~600 bytes worst-case; a step with
// dozens of findings still fits inside the row limit.
const (
	MaxClaimValueBytes = 256
	MaxSentenceBytes   = 240
)

// NewSignal is the canonical constructor — applies the
// truncation invariants and stamps RecordedAt. Callers should
// always go through it rather than constructing the struct
// inline.
func NewSignal(detector string, sev Severity, claimType, claimValue, sentence, evidence, detail string) Signal {
	return Signal{
		Detector:         detector,
		Severity:         sev,
		ClaimType:        claimType,
		ClaimValue:       truncate(claimValue, MaxClaimValueBytes),
		Sentence:         truncate(sentence, MaxSentenceBytes),
		EvidenceSearched: evidence,
		Detail:           detail,
		RecordedAt:       time.Now().UTC(),
	}
}
