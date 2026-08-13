package executor

import "strings"

// Claim-verification guidance injected into every agent system prompt.
//
// verifyRoleClaims runs after EVERY agent step on EVERY deployment: a step that
// reports testing.passed with no execution-class tool call in its toolAudit, or a
// file count materially above the real diff, fails hard. The rule is enforced
// everywhere — and before this block it was explained almost nowhere. Only
// basic-swarm and dev-swarm told their agents ("never fabricate test output …"), so
// on the other four shipped presets an honest-but-sloppy agent met a daemon
// invariant as an unexplained failure. A preset cannot fix that (LLD 09 §13.1): it
// is the operator's file and an upgrade never rewrites it.
//
// GATED ONLY ON "A SYSTEM PROMPT IS ALREADY BEING COMPOSED". There is no capability
// to gate on — verifyRoleClaims is not configurable — but a role with no prompt, no
// canonical context and no skills gets NO systemPrompt key at all, and the entrypoint
// then applies its own default. Emitting a bare integrity block in that case would
// REPLACE that default with three sentences: a worse outcome than the gap it closes.
// So the caller keeps its existing guard and this function is unconditional within it.
//
// DELIBERATELY STATES THE NORM, NOT THE MECHANISM. It names no check, no tool and no
// field the gate reads. Describing how detection works would hand an agent the
// cheapest way to satisfy it — one trivial command to make an execution-class call
// appear — turning a deception check into a bypass hint. What an agent needs is the
// standard it is held to and the safe way out ("say a check did not run"), which is
// exactly what an honest agent lacking this text gets wrong.
const claimVerificationSystemPromptBlock = `
REPORTING INTEGRITY — your claims are checked against what you actually did.

Report only outcomes you produced in this step. If a check could not be run, or a
change did not land, say so plainly and describe what you did instead: an accurate
partial result is accepted and routed onward, while a claim that does not match the
evidence fails the step outright.
`

// composeSystemPromptWithClaimVerification appends the reporting-integrity block.
//
// Appended after the role identity, like the other capability blocks — the agent's
// primary contract is "be the X role"; this is the standard that contract is judged
// against.
func composeSystemPromptWithClaimVerification(prompt string) string {
	if strings.TrimSpace(prompt) == "" {
		return claimVerificationSystemPromptBlock
	}
	return prompt + "\n" + claimVerificationSystemPromptBlock
}
