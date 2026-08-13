package api

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"vornik.io/vornik/internal/chat"
)

// Per-execution tool grants (registry design §10.1–§10.4).
//
// A project's whole MCP surface is advertised to every role on every iteration of
// the agent tool loop. Measured: 52,430 bytes of schema (~13,100 tokens) sent to
// `assistant · research` every iteration, while the median execution of that role
// touches tools worth 2,256 bytes — a 95.7% overshoot, re-paid ~8 times per step.
//
// Static per-role ceilings cannot fix this: a generalist role's LIFETIME union is
// nearly the full surface, so the best static saving on the hot path measured 0–15%.
// The lead knows what THIS execution needs, so it grants per execution.
//
// THE INVARIANT. A grant only ever SUBTRACTS:
//
//	advertised = project ∩ ceiling ∩ grant     (tools/list, recomputed every call)
//	permitted  = project ∩ ceiling             (/mcp/call, per invocation)
//
// The lead's context can carry attacker-influenced bytes, so "grant place_order to
// the next step" must be unable to succeed — and it cannot, because a grant is never
// consulted when deciding whether an invocation is permitted. The worst a hostile
// grant achieves is no narrowing, or a wasted iteration when it narrows something a
// step then needs.
//
// That is why nothing here is exported to the /mcp/call path. Enforcement reads the
// ceiling alone (§10.4).

// maxEscalationsPerStep bounds escalation requests within a step.
//
// Escalation adds no privilege — it can only reach tools already inside the ceiling
// — but an injected prompt can force repeated cycles, each audited, spamming the
// audit pipeline and burning iterations. A rate limit, not a security control.
const maxEscalationsPerStep = 3

// ToolGrant is the lead's REQUEST for one step, stored verbatim.
//
// Stored as a request, never as a resolved tool set. The resolution is recomputed on
// every advertise call, so a ceiling tightened by hot reload takes effect
// immediately and a stale grant cannot outlive it (§10.4 drift).
type ToolGrant struct {
	// StepID scopes the grant. A grant for one step says nothing about another.
	StepID string `json:"step_id"`
	// Tools is what the lead asked for, as written.
	Tools []string `json:"tools"`
	// Escalations counts granted escalation requests against maxEscalationsPerStep.
	Escalations int `json:"escalations"`
}

// GrantOutcome reports what happened to a grant request.
//
// Refused names are carried for the AUDIT ROW only. They must never reach the agent:
// the lead's context can contain injected text, and echoing which names were refused
// lets that text enumerate the ceiling by probing (§10.3(4)). Message is what the
// agent sees.
type GrantOutcome struct {
	Accepted     []string
	RefusedNames []string
	Message      string
	CeilingHash  string
}

// EvaluateToolGrant checks a requested grant against the ceiling.
//
// Refuses the whole request when it names anything outside the ceiling rather than
// silently trimming: a lead that believes it scoped a step, and did not, would
// otherwise proceed on a false premise. The refusal is generic to the agent and
// itemised in the audit.
func EvaluateToolGrant(requested, ceiling []string) GrantOutcome {
	out := GrantOutcome{CeilingHash: CeilingHash(ceiling)}
	// An empty ceiling means "unrestricted" everywhere else in this system, so a
	// grant against it can only narrow — nothing to refuse.
	for _, want := range requested {
		if len(ceiling) == 0 || mcpRoleToolAllowed(ceiling, want) {
			out.Accepted = append(out.Accepted, want)
			continue
		}
		out.RefusedNames = append(out.RefusedNames, want)
	}
	if len(out.RefusedNames) > 0 {
		out.Accepted = nil
		// Deliberately names no tool. The count is safe: it leaks one integer, not
		// the ceiling's membership.
		out.Message = "grant refused: it names tools outside this role's allowed set " +
			"(see the audit trail for details); narrow the request and retry"
	}
	return out
}

// GrantedTools narrows an advertised set by an accepted grant.
//
// Layered on advertisedTools rather than replacing it, so the ceiling is applied
// even if a grant somehow names something outside it — belt and braces around the
// subtract-only invariant.
func GrantedTools(all []chat.Tool, ceiling, grant []string) []chat.Tool {
	scoped := advertisedTools(all, ceiling)
	if len(grant) == 0 {
		return scoped
	}
	return advertisedTools(scoped, grant)
}

// MayEscalate reports whether a step has escalation budget left.
func MayEscalate(g *ToolGrant) bool {
	return g == nil || g.Escalations < maxEscalationsPerStep
}

// CeilingHash digests a resolved ceiling for the audit row.
//
// Recorded with every grant so a later reviewer can distinguish "this tool was never
// in the ceiling" from "the ceiling was tightened after the grant" — contents alone
// cannot separate those (§10.4). Order-independent, because a reordered allowlist is
// the same ceiling.
func CeilingHash(ceiling []string) string {
	if len(ceiling) == 0 {
		return "unrestricted"
	}
	sorted := make([]string, len(ceiling))
	copy(sorted, ceiling)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return hex.EncodeToString(sum[:8])
}
