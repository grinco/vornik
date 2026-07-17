package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestFSWorkflowWriter_WritesUnderWorkflowsDir is the happy path: a valid
// workflowID is written to <deployed>/workflows/<id>.md with the given body.
func TestFSWorkflowWriter_WritesUnderWorkflowsDir(t *testing.T) {
	deployed := t.TempDir()
	if err := os.MkdirAll(filepath.Join(deployed, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := &fsWorkflowWriter{deployedConfigDir: deployed}
	body := []byte("---\nworkflowId: wf1\n---\nbody\n")
	if _, err := w.Write(context.Background(), "wf1", body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(deployed, "workflows", "wf1.md"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("content = %q, want %q", got, body)
	}
}

// TestFSWorkflowWriter_RejectsUnsafeID covers the safepath.CleanPathComponent
// guard (path separators, traversal, empty/dot) — no unsafe id may produce a
// file, in or out of the workflows dir.
func TestFSWorkflowWriter_RejectsUnsafeID(t *testing.T) {
	deployed := t.TempDir()
	if err := os.MkdirAll(filepath.Join(deployed, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := &fsWorkflowWriter{deployedConfigDir: deployed}
	for _, id := range []string{"../evil", "a/b", "..", "", ".", `a\b`, "sub/../../etc/x"} {
		if _, err := w.Write(context.Background(), id, []byte("x")); err == nil {
			t.Errorf("Write(%q) expected error, got nil", id)
		}
	}
	entries, err := os.ReadDir(filepath.Join(deployed, "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no files written for unsafe ids, got %d: %v", len(entries), entries)
	}
	// Nothing escaped to the config root either.
	rootEntries, _ := os.ReadDir(deployed)
	if len(rootEntries) != 1 { // only workflows/
		t.Fatalf("expected only workflows/ under deployed root, got %v", rootEntries)
	}
}
