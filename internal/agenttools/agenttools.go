// Package agenttools is the ONE declaration of the tools an agent container
// can run: name, gate, exemption reason, the daemon's always-granted baseline,
// when the definition is offered, and the OpenAI function definition itself.
//
// Everything else is a derived view. The container's shell registries
// (images/vornik-agent/tool_registry.generated.sh — BUILTIN_TOOL_NAMES_JSON,
// UNGATED_TOOL_NAMES_JSON, UNGATED_TOOL_PREFIXES_JSON, TOOL_REGISTRY_JSON) are
// generated from Tools by `go run ./cmd/docs-gen tools`, so they cannot
// disagree with this file; internal/contractreg checks the one thing generation
// cannot make unrepresentable — that every declared tool has an exec_tool
// dispatch case and every case is declared. Design of record:
// https://docs.vornik.io
//
// Before 2026-09-03 this package held names only, and the same vocabulary was
// hand-mirrored in three places in the entrypoint plus the exemption lists in
// contractreg, kept in agreement by lint and nothing else. Four recurrences of
// one class came out of that — the 2026.8.1 allowlist bypass, both gates
// failing open, an advertisement path that consulted no registry, and ten
// production tool names that could not be tool names because nothing said
// what a tool name is. Each fix added a checker; this file removes the copies.
//
// The package is a dependency-free leaf: it imports nothing from the rest of
// the tree, so any layer may consume it without an import-law violation.
package agenttools

import (
	"regexp"
	"sort"
	"strings"
)

// Gate says which predicate permits a call. There is no third value: a tool
// is on the role's effective allowlist, or it is exempt with a reason.
type Gate int

const (
	// GateAllowlist — permitted iff the name is on the role's effective
	// allowlist (config.permissions.allowedTools in task.json, which the daemon
	// has already unioned with AlwaysGranted). Agent runtime contract §7.1 rule 1.
	GateAllowlist Gate = iota
	// GateExemptByDesign — permitted for every role, in BOTH gates (execution
	// and advertisement), never one. ExemptReason is required. §7.1 rule 2.
	GateExemptByDesign
)

// Advertise says when tool_definitions() in the container offers the tool's
// definition to the model. A CLOSED set: the generator emits the token, the
// entrypoint maps tokens to environment tests in ONE case function with a
// refusing default arm, and contractreg holds the two equal in both
// directions. Advertisement can only NARROW — the fail-closed filter (exempt,
// or declared AND allowed) runs after it — so a token decides presence for a
// tool whose backing service may be absent; it never grants.
type Advertise int

const (
	// AdvertiseAlways — in every tools array.
	AdvertiseAlways Advertise = iota
	// AdvertiseNever — never from tool_definitions(); another path appends
	// the definition by name (tool_search, from rebuild_tools_file when MCP
	// deferral is on).
	AdvertiseNever
	// AdvertiseWhenMemoryURL — VORNIK_MEM_URL is set (a memory backend is configured).
	AdvertiseWhenMemoryURL
	// AdvertiseWhenAPIURL — VORNIK_API_URL is set (the daemon API is reachable).
	AdvertiseWhenAPIURL
	// AdvertiseWhenTaskBound — VORNIK_API_URL and VORNIK_TASK_ID are set (a task step).
	AdvertiseWhenTaskBound
	// AdvertiseWhenResultHygiene — tool_result_hygiene_enabled holds
	// (VORNIK_TOOL_RESULT_HYGIENE, default on): spill exists to page through.
	AdvertiseWhenResultHygiene
)

// advertiseTokens are the shell-facing spellings, indexed by Advertise. The
// generator emits them and the entrypoint's tool_advertised_now() switches on
// them; contractreg compares that switch's labels to this list.
var advertiseTokens = [...]string{
	AdvertiseAlways:            "always",
	AdvertiseNever:             "never",
	AdvertiseWhenMemoryURL:     "when_memory_url",
	AdvertiseWhenAPIURL:        "when_api_url",
	AdvertiseWhenTaskBound:     "when_task_bound",
	AdvertiseWhenResultHygiene: "when_result_hygiene",
}

// Token is the shell-facing spelling of the advertise condition.
func (a Advertise) Token() string {
	if int(a) < 0 || int(a) >= len(advertiseTokens) {
		return ""
	}
	return advertiseTokens[a]
}

// AdvertiseTokens returns every token, in enum order.
func AdvertiseTokens() []string {
	return append([]string(nil), advertiseTokens[:]...)
}

