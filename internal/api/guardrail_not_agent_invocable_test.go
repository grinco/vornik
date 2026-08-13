package api

import (
	"strings"
	"testing"
)

// No guardrail may be agent-invocable.
//
// This test carries a security argument, so it is worth stating what it defends.
//
// Registry design §10.1 lets the project lead narrow a step's advertised tools per
// execution. Grants can only SUBTRACT from the operator-authored ceiling, which
// makes privilege escalation impossible — but the §10.1 review raised a second
// threat that subtraction does not answer: a lead induced by prompt injection could
// STRIP a safety-relevant tool, leaving a step without a guardrail it would
// otherwise have had.
//
// §10.3 answers that by verification rather than by adding a floor: in this product
// no guardrail is a tool an agent elects to call. Secret redaction runs on the
// daemon's request path (internal/secrets), the tool_audit_log row is written by the
// daemon when a tool is invoked, and the memory gate stack runs inside IngestText.
// An agent cannot decline any of them, so a grant cannot strip them.
//
// That claim is only as good as its enforcement, which is this test. If a guardrail
// ever becomes something an agent must choose to call, this fails and the §10.3
// argument must be revisited — either by making that guardrail non-elective again,
// or by introducing the safety floor §10.3 declined.
//
// NOTE the threat predates grants: a static role allowlist could already omit a
// guardrail tool today. So the fix, if this ever fails, belongs at the guardrail —
// not in a floor that would only cover the grant path.

// guardrailWords are the shapes a safety-relevant tool name would plausibly take.
// Deliberately broad: a false positive costs one code review, a false negative
// costs the security argument.
var guardrailWords = []string{
	"audit", "moderat", "redact", "disclos", "guard",
	"approve", "approval", "consent", "policy", "compliance",
}

func TestNoBuiltinToolIsAGuardrail(t *testing.T) {
	// The built-in surface the executor grants by default (plan.go's
	// allowedTools default). If a guardrail ever joins this list it becomes
	// something a narrow grant can remove.
	builtins := []string{"file_read", "file_write", "run_shell", "current_time"}

	for _, name := range builtins {
		for _, word := range guardrailWords {
			if strings.Contains(strings.ToLower(name), word) {
				t.Errorf("built-in tool %q looks like a guardrail (matched %q).\n\n"+
					"Registry design §10.3 rests a security argument on NO guardrail being "+
					"agent-invocable: that is why per-execution grants need no safety floor. "+
					"If this tool really is a guardrail, either make it non-elective on the "+
					"daemon request path, or add the floor §10.3 declined — do not simply "+
					"widen this test.", name, word)
			}
		}
	}
}

// absenceFailsClosed lists tools that match a guardrail word but whose ABSENCE
// cannot weaken enforcement — the precise rule §10.3 needs. Each entry carries why.
//
// The rule is NOT "no tool named like a guardrail". It is "no tool whose absence
// weakens enforcement". A tool that can only ever tighten when removed is safe for a
// grant to omit.
var absenceFailsClosed = map[string]string{
	// skill_approve promotes a DRAFT knowledge skill to ACTIVE. The guardrail is
	// that a draft is INERT until approved, which is the default state — so
	// withholding this tool means nothing gets promoted. Absence fails CLOSED.
	// Separately gated by the skill_admin key capability, independent of any role
	// allowlist or grant.
	"skill_approve": "promotes draft->active; absence leaves drafts inert (fails closed) " +
		"and it is separately gated by the skill_admin key capability",
}

// TestCompanionToolsCarryNoGuardrail applies the same rule to the companion MCP
// surface, which is the tool set this daemon defines itself (as opposed to tools
// proxied from third-party MCP servers, whose names we do not control).
//
// Third-party servers are deliberately out of scope: we cannot stop someone wiring
// an MCP server that exposes a "policy_check" tool. What we CAN pin is that vornik
// never makes one of its own guardrails elective, which is what the §10.3 argument
// actually needs.
func TestCompanionToolsCarryNoGuardrail(t *testing.T) {
	for _, def := range companionToolDefs() {
		if why, ok := absenceFailsClosed[def.Name]; ok {
			if why == "" {
				t.Errorf("%s is allowlisted with no reason", def.Name)
			}
			continue
		}
		for _, word := range guardrailWords {
			if !strings.Contains(strings.ToLower(def.Name), word) {
				continue
			}
			// A read-only inspection tool is not a guardrail: removing it cannot
			// disable a check. Only tools whose ABSENCE weakens enforcement matter.
			t.Errorf("companion tool %q matches guardrail word %q.\n\n"+
				"If this tool ENFORCES something (rather than merely reporting it), "+
				"§10.3's claim that no guardrail is agent-invocable is false and the "+
				"per-execution grant design needs a safety floor. If it only reports, "+
				"add it to the allowlist below with a one-line reason.", def.Name, word)
		}
	}
}
