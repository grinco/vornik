package agentbench

import "sort"

// Tool equivalence for CORE-MISS scoring only.
//
// A core miss is the hard-fail class: a tool EVERY recorded path used that the
// lead did not grant. The first real baseline (2026-08-14) produced five, and
// all five were the rule misfiring rather than a grant defect:
//
//	sw-02, sw-05, sw-08  core wanted git_status       — lead granted run_shell
//	sw-03, sw-10         core wanted read_many_files  — lead granted file_read, grep, glob
//
// Two things were wrong. First, "core" is the INTERSECTION of the unrestricted
// runs, so it is exactly where an agent's HABITS concentrate: three runs that
// each reflexively call `git_status` make it look mandatory. sw-05-table-test's
// entire core was {git_status, run_shell} for a task whose prompt forbids
// touching any existing file — a task that cannot require VCS inspection.
// Second, the rule demanded the specific tool the gold runs happened to reach
// for, blind to a granted tool with the same capability.
//
// Operator ruling: make core membership substitution-aware. A core tool is
// satisfied when the grant contains ANY tool of the same capability.
//
// WHY run_shell IS A MEMBER OF EVERY CLI-EXPRESSIBLE CLASS, which is the
// judgement call here. Granting a shell genuinely confers those capabilities —
// `git status`, `cat`, `grep` are all one command away — so a policy that
// granted run_shell and withheld git_status has not blocked the agent from
// anything, and calling that a hard failure would be false. The real objection
// to a blanket shell grant is that it grants far MORE than the task needs, and
// that is what grant PRECISION measures. Each metric keeps its own job: core
// miss asks "could the agent do the work", precision asks "was the grant
// tight". Blurring them would leave over-granting scored twice and
// under-granting scored wrongly.
//
// SUBSTITUTION IS DIRECTED, which is the subtlety that matters. A shell
// substitutes for git_status; glob does NOT substitute for a shell. Modelling
// run_shell as a class MEMBER would make the relation symmetric and let any
// file tool satisfy a core requirement for run_shell — laundering a real miss
// through the very mechanism meant to stop false ones. So peers are symmetric
// among themselves, and the shell is handled separately as a one-way provider.
//
// Deliberately NOT equivalence classes: memory_search, skill_fetch,
// backlog_deposit, query_api and the MCP surface. None is reachable from a
// shell inside the agent container, and none substitutes for another.
var toolEquivalenceClasses = [][]string{
	// Inspecting version-control state.
	{"git_status", "git_diff", "git_log", "git_show"},
	// Reading file contents, one file or many.
	{"file_read", "read_many_files"},
	// Locating files by name or by content.
	{"grep", "glob", "read_many_files"},
	// Running the project's own checks.
	{"test_run", "lint_run", "typecheck_run"},
}

// shellTool is the one-way provider: it satisfies any core tool that has a
// declared capability class, because each of those is a command away. Nothing
// satisfies IT — an agent handed glob cannot run arbitrary commands.
const shellTool = "run_shell"

// equivalentTools returns every tool that can stand in for the given one,
// including itself. Peers only — the shell provider is applied separately,
// because including it here would make the relation look symmetric to callers.
func equivalentTools(tool string) []string {
	tool = normaliseTool(tool)
	out := map[string]bool{tool: true}
	for _, class := range toolEquivalenceClasses {
		if !containsString(class, tool) {
			continue
		}
		for _, member := range class {
			out[member] = true
		}
	}
	names := make([]string, 0, len(out))
	for name := range out {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// shellExpressible reports whether a shell can do this tool's job. True exactly
// when the tool has a declared capability class: those are the file, VCS and
// check operations that are one command away.
func shellExpressible(tool string) bool {
	tool = normaliseTool(tool)
	for _, class := range toolEquivalenceClasses {
		if containsString(class, tool) {
			return true
		}
	}
	return false
}

// coreSatisfiedBy reports which granted tool covers a core tool, if any.
//
// Returns the SUBSTITUTE rather than a bare bool so the verdict can record how
// a core requirement was met. A pass earned by an equivalent tool is a
// different fact from a pass earned by the tool itself, and a metric that hides
// which one happened cannot be audited later.
func coreSatisfiedBy(coreTool string, accepted map[string]bool) (string, bool) {
	coreTool = normaliseTool(coreTool)
	if accepted[coreTool] {
		return coreTool, true
	}
	for _, alt := range equivalentTools(coreTool) {
		if accepted[alt] {
			return alt, true
		}
	}
	// The shell is checked LAST so a peer tool is credited in preference to it:
	// "covered by file_read" is a more informative audit line than "covered by
	// the shell", and the shell would otherwise absorb every substitution.
	if accepted[shellTool] && shellExpressible(coreTool) {
		return shellTool, true
	}
	return "", false
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
