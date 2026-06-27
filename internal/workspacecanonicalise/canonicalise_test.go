package workspacecanonicalise

import (
	"os"
	"path/filepath"
	"testing"
)

// fixture helpers — each test builds an isolated workspace tree
// so the file-rename steps don't leak across cases.

func makeProject(t *testing.T, root, projectID string, dirs ...string) string {
	t.Helper()
	dir := filepath.Join(root, projectID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for _, d := range dirs {
		full := filepath.Join(dir, d)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		// Drop a marker file so the rename actually preserves
		// content. Catches a bug where the rename would
		// inadvertently lose data.
		if err := os.WriteFile(filepath.Join(full, "PROJECT_CONTEXT.md"), []byte("# spec"), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return dir
}

// TestCanonicaliseAll_Migrates pins the happy path: a workspace
// with only autonomy/ gets renamed; the marker file lands at
// the new location.
func TestCanonicaliseAll_Migrates(t *testing.T) {
	root := t.TempDir()
	dir := makeProject(t, root, "alpha", "autonomy")

	results, err := CanonicaliseAll(root)
	if err != nil {
		t.Fatalf("CanonicaliseAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Outcome != OutcomeMigrated {
		t.Errorf("outcome = %q, want migrated", results[0].Outcome)
	}
	// .autonomy/ exists, autonomy/ gone.
	if _, err := os.Stat(filepath.Join(dir, ".autonomy", "PROJECT_CONTEXT.md")); err != nil {
		t.Errorf("PROJECT_CONTEXT.md should be at .autonomy/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "autonomy")); !os.IsNotExist(err) {
		t.Errorf("autonomy/ should be gone")
	}
}

// TestCanonicaliseAll_AlreadyCanonical: .autonomy/ exists, no
// autonomy/ — no-op.
func TestCanonicaliseAll_AlreadyCanonical(t *testing.T) {
	root := t.TempDir()
	dir := makeProject(t, root, "alpha", ".autonomy")
	results, _ := CanonicaliseAll(root)
	if results[0].Outcome != OutcomeAlreadyCanonical {
		t.Errorf("outcome = %q, want already_canonical", results[0].Outcome)
	}
	// File still there.
	if _, err := os.Stat(filepath.Join(dir, ".autonomy", "PROJECT_CONTEXT.md")); err != nil {
		t.Errorf("PROJECT_CONTEXT.md should still be at .autonomy/: %v", err)
	}
}

// TestCanonicaliseAll_MixedSkipped: both directories present —
// outcome=mixed, neither directory touched.
func TestCanonicaliseAll_MixedSkipped(t *testing.T) {
	root := t.TempDir()
	dir := makeProject(t, root, "alpha", "autonomy", ".autonomy")
	results, _ := CanonicaliseAll(root)
	if results[0].Outcome != OutcomeMixed {
		t.Errorf("outcome = %q, want mixed", results[0].Outcome)
	}
	// Both intact.
	if _, err := os.Stat(filepath.Join(dir, "autonomy")); err != nil {
		t.Errorf("autonomy/ should still exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".autonomy")); err != nil {
		t.Errorf(".autonomy/ should still exist: %v", err)
	}
}

// TestCanonicaliseAll_NoConvention: neither dir present — no-op.
func TestCanonicaliseAll_NoConvention(t *testing.T) {
	root := t.TempDir()
	makeProject(t, root, "alpha") // no .autonomy/ or autonomy/
	results, _ := CanonicaliseAll(root)
	if results[0].Outcome != OutcomeNoConvention {
		t.Errorf("outcome = %q, want no_convention", results[0].Outcome)
	}
}

// TestScan_NeverWrites confirms the dry-run scanner doesn't
// rename anything — the same Result shape comes back as
// CanonicaliseAll would emit, but the legacy directory survives.
func TestScan_NeverWrites(t *testing.T) {
	root := t.TempDir()
	dir := makeProject(t, root, "alpha", "autonomy")
	results, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if results[0].Outcome != OutcomeMigrated {
		t.Errorf("scan should report would-migrate; got %q", results[0].Outcome)
	}
	// autonomy/ still present after the scan.
	if _, err := os.Stat(filepath.Join(dir, "autonomy")); err != nil {
		t.Errorf("scan should not rename; autonomy/ disappeared: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".autonomy")); !os.IsNotExist(err) {
		t.Errorf("scan should not create .autonomy/; got existence")
	}
}

// TestCanonicaliseOne_Selective: --project mode operates on one
// workspace without touching siblings.
func TestCanonicaliseOne_Selective(t *testing.T) {
	root := t.TempDir()
	dirA := makeProject(t, root, "alpha", "autonomy")
	dirB := makeProject(t, root, "beta", "autonomy")
	res, err := CanonicaliseOne(root, "alpha", false)
	if err != nil {
		t.Fatalf("CanonicaliseOne: %v", err)
	}
	if res.Outcome != OutcomeMigrated {
		t.Errorf("outcome = %q, want migrated", res.Outcome)
	}
	// alpha migrated; beta untouched.
	if _, err := os.Stat(filepath.Join(dirA, ".autonomy")); err != nil {
		t.Errorf("alpha should be migrated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirB, "autonomy")); err != nil {
		t.Errorf("beta should be untouched: %v", err)
	}
}

// TestCanonicaliseOne_MissingWorkspace: operator typo → returns
// OutcomeNoConvention rather than erroring.
func TestCanonicaliseOne_MissingWorkspace(t *testing.T) {
	root := t.TempDir()
	res, err := CanonicaliseOne(root, "ghost", false)
	if err != nil {
		t.Fatalf("CanonicaliseOne: %v", err)
	}
	if res.Outcome != OutcomeNoConvention {
		t.Errorf("missing workspace should be no_convention; got %q", res.Outcome)
	}
}

// TestCountAndListHelpers pin the operator-facing accessors.
func TestCountAndListHelpers(t *testing.T) {
	root := t.TempDir()
	_ = makeProject(t, root, "alpha", "autonomy")              // migrated
	_ = makeProject(t, root, "beta", ".autonomy")              // canonical
	_ = makeProject(t, root, "gamma", "autonomy", ".autonomy") // mixed
	_ = makeProject(t, root, "delta")                          // none

	results, _ := Scan(root)
	if got := CountLegacy(results); got != 1 {
		t.Errorf("CountLegacy = %d, want 1", got)
	}
	if got := CountMixed(results); got != 1 {
		t.Errorf("CountMixed = %d, want 1", got)
	}
	if got := LegacyProjects(results); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("LegacyProjects = %v, want [alpha]", got)
	}
	if got := MixedProjects(results); len(got) != 1 || got[0] != "gamma" {
		t.Errorf("MixedProjects = %v, want [gamma]", got)
	}
}

// TestScan_SkipsDottedTopLevel — the workspace root may contain
// scratch dirs like .tmp/; the scanner must skip them rather than
// treating them as workspaces. Catches a regression that would
// loop on hidden ops dirs.
func TestScan_SkipsDottedTopLevel(t *testing.T) {
	root := t.TempDir()
	makeProject(t, root, "alpha", "autonomy")
	// Sneaky: a top-level .tmp/ that contains an autonomy/
	// directory of its own.
	if err := os.MkdirAll(filepath.Join(root, ".tmp", "autonomy"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	results, _ := Scan(root)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (.tmp/ should be skipped)", len(results))
	}
	if results[0].ProjectID != "alpha" {
		t.Errorf("only alpha should be reported; got %q", results[0].ProjectID)
	}
}

// TestCanonicaliseAll_EmptyRoot returns nil + nil for a root
// that exists but has no subdirectories. Defensive against fresh
// deployments.
func TestCanonicaliseAll_EmptyRoot(t *testing.T) {
	root := t.TempDir()
	results, err := CanonicaliseAll(root)
	if err != nil {
		t.Fatalf("empty root should not error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results should be empty; got %v", results)
	}
}

// TestCanonicaliseAll_MissingRoot returns an explicit error so
// the CLI surfaces "your --workspaces-root path doesn't exist".
func TestCanonicaliseAll_MissingRoot(t *testing.T) {
	_, err := CanonicaliseAll(filepath.Join(t.TempDir(), "doesnotexist"))
	if err == nil {
		t.Errorf("expected error for missing root")
	}
}
