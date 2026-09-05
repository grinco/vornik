package agentloop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// September 2026 code audit: an existing dangling symlink was treated as a
// nonexistent lexical tail, letting file_write create its target outside ws.
func TestAuditWriteDanglingSymlink(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "created")
	if err := os.Symlink(target, filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	got := Dispatch(Env{Workspace: ws}, "file_write", json.RawMessage(`{"path":"link","content":"audit"}`))
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("outside file created: %v; response %s", err, got)
	}
	if !strings.HasPrefix(got, "ERROR:") {
		t.Errorf("expected refusal, got %s", got)
	}
}

// September 2026 code audit: the predictable edit temporary filename could
// be a repository-supplied symlink to a writable file outside the workspace.
func TestAuditEditTempSymlink(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "protected")
	mustWrite(t, target, "unchanged")
	mustWrite(t, filepath.Join(ws, "file"), "old")
	if err := os.Symlink(target, filepath.Join(ws, "file.tmp.edit")); err != nil {
		t.Fatal(err)
	}
	got := Dispatch(Env{Workspace: ws}, "file_edit", json.RawMessage(`{"path":"file","old_string":"old","new_string":"new"}`))
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged" {
		t.Errorf("outside file overwritten: %q %v; response %s", data, err, got)
	}
}

func TestAuditWriteInsideLinks(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "real")
	mustWrite(t, filepath.Join(ws, "dir", "script"), "old")
	if err := os.Chmod(filepath.Join(ws, "dir", "script"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"workspace": ws, "real/link": "dir"} {
		if err := os.Symlink(target, filepath.Join(base, link)); err != nil {
			t.Fatal(err)
		}
	}
	env := Env{Workspace: filepath.Join(base, "workspace")}
	for _, c := range []struct{ name, args string }{
		{"file_write", `{"path":"link/new/file","content":"created"}`},
		{"file_edit", `{"path":"link/script","old_string":"old","new_string":"new"}`},
	} {
		if got := Dispatch(env, c.name, json.RawMessage(c.args)); !strings.HasPrefix(got, "OK:") {
			t.Fatal(got)
		}
	}
	st, err := os.Stat(filepath.Join(ws, "dir", "script"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("executable mode lost: %v", st.Mode())
	}
	entries, err := os.ReadDir(filepath.Join(ws, "dir"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".vornik-edit-") {
			t.Fatal("temporary file leaked")
		}
	}
}

func TestAuditReplaceCollisionAndRenameFailure(t *testing.T) {
	ws := t.TempDir()
	root, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	mustWrite(t, filepath.Join(ws, "collision"), "unchanged")
	if err := replaceInRoot(root, "target", "collision", []byte("new"), 0o644); err == nil {
		t.Fatal("collision accepted")
	}
	data, err := root.ReadFile("collision")
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("collision changed: %q %v", data, err)
	}
	if err := root.Mkdir("directory", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceInRoot(root, "directory", "temporary", []byte("new"), 0o644); err == nil {
		t.Fatal("directory replacement accepted")
	}
	if _, err := root.Stat("temporary"); !os.IsNotExist(err) {
		t.Fatalf("temporary file leaked: %v", err)
	}
}

// Recursive grep must apply the same workspace boundary to discovered leaves
// as file_read applies to directly supplied paths (audit 2026-09-05).
func TestAuditGrepSymlink(t *testing.T) {
	ws, out := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(out, "secret"), "AUDIT_CANARY")
	mustWrite(t, filepath.Join(ws, "inside"), "allowed")
	for link, target := range map[string]string{"escape": filepath.Join(out, "secret"), "internal": "inside"} {
		if err := os.Symlink(target, filepath.Join(ws, link)); err != nil {
			t.Fatal(err)
		}
	}
	got := Dispatch(Env{Workspace: ws}, "grep", json.RawMessage(`{"pattern":"AUDIT_CANARY|allowed","output_mode":"content"}`))
	if strings.Contains(got, "AUDIT_CANARY") {
		t.Fatalf("outside content disclosed: %s", got)
	}
	if !strings.Contains(got, "internal:1:allowed") {
		t.Fatalf("internal symlink lost: %s", got)
	}
}
