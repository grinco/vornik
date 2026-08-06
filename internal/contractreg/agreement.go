package contractreg

import (
	"fmt"
	"sort"
)

// UngatedByDesign are agent tools deliberately exempt from the per-role
// allowlist, encoded as DATA with a reason rather than as inline string
// comparisons in the shell.
//
// This distinction is the whole point: before this list existed, "intentionally
// ungated" and "accidentally ungated" were indistinguishable, and four tools sat
// in the second category for as long as nobody looked (memory_search,
// skill_fetch, get_conversation_window, summarize_thread — see
// https://docs.vornik.io, 2026-08-06). Adding a name here is a security decision and
// should be reviewed as one.
var UngatedByDesign = map[string]string{
	"tool_search": "discovery tool — a role cannot ask for a tool it cannot see; " +
		"gating discovery would make deferred MCP tools unreachable by design",
	"tool_result_read": "pagination companion to every other tool's output; " +
		"gating it would strand results the role was already permitted to produce",
}

// CheckAgentToolAgreement enforces the invariant that the four agent-tool
// registries describe the same vocabulary:
//
//	every name with an exec_tool dispatch case must appear in
//	BUILTIN_TOOL_NAMES_JSON, in is_builtin_tool(), and in
//	agenttools.builtinTools — unless it is UngatedByDesign.
//
// Why each half matters, and why this is a security check rather than tidiness:
//
//   - Missing from is_builtin_tool() → the EXECUTION gate
//     (`is_builtin_tool "$name" && ! builtin_tool_allowed "$name"`) short-circuits
//     false, so the per-role allowlist is never consulted. The tool runs for any
//     role.
//   - Missing from BUILTIN_TOOL_NAMES_JSON → the ADVERTISEMENT filter
//     (`($builtin | index($name) | not) or ($allowed | ...)`) is satisfied by the
//     first clause, so the tool is offered to every role's model regardless of
//     its allowlist.
//
// Both gates are phrased as "is this a builtin?", so both FAIL OPEN on absence.
// That is why the check is a hard failure: an omission is not cosmetic.
func CheckAgentToolAgreement(t *Table) []Finding {
	const check = "registry-disagreement"
	var out []Finding

	dispatch := t.setOf(KindAgentToolDispatch)
	advertised := t.setOf(KindAgentToolAdvertised)
	gate := t.setOf(KindAgentToolGate)
	goList := t.setOf(KindAgentToolGo)

	names := make([]string, 0, len(dispatch))
	for n := range dispatch {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		if _, ok := UngatedByDesign[name]; ok {
			continue
		}
		e := t.Get(KindAgentToolDispatch, name)
		var src []string
		if e != nil {
			src = e.Sources
		}
		if !gate.has(name) {
			out = append(out, Finding{
				Check: check, Name: name, Sources: src,
				Detail: "has an exec_tool dispatch case but is absent from is_builtin_tool() — " +
					"the execution-time allowlist gate is SKIPPED for it, so every role can call it " +
					"regardless of allowedTools (privilege bypass, fail-open)",
			})
		}
		if !advertised.has(name) {
			out = append(out, Finding{
				Check: check, Name: name, Sources: src,
				Detail: "has an exec_tool dispatch case but is absent from BUILTIN_TOOL_NAMES_JSON — " +
					"the advertisement filter offers it to every role's model regardless of " +
					"allowedTools (fail-open)",
			})
		}
		if !goList.has(name) {
			out = append(out, Finding{
				Check: check, Name: name, Sources: src,
				Detail: "has an exec_tool dispatch case but is absent from " +
					"internal/agenttools.builtinTools — role-library validation and the prompt " +
					"linters will not recognise legitimate use of it",
			})
		}
	}

	// The reverse direction: a name declared as a builtin with no way to run is
	// a phantom — a role can be granted it and the grant means nothing.
	for _, kind := range []Kind{KindAgentToolAdvertised, KindAgentToolGate, KindAgentToolGo} {
		for _, name := range t.Names(kind) {
			if dispatch.has(name) {
				continue
			}
			if _, ok := UngatedByDesign[name]; ok {
				continue
			}
			e := t.Get(kind, name)
			var src []string
			if e != nil {
				src = e.Sources
			}
			out = append(out, Finding{
				Check: check, Name: name, Sources: src,
				Detail: fmt.Sprintf("declared in %s but has no exec_tool dispatch case — "+
					"a role granted this tool would find nothing behind it", kind),
			})
		}
	}
	return out
}

type nameSet map[string]bool

func (s nameSet) has(n string) bool { return s[n] }

// Set is declared on Table returning map[string]bool; give it the helper.
func (t *Table) setOf(kind Kind) nameSet { return nameSet(t.Set(kind)) }
