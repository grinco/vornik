package fixitdoctor

import "vornik.io/vornik/internal/version"

// FixItEnvelope is the schema-constrained shape the repair-chat LLM
// call is forced into via response_format=json_schema (task 3.2,
// fix-it-doctor-design.md §5.2). Mirrors projectwizard's Envelope
// pattern: Message is always operator-facing prose; Actions is the
// (possibly empty) set of remediation proposals; Resolved signals the
// LLM's belief that the underlying failure is fixed — the server never
// trusts that belief blindly (see Resolved handling in service.go).
type FixItEnvelope struct {
	Message  string           `json:"message"`
	Actions  []ProposedAction `json:"actions,omitempty"`
	Resolved bool             `json:"resolved"`
}

// ProposedAction is one remediation the LLM proposes. Kind MUST be one
// of the fixed ActionKind enum; Params carries whatever scalar
// arguments that Kind needs (e.g. retry_task needs "task_id"). Task
// 3.2 only parses + validates + renders proposals — actually
// DISPATCHING an action to execute is task 3.3's deny-by-default
// dispatcher. Only ActionKindLinkOut is "executable" here, in the
// sense that it's pure client-side navigation with no server mutation.
type ProposedAction struct {
	Kind   ActionKind        `json:"kind"`
	Label  string            `json:"label"`
	Params map[string]string `json:"params,omitempty"`
}

// ActionKind is the fixed, closed vocabulary of remediation actions
// the Fix-It Doctor can propose (fix-it-doctor-design.md §5.2/§5.3).
// The server validates every proposed action's Kind against this
// enum (plus its params, see validateActionParams) and DROPS anything
// that doesn't resolve — the LLM can never expand this vocabulary by
// asking nicely, including via injected/untrusted content in the
// grounding bundle.
type ActionKind string

// ActionKind* enumerate the fixed vocabulary. ActionKindConfigApply is
// Enterprise-Edition-gated (§5.3) — see AllowedActionKinds.
const (
	// ActionKindConfigApplyGate proposes flipping a feature/config gate
	// on (e.g. enabling a prerequisite). Requires param "key".
	ActionKindConfigApplyGate ActionKind = "config_apply_gate"
	// ActionKindConfigApply proposes writing an arbitrary config value.
	// Enterprise-only — see AllowedActionKinds. Requires params "key"
	// and "value".
	ActionKindConfigApply ActionKind = "config_apply"
	// ActionKindRetryTask proposes re-queueing a failed task. Requires
	// param "task_id".
	ActionKindRetryTask ActionKind = "retry_task"
	// ActionKindReprobeIntegration proposes re-running an integration's
	// health probe. Requires param "integration_id".
	ActionKindReprobeIntegration ActionKind = "reprobe_integration"
	// ActionKindSetSecret proposes that the operator set a secret at a
	// config key — the action itself never carries the secret VALUE,
	// only the key path the operator needs to fill in elsewhere.
	// Requires param "key".
	ActionKindSetSecret ActionKind = "set_secret"
	// ActionKindLinkOut is pure client-side navigation (e.g. "open the
	// integration's setup page") — no server mutation, so it's the one
	// kind task 3.2 actually wires end-to-end. Requires param "url",
	// which MUST be a same-origin absolute path (starts with "/") —
	// never an external URL or a non-http(s) scheme — so a prompt
	// injection in the grounding bundle can't turn this into an
	// exfiltration or phishing redirect.
	ActionKindLinkOut ActionKind = "link_out"
)

// allActionKinds is the complete declared enum, in schema order.
// AllowedActionKinds filters this down per edition.
var allActionKinds = []ActionKind{
	ActionKindConfigApplyGate,
	ActionKindConfigApply,
	ActionKindRetryTask,
	ActionKindReprobeIntegration,
	ActionKindSetSecret,
	ActionKindLinkOut,
}

// enterpriseOnlyActionKinds lists the ActionKinds gated out of
// Community-Edition builds (fix-it-doctor-design.md §5.3).
var enterpriseOnlyActionKinds = map[ActionKind]bool{
	ActionKindConfigApply: true,
}

// AllowedActionKinds returns the ActionKind vocabulary this edition
// exposes, in declared enum order. A Community build never sees
// config_apply — neither in the response_format schema handed to the
// LLM nor as a kind the server will accept from a (possibly
// hallucinating, possibly prompt-injected) model response.
func AllowedActionKinds(edition string) []ActionKind {
	enterprise := version.NormalizeEdition(edition) == version.EditionEnterprise
	out := make([]ActionKind, 0, len(allActionKinds))
	for _, k := range allActionKinds {
		if enterpriseOnlyActionKinds[k] && !enterprise {
			continue
		}
		out = append(out, k)
	}
	return out
}

// IsAllowedActionKind reports whether kind is both a declared ActionKind
// AND allowed for edition — the single-value form of AllowedActionKinds,
// used by the dispatcher (dispatch.go) to re-assert the guardrail at
// execution time rather than rebuilding the allowed-set on every call.
func IsAllowedActionKind(kind ActionKind, edition string) bool {
	if enterpriseOnlyActionKinds[kind] && version.NormalizeEdition(edition) != version.EditionEnterprise {
		return false
	}
	for _, k := range allActionKinds {
		if k == kind {
			return true
		}
	}
	return false
}
