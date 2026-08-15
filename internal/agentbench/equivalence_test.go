package agentbench

import "testing"

func accepted(tools ...string) map[string]bool {
	return set(normaliseTools(tools))
}

// The five core misses the first real baseline produced (2026-08-14). All five
// were the rule misfiring rather than a grant defect, and this pins that the
// substitution rule clears exactly those cases.
func TestCoreSatisfiedBy_ClearsTheBaselinesFalseMisses(t *testing.T) {
	cases := []struct {
		name, core string
		granted    []string
		wantVia    string
	}{
		{"sw-02/05/08: git_status vs a shell grant", "git_status",
			[]string{"file_read", "file_write", "glob", "run_shell"}, "run_shell"},
		{"sw-03: read_many_files vs file_read", "read_many_files",
			[]string{"file_read", "file_write", "glob", "grep", "run_shell"}, "file_read"},
		{"sw-10: read_many_files vs grep+file_read", "read_many_files",
			[]string{"current_time", "file_read", "glob", "grep", "memory_search", "run_shell"}, "file_read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			via, ok := coreSatisfiedBy(tc.core, accepted(tc.granted...))
			if !ok {
				t.Fatalf("%s still reads as a core miss against %v", tc.core, tc.granted)
			}
			if via != tc.wantVia {
				t.Errorf("satisfied via %q, want %q", via, tc.wantVia)
			}
		})
	}
}

// The tool itself always wins, so an exact grant is never reported as a
// substitution and the audit trail stays honest.
func TestCoreSatisfiedBy_PrefersTheToolItself(t *testing.T) {
	via, ok := coreSatisfiedBy("git_status", accepted("run_shell", "git_status"))
	if !ok || via != "git_status" {
		t.Errorf("via = %q, ok = %v; an exact grant must not be recorded as a substitution", via, ok)
	}
}

// Substitution must not become "anything satisfies anything". A lead that
// granted only file tools has genuinely blocked the agent from project memory.
func TestCoreSatisfiedBy_RefusesUnrelatedTools(t *testing.T) {
	for _, core := range []string{"memory_search", "skill_fetch", "backlog_deposit", "query_api"} {
		if via, ok := coreSatisfiedBy(core, accepted("file_read", "run_shell", "grep", "glob")); ok {
			t.Errorf("%s claimed satisfied via %q; none of those reach it", core, via)
		}
	}
}

// A shell is not granted, so the CLI-expressible classes cannot be used to
// launder a genuine miss.
func TestCoreSatisfiedBy_WithoutAShellAMissIsStillAMiss(t *testing.T) {
	if via, ok := coreSatisfiedBy("git_status", accepted("file_read", "file_write")); ok {
		t.Errorf("git_status claimed satisfied via %q with no shell and no git tool", via)
	}
}

func TestEquivalentTools(t *testing.T) {
	t.Run("a tool is equivalent to itself even with no class", func(t *testing.T) {
		if got := equivalentTools("memory_search"); len(got) != 1 || got[0] != "memory_search" {
			t.Errorf("equivalentTools(memory_search) = %v", got)
		}
	})
	t.Run("qualified names resolve to the bare form", func(t *testing.T) {
		via, ok := coreSatisfiedBy("functions.git_status", accepted("run_shell"))
		if !ok || via != "run_shell" {
			t.Errorf("a qualified core tool did not normalise: via=%q ok=%v", via, ok)
		}
	})
	t.Run("membership is symmetric", func(t *testing.T) {
		// git_diff standing in for git_status must also work the other way, or
		// the score would depend on which one the gold run happened to use.
		if _, ok := coreSatisfiedBy("git_diff", accepted("git_status")); !ok {
			t.Error("git_status does not satisfy git_diff, but git_diff satisfies git_status")
		}
	})
}

// The directed half of the relation, and the reason run_shell is not a class
// member: modelling it as one makes substitution symmetric, and any file tool
// would then satisfy a core requirement for the shell — laundering a real miss
// through the mechanism built to stop false ones.
func TestCoreSatisfiedBy_NothingSubstitutesForTheShell(t *testing.T) {
	for _, granted := range [][]string{
		{"glob"}, {"grep", "glob"}, {"file_read", "read_many_files"},
		{"git_status", "git_diff"}, {"test_run", "lint_run", "typecheck_run"},
	} {
		if via, ok := coreSatisfiedBy("run_shell", accepted(granted...)); ok {
			t.Errorf("run_shell claimed satisfied via %q by %v; none of those run commands",
				via, granted)
		}
	}
	if via, ok := coreSatisfiedBy("run_shell", accepted("run_shell")); !ok || via != "run_shell" {
		t.Errorf("run_shell does not satisfy itself: via=%q ok=%v", via, ok)
	}
}

// A peer is credited ahead of the shell: "covered by file_read" is a more
// informative audit line than "covered by the shell", and the shell would
// otherwise absorb every substitution and hide the specific one.
func TestCoreSatisfiedBy_CreditsAPeerAheadOfTheShell(t *testing.T) {
	via, ok := coreSatisfiedBy("read_many_files", accepted("file_read", "run_shell"))
	if !ok || via != "file_read" {
		t.Errorf("via = %q, want file_read credited ahead of run_shell", via)
	}
}

func TestShellExpressible(t *testing.T) {
	for _, yes := range []string{"git_status", "file_read", "grep", "test_run"} {
		if !shellExpressible(yes) {
			t.Errorf("%s should be shell-expressible", yes)
		}
	}
	// A shell inside the agent container cannot reach project memory, the skill
	// index, or the MCP surface — so granting one does not cover them.
	for _, no := range []string{"memory_search", "skill_fetch", "backlog_deposit", "query_api", "run_shell"} {
		if shellExpressible(no) {
			t.Errorf("%s must NOT be reachable by granting a shell", no)
		}
	}
}