// Tool is one agent tool. Schema is attached at init from schemas/<Name>.json.
type Tool struct {
	Name string
	Gate Gate
	// ExemptReason is non-empty iff Gate == GateExemptByDesign. An exemption
	// is a security decision recorded as data, so "intentionally ungated" and
	// "accidentally ungated" are never the same observation.
	ExemptReason string
	// AlwaysGranted — the daemon folds the tool onto every role's effective
	// allowlist (executor/plan.go withAlwaysGrantedTools). Operator ruling
	// 2026-08-14: only tools that read what the project already knows.
	AlwaysGranted bool
	// Acts — writes, executes, deposits, or leaves the deployment. An Acts tool
	// is never AlwaysGranted; the test enforces it.
	Acts      bool
	Advertise Advertise
	// Runtime says which program implements the tool's dispatch case: a bash
	// case in the entrypoint's exec_tool (the default) or a handler in
	// internal/agentloop reached through `vornik-agent-helper exec-tool`. The
	// gate is the same either way and runs BEFORE the runtime is consulted
	// (agent-tool dispatch design §2). contractreg holds the three views —
	// this field, the bash cases, the Go handlers — in agreement.
	Runtime Runtime
	// Schema is the OpenAI function definition ({"type":"function","function":{...}}).
	Schema []byte
}

// Runtime is where a tool's dispatch case lives.
type Runtime int

const (
	// RuntimeShell — a case body in entrypoint.sh's exec_tool.
	RuntimeShell Runtime = iota
	// RuntimeHelper — `vornik-agent-helper exec-tool <name> <args-json>`, a
	// handler in internal/agentloop.Handlers.
	RuntimeHelper
)

