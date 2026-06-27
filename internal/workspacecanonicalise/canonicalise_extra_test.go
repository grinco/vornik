package workspacecanonicalise

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCanonicaliseOne_EmptyRoot covers the empty-workspaces-root
// guard (canonicalise.go:96-98).
func TestCanonicaliseOne_EmptyRoot(t *testing.T) {
	_, err := CanonicaliseOne("", "alpha", false)
	if err == nil || !strings.Contains(err.Error(), "workspaces root") {
		t.Fatalf("CanonicaliseOne empty root err = %v, want workspaces-root error", err)
	}
}

// TestCanonicaliseOne_EmptyProjectID covers the empty-project-id
// guard (canonicalise.go:99-101).
func TestCanonicaliseOne_EmptyProjectID(t *testing.T) {
	_, err := CanonicaliseOne(t.TempDir(), "", false)
	if err == nil || !strings.Contains(err.Error(), "project id") {
		t.Fatalf("CanonicaliseOne empty project id err = %v, want project-id error", err)
	}
}

// TestCanonicaliseOne_WorkspaceIsFile covers the not-a-directory
// branch (canonicalise.go:108-110): a project ID that names a regular
// file, not a workspace directory, must report OutcomeError rather
// than attempting a rename that would corrupt the path.
func TestCanonicaliseOne_WorkspaceIsFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "alpha"), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	res, err := CanonicaliseOne(root, "alpha", false)
	if err != nil {
		t.Fatalf("CanonicaliseOne: %v", err)
	}
	if res.Outcome != OutcomeError {
		t.Fatalf("outcome = %q, want error", res.Outcome)
	}
	if !strings.Contains(res.Error, "not a directory") {
		t.Fatalf("res.Error = %q, want not-a-directory message", res.Error)
	}
}

// TestCanonicaliseOne_StatNonExistError covers the non-ENOENT stat
// error branch (canonicalise.go:107). A workspace path whose parent
// component is a regular file yields ENOTDIR from os.Stat — neither
// ErrNotExist (which would be no_convention) nor success — so the
// function must surface OutcomeError.
func TestCanonicaliseOne_StatNonExistError(t *testing.T) {
	root := t.TempDir()
	// root/file is a regular file; ask for project "file/child" so
	// the stat of root/file/child traverses through a non-directory.
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := CanonicaliseOne(root, filepath.Join("file", "child"), false)
	if err != nil {
		t.Fatalf("CanonicaliseOne: %v", err)
	}
	if res.Outcome != OutcomeError {
		t.Fatalf("outcome = %q, want error (ENOTDIR stat)", res.Outcome)
	}
	if res.Error == "" {
		t.Fatalf("res.Error empty, want a stat error string")
	}
}

// TestWalk_EmptyRoot covers the empty-root guard inside walk
// (canonicalise.go:117-119), reached via the public Scan entry point.
func TestWalk_EmptyRoot(t *testing.T) {
	_, err := Scan("")
	if err == nil || !strings.Contains(err.Error(), "workspaces root") {
		t.Fatalf("Scan empty root err = %v, want workspaces-root error", err)
	}
}

// TestScan_SkipsNonDirEntries covers the !e.IsDir() continue
// (canonicalise.go:126-127): a regular file sitting at the top level
// of the workspaces root is not a project and must be ignored.
func TestScan_SkipsNonDirEntries(t *testing.T) {
	root := t.TempDir()
	makeProject(t, root, "alpha", "autonomy")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("top-level file"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	results, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(results) != 1 || results[0].ProjectID != "alpha" {
		t.Fatalf("results = %+v, want only [alpha] (top-level file skipped)", results)
	}
}

// TestCanonicaliseDir_RenameError covers the os.Rename failure branch
// (canonicalise.go:161-164): when autonomy/ would be migrated but the
// destination .autonomy/ already exists as a NON-directory (a file),
// os.Rename fails and the outcome flips to OutcomeError. This is the
// security-relevant guard that a botched rename never silently
// reports success. Skipped on platforms where rename-onto-file
// semantics differ.
func TestCanonicaliseDir_RenameError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rename-onto-file semantics differ on windows")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "alpha")
	if err := os.MkdirAll(filepath.Join(dir, LegacyDir), 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	// .autonomy exists but as a regular FILE — isDir() returns false
	// (so it's not "mixed"), yet os.Rename(legacyDir -> file) fails
	// with ENOTDIR / EEXIST because you can't rename a dir onto a
	// file.
	if err := os.WriteFile(filepath.Join(dir, CanonicalDir), []byte("blocker"), 0o644); err != nil {
		t.Fatalf("write canonical-as-file: %v", err)
	}
	res := canonicaliseDir(dir, "alpha", false /* dryRun */)
	if res.Outcome != OutcomeError {
		t.Fatalf("outcome = %q, want error (rename onto file should fail)", res.Outcome)
	}
	if res.Error == "" {
		t.Fatalf("res.Error empty, want rename error string")
	}
	// The legacy dir must survive a failed rename.
	if _, err := os.Stat(filepath.Join(dir, LegacyDir)); err != nil {
		t.Fatalf("legacy dir should survive failed rename: %v", err)
	}
}

// TestIsDir_NonExistStatError covers the non-ENOENT error return in
// isDir (canonicalise.go:182): a path whose parent is a regular file
// yields ENOTDIR, which isDir must propagate as a real error (not the
// (false, nil) "doesn't exist" answer).
func TestIsDir_NonExistStatError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ok, err := isDir(filepath.Join(root, "file", "child"))
	if err == nil {
		t.Fatalf("isDir through a file = nil error, want ENOTDIR")
	}
	if ok {
		t.Fatalf("isDir ok = true on error, want false")
	}
}

// TestIsDir_FollowsSymlinkToDir documents the intentional symlink
// behaviour called out in the source comment: the convention dir
// itself MAY be a symlink (e.g. an ops-mounted shared spec), so isDir
// follows it. A symlink pointing at a real directory reads as a dir.
func TestIsDir_FollowsSymlinkToDir(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	ok, err := isDir(link)
	if err != nil {
		t.Fatalf("isDir(symlink->dir): %v", err)
	}
	if !ok {
		t.Fatalf("isDir(symlink->dir) = false, want true (symlinks followed)")
	}
	// And a symlink to a FILE reads as not-a-dir.
	fileTarget := filepath.Join(root, "f")
	if err := os.WriteFile(fileTarget, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	fileLink := filepath.Join(root, "flink")
	if err := os.Symlink(fileTarget, fileLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	ok, err = isDir(fileLink)
	if err != nil {
		t.Fatalf("isDir(symlink->file): %v", err)
	}
	if ok {
		t.Fatalf("isDir(symlink->file) = true, want false")
	}
}
