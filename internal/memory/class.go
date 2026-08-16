package memory

import "time"

// ContentClass enumerates the vornik-wide built-in classes for
// memory chunks. Adding a class here is a vornik-side change;
// projects extend the catalog (never redefine) via Config — see
// https://docs.vornik.io §12 resolved
// decision #3.
type ContentClass string

const (
	ClassResearch      ContentClass = "research"
	ClassSpec          ContentClass = "spec"
	ClassDecision      ContentClass = "decision"
	ClassCommitMsg     ContentClass = "commit_msg"
	ClassDiagnostic    ContentClass = "diagnostic"
	ClassExternalFetch ContentClass = "external_fetch"
	ClassSummary       ContentClass = "summary"
	ClassUnclassified  ContentClass = "unclassified"
	// ClassCompanionNote tags content deposited by a host-LLM
	// companion client through the `remember` MCP tool (LLD 22).
	// Distinct from ClassUnclassified ("classifier hasn't run yet")
	// — companion-origin chunks are deliberate human deposits,
	// shorter-lived than swarm-produced artifacts, lower default
	// confidence than agent output.
	ClassCompanionNote ContentClass = "companion_note"
	// ClassChatMemory tags content deposited from a chat channel
	// through the dispatcher's `remember` tool (chat memory-write
	// design, slices 4-5). A human typed it in Slack/Telegram/etc.
	// and asked the assistant to keep it — a deliberate deposit, like
	// a companion note, but originating in an operator conversation
	// rather than a host-LLM companion client. Own class so its
	// retention (90d TTL) and retrieval weight (0.3 confidence) move
	// independently of research/decision chunks, per §5.5.
	ClassChatMemory ContentClass = "chat_memory"
)

// ClassPolicy is the per-class behaviour bundle: default
// confidence, TTL, role-of-record shortcut. Operators tune via
// project YAML; the values here are the vornik-shipped baseline.
type ClassPolicy struct {
	// DefaultConfidence stamped on chunks of this class when no
	// validator overrides. 0..1.
	DefaultConfidence float32
	// TTL after which chunks of this class expire. Zero means
	// never expire.
	TTL time.Duration
	// RoleOfRecord — when the producer role's swarm config has
	// this set true, chunks of this class land at
	// validation_status='verified' immediately, bypassing the
	// LLM validator. See design §6 (Pillar 2.9 MDM "system of
	// record" pattern).
	RoleOfRecordEligible bool
}

// DefaultClassPolicies is the vornik-wide policy table. Each class
// gets a rationale comment so operators reading this file
// understand why the defaults are what they are.
var DefaultClassPolicies = map[ContentClass]ClassPolicy{
	// Research outputs from scout/researcher roles. Mid-confidence
	// because research often summarises external sources without
	// strong verification. 90-day TTL: research goes stale slowly
	// but goes stale.
	ClassResearch: {
		DefaultConfidence:    0.6,
		TTL:                  90 * 24 * time.Hour,
		RoleOfRecordEligible: false,
	},
	// Specs from analyst role + design docs. High confidence
	// because they're the authoritative product description. No
	// TTL — specs supersede explicitly via supersedes_id, not by
	// time.
	ClassSpec: {
		DefaultConfidence:    0.8,
		TTL:                  0,
		RoleOfRecordEligible: true,
	},
	// Decisions from architect/reviewer rulings. Highest
	// confidence; never expire.
	ClassDecision: {
		DefaultConfidence:    0.9,
		TTL:                  0,
		RoleOfRecordEligible: true,
	},
	// Commit messages from coder. Low confidence (often terse,
	// often wrong about scope). 30-day TTL because their
	// usefulness drops off quickly — what mattered last week is
	// usually superseded by this week.
	ClassCommitMsg: {
		DefaultConfidence:    0.4,
		TTL:                  30 * 24 * time.Hour,
		RoleOfRecordEligible: false,
	},
	// Diagnostic dumps from failed steps. Low confidence + 7-day
	// TTL: useful for debugging the last week's failures, not
	// useful as long-term project knowledge.
	ClassDiagnostic: {
		DefaultConfidence:    0.2,
		TTL:                  7 * 24 * time.Hour,
		RoleOfRecordEligible: false,
	},
	// External fetches from scraper/web_fetch tools. Low
	// confidence (we don't control the source) + 14-day TTL.
	ClassExternalFetch: {
		DefaultConfidence:    0.3,
		TTL:                  14 * 24 * time.Hour,
		RoleOfRecordEligible: false,
	},
	// Summaries — autonomy-lead-generated digests. Mid confidence;
	// 30-day TTL.
	ClassSummary: {
		DefaultConfidence:    0.5,
		TTL:                  30 * 24 * time.Hour,
		RoleOfRecordEligible: false,
	},
	// Catch-all when classification fails or producer role is
	// unrecognised. Mid-low confidence + 30-day TTL — short enough
	// that bad classifications age out but long enough that
	// genuinely useful unclassified content survives until
	// operators reclass it.
	ClassUnclassified: {
		DefaultConfidence:    0.3,
		TTL:                  30 * 24 * time.Hour,
		RoleOfRecordEligible: false,
	},
	// Companion-deposited notes (LLD 22). Low default confidence
	// because companion content lacks the upstream claim audit a
	// swarm role's output carries — a human typed it through Claude
	// but it was never round-tripped through a validator. 30-day
	// TTL matches unclassified so stale chat-derived notes age out
	// rather than accumulating. Never role-of-record: a companion
	// note is a thought, not a system-of-record entry.
	ClassCompanionNote: {
		DefaultConfidence:    0.3,
		TTL:                  30 * 24 * time.Hour,
		RoleOfRecordEligible: false,
	},
	// Chat-deposited memory (chat memory-write design §5.5,
	// operator-signed-off 2026-07-30). Low confidence (0.3): a chat
	// aside is untrusted input that loses to a verified document on
	// any tie, but not zero — a chunk that can never win is a write
	// path with no read path. 90-day TTL matches the shortest
	// existing class horizon (research) rather than inventing a
	// fourth: a chat aside is the most likely thing to be stale and
	// the least likely to be missed. NEVER role-of-record: a chat
	// note is a thought, not a system-of-record entry.
	ClassChatMemory: {
		DefaultConfidence:    0.3,
		TTL:                  90 * 24 * time.Hour,
		RoleOfRecordEligible: false,
	},
}