// HelperNames returns, sorted, the tools declared RuntimeHelper. The generator
// emits it as HELPER_TOOL_NAMES_JSON; the entrypoint's exec_tool delegates
// exactly these to the helper, after the gate.
func HelperNames() []string {
	var out []string
	for _, t := range Tools {
		if t.Runtime == RuntimeHelper {
			out = append(out, t.Name)
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// RunsInHelper reports whether a declared tool's dispatch is the helper's.
func RunsInHelper(name string) bool {
	t := byName[name]
	return t != nil && t.Runtime == RuntimeHelper
}

// Tools is the declaration, in ADVERTISEMENT order: the order a model sees the
// definitions, which test/agent/tool_definitions_golden_test.sh pins. Names()
// is the sorted view for consumers that want one. The Never tool sits last.
var Tools = []Tool{
	{Name: "file_read", Runtime: RuntimeHelper},
	{Name: "file_write", Acts: true, Runtime: RuntimeHelper},
	{Name: "run_shell", Acts: true},
	{Name: "current_time", Runtime: RuntimeHelper},
	{Name: "file_edit", Acts: true, Runtime: RuntimeHelper},
	{Name: "read_many_files", Runtime: RuntimeHelper},
	{Name: "grep", Runtime: RuntimeHelper},
	{Name: "glob", Runtime: RuntimeHelper},
	{Name: "git_status", Runtime: RuntimeHelper},
	{Name: "git_diff", Runtime: RuntimeHelper},
	{Name: "git_log", Runtime: RuntimeHelper},
	{Name: "git_show", Runtime: RuntimeHelper},
	{Name: "test_run", Acts: true},
	{Name: "lint_run", Acts: true},
	{Name: "typecheck_run", Acts: true},
	{Name: "backlog_deposit", Acts: true},
	{
		Name: "tool_result_read", Gate: GateExemptByDesign,
		ExemptReason: "pagination companion to every other tool's output; " +
			"gating it would strand results the role was already permitted to produce",
		Advertise: AdvertiseWhenResultHygiene,
	},
	// memory_search reads project memory. Universal since 2026-08-14 (it only
	// reads what the project already knows), advertised only when a memory
	// backend is configured — offering a tool with nothing behind it costs the
	// model an iteration.
	{Name: "memory_search", AlwaysGranted: true, Advertise: AdvertiseWhenMemoryURL},
	{Name: "get_conversation_window", Advertise: AdvertiseWhenTaskBound},
	{Name: "summarize_thread", Advertise: AdvertiseWhenTaskBound},
	// skill_fetch reads the skill index. Universal since 2026-08-14, gated in
	// advertisement since 2026-08-22 — it used to ride the unfiltered
	// extras_ungated append and was advertised-then-refused in the fallback
	// allowlist state.
	{Name: "skill_fetch", AlwaysGranted: true, Advertise: AdvertiseWhenAPIURL},
	{Name: "query_api", Acts: true, Advertise: AdvertiseWhenAPIURL},
	{Name: "list_apis", Advertise: AdvertiseWhenAPIURL},
	{
		Name: "tool_search", Gate: GateExemptByDesign,
		ExemptReason: "discovery tool — a role cannot ask for a tool it cannot see; " +
			"gating discovery would make deferred MCP tools unreachable by design",
		Advertise: AdvertiseNever,
	},
}

// Prefix is a name prefix the container deliberately does not gate because
// the grant is enforced somewhere else. A prefix exempts an open-ended set of
// names, so it carries the same requirement as an exemption: a reason.
type Prefix struct {
	Prefix, Reason string
}

// UngatedPrefixes is the declaration of prefix exemptions.
var UngatedPrefixes = []Prefix{{
	Prefix: "mcp__",
	Reason: "gated daemon-side by Server.roleAllowsMCPTool at /mcp/call, with the " +
		"advertised catalogue filtered through the same predicate (roleToolAllowlist). " +
		"The container cannot reproduce that decision: the daemon's 'role declared no " +
		"allowedTools implies unrestricted' rule is invisible in the input contract " +
		"(buildAgentInput substitutes a default, so declared-nothing and declared-these " +
		"arrive identically), and re-implementing the daemon's four-shape matcher in jq " +
		"would put a second copy of one security predicate in a second language — the " +
		"anti-pattern that produced the 2026.8.1 bypass. The residual gap (both gates " +
		"absent in a daemon resolution-gap state) is filed, not hidden: see the backlog " +
		"and agent runtime contract LLD §7.1",
}}

// mcpToolPrefix marks a tool name as MCP-provided. MCP tool names are
// `mcp__<server>__<tool>` and can only be validated against the live
// daemon's configured servers, not statically.
const mcpToolPrefix = "mcp__"

var (
	// BuiltinName is the grammar a declared tool's name must match.
	BuiltinName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// WellFormedName is the widest shape ANY name reaching a model can take,
	// including an MCP tool id a vendor chose. Not guessed: it is the
	// function-name grammar of the providers the daemon speaks (OpenAI
	// `^[a-zA-Z0-9_-]+$`, Anthropic `^[a-zA-Z0-9_-]{1,128}$`), which every
	// tool definition passes through before a model can call it. A name
	// outside it was never advertised and can never be a legitimate call —
	// markup, whitespace, call syntax, dots — on any deployment, now or later.
	WellFormedName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// IsWellFormedName reports whether the name has the shape of a tool name at
// all. It says nothing about existence: a well-formed unknown name is a
// hallucination, not a malformed one.
func IsWellFormedName(name string) bool {
	return WellFormedName.MatchString(name)
}

var byName = func() map[string]*Tool {
	m := make(map[string]*Tool, len(Tools))
	for i := range Tools {
		m[Tools[i].Name] = &Tools[i]
	}
	return m
}()

// Get returns the declaration for name, or nil.
func Get(name string) *Tool {
	return byName[name]
}

// IsBuiltin reports whether name is a declared agent tool.
func IsBuiltin(name string) bool {
	return byName[name] != nil
}

// IsMCPTool reports whether name is an MCP-provided tool reference
// (the `mcp__server__tool` convention). These are dynamic and cannot
// be validated against a static list — the composer's compose/commit
// path checks the server is actually configured (design §5.3).
func IsMCPTool(name string) bool {
	return strings.HasPrefix(name, mcpToolPrefix) && len(name) > len(mcpToolPrefix)
}

// Names returns every declared tool name in sorted order.
func Names() []string {
	out := make([]string, 0, len(Tools))
	for _, t := range Tools {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// Set returns the declared names as a set, a fresh copy per call.
func Set() map[string]bool {
	out := make(map[string]bool, len(Tools))
	for _, t := range Tools {
		out[t.Name] = true
	}
	return out
}

// Offerable returns the names tool_definitions() may offer — every declared
// tool whose Advertise is not Never. This is the container's
// BUILTIN_TOOL_NAMES_JSON: the "declared" set the fail-closed advertisement
// filter consults, and what is_builtin_tool() uses to word a refusal.
func Offerable() []string {
	var out []string
	for _, t := range Tools {
		if t.Advertise != AdvertiseNever {
			out = append(out, t.Name)
		}
	}
	sort.Strings(out)
	return out
}

// AlwaysGranted returns the tools every role may call regardless of its
// allowedTools list. The returned slice is a copy — callers append to it.
func AlwaysGranted() []string {
	var out []string
	for _, t := range Tools {
		if t.AlwaysGranted {
			out = append(out, t.Name)
		}
	}
	return out
}

// IsAlwaysGranted reports whether a tool is in the universal baseline.
func IsAlwaysGranted(name string) bool {
	t := byName[name]
	return t != nil && t.AlwaysGranted
}

// Exempt returns the tools exempt from both gates, with their reasons.
func Exempt() map[string]string {
	out := map[string]string{}
	for _, t := range Tools {
		if t.Gate == GateExemptByDesign {
			out[t.Name] = t.ExemptReason
		}
	}
	return out
}

// ExemptNames returns the exempt tool names, sorted.
func ExemptNames() []string {
	var out []string
	for n := range Exempt() {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// UngatedPrefixMap returns the prefix exemptions as prefix → reason.
func UngatedPrefixMap() map[string]string {
	out := map[string]string{}
	for _, p := range UngatedPrefixes {
		out[p.Prefix] = p.Reason
	}
	return out
}
