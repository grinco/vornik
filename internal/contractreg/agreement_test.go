package contractreg

import (
	"path/filepath"
	"runtime"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func realEntrypoint(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "images", "vornik-agent", "entrypoint.sh")
}

// TestAgentToolRegistriesAgree_RealEntrypoint is the security regression test
// for the 2026-08-06 allowlist bypass.
//
// Four registries name agent tools: agenttools.builtinTools (Go),
// BUILTIN_TOOL_NAMES_JSON, is_builtin_tool()'s case list, and exec_tool's
// dispatch cases. Both container-side gates are phrased "is this a builtin?", so
// a name missing from those lists FAILS OPEN:
//
//   - execution (entrypoint.sh, exec_tool) skips the allowlist check entirely;
//   - advertisement offers the tool to every role's model regardless of its
//     allowedTools.
//
// Before the fix, memory_search, skill_fetch, get_conversation_window and
// summarize_thread were implemented and dispatchable while absent from both
// gates — callable by every role irrespective of allowedTools, defeating the
// role-library capability boundary that nl-automation-composer-design.md §5.3
// documents as the outer bound of every composed automation.
//
// This runs against the REAL entrypoint.sh, not a fixture, deliberately: a
// fixture would keep passing while production drifted.
func TestAgentToolRegistriesAgree_RealEntrypoint(t *testing.T) {
	tbl := New()
	tbl.AddAgentToolsGo()
	if err := tbl.AddEntrypointSurfaces(realEntrypoint(t)); err != nil {
		t.Fatalf("extract entrypoint surfaces: %v", err)
	}

	findings := CheckAgentToolAgreement(tbl)
	for _, f := range findings {
		t.Errorf("agent-tool registry disagreement: %s", f)
	}
	if len(findings) > 0 {
		t.Logf("%d disagreement(s); every one is a fail-open gate or a phantom grant", len(findings))
	}
}

// TestAddEntrypointSurfaces_ExtractsAllThree guards the parser itself. A silent
// extraction failure would make the agreement check vacuously green, which is
// the worst outcome available: a security check that passes because it looked at
// nothing.
func TestAddEntrypointSurfaces_ExtractsAllThree(t *testing.T) {
	tbl := New()
	if err := tbl.AddEntrypointSurfaces(realEntrypoint(t)); err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, kind := range []Kind{KindAgentToolAdvertised, KindAgentToolGate, KindAgentToolDispatch} {
		if got := tbl.Names(kind); len(got) < 10 {
			t.Errorf("%s extracted %d names (%v); expected the real registry, "+
				"so the parser has probably gone blind", kind, len(got), got)
		}
	}
	// Sanity: a tool everyone agrees on must appear on all three surfaces.
	for _, kind := range []Kind{KindAgentToolAdvertised, KindAgentToolGate, KindAgentToolDispatch} {
		if tbl.Get(kind, "file_read") == nil {
			t.Errorf("file_read missing from %s — parser is wrong", kind)
		}
	}
}

// TestAddEntrypointSurfaces_RejectsGlobCaseLabels pins that the case-label regex
// does not harvest shell globs as tool names. `-*)`, `""|*[!0-9]*)` and
// `"$WORKSPACE"/.tool_results/*)` all appear in the real file; harvesting them
// would fill the dispatch set with junk and produce phantom findings.
func TestAddEntrypointSurfaces_RejectsGlobCaseLabels(t *testing.T) {
	tbl := New()
	if err := tbl.AddEntrypointSurfaces(realEntrypoint(t)); err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, kind := range []Kind{KindAgentToolGate, KindAgentToolDispatch} {
		for _, n := range tbl.Names(kind) {
			for _, bad := range []string{"*", "[", "]", "/", "$", "\"", "-"} {
				if containsRune(n, bad) {
					t.Errorf("%s harvested a non-identifier %q — the case-label regex is too loose", kind, n)
				}
			}
		}
	}
}

func containsRune(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestUngatedByDesign_EveryExemptionHasAReason — an exemption without a stated
// reason is how "accidentally ungated" gets laundered into "intentionally
// ungated". The map's values are load-bearing documentation.
func TestUngatedByDesign_EveryExemptionHasAReason(t *testing.T) {
	for name, reason := range UngatedByDesign {
		if len(reason) < 20 {
			t.Errorf("UngatedByDesign[%q] has no substantive reason (%q) — "+
				"state why gating it would be wrong, or gate it", name, reason)
		}
	}
}
