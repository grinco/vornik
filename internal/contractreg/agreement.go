package contractreg

import (
	"fmt"
	"sort"

	"vornik.io/vornik/internal/agenttools"
)

// UngatedByDesign and UngatedPrefixesByDesign are the exemption registries —
// tools and name prefixes deliberately exempt from the per-role allowlist,
// each with a recorded reason so "intentionally ungated" and "accidentally
// ungated" are never the same observation.
//
// Since 2026-09-03 they are views of the declaration in internal/agenttools
// (Tool.Gate == GateExemptByDesign, agenttools.UngatedPrefixes): one copy of
// the vocabulary, and the shell's UNGATED_TOOL_NAMES_JSON /
// UNGATED_TOOL_PREFIXES_JSON are generated from the same source. The names are
// kept here so the checks read as they did; adding an exemption is still a
// security decision, made in agenttools.Tools and reviewed there.
var (
	UngatedByDesign         = agenttools.Exempt()
	UngatedPrefixesByDesign = agenttools.UngatedPrefixMap()
)

// CheckAgentToolAgreement enforces that the surfaces naming agent tools
// describe one vocabulary:
//
//	every name with an exec_tool dispatch case appears in agenttools.Tools,
//	in TOOL_REGISTRY_JSON, and in BUILTIN_TOOL_NAMES_JSON — unless it is
//	exempt by design — and the reverse: a declared name has a dispatch case.
//
// Since 2026-09-03 the shell registries are GENERATED from agenttools.Tools,
// so three of the four sides cannot disagree with each other unless the
// generated file is stale (which `make docs-gen-check` also catches). The
// dispatch case is the side generation cannot know — it is code — and the
// dispatch↔declaration comparison in both directions is what this check is
// for. is_builtin_tool()'s hand-written case list, the fourth registry the
// 2026-08-06 design counted, is gone: it reads BUILTIN_TOOL_NAMES_JSON.
//
// The consequences inverted on 2026-08-20 when both gates were flipped to fail
// closed (agent runtime contract §7.1): an omission now costs a capability
// (refused for every role, never advertised) instead of granting one. Still a
// hard failure — a declared-but-undispatchable tool should be visibly broken
// rather than silently either way.
func CheckAgentToolAgreement(t *Table) []Finding {
	const check = "registry-disagreement"
	var out []Finding

	dispatch := t.setOf(KindAgentToolDispatch)
	advertised := t.setOf(KindAgentToolAdvertised)
	registry := t.setOf(KindAgentToolRegistry)
	goList := t.setOf(KindAgentToolGo)

	names := make([]string, 0, len(dispatch))
	for n := range dispatch {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		e := t.Get(KindAgentToolDispatch, name)
		var src []string
		if e != nil {
			src = e.Sources
		}
		if !goList.has(name) {
			out = append(out, Finding{
				Check: check, Name: name, Sources: src,
				Detail: "has an exec_tool dispatch case but is not declared in " +
					"internal/agenttools.Tools — the gates refuse it for every role, it is never " +
					"advertised, and role-library validation will not recognise legitimate use of it",
			})
		}
		if len(registry) > 0 && !registry.has(name) {
			out = append(out, Finding{
				Check: check, Name: name, Sources: src,
				Detail: "has an exec_tool dispatch case but is absent from TOOL_REGISTRY_JSON — " +
					"the generated registry is stale (run `make docs-gen`) or the declaration was " +
					"never made",
			})
		}
		if _, ok := UngatedByDesign[name]; ok {
			continue
		}
		if !advertised.has(name) {
			out = append(out, Finding{
				Check: check, Name: name, Sources: src,
				Detail: "has an exec_tool dispatch case but is absent from BUILTIN_TOOL_NAMES_JSON — " +
					"never advertised and refused as unknown for every role (capability loss); " +
					"declare it, or record why it is exempt",
			})
		}
	}

	// The reverse direction: a name declared with no way to run is a phantom —
	// a role can be granted it and the grant means nothing. A Go handler in
	// internal/agentloop is a way to run (agent-tool dispatch design §4);
	// whether it and a bash case coexist is CheckHelperDispatchAgreement's
	// question, not this one's.
	helper := t.setOf(KindAgentToolHelperDispatch)
	for _, kind := range []Kind{KindAgentToolAdvertised, KindAgentToolRegistry, KindAgentToolGo} {
		for _, name := range t.Names(kind) {
			if dispatch.has(name) || helper.has(name) {
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

// CheckShellRegistryMatchesDeclaration compares the generated registry file,
// as parsed, against internal/agenttools — belt and braces over
// `make docs-gen-check`, and the check that keeps the parser honest: a stale
// or hand-edited tool_registry.generated.sh, or a parser that has gone blind
// on one array, fails here by name.
func CheckShellRegistryMatchesDeclaration(t *Table) []Finding {
	const check = "shell-registry-stale"
	var out []Finding
	compare := func(kind Kind, want []string, what string) {
		got := t.Names(kind)
		if len(got) == 0 {
			out = append(out, Finding{Check: check, Name: "(none extracted)",
				Detail: fmt.Sprintf("no entries were extracted from %s — the parse has broken, and "+
					"comparing an empty set would look like agreement", what)})
			return
		}
		gotSet, wantSet := map[string]bool{}, map[string]bool{}
		for _, g := range got {
			gotSet[g] = true
		}
		for _, w := range want {
			wantSet[w] = true
		}
		for _, g := range got {
			if !wantSet[g] {
				out = append(out, Finding{Check: check, Name: g, Sources: entrySources(t, kind, g),
					Detail: fmt.Sprintf("%s carries %q, which internal/agenttools does not declare — "+
						"the generated file is stale or hand-edited; run `make docs-gen`", what, g)})
			}
		}
		for _, w := range want {
			if !gotSet[w] {
				out = append(out, Finding{Check: check, Name: w,
					Detail: fmt.Sprintf("internal/agenttools declares %q but %s lacks it — "+
						"the generated file is stale; run `make docs-gen`", w, what)})
			}
		}
	}
	compare(KindAgentToolAdvertised, agenttools.Offerable(), "BUILTIN_TOOL_NAMES_JSON")
	compare(KindAgentToolRegistry, agenttools.Names(), "TOOL_REGISTRY_JSON")
	compare(KindAgentToolAdvertiseToken, agenttools.AdvertiseTokens(), "ADVERTISE_TOKENS_JSON")
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CheckAdvertiseTokensAgree holds tool_advertised_now()'s case labels equal to
// ADVERTISE_TOKENS_JSON in both directions, and requires the refusing default
// arm. The same shape as the dispatch check, for the same reason: the token
// set is data the generator emits, the case is code, and only a comparison can
// say they agree. A token with no case advertises nothing (a silent capability
// loss); a case with no token is dead code that reads like a rule; a missing
// default arm would let an unknown token fall through to whatever bash does.
func CheckAdvertiseTokensAgree(t *Table) []Finding {
	const check = "advertise-token-disagreement"
	out := checkRegistryAgreement(t, registryAgreement{
		check:    check,
		kind:     KindAgentToolAdvertiseCase,
		registry: keyed(t.Names(KindAgentToolAdvertiseToken)),
		emptyDetail: "no case labels were extracted from tool_advertised_now() — the function is " +
			"missing or its shape changed, and comparing empty sets would look like agreement",
		shellOnly: "tool_advertised_now() handles a token ADVERTISE_TOKENS_JSON does not emit — " +
			"dead code that reads like a rule. Declare the Advertise value in agenttools, or drop the arm.",
		registryOnly: "ADVERTISE_TOKENS_JSON emits a token tool_advertised_now() has no case for — " +
			"every tool declared with it is silently never advertised. Add the arm.",
	})
	if len(t.Names(KindAgentToolAdvertiseToken)) == 0 {
		out = append(out, Finding{Check: check, Name: "(none extracted)",
			Detail: "no tokens were extracted from ADVERTISE_TOKENS_JSON — the registry file is " +
				"missing or the parse has broken"})
	}
	if !t.setOf(KindAgentToolAdvertisementFilter).has("advertise-default-refuses") {
		out = append(out, Finding{Check: check, Name: "tool_advertised_now",
			Detail: "has no refusing default arm (`*) return 1 ;;`) — an unknown token must " +
				"advertise nothing, not fall through"})
	}
	return out
}

// keyed turns a name list into the map shape checkRegistryAgreement compares.
func keyed(names []string) map[string]string {
	m := make(map[string]string, len(names))
	for _, n := range names {
		m[n] = n
	}
	return m
}

// CheckNeverAdvertisedIsAppendedByName requires every registry entry whose
// advertise token is "never" to be passed to tool_definition_for somewhere in
// the entrypoint. A declared tool that tool_definitions() skips and nothing
// else offers is a capability loss with no symptom. Presence only, like the
// dispatch check: whether the call sits behind a live condition is what the
// shell tests (test-entrypoint-deferred-mcp.sh) exercise.
func CheckNeverAdvertisedIsAppendedByName(t *Table) []Finding {
	const check = "never-advertised-unreachable"
	var out []Finding
	appended := t.setOf(KindAgentToolAppendedByName)
	for _, name := range t.Names(KindAgentToolNeverAdvertised) {
		if appended.has(name) {
			continue
		}
		out = append(out, Finding{Check: check, Name: name, Sources: entrySources(t, KindAgentToolNeverAdvertised, name),
			Detail: "is declared with Advertise = Never, so tool_definitions() skips it, and no path " +
				"in the entrypoint appends it by name (tool_definition_for " + name + ") — declared " +
				"and unreachable"})
	}
	return out
}

// CheckToolDefinitionsReadRegistryOnly is the successor of the 2026-08-22
// "third advertisement path" check. It asks whether a path exists by which a
// definition reaches the model without consulting a registry — and since
// 2026-09-03 the answer is structural: tool_definitions() reads
// TOOL_REGISTRY_JSON and carries no inline definition and no heredoc, so
// there is no append step for such a path to be. Both markers must be
// present, plus the fail-closed filter itself.
func CheckToolDefinitionsReadRegistryOnly(t *Table) []Finding {
	const check = "advertisement-path-ungated"
	markers := t.setOf(KindAgentToolAdvertisementFilter)
	var out []Finding
	if !markers.has("fail-closed-filter") {
		out = append(out, Finding{Check: check, Name: "tool_definitions",
			Detail: "the fail-closed advertisement filter (exempt, or declared AND allowed) is " +
				"missing from tool_definitions() — or the extraction that looks for it has broken. " +
				"Either deserves the build failure."})
	}
	if !markers.has("definitions-registry-only") {
		out = append(out, Finding{Check: check, Name: "tool_definitions",
			Detail: "tool_definitions() does not read TOOL_REGISTRY_JSON, or carries an inline " +
				"definition (a `\"name\":` literal or a heredoc) — a definition the registry does " +
				"not know about is the 2026-08-22 bypass class: it reaches the model without any " +
				"registry consulted. Declare the tool in internal/agenttools and regenerate."})
	}
	return out
}

// CheckRegistryFileSourced requires the entrypoint to source the generated
// registry, to consult it from is_builtin_tool(), and to declare none of the
// registry variables itself. A second declaration beside the sourced one is
// the hand-mirrored copy this design removed, back as data.
func CheckRegistryFileSourced(t *Table) []Finding {
	const check = "registry-not-sourced"
	markers := t.setOf(KindAgentToolAdvertisementFilter)
	var out []Finding
	if !markers.has("registry-sourced") {
		out = append(out, Finding{Check: check, Name: ToolRegistryFile,
			Detail: "the entrypoint does not source ${VORNIK_TOOL_REGISTRY:-…/" + ToolRegistryFile + "} — " +
				"its gates would run on empty registries"})
	}
	if markers.has("registry-declared-inline") {
		out = append(out, Finding{Check: check, Name: "entrypoint.sh",
			Detail: "the entrypoint declares a registry variable itself as well as sourcing the " +
				"generated file — a second copy of the vocabulary; delete it and regenerate"})
	}
	if !markers.has("gate-reads-registry") {
		out = append(out, Finding{Check: check, Name: "is_builtin_tool",
			Detail: "is_builtin_tool() does not consult BUILTIN_TOOL_NAMES_JSON — a hand-written " +
				"case list there is the registry the 2026-09-03 design retired"})
	}
	return out
}
