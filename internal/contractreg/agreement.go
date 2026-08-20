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

// UngatedPrefixesByDesign are name PREFIXES the agent container deliberately
// does not gate, because the grant is enforced somewhere else. Same contract as
// UngatedByDesign — data with a reason, reviewed as a security decision — and a
// stricter one in practice, since a prefix exempts an open-ended set of names
// rather than one.
var UngatedPrefixesByDesign = map[string]string{
	"mcp__": "gated daemon-side by Server.roleAllowsMCPTool at /mcp/call, with the " +
		"advertised catalogue filtered through the same predicate (roleToolAllowlist). " +
		"The container cannot reproduce that decision: the daemon's 'role declared no " +
		"allowedTools implies unrestricted' rule is invisible in the input contract " +
		"(buildAgentInput substitutes a default, so declared-nothing and declared-these " +
		"arrive identically), and re-implementing the daemon's four-shape matcher in jq " +
		"would put a second copy of one security predicate in a second language — the " +
		"anti-pattern that produced the 2026.8.1 bypass. The residual gap (both gates " +
		"absent in a daemon resolution-gap state) is filed, not hidden: see the backlog " +
		"and agent runtime contract LLD §7.1",
}

// CheckAgentToolAgreement enforces the invariant that the four agent-tool
// registries describe the same vocabulary:
//
//	every name with an exec_tool dispatch case must appear in
//	BUILTIN_TOOL_NAMES_JSON, in is_builtin_tool(), and in
//	agenttools.builtinTools — unless it is UngatedByDesign.
//
// Why each half matters. NOTE the consequences inverted on 2026-08-20 when both
// gates were flipped to fail closed (agent runtime contract §7.1); this check
// stays a hard failure, for the opposite reason it was introduced:
//
//   - Missing from is_builtin_tool() → the tool is refused for every role, and
//     reported as "unknown tool" rather than "not allowed for this role".
//     Until the flip the EXECUTION gate short-circuited false instead and the
//     per-role allowlist was never consulted, so the tool ran for ANY role.
//   - Missing from BUILTIN_TOOL_NAMES_JSON → the tool is never advertised.
//     Until the flip absence SATISFIED the advertisement filter
//     (`($builtin | index($name) | not) or ...`), so it was offered to every
//     role's model regardless of its allowlist.
//
// So an omission now costs a capability instead of granting one. It is still not
// cosmetic: a declared-but-unregistered tool should be visibly broken rather
// than silently either way, and nothing about fail-closed gates makes it safe
// for the four registries to disagree.
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

// CheckUngatedExemptionAgreement enforces that the shell's exemption registry
// (UNGATED_TOOL_NAMES_JSON) and UngatedByDesign name the same tools.
//
// UngatedByDesign's contract is that an exemption is a reviewed security
// decision recorded as data with a reason. Until 2026-08-20 the execution gate
// undercut that by hardcoding the same names beside it:
//
//	[ "$name" != "tool_search" ] && [ "$name" != "tool_result_read" ] && \
//	  is_builtin_tool "$name" && ! builtin_tool_allowed "$name"
//
// The fail-closed rewrite replaced those literals with a JSON registry the shell
// consumes and this package parses, so the vocabulary is declared once per side
// instead of scraped out of string tests. Stray `"$name" = "x"` comparisons are
// still harvested into the same kind, because reintroducing one is how an
// unreviewed exemption would come back.
//
// Both directions are bugs, with opposite consequences:
//
//   - shell-only exemption → a tool bypasses the per-role allowlist with NO
//     recorded reason and no review. This is the 2026.8.1 bypass class.
//   - registry-only exemption → the runtime gates a tool the design says must
//     stay reachable, so a role legitimately denied nothing loses a capability
//     quietly.
//
// An empty extraction is itself a failure: if the parse breaks, comparing two
// empty sets would read as agreement.
func CheckUngatedExemptionAgreement(t *Table) []Finding {
	return checkRegistryAgreement(t, registryAgreement{
		check:    "ungated-exemption-agreement",
		kind:     KindAgentToolInlineExempt,
		registry: UngatedByDesign,
		emptyDetail: "no exemptions were extracted from UNGATED_TOOL_NAMES_JSON — the parse " +
			"has broken, and comparing two empty sets would look like agreement",
		shellOnly: "exempted by the container but absent from UngatedByDesign — an allowlist " +
			"bypass with no recorded reason and no review. Add it to the registry with a " +
			"reason, or stop exempting it.",
		registryOnly: "listed in UngatedByDesign but NOT exempted by the container — the " +
			"runtime gates a tool the design says must stay reachable. Either exempt it in " +
			"entrypoint.sh or drop it from the registry.",
	})
}

