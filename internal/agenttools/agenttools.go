// Package agenttools is the single source of truth for the built-in
// structured tool names available inside any agent container.
//
// It was promoted out of internal/api's role-prompt linter
// (doctor_prompt_lint.go) so that BOTH the linter and the new role-
// library doctor check (internal/rolelibrary) validate tool names
// against one list instead of drifting copies. The package is a
// dependency-free leaf: it imports nothing from the rest of the tree,
// so any layer may consume it without an import-law violation.
//
// "Built-in" here means the tools the executor wires into every agent
// process regardless of MCP configuration (file_read, run_shell, the
// git_* family, …). MCP-provided tools (`mcp__server__tool`) are
// dynamic — their availability depends on the daemon's configured MCP
// servers — so they are NOT enumerated here; callers that need to
// accept MCP names do so via IsMCPTool.
package agenttools

import (
	"sort"
	"strings"
)

// builtinTools is the canonical set of built-in agent tool names. A
// role may reference these in its prompt / allowlist. Keep this list
// in sync with the executor's tool wiring (internal/dispatcher's
// agent process) — adding a tool the executor exposes but omitting it
// here makes the linters flag legitimate usage.
var builtinTools = map[string]bool{
	"file_read":        true,
	"file_write":       true,
	"file_edit":        true,
	"run_shell":        true,
	"current_time":     true,
	"read_many_files":  true,
	"grep":             true,
	"glob":             true,
	"git_status":       true,
	"git_diff":         true,
	"git_log":          true,
	"git_show":         true,
	"test_run":         true,
	"lint_run":         true,
	"typecheck_run":    true,
	"memory_search":    true,
	"tool_result_read": true,
	"query_api":        true,
	"list_apis":        true,
	// Added 2026-08-06: each of these has an exec_tool dispatch case in
	// images/vornik-agent/entrypoint.sh and was missing here, so the role-library
	// validator and the prompt linters did not recognise legitimate use of them.
	// internal/contractreg's registry-disagreement check now keeps the four
	// agent-tool registries in agreement on every `make lint`.
	"backlog_deposit":         true,
	"skill_fetch":             true,
	"get_conversation_window": true,
	"summarize_thread":        true,
}

// alwaysGranted are the built-in tools every role gets whatever its
// allowedTools declares.
//
// A role's allowlist exists to bound what an agent can DO — write files,
// run shell, reach an external API. These two only let it find out what
// the project already knows: memory_search reads project memory,
// skill_fetch reads the skill index. Neither writes, executes, or leaves
// the deployment, so gating them buys no containment and costs the agent
// the context that would have kept it from guessing.
//
// Operator ruling, 2026-08-14. It came out of a benchmark gold review:
// both tools showed up in recorded paths for tasks that did not obviously
// need them, which read as agent habit contaminating the ground truth.
// The operator's answer was that they should be universal, which makes
// the observation unremarkable rather than suspicious — a tool everyone
// has is not evidence of anything.
//
// Daemon-side rather than in the swarm presets for the same reason the
// tool-grant guidance is: a preset reaches new installs only, because an
// upgrade does not rewrite an operator's own swarm file.
var alwaysGranted = []string{
	"memory_search",
	"skill_fetch",
}

// AlwaysGranted returns the built-in tools every role may call regardless
// of its allowedTools list. The returned slice is a copy — callers append
// to it.
func AlwaysGranted() []string {
	return append([]string(nil), alwaysGranted...)
}

// IsAlwaysGranted reports whether a tool is in the universal baseline.
func IsAlwaysGranted(name string) bool {
	for _, t := range alwaysGranted {
		if t == name {
			return true
		}
	}
	return false
}

// mcpToolPrefix marks a tool name as MCP-provided. MCP tool names are
// `mcp__<server>__<tool>` and can only be validated against the live
// daemon's configured servers, not statically.
const mcpToolPrefix = "mcp__"

// IsBuiltin reports whether name is a built-in agent tool.
func IsBuiltin(name string) bool {
	return builtinTools[name]
}

// IsMCPTool reports whether name is an MCP-provided tool reference
// (the `mcp__server__tool` convention). These are dynamic and cannot
// be validated against a static list — the composer's compose/commit
// path checks the server is actually configured (design §5.3).
func IsMCPTool(name string) bool {
	return strings.HasPrefix(name, mcpToolPrefix) && len(name) > len(mcpToolPrefix)
}

// Names returns the built-in tool names in sorted order. Callers that
// need a stable, presentable list (docs, grounding prompts, doctor
// output) use this rather than ranging the unexported map.
func Names() []string {
	out := make([]string, 0, len(builtinTools))
	for name := range builtinTools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Set returns a copy of the built-in tool set as a map, for callers
// that want membership lookups without exporting the internal map.
func Set() map[string]bool {
	out := make(map[string]bool, len(builtinTools))
	for name := range builtinTools {
		out[name] = true
	}
	return out
}
