package executor

import "strings"

// Tool-grant guidance injected into the agent system prompt.
//
// WHY THIS IS DAEMON-SIDE AND NOT IN THE SWARM PRESETS. An existing deployment's
// swarm YAML is the operator's file; upgrading the binary does not rewrite it. So
// guidance that lives only in configs/swarms/*.md reaches NEW installs and nobody
// else, and every current customer's leads would never learn the tool exists. This
// block ships with the binary, so an upgrade updates every deployment's agents at
// once. The presets carry the same guidance for discoverability when an operator
// reads their own config, but this is what makes it true in the field.
//
// Deliberately short. The feature exists to cut a large advertised tool surface down
// to what a step needs, saving far more per iteration than this block costs; spending
// ~90 tokens to explain it is a trade worth making, spending 900 would not be. The
// measured saving is in the registry LLD, not here — a figure from one deployment's
// audit history does not belong in a shipped comment.
//
// Gated on the tool actually being advertised (grantToolAvailable) — telling an agent
// to call a tool it does not have wastes the tokens AND teaches it to hallucinate a
// call that will fail.
const toolGrantSystemPromptBlock = `
TOOL BUDGET — call mcp__vornik__grant_step_tools early.

Your prompt currently carries the schema of every MCP tool this project has, on
every turn of your tool loop. Most steps need a handful. Naming the ones you
actually need shrinks every later turn of this step.

* Pass the fully-qualified names (mcp__server__tool) you expect to use.
* You can only narrow within what your role already permits — you cannot grant
  yourself a tool you lack, and naming one refuses the whole request.
* If you turn out to need more, call it again with escalation=true (limited
  attempts). Under-asking costs one extra turn, so prefer a slightly generous list
  over a minimal one.
`

// composeSystemPromptWithToolGrant appends the tool-budget block when the grant tool
// is available to this step.
//
// Appended last, after the role identity and the canonical-context block, because it
// is a housekeeping instruction rather than part of the agent's contract — the same
// ordering rationale as composeSystemPromptWithCanonicalContext.
func composeSystemPromptWithToolGrant(prompt string, grantToolAvailable bool) string {
	if !grantToolAvailable {
		return prompt
	}
	if strings.TrimSpace(prompt) == "" {
		return toolGrantSystemPromptBlock
	}
	return prompt + "\n" + toolGrantSystemPromptBlock
}
