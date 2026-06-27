package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/registry"
)

// helper: build a workspace with the named files (relative paths)
// already populated.
func makeWorkspace(t *testing.T, rel ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range rel {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte("prior content"), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return dir
}

// TestApplyWorkflowArtifactCleanup_DeletesListedFiles — the canonical
// happy path that closes the research.md leak: the canonical artifact
// is removed before the workflow's entrypoint step starts.
func TestApplyWorkflowArtifactCleanup_DeletesListedFiles(t *testing.T) {
	ws := makeWorkspace(t,
		"artifacts/out/research.md",
		"artifacts/out/deliverable.md",
		"artifacts/out/summary.txt",
	)
	wf := &registry.Workflow{
		ID: "research",
		CleanupArtifacts: []string{
			"artifacts/out/research.md",
			"artifacts/out/deliverable.md",
			"artifacts/out/summary.txt",
		},
	}
	res := applyWorkflowArtifactCleanup(ws, wf, zerolog.Nop())
	if len(res.Deleted) != 3 {
		t.Fatalf("expected 3 deletions, got %+v", res)
	}
	for _, rel := range wf.CleanupArtifacts {
		if _, err := os.Stat(filepath.Join(ws, rel)); !os.IsNotExist(err) {
			t.Fatalf("file %q still present after cleanup (err=%v)", rel, err)
		}
	}
}

// TestApplyWorkflowArtifactCleanup_MissingFilesAreSilentlyOK — when
// the prior task didn't write the file (or the project is fresh),
// the cleanup must be a no-op for that path, not an error.
func TestApplyWorkflowArtifactCleanup_MissingFilesAreSilentlyOK(t *testing.T) {
	ws := makeWorkspace(t) // no files inside
	wf := &registry.Workflow{
		ID:               "research",
		CleanupArtifacts: []string{"artifacts/out/research.md"},
	}
	res := applyWorkflowArtifactCleanup(ws, wf, zerolog.Nop())
	if len(res.Deleted) != 0 {
		t.Fatalf("expected 0 deletions, got %+v", res)
	}
	if len(res.Missing) != 1 {
		t.Fatalf("expected 1 missing path, got %+v", res)
	}
	if len(res.Errored) != 0 {
		t.Fatalf("missing-files must not be errored, got %+v", res)
	}
}

// TestApplyWorkflowArtifactCleanup_FieldAbsentIsNoop — the cleanup
// is opt-in: workflows that don't set CleanupArtifacts get exactly
// today's behaviour.
func TestApplyWorkflowArtifactCleanup_FieldAbsentIsNoop(t *testing.T) {
	ws := makeWorkspace(t, "artifacts/out/research.md")
	wf := &registry.Workflow{ID: "no-cleanup-listed"} // empty CleanupArtifacts
	res := applyWorkflowArtifactCleanup(ws, wf, zerolog.Nop())
	if len(res.Deleted)+len(res.Missing)+len(res.Errored)+len(res.Skipped) != 0 {
		t.Fatalf("noop expected, got %+v", res)
	}
	// File must remain — the workflow didn't ask for it to be cleaned.
	if _, err := os.Stat(filepath.Join(ws, "artifacts/out/research.md")); err != nil {
		t.Fatalf("file vanished without opt-in: %v", err)
	}
}

// TestApplyWorkflowArtifactCleanup_EmptyWorkspaceDirIsNoop — defense
// against a misconfigured executor where the workspace path didn't
// resolve. Better to silently skip than panic on join.
func TestApplyWorkflowArtifactCleanup_EmptyWorkspaceDirIsNoop(t *testing.T) {
	wf := &registry.Workflow{
		ID:               "research",
		CleanupArtifacts: []string{"artifacts/out/research.md"},
	}
	res := applyWorkflowArtifactCleanup("", wf, zerolog.Nop())
	if len(res.Deleted) != 0 {
		t.Fatalf("expected no-op on empty workspaceDir, got %+v", res)
	}
}

// TestApplyWorkflowArtifactCleanup_RejectsAbsolutePath — the helper
// is workspace-scoped; an entry like "/etc/passwd" must NOT escape.
func TestApplyWorkflowArtifactCleanup_RejectsAbsolutePath(t *testing.T) {
	ws := makeWorkspace(t)
	wf := &registry.Workflow{
		ID:               "research",
		CleanupArtifacts: []string{"/etc/passwd"},
	}
	res := applyWorkflowArtifactCleanup(ws, wf, zerolog.Nop())
	if len(res.Skipped) != 1 {
		t.Fatalf("expected 1 skipped absolute path, got %+v", res)
	}
}

// TestApplyWorkflowArtifactCleanup_RejectsParentTraversal — same
// reason as above: a `../other-project/file` entry must NOT be
// honoured.
func TestApplyWorkflowArtifactCleanup_RejectsParentTraversal(t *testing.T) {
	ws := makeWorkspace(t)
	wf := &registry.Workflow{
		ID:               "research",
		CleanupArtifacts: []string{"../escape.txt", "..", "x/../../escape.txt"},
	}
	res := applyWorkflowArtifactCleanup(ws, wf, zerolog.Nop())
	if len(res.Skipped) != 3 {
		t.Fatalf("expected 3 skipped, got %+v", res)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("nothing should have been deleted, got %+v", res)
	}
}

// TestApplyWorkflowArtifactCleanup_RefusesDirectory — operators may
// type a path that resolves to a directory; the helper must NOT
// recurse-delete a tree.
func TestApplyWorkflowArtifactCleanup_RefusesDirectory(t *testing.T) {
	ws := makeWorkspace(t, "artifacts/out/research.md") // creates artifacts/out/ as a dir
	wf := &registry.Workflow{
		ID:               "research",
		CleanupArtifacts: []string{"artifacts/out"}, // a directory
	}
	res := applyWorkflowArtifactCleanup(ws, wf, zerolog.Nop())
	if len(res.Skipped) != 1 {
		t.Fatalf("expected directory to be skipped, got %+v", res)
	}
	// The file inside the directory must still be present — the
	// directory entry was refused.
	if _, err := os.Stat(filepath.Join(ws, "artifacts/out/research.md")); err != nil {
		t.Fatalf("file inside directory was wrongly removed: %v", err)
	}
}

// TestApplyWorkflowArtifactCleanup_NilWorkflowIsNoop — defensive
// surface: a nil *Workflow passed accidentally must yield a clean
// zero-value result.
func TestApplyWorkflowArtifactCleanup_NilWorkflowIsNoop(t *testing.T) {
	ws := makeWorkspace(t, "artifacts/out/research.md")
	res := applyWorkflowArtifactCleanup(ws, nil, zerolog.Nop())
	if len(res.Deleted) != 0 {
		t.Fatalf("expected no-op on nil workflow, got %+v", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "artifacts/out/research.md")); err != nil {
		t.Fatalf("file vanished on nil workflow: %v", err)
	}
}

// TestIsSafeWorkspacePath_TableDriven pins the path-safety guard.
func TestIsSafeWorkspacePath_TableDriven(t *testing.T) {
	cases := []struct {
		path string
		safe bool
	}{
		{"artifacts/out/research.md", true},
		{"./artifacts/out/research.md", true},
		{"a/b/c.txt", true},
		{"", false},
		{".", false},
		{"..", false},
		{"../outside", false},
		{"x/../../outside", false},
		{"/etc/passwd", false},
	}
	for _, tc := range cases {
		got := isSafeWorkspacePath(tc.path)
		if got != tc.safe {
			t.Errorf("isSafeWorkspacePath(%q) = %v, want %v", tc.path, got, tc.safe)
		}
	}
}
