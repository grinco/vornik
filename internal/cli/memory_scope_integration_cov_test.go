//go:build integration

package cli

// Integration coverage for `vornikctl memory scope {list,retag}`
// (memory_scope.go). Drives the run* handlers against the live test
// Postgres via the dbcov* harness. Asserts both the human-facing
// output and the real DB effect of retag.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func memoryScope_reset() {
	memoryScopeListProject, memoryScopeListJSON = "", false
	memoryScopeRetagProject, memoryScopeRetagFrom, memoryScopeRetagTo = "", "", ""
	memoryScopeRetagPattern = ""
	memoryScopeRetagDryRun, memoryScopeRetagYes = false, false
}

func TestIntegration_MemoryScopeList_TableAndCounts(t *testing.T) {
	db := dbcovSetup(t)
	memoryScope_reset()

	proj := dbcovUniqueProject("scope-list")
	dbcovCleanupProject(t, db, proj)
	// 2 chunks in scope github.com/acme/a, 1 in /b, 1 uncategorized.
	dbcovSeedChunk(t, db, proj, "docs-1", "alpha", "github.com/acme/a")
	dbcovSeedChunk(t, db, proj, "docs-2", "beta", "github.com/acme/a")
	dbcovSeedChunk(t, db, proj, "docs-3", "gamma", "github.com/acme/b")
	dbcovSeedChunk(t, db, proj, "docs-4", "delta", "")

	memoryScopeListProject = proj
	out, err := dbcovCapture(t, func() error { return runMemoryScopeList(memoryScopeListCmd, nil) })
	if err != nil {
		t.Fatalf("runMemoryScopeList: %v", err)
	}
	if !strings.Contains(out, "SCOPE") || !strings.Contains(out, "CHUNKS") {
		t.Fatalf("missing table header:\n%s", out)
	}
	if !strings.Contains(out, "github.com/acme/a") {
		t.Errorf("expected acme/a scope row:\n%s", out)
	}
	if !strings.Contains(out, "<uncategorized>") {
		t.Errorf("expected uncategorized bucket:\n%s", out)
	}
}

func TestIntegration_MemoryScopeList_JSON(t *testing.T) {
	db := dbcovSetup(t)
	memoryScope_reset()

	proj := dbcovUniqueProject("scope-json")
	dbcovCleanupProject(t, db, proj)
	dbcovSeedChunk(t, db, proj, "s", "x", "github.com/acme/j")

	memoryScopeListProject = proj
	memoryScopeListJSON = true
	out, err := dbcovCapture(t, func() error { return runMemoryScopeList(memoryScopeListCmd, nil) })
	if err != nil {
		t.Fatalf("runMemoryScopeList json: %v", err)
	}
	var parsed struct {
		Project string `json:"project"`
		Scopes  []struct {
			Scope  string `json:"scope"`
			Chunks int    `json:"chunks"`
		} `json:"scopes"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if parsed.Project != proj || parsed.Total != 1 || len(parsed.Scopes) != 1 {
		t.Fatalf("unexpected json: %+v", parsed)
	}
	if parsed.Scopes[0].Scope != "github.com/acme/j" || parsed.Scopes[0].Chunks != 1 {
		t.Errorf("scope row wrong: %+v", parsed.Scopes[0])
	}
}

func TestIntegration_MemoryScopeList_EmptyProject(t *testing.T) {
	db := dbcovSetup(t)
	memoryScope_reset()
	proj := dbcovUniqueProject("scope-empty")
	dbcovCleanupProject(t, db, proj)

	memoryScopeListProject = proj
	out, err := dbcovCapture(t, func() error { return runMemoryScopeList(memoryScopeListCmd, nil) })
	if err != nil {
		t.Fatalf("runMemoryScopeList: %v", err)
	}
	if !strings.Contains(out, "no chunks for project") {
		t.Errorf("expected empty notice, got:\n%s", out)
	}
}

func TestIntegration_MemoryScopeRetag_RejectsEmptyTo(t *testing.T) {
	dbcovSetup(t)
	memoryScope_reset()
	memoryScopeRetagProject = "anything"
	memoryScopeRetagTo = "   " // whitespace trims to empty
	err := runMemoryScopeRetag(memoryScopeRetagCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--to must be non-empty") {
		t.Fatalf("expected --to guard, got %v", err)
	}
}

func TestIntegration_MemoryScopeRetag_DryRunNoWrite(t *testing.T) {
	db := dbcovSetup(t)
	memoryScope_reset()
	proj := dbcovUniqueProject("scope-dry")
	dbcovCleanupProject(t, db, proj)
	dbcovSeedChunk(t, db, proj, "s", "a", "") // uncategorized

	memoryScopeRetagProject = proj
	memoryScopeRetagTo = "github.com/acme/new"
	memoryScopeRetagDryRun = true
	out, err := dbcovCapture(t, func() error { return runMemoryScopeRetag(memoryScopeRetagCmd, nil) })
	if err != nil {
		t.Fatalf("retag dry-run: %v", err)
	}
	if !strings.Contains(out, "Affected chunks:   1") || !strings.Contains(out, "dry run") {
		t.Fatalf("dry-run output wrong:\n%s", out)
	}
	// DB unchanged: still NULL.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id=$1 AND repo_scope IS NULL`, proj).Scan(&n); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if n != 1 {
		t.Errorf("dry-run mutated the DB: %d rows still NULL, want 1", n)
	}
}

