package contractreg

import (
	"fmt"
	"sort"
)

// CheckPhantomGrants reports contracts that name a capability with no
// implementation behind it: a role or swarm granting a tool that cannot run.
//
// This is the drift problem arriving from the code side. `configs/extractors.yaml`
// was named by a design doc for ~2.5 months and never existed; a role granting a
// misspelled tool is the same defect with a smaller blast radius — the grant
// silently means nothing, and the role's prompt tells a model to use something it
// will never be offered.
//
// mcp__ grants are skipped by construction: server-side existence needs a live
// daemon, so KindMCPGrant is contract-only (see its doc comment). Checking them
// statically would produce a false failure on every deployment whose MCP servers
// differ from the author's.
func CheckPhantomGrants(t *Table, grants map[string][]string) []Finding {
	const check = "phantom-contract"
	var out []Finding

	// A tool can run if it has a dispatch case, or is a system handler, or is a
	// deliberate exemption. The Go and advertised lists are NOT sufficient
	// evidence on their own — that asymmetry is what CheckAgentToolAgreement
	// exists to police, and treating them as proof here would let the two checks
	// cover for each other.
	runnable := t.setOf(KindAgentToolDispatch)
	helper := t.setOf(KindAgentToolHelperDispatch) // a Go handler is a way to run (dispatch design §4)
	handlers := t.setOf(KindSystemHandler)

	holders := make([]string, 0, len(grants))
	for h := range grants {
		holders = append(holders, h)
	}
	sort.Strings(holders)

	for _, holder := range holders {
		for _, tool := range grants[holder] {
			if tool == "" {
				continue
			}
			if isMCPName(tool) {
				continue
			}
			if _, ok := UngatedByDesign[tool]; ok {
				continue
			}
			if runnable.has(tool) || helper.has(tool) || handlers.has(tool) {
				continue
			}
			out = append(out, Finding{
				Check:   check,
				Name:    tool,
				Sources: []string{holder},
				Detail: fmt.Sprintf("%s grants %q, which has no exec_tool dispatch case and is not a "+
					"system handler — the grant means nothing, and a prompt telling the model to use "+
					"it will never see it offered", holder, tool),
			})
		}
	}
	return out
}

func isMCPName(n string) bool {
	return len(n) > len(agentMCPPrefix) && n[:len(agentMCPPrefix)] == agentMCPPrefix
}

// CheckNeverSetBuildTags reports build tags that gate files but are set nowhere
// in the build plumbing. Everything behind such a tag is unbuilt and unrun, and
// — critically — invisible to any reachability analysis, because it is never
// compiled. See TagAudit.
func CheckNeverSetBuildTags(a TagAudit) []Finding {
	const check = "never-set-build-tag"
	var out []Finding
	for _, tag := range a.NeverSet() {
		files := a.FilesByTag[tag]
		sort.Strings(files)
		detail := fmt.Sprintf("build tag %q gates %d file(s) but is set nowhere in the build "+
			"plumbing — that code is never compiled, so it is also invisible to reachability "+
			"analysis rather than merely unreachable", tag, len(files))
		if allTestFiles(files) {
			detail += ". Every gated file is a _test.go, so the effect is tests that NEVER RUN — " +
				"a coverage claim the suite does not actually make"
		}
		out = append(out, Finding{Check: check, Name: tag, Detail: detail, Sources: files})
	}
	return out
}

func allTestFiles(files []string) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if len(f) < len("_test.go") || f[len(f)-len("_test.go"):] != "_test.go" {
			return false
		}
	}
	return true
}