// roleClassMap is the deterministic producer-role → content-class
// lookup used when no LLM classifier is wired (Phase 2 default).
// Roles not in this map fall through to ClassUnclassified.
//
// This is an interim simplification of the design's LLM classifier:
// the rag-ingest workflow's classify step. A deterministic table is
// fast, cheap, and accurate enough for the producer-side roles we
// already know. Operators wanting LLM classification configure it
// per project (lands when Phase 4's validator pattern proves out).
//
// Coverage rule: every role declared in configs/swarms/*.yaml should
// either appear here or be deliberately omitted (when its output
// isn't load-bearing project knowledge — e.g. dispatcher's chat
// routing). When vornikctl memory reclassify reports "unknown role
// (not in roleClassMap)" for a role you DO want classified, add it
// here.
var roleClassMap = map[string]ContentClass{
	// Generic dev/research/QA roles (basic-swarm, dev-swarm shapes).
	"researcher":  ClassResearch,
	"scout":       ClassResearch,
	"analyst":     ClassSpec,
	"writer":      ClassResearch,
	"coder":       ClassCommitMsg,
	"developer":   ClassCommitMsg,
	"engineer":    ClassCommitMsg,
	"implementer": ClassCommitMsg,
	"reviewer":    ClassDecision,
	"architect":   ClassDecision,
	"tester":      ClassDiagnostic,
	"qa":          ClassDiagnostic,
	"verifier":    ClassDiagnostic,

	// Workflow / orchestration roles. The lead's output is the
	// project plan + per-task rationale — classified as spec because
	// it specifies what should happen, not because it has authority
	// to overrule (that's the reviewer/architect/risk-officer's
	// domain → decision).
	"lead": ClassSpec,
	// Feasibility-pass is the "go / no-go before planning" step —
	// short rationale that closes off bad directions. Decisive in
	// scope so it lands under decision rather than spec.
	"feasibility": ClassDecision,

	// Multi-modal / external-fetch shapes.
	// Vision OCRs images/PDFs and emits the extracted text. Like a
	// scraper fetch in terms of trust profile: the content came from
	// outside the project and needs the shorter 14-day TTL.
	"vision": ClassExternalFetch,

	// Trading-swarm shapes (ibkr-trader). Strategist drafts
	// proposals (spec), risk-officer rules go/no-go (decision),
	// executor logs the trade fill (commit_msg shape — terse
	// after-the-fact record, low TTL).
	"strategist":   ClassSpec,
	"risk-officer": ClassDecision,
	"executor":     ClassCommitMsg,

	// Document-ingest shape. The companion-rag-ingest workflow stamps
	// this role on every file an operator deposits through
	// /vornik-companion:rag-ingest — in practice the low-level designs,
	// which are the project's authoritative design record. Its absence
	// here left 497 chunks stuck at ClassUnclassified (0.3 confidence,
	// never role-of-record), so recall ranked agent-produced review
	// artifacts ABOVE the designs they review, and the LLM backfill
	// re-billed the same rows every tick forever (2026-08-15).
	//
	// spec rather than a narrower class because the dominant content is
	// design documents and spec is what the "LLDs are the design record"
	// retrieval rule keys on. Operators ingesting non-design files
	// through the same path inherit spec authority; that is the accepted
	// trade of a generic deposit path having one role.
	"rag-ingester": ClassSpec,

	// Deliberately omitted (chat routing is ephemeral — chunks from
	// these roles aren't load-bearing project knowledge; classify
	// keeps them unclassified so operators can decide whether to
	// retain them):
	//   - dispatcher
}

// ClassifyByRole returns the content class for a producer role plus
// the policy bundle. Unknown roles → unclassified with the
// catch-all policy. Always returns a valid (class, policy) pair.
//
// LLD 22 shortcut: any producer role prefixed with "companion:" is
// a host-LLM client deposit (Claude Code, Codex, etc.) and routes to
// ClassCompanionNote without entering the roleClassMap.
func ClassifyByRole(role string) (ContentClass, ClassPolicy) {
	if len(role) > len("companion:") && role[:len("companion:")] == "companion:" {
		return ClassCompanionNote, DefaultClassPolicies[ClassCompanionNote]
	}
	if class, ok := roleClassMap[role]; ok {
		if pol, ok := DefaultClassPolicies[class]; ok {
			return class, pol
		}
	}
	return ClassUnclassified, DefaultClassPolicies[ClassUnclassified]
}

// IsValidClass reports whether the string is a vornik-built-in
// class. Used by gate validators to reject unknown classes.
func IsValidClass(s string) bool {
	switch ContentClass(s) {
	case ClassResearch, ClassSpec, ClassDecision, ClassCommitMsg,
		ClassDiagnostic, ClassExternalFetch, ClassSummary, ClassUnclassified,
		ClassCompanionNote, ClassChatMemory:
		return true
	}
	return false
}
