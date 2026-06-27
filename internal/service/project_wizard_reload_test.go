package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestFSProjectWriterTriggersReload covers the 2026-05-30 fix: after a
// successful project write, the writer triggers a synchronous config
// reload so the new project is registered in-memory before the commit
// endpoint's /ui/projects/{id} redirect (previously the redirect raced
// the async file-watcher → "project not found" until restart).
func TestFSProjectWriterTriggersReload(t *testing.T) {
	dir := t.TempDir()
	reloads := 0
	w := newFSProjectWriter(dir, func() error { reloads++; return nil })

	url, err := w.Write(context.Background(), "proj-test", []byte("projectId: proj-test\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if url != "/ui/projects/proj-test" {
		t.Errorf("url = %q, want /ui/projects/proj-test", url)
	}
	if reloads != 1 {
		t.Errorf("reload called %d times, want exactly 1", reloads)
	}
	if _, err := os.Stat(filepath.Join(dir, "projects", "proj-test.yaml")); err != nil {
		t.Errorf("project file not written: %v", err)
	}
}

// TestFSProjectWriterReloadFailureDoesNotFailCommit — the file is
// already on disk when reload runs, so a reload error must not fail the
// commit (the file-watcher remains the fallback). A failed commit on a
// written file would be worse: the operator sees an error for a project
// that actually exists.
func TestFSProjectWriterReloadFailureDoesNotFailCommit(t *testing.T) {
	dir := t.TempDir()
	w := newFSProjectWriter(dir, func() error { return errors.New("reload boom") })
	if _, err := w.Write(context.Background(), "proj-reload-fail", []byte("x")); err != nil {
		t.Fatalf("reload failure must not fail Write, got %v", err)
	}
}

// TestFSProjectWriterNilReloadOK — no reloader wired (no-watcher /
// reloader-less deployment) must still write cleanly.
func TestFSProjectWriterNilReloadOK(t *testing.T) {
	dir := t.TempDir()
	w := newFSProjectWriter(dir, nil)
	if _, err := w.Write(context.Background(), "proj-nil-reload", []byte("x")); err != nil {
		t.Fatalf("Write with nil reload: %v", err)
	}
}