// CheckUngatedPrefixAgreement enforces that the shell's UNGATED_TOOL_PREFIXES_JSON
// and UngatedPrefixesByDesign name the same prefixes.
//
// Separate from CheckUngatedExemptionAgreement because the failure is worse in
// both directions: a shell-only prefix exempts an unbounded set of names with no
// recorded reason, and a registry-only prefix means the container is gating a
// family of tools the design says is gated elsewhere — so they are refused twice
// and reachable nowhere.
func CheckUngatedPrefixAgreement(t *Table) []Finding {
	return checkRegistryAgreement(t, registryAgreement{
		check:    "ungated-prefix-agreement",
		kind:     KindAgentToolUngatedPrefix,
		registry: UngatedPrefixesByDesign,
		emptyDetail: "no prefix exemptions were extracted from UNGATED_TOOL_PREFIXES_JSON — " +
			"the parse has broken, and comparing two empty sets would look like agreement",
		shellOnly: "exempted by the container's UNGATED_TOOL_PREFIXES_JSON but absent from " +
			"UngatedPrefixesByDesign — an open-ended allowlist bypass with no recorded " +
			"reason and no review. Add it to the registry with a reason, or stop exempting it.",
		registryOnly: "listed in UngatedPrefixesByDesign but NOT exempted by the container — " +
			"tools under this prefix are gated in a place the design says does not gate " +
			"them, so a role permitted them elsewhere is refused here.",
	})
}

// registryAgreement parameterises one both-directions comparison between a
// vocabulary declared in the shell and its Go registry.
type registryAgreement struct {
	check        string
	kind         Kind
	registry     map[string]string
	emptyDetail  string
	shellOnly    string
	registryOnly string
}

// checkRegistryAgreement compares one extracted shell vocabulary against its Go
// registry in both directions.
//
// Extracted when the prefix check arrived as a near-copy of the exemption check.
// Two hand-maintained implementations of "compare these two registries" is the
// same fault this package exists to catch, one level up — so the second instance
// became the primitive rather than the precedent.
//
// An empty extraction is itself a failure: if the parse breaks, comparing two
// empty sets would read as agreement.
func checkRegistryAgreement(t *Table, spec registryAgreement) []Finding {
	extracted := t.Names(spec.kind)
	if len(extracted) == 0 {
		return []Finding{{
			Check:  spec.check,
			Name:   "(none extracted)",
			Detail: spec.emptyDetail,
		}}
	}

	inShell := make(map[string]bool, len(extracted))
	for _, n := range extracted {
		inShell[n] = true
	}

	var out []Finding
	for _, n := range extracted {
		if _, ok := spec.registry[n]; !ok {
			out = append(out, Finding{
				Check:   spec.check,
				Name:    n,
				Detail:  spec.shellOnly,
				Sources: entrySources(t, spec.kind, n),
			})
		}
	}
	for n := range spec.registry {
		if !inShell[n] {
			out = append(out, Finding{
				Check:  spec.check,
				Name:   n,
				Detail: spec.registryOnly,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// entrySources returns an entry's source locations, or nil when absent.
func entrySources(t *Table, kind Kind, name string) []string {
	if e := t.Get(kind, name); e != nil {
		return e.Sources
	}
	return nil
}
