package api

// Classifying a tool call whose NAME is structurally impossible.
//
// `tool_audit_log` on task_20260820200803_930671c81e4b9b99 recorded a tool named
// literally:
//
//	file_write</think>allowed_paths
//
// A model leaked a reasoning-block terminator into its tool call. The EXECUTION
// half is fixed (`6aeabc5e`): the gates fail closed, so a name matching nothing
// is refused before it reaches a dispatch case, and a name that merely BEGINS
// with a real tool id cannot run that tool. `test/agent/tool_gate_failclosed_test.sh`
// calls the literal identity and asserts no file is written.
//
// WHAT REMAINED IS THE FORENSIC HALF, and a 2026-09-03 sweep of production shows
// why it matters: ten distinct malformed names, the most recent from that same
// morning. Every one was correctly refused — and every one was stored with
// `outcome_class = ''`. So the class of failure that says "a model is leaking
// reasoning markers into tool calls" is invisible to the very index built to
// make failure classes queryable (`idx_tool_audit_log_outcome_class`). Refusing
// correctly and recording nothing means nobody finds out.
//
// Also settled by that sweep, since the item asked: the step's BUDGET was not
// mis-attributed. The agent's budget counts LLM iterations, not tool
// executions, and a malformed call genuinely consumed an iteration — the model
// spent a turn producing it. There is nothing to correct there.
//
// WHY THIS IS SHAPE AND NOT TEXT. The connector-auth design is explicit that a
// failure class must be "derived from the TRANSPORT — an HTTP status, or a typed
// sentinel from the credential layer — never from message text. Relocating the
// sniffing would not have fixed anything." Sniffing `ERROR: unknown tool:` out
// of `tool_output` would be exactly the practice that design forbids.
//
// A NAME'S SHAPE is not message text. It is a structural property of the
// identifier itself, decidable with no registry, no vocabulary and no output:
// `file_write</think>allowed_paths` cannot be a tool name on any deployment,
// present or future, because tool names do not contain `<`. That makes this
// classification impossible to false-positive on a legitimate tool — including
// an MCP tool from a server the daemon has never seen — which is what makes it
// safe to apply at ingest.
//
// Backlog: "P2 — A malformed tool name is persisted (execution half FIXED
// 2026-08-20)".

import "vornik.io/vornik/internal/agenttools"

// ToolOutcomeClassMalformedName marks a call whose tool NAME could not be a tool
// name — the signature of a model leaking control or reasoning markup into its
// tool call rather than of any tool failing.
//
// Deliberately distinct from a generic unknown-name refusal. Both are refused
// identically and correctly; only this one tells an operator that a MODEL is
// misbehaving, which is a different action (change the model, or its prompt)
// from a stale tool reference (fix the workflow).
const ToolOutcomeClassMalformedName = "malformed_tool_name"

// classifyToolNameShape returns ToolOutcomeClassMalformedName when name cannot
// be a tool name, or "" when it is shaped like one.
//
// An empty name returns "" rather than the class: absent and malformed are
// different, and an ingest with no name at all is a client bug to be found
// elsewhere rather than a model leaking markup.
//
// A derived view of the declared grammar, not a second list (agent-tool
// declaration design §5). agenttools.WellFormedName is the function-name
// grammar of the providers the daemon speaks, which every tool definition
// passes through before a model can call it — so a name outside it was never
// advertised and can never be a legitimate call, from a builtin or from any
// vendor's MCP tool, now or later. Until 2026-09-03 this was a denylist of
// characters enumerated from the ten production shapes; it admitted `.`, `:`
// and `/` only because nothing had stated the rule.
func classifyToolNameShape(name string) string {
	if name == "" || agenttools.IsWellFormedName(name) {
		return ""
	}
	return ToolOutcomeClassMalformedName
}
