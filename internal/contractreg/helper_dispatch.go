package contractreg

import (
	"fmt"
	"sort"
	"strconv"

	"vornik.io/vornik/internal/agenttools"
)

// CheckHelperDispatchAgreement holds the three views of "where does this tool
// run" level (agent-tool dispatch design §4): the declaration's Runtime
// field, the bash cases in exec_tool, and the handlers in internal/agentloop.
// A declared tool is dispatchable by a bash case XOR a helper handler:
//   - both is double dispatch — the bash case wins and the handler is dead;
//   - RuntimeHelper with no handler is a promise nothing keeps;
//   - a handler for a tool not declared RuntimeHelper is unreachable — the
//     bash branch would never send it, and the helper's belt would refuse it;
//   - HELPER_TOOL_NAMES_JSON must equal the declaration's helper set, both
//     ways, or exec_tool delegates a different set than the one declared.
func CheckHelperDispatchAgreement(t *Table) []Finding {
	const check = "helper-dispatch"
	var out []Finding
	bash := t.setOf(KindAgentToolDispatch)
	handlers := t.setOf(KindAgentToolHelperDispatch)
	listed := t.setOf(KindAgentToolHelperListed)
	declared := map[string]bool{}
	for _, n := range agenttools.HelperNames() {
		declared[n] = true
	}
	for _, tool := range agenttools.Tools {
		n := tool.Name
		switch {
		case declared[n] && handlers.has(n) && bash.has(n):
			out = append(out, Finding{Check: check, Name: n, Sources: entrySources(t, KindAgentToolDispatch, n),
				Detail: "double dispatch — declared RuntimeHelper with a handler in internal/agentloop AND a bash case in exec_tool; the bash case wins and the handler is dead. Delete the case"})
		case declared[n] && !handlers.has(n):
			out = append(out, Finding{Check: check, Name: n,
				Detail: "declared RuntimeHelper but internal/agentloop.Handlers has no entry — the bash branch would delegate it and the helper would refuse it"})
		case !declared[n] && handlers.has(n):
			out = append(out, Finding{Check: check, Name: n,
				Detail: "internal/agentloop.Handlers implements it but the declaration says RuntimeShell — the bash branch never delegates it, so the handler is unreachable; declare RuntimeHelper or delete the handler"})
		}
		if declared[n] != listed.has(n) {
			out = append(out, Finding{Check: check, Name: n, Sources: entrySources(t, KindAgentToolHelperListed, n),
				Detail: fmt.Sprintf("HELPER_TOOL_NAMES_JSON lists it: %t; the declaration says RuntimeHelper: %t — the generated file is stale; run `make docs-gen`", listed.has(n), declared[n])})
		}
	}
	for n := range listed {
		if !agenttools.IsBuiltin(n) {
			out = append(out, Finding{Check: check, Name: n, Sources: entrySources(t, KindAgentToolHelperListed, n),
				Detail: "HELPER_TOOL_NAMES_JSON carries a name the declaration does not know"})
		}
	}
	for n := range handlers {
		if !agenttools.IsBuiltin(n) {
			out = append(out, Finding{Check: check, Name: n,
				Detail: "internal/agentloop.Handlers implements a name the declaration does not know"})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}

// CheckHelperBranchIsGated pins the ONE shape exec_tool may have around the
// helper branch (agent-tool dispatch design §4), by line order and on
// separate lines: the gate's `if ! tool_call_permitted`, its closing `fi`,
// exactly one `tool_runs_in_helper` call, then `case "$name" in`. Any other
// arrangement — the branch before the gate closes, after the case, on the
// gate's own line, or twice — is a finding. This is the property whose loss
// would be a silent gate bypass, so it is a lint rather than a comment; and
// it is deliberately line-based rather than a bash parser, so a refactor that
// wants a different shape changes the check in the same commit.
func CheckHelperBranchIsGated(t *Table) []Finding {
	const check = "helper-branch-gated"
	if t.Get(KindAgentToolAdvertisementFilter, "helper-list-present") == nil && len(t.Names(KindAgentToolHelperBranch)) == 0 {
		// A tree with no helper mechanism at all has nothing to order.
		return nil
	}
	line := func(name string) (int, bool) {
		e := t.Get(KindAgentToolHelperBranch, name)
		if e == nil {
			return 0, false
		}
		n, err := strconv.Atoi(e.Status)
		return n, err == nil
	}
	var out []Finding
	gate, okGate := line("gate")
	gateEnd, okEnd := line("gate-end")
	branch, okBranch := line("helper-branch#1")
	caseAt, okCase := line("case")
	if _, twice := line("helper-branch#2"); twice {
		out = append(out, Finding{Check: check, Name: "helper-branch#2", Sources: entrySources(t, KindAgentToolHelperBranch, "helper-branch#2"),
			Detail: "tool_runs_in_helper is consulted more than once in exec_tool — one gated branch is the shape"})
	}
	if !okGate || !okEnd || !okCase {
		out = append(out, Finding{Check: check, Name: "exec_tool",
			Detail: fmt.Sprintf("could not locate the gate (%t), its closing fi (%t) and the dispatch case (%t) in exec_tool — the shell's shape changed; fix the extractor rather than the check", okGate, okEnd, okCase)})
		return out
	}
	if !okBranch {
		out = append(out, Finding{Check: check, Name: "exec_tool",
			Detail: "HELPER_TOOL_NAMES_JSON is generated but exec_tool never consults tool_runs_in_helper — helper tools would fall through to the bash case list and be refused as unknown"})
		return out
	}
	if gate >= gateEnd || gateEnd >= branch || branch >= caseAt {
		out = append(out, Finding{Check: check, Name: "exec_tool", Sources: entrySources(t, KindAgentToolHelperBranch, "helper-branch#1"),
			Detail: fmt.Sprintf("the helper branch must sit AFTER the gate's fi and BEFORE the case, each on its own line: gate=%d fi=%d branch=%d case=%d", gate, gateEnd, branch, caseAt)})
	}
	return out
}