func TestIntegration_MemoryScopeRetag_PromotesUncategorized(t *testing.T) {
	db := dbcovSetup(t)
	memoryScope_reset()
	proj := dbcovUniqueProject("scope-promote")
	dbcovCleanupProject(t, db, proj)
	dbcovSeedChunk(t, db, proj, "docs-1", "a", "")
	dbcovSeedChunk(t, db, proj, "docs-2", "b", "")
	dbcovSeedChunk(t, db, proj, "other", "c", "github.com/acme/keep") // must NOT move

	memoryScopeRetagProject = proj
	memoryScopeRetagTo = "github.com/acme/promoted"
	memoryScopeRetagYes = true // skip prompt
	out, err := dbcovCapture(t, func() error { return runMemoryScopeRetag(memoryScopeRetagCmd, nil) })
	if err != nil {
		t.Fatalf("retag: %v", err)
	}
	if !strings.Contains(out, "Retagged 2 chunks.") {
		t.Fatalf("expected 2 retagged:\n%s", out)
	}
	var promoted, kept int
	_ = db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id=$1 AND repo_scope=$2`, proj, "github.com/acme/promoted").Scan(&promoted)
	_ = db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id=$1 AND repo_scope=$2`, proj, "github.com/acme/keep").Scan(&kept)
	if promoted != 2 {
		t.Errorf("promoted = %d, want 2", promoted)
	}
	if kept != 1 {
		t.Errorf("kept = %d, want 1 (already-scoped chunk wrongly moved)", kept)
	}
}

func TestIntegration_MemoryScopeRetag_FromSpecificScopeWithPattern(t *testing.T) {
	db := dbcovSetup(t)
	memoryScope_reset()
	proj := dbcovUniqueProject("scope-pattern")
	dbcovCleanupProject(t, db, proj)
	dbcovSeedChunk(t, db, proj, "lld-arch", "a", "github.com/acme/old")  // matches from+pattern
	dbcovSeedChunk(t, db, proj, "readme", "b", "github.com/acme/old")    // matches from, not pattern
	dbcovSeedChunk(t, db, proj, "lld-other", "c", "github.com/acme/oth") // matches pattern, not from

	memoryScopeRetagProject = proj
	memoryScopeRetagFrom = "github.com/acme/old"
	memoryScopeRetagTo = "github.com/acme/fixed"
	memoryScopeRetagPattern = "lld-%"
	memoryScopeRetagYes = true
	out, err := dbcovCapture(t, func() error { return runMemoryScopeRetag(memoryScopeRetagCmd, nil) })
	if err != nil {
		t.Fatalf("retag: %v", err)
	}
	if !strings.Contains(out, "Retagged 1 chunks.") {
		t.Fatalf("expected exactly 1 retagged:\n%s", out)
	}
	if !strings.Contains(out, "Source-name LIKE:  lld-%") {
		t.Errorf("pattern not echoed:\n%s", out)
	}
	var fixed int
	_ = db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id=$1 AND repo_scope=$2`, proj, "github.com/acme/fixed").Scan(&fixed)
	if fixed != 1 {
		t.Errorf("fixed = %d, want 1", fixed)
	}
}

func TestIntegration_MemoryScopeRetag_InteractiveConfirmYes(t *testing.T) {
	db := dbcovSetup(t)
	memoryScope_reset()
	proj := dbcovUniqueProject("scope-prompt")
	dbcovCleanupProject(t, db, proj)
	dbcovSeedChunk(t, db, proj, "s", "a", "")

	// Feed "y" on stdin so the interactive confirmation accepts.
	r, w, _ := os.Pipe()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()
	_, _ = w.WriteString("y\n")
	_ = w.Close()

	memoryScopeRetagProject = proj
	memoryScopeRetagTo = "github.com/acme/confirmed"
	// memoryScopeRetagYes left false → prompt path.
	out, err := dbcovCapture(t, func() error { return runMemoryScopeRetag(memoryScopeRetagCmd, nil) })
	_ = r.Close()
	if err != nil {
		t.Fatalf("retag confirm: %v", err)
	}
	if !strings.Contains(out, "Retagged 1 chunks.") {
		t.Fatalf("expected retag after confirm:\n%s", out)
	}
}

func TestIntegration_MemoryScopeRetag_InteractiveConfirmAbort(t *testing.T) {
	db := dbcovSetup(t)
	memoryScope_reset()
	proj := dbcovUniqueProject("scope-prompt-no")
	dbcovCleanupProject(t, db, proj)
	dbcovSeedChunk(t, db, proj, "s", "a", "")

	r, w, _ := os.Pipe()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()
	_, _ = w.WriteString("n\n")
	_ = w.Close()

	memoryScopeRetagProject = proj
	memoryScopeRetagTo = "github.com/acme/nope"
	_, err := dbcovCapture(t, func() error { return runMemoryScopeRetag(memoryScopeRetagCmd, nil) })
	_ = r.Close()
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("expected abort on 'n', got %v", err)
	}
	// Still NULL — nothing written.
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id=$1 AND repo_scope IS NULL`, proj).Scan(&n)
	if n != 1 {
		t.Errorf("abort mutated DB: %d NULL rows, want 1", n)
	}
}

func TestIntegration_MemoryScopeRetag_NothingToDo(t *testing.T) {
	db := dbcovSetup(t)
	memoryScope_reset()
	proj := dbcovUniqueProject("scope-noop")
	dbcovCleanupProject(t, db, proj)
	// No uncategorized chunks → count 0 → "(nothing to do)".
	dbcovSeedChunk(t, db, proj, "s", "a", "github.com/acme/x")

	memoryScopeRetagProject = proj
	memoryScopeRetagTo = "github.com/acme/y"
	memoryScopeRetagYes = true
	out, err := dbcovCapture(t, func() error { return runMemoryScopeRetag(memoryScopeRetagCmd, nil) })
	if err != nil {
		t.Fatalf("retag: %v", err)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("expected nothing-to-do:\n%s", out)
	}
}
