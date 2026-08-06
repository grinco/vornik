package main

import (
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/contractreg"
)

// TestClassify_TestReachableIsNotDead pins the correction that took the reported
// count from 195 to 7.
//
// `deadcode` ignores test files by default, so a production helper exercised only
// from _test.go appears unreachable. The first real triage surfaced
// ContextWithScopeForTesting — a function that NAMES itself a test seam — as
// "likely dead". Deleting it, or any of the 187 like it, would have broken the
// suite while the tool reported success.
//
// A symbol is only unreferenced when it is unreachable in BOTH runs: without
// tests, and with them.
func TestClassify_TestReachableIsNotDead(t *testing.T) {
	table := contractreg.New()

	deadEverywhere := symbol{Pkg: "p", Func: "TrulyDead", File: "p/a.go", Line: 1}
	testReachable := symbol{Pkg: "p", Func: "ContextWithScopeForTesting", File: "p/b.go", Line: 2}
	inATestFile := symbol{Pkg: "p", Func: "helper", File: "p/c_test.go", Line: 3}

	unreachable := map[string]symbol{
		deadEverywhere.key(): deadEverywhere,
		testReachable.key():  testReachable,
		inATestFile.key():    inATestFile,
	}
	// The test-inclusive run still cannot reach TrulyDead, but DOES reach
	// testReachable — so only the former survives as unreferenced.
	stillDeadWithTests := map[string]symbol{
		deadEverywhere.key(): deadEverywhere,
		inATestFile.key():    inATestFile,
	}

	unref, testOnly, rescued := classify(table, unreachable, stillDeadWithTests)

	if len(unref) != 1 || unref[0].Func != "TrulyDead" {
		t.Errorf("unreferenced = %v, want exactly TrulyDead", funcNames(unref))
	}
	if len(testOnly) != 2 {
		t.Errorf("test-only = %v, want the _test.go symbol AND the test-reachable "+
			"production helper — the second is the class that made the count wrong", funcNames(testOnly))
	}
	if len(rescued) != 0 {
		t.Errorf("rescued = %v, want none (empty contract table)", funcNames(rescued))
	}
}

// TestClassify_ContractRescueBeatsDeadVerdict — a symbol named by a contract is
// live-by-contract even with no caller. This is the axis that makes the whole
// model work: name-dispatched entry points must never be reported as dead.
func TestClassify_ContractRescueBeatsDeadVerdict(t *testing.T) {
	table := contractreg.New()
	table.Add(contractreg.KindSystemHandler, "forge.post_review", "wiring")
	table.Add(contractreg.KindAgentToolDispatch, "memory_search", "entrypoint.sh:1")

	named := symbol{Pkg: "p", Func: "MemorySearch", File: "p/a.go", Line: 1}
	unnamed := symbol{Pkg: "p", Func: "Whatever", File: "p/b.go", Line: 2}
	unreachable := map[string]symbol{named.key(): named, unnamed.key(): unnamed}

	unref, _, rescued := classify(table, unreachable, unreachable)

	if len(rescued) != 1 || rescued[0].Func != "MemorySearch" {
		t.Errorf("rescued = %v, want MemorySearch (CamelCase must match the "+
			"snake_case contract name memory_search)", funcNames(rescued))
	}
	if len(unref) != 1 || unref[0].Func != "Whatever" {
		t.Errorf("unreferenced = %v, want only Whatever", funcNames(unref))
	}
}

// TestNamedByContract_SpellingForms — contract names are snake_case, Go symbols
// are CamelCase, and methods arrive as Receiver.Method.
func TestNamedByContract_SpellingForms(t *testing.T) {
	table := contractreg.New()
	table.Add(contractreg.KindAgentToolDispatch, "summarize_thread", "e:1")

	for _, in := range []string{"SummarizeThread", "summarize_thread", "Handler.SummarizeThread"} {
		if !namedByContract(table, in) {
			t.Errorf("namedByContract(%q) = false, want true", in)
		}
	}
	if namedByContract(table, "SomethingElse") {
		t.Error("an unrelated symbol must not be rescued")
	}
}

// TestNamedByContract_IgnoresDeclaredOnly — an LLD `delivers:` promise is not
// evidence that code is live. Treating prose as reachability ground truth would
// couple a static-analysis result to doc upkeep.
func TestNamedByContract_IgnoresDeclaredOnly(t *testing.T) {
	table := contractreg.New()
	table.AddWithStatus(contractreg.KindDeclared, "promised_thing", "some-design.md", "design")
	if namedByContract(table, "promised_thing") {
		t.Error("a delivers: entry must NOT rescue a symbol — see the reachability design §11")
	}
}

func TestToSnake(t *testing.T) {
	cases := map[string]string{
		"SummarizeThread": "summarize_thread",
		"MemorySearch":    "memory_search",
		"lower":           "lower",
		"ABC":             "a_b_c",
		"":                "",
	}
	for in, want := range cases {
		if got := toSnake(in); got != want {
			t.Errorf("toSnake(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMainPackages_FindsEveryEntryPoint — "dead" only means anything relative to
// the FULL set of mains, so a missed entry point silently converts live code into
// a dead verdict.
func TestMainPackages_FindsEveryEntryPoint(t *testing.T) {
	root := repoRootForTest(t)
	got, err := mainPackages(root)
	if err != nil {
		t.Fatalf("mainPackages: %v", err)
	}
	if len(got) < 8 {
		t.Fatalf("found only %d main packages (%v) — a missed entry point turns live "+
			"code into a dead verdict", len(got), got)
	}
	// Both edition assembly mains must be present: that is what makes the
	// EE/CE union fall out without an export-then-analyse dance (§5.1).
	want := map[string]bool{"./cmd/vornik": false, "./cmd/vornik-enterprise": false}
	for _, g := range got {
		if _, ok := want[g]; ok {
			want[g] = true
		}
	}
	for pkg, found := range want {
		if !found {
			t.Errorf("%s missing from the main set — per-edition reachability breaks without it", pkg)
		}
	}
}

// TestCeiling_MalformedIsAnError — a corrupt ceiling that read as 0 would turn the
// gate off exactly when someone had broken something.
func TestCeiling_MalformedIsAnError(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{"nope\n", "-1\n", "# comment only\n"} {
		p := filepath.Join(dir, "c.txt")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadCeiling(p); err == nil {
			t.Errorf("loadCeiling(%q) returned no error", body)
		}
	}
}

// TestCeiling_RoundTrip — writeCeiling's output must be readable by loadCeiling,
// comment header and all.
func TestCeiling_RoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "c.txt")
	if err := writeCeiling(p, 42); err != nil {
		t.Fatalf("writeCeiling: %v", err)
	}
	got, err := loadCeiling(p)
	if err != nil {
		t.Fatalf("loadCeiling: %v", err)
	}
	if got != 42 {
		t.Errorf("round-trip = %d, want 42", got)
	}
}

func funcNames(syms []symbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.Func)
	}
	return out
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repoRoot: %v", err)
	}
	return root
}
