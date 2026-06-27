package service

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestFsProjectWriter_FileMode0600 asserts the wizard adapter
// writes new project YAML files at 0o600 — owner-read only.
//
// Originally the writer used 0o644, which made the file world-
// readable. Project YAML can carry inline LLM gateway tokens,
// webhook secrets, and MCP credentials, so 0o644 leaks them to
// any other local user on the host.
//
// The test creates a fresh temp dir, writes via the production
// adapter, then stats the resulting file. Skip on Windows where
// Unix permission bits are advisory.
func TestFsProjectWriter_FileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes are advisory on Windows")
	}
	dir := t.TempDir()
	w := &fsProjectWriter{configsDir: dir}
	url, err := w.Write(context.Background(), "test-project", []byte("projectId: test-project\n"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.HasPrefix(url, "/ui/projects/") {
		t.Errorf("returned URL not /ui/projects/-prefixed: %q", url)
	}
	target := filepath.Join(dir, "projects", "test-project.yaml")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("expected file mode 0o600, got %o", perm)
	}
}

// TestFsProjectWriter_DirMode0700 asserts the projects parent
// directory is created with 0o700 — owner-only listing. A
// world-listable parent (0o755) means any local user can
// enumerate which projects exist by name, even if the YAML
// files themselves are 0o600.
func TestFsProjectWriter_DirMode0700(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes are advisory on Windows")
	}
	dir := t.TempDir()
	w := &fsProjectWriter{configsDir: dir}
	if _, err := w.Write(context.Background(), "test-project", []byte("projectId: test-project\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "projects"))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("expected dir mode 0o700, got %o", perm)
	}
}
