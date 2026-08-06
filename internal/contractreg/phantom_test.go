package contractreg

import (
	"path/filepath"
	"testing"
)

// TestCheckPhantomGrants pins the "a grant that means nothing" class.
func TestCheckPhantomGrants(t *testing.T) {
	tbl := New()
	tbl.Add(KindAgentToolDispatch, "file_read", "entrypoint.sh:1")
	tbl.Add(KindSystemHandler, "forge.post_review", "registry")
	// Declared-but-not-runnable: exactly the asymmetry the agreement check
	// polices. CheckPhantomGrants must NOT treat these as evidence a tool runs,
	// or the two checks would cover for each other.
	tbl.Add(KindAgentToolGo, "ghost_tool", "agenttools.go")
	tbl.Add(KindAgentToolAdvertised, "ghost_tool", "entrypoint.sh:203")

	grants := map[string][]string{
		"configs/role-library/ok.md":     {"file_read", "forge.post_review"},
		"configs/role-library/broken.md": {"nonexistent_tool"},
		"configs/role-library/ghost.md":  {"ghost_tool"},
		"configs/role-library/mcp.md":    {"mcp__scraper__fetch"},
		"configs/role-library/exempt.md": {"tool_search"},
	}

	got := CheckPhantomGrants(tbl, grants)
	byName := map[string]bool{}
	for _, f := range got {
		byName[f.Name] = true
	}

	if !byName["nonexistent_tool"] {
		t.Error("a role granting a tool with no dispatch case must be reported")
	}
	if !byName["ghost_tool"] {
		t.Error("declared in the Go/advertised lists is NOT evidence a tool can run — " +
			"a grant for it is still phantom, and letting it pass would make this check " +
			"depend on the very agreement the other check verifies")
	}
	if byName["file_read"] || byName["forge.post_review"] {
		t.Error("a runnable tool or a system handler must not be reported")
	}
	if byName["mcp__scraper__fetch"] {
		t.Error("mcp__ grants are contract-only — static checking would false-fail on " +
			"every deployment whose MCP servers differ from the author's")
	}
	if byName["tool_search"] {
		t.Error("UngatedByDesign names must be exempt here too")
	}
}

// TestAuditBuildTags_RealTree finds the real never-set tag. Regression for the
// 2026-08-06 finding that smoke_codex gates a test which therefore never runs.
func TestAuditBuildTags_RealTree(t *testing.T) {
	root := repoRoot(t)
	audit, err := AuditBuildTags(root, []string{
		filepath.Join(root, "Makefile"),
		filepath.Join(root, ".github", "workflows"),
	})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}

	// Sanity: the parser must actually find the tags this repo uses, or a green
	// result would mean nothing.
	if len(audit.FilesByTag["integration"]) < 10 {
		t.Errorf("expected many integration-tagged files, got %d — parser is blind",
			len(audit.FilesByTag["integration"]))
	}
	if !audit.SetTags["integration"] {
		t.Error("integration is set in the Makefile; the plumbing scan missed it")
	}

	neverSet := audit.NeverSet()
	t.Logf("tags gating files but never set: %v", neverSet)

	findings := CheckNeverSetBuildTags(audit)
	if len(neverSet) != len(findings) {
		t.Errorf("expected one finding per never-set tag, got %d for %v", len(findings), neverSet)
	}
	// smoke_codex is the known instance; assert the test-file wording fires.
	for _, f := range findings {
		if f.Name == "smoke_codex" {
			if !containsRune(f.Detail, "NEVER RUN") {
				t.Errorf("smoke_codex gates only _test.go files, so the finding must say the "+
					"tests never run; got: %s", f.Detail)
			}
		}
	}
}

// TestAllTestFiles covers the helper that decides the "tests never run" wording.
func TestAllTestFiles(t *testing.T) {
	if !allTestFiles([]string{"a/b_test.go"}) {
		t.Error("single test file should be all-test")
	}
	if allTestFiles([]string{"a/b_test.go", "a/c.go"}) {
		t.Error("mixed set is not all-test")
	}
	if allTestFiles(nil) {
		t.Error("empty set is not all-test")
	}
}
