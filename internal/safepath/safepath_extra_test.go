package safepath

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCleanFileNameRejectsDotComponent covers the branch where the
// Base() of the input is itself a forbidden component (".."), so
// CleanPathComponent rejects it before the "no path components"
// equality check runs (safepath.go:29-31).
func TestCleanFileNameRejectsDotComponent(t *testing.T) {
	if _, err := CleanFileName(".."); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("CleanFileName(%q) error = %v, want not-allowed refusal", "..", err)
	}
	// A trailing-slash form whose Base is "." must also be refused,
	// exercising the same CleanPathComponent error path.
	if _, err := CleanFileName("foo/"); err == nil {
		t.Fatalf("CleanFileName(%q) = nil error, want refusal", "foo/")
	}
}

// TestJoinUnderEmptyRoot covers the empty-root guard (safepath.go:42-44).
func TestJoinUnderEmptyRoot(t *testing.T) {
	if _, err := JoinUnder("", "anything.txt"); err == nil || !strings.Contains(err.Error(), "root path is empty") {
		t.Fatalf("JoinUnder empty root error = %v, want empty-root refusal", err)
	}
}

// TestJoinUnderRejectsFileAsDirComponent covers the non-ENOENT error
// branch in JoinUnder (safepath.go:67-69): when a path component that
// must act as a directory is actually a regular file, EvalSymlinks
// surfaces ENOTDIR — not os.ErrNotExist — and evalExistingPrefix must
// propagate it rather than silently treating the leaf as "missing".
// A swallowed error here would let a write target an unexpected path.
func TestJoinUnderRejectsFileAsDirComponent(t *testing.T) {
	root := t.TempDir()
	// Create a regular file inside root, then try to descend through it.
	if err := os.WriteFile(filepath.Join(root, "afile"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, err := JoinUnder(root, "afile", "child.txt")
	if err == nil {
		t.Fatalf("JoinUnder through a file = nil error, want resolve failure")
	}
	if !strings.Contains(err.Error(), "resolve symlink path") {
		t.Fatalf("JoinUnder through a file error = %v, want resolve-symlink-path error", err)
	}
}

// TestJoinUnderRealNestedSymlinkChainStaysInside exercises the
// deepest-existing-prefix resolution where a symlinked directory
// resolves to a real location inside root and the joined leaf is
// returned canonicalised (safepath.go:74-77 accept path).
func TestJoinUnderRealNestedSymlinkChainStaysInside(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "data", "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	// A symlink at root/alias -> root/data/real (both inside root).
	if err := os.Symlink(realDir, filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	got, err := JoinUnder(root, "alias", "leaf.txt")
	if err != nil {
		t.Fatalf("JoinUnder via inside-root symlink: %v", err)
	}
	// Resolve root too, since JoinUnder canonicalises the reference.
	wantRoot := root
	if r, e := filepath.EvalSymlinks(root); e == nil {
		wantRoot = r
	}
	want := filepath.Join(wantRoot, "data", "real", "leaf.txt")
	if got != want {
		t.Fatalf("JoinUnder via inside-root symlink = %q, want %q", got, want)
	}
}

// TestEvalExistingPrefixDeepestPrefix pins the core behaviour of
// evalExistingPrefix: given a path whose deepest few components do not
// exist yet, it resolves the existing prefix and re-appends the
// missing tail in order (safepath.go:90-93).
func TestEvalExistingPrefixDeepestPrefix(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	candidate := filepath.Join(existing, "c", "d.txt") // c/ and d.txt don't exist
	resolved, ok, err := evalExistingPrefix(candidate)
	if err != nil {
		t.Fatalf("evalExistingPrefix: %v", err)
	}
	if !ok {
		t.Fatalf("evalExistingPrefix ok = false, want true (root exists)")
	}
	// EvalSymlinks(existing) may differ from existing on macOS-style
	// roots; normalise via the same call for a robust comparison.
	wantPrefix := existing
	if r, e := filepath.EvalSymlinks(existing); e == nil {
		wantPrefix = r
	}
	want := filepath.Join(wantPrefix, "c", "d.txt")
	if resolved != want {
		t.Fatalf("evalExistingPrefix = %q, want %q", resolved, want)
	}
}

// TestEvalExistingPrefixNonExistError covers the non-ENOENT error
// branch (safepath.go:95-97): a file used as a directory component
// yields ENOTDIR, which must be returned rather than treated as a
// missing component.
func TestEvalExistingPrefixNonExistError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, ok, err := evalExistingPrefix(filepath.Join(root, "f", "under"))
	if err == nil {
		t.Fatalf("evalExistingPrefix through file = nil error, want ENOTDIR propagation")
	}
	if ok {
		t.Fatalf("evalExistingPrefix ok = true on error, want false")
	}
	if !strings.Contains(err.Error(), "resolve symlink path") {
		t.Fatalf("error = %v, want resolve-symlink-path wrap", err)
	}
}

// TestEvalExistingPrefixFullyMissingResolvesAtRoot pins the
// deepest-existing-prefix walk for a path where NO component below the
// filesystem root exists. On Linux EvalSymlinks("/") always succeeds,
// so the walk terminates at "/" (not via the parent==cur guard, which
// is therefore unreachable here) and re-appends the entire missing
// tail in order: the path is returned verbatim with ok=true. This is
// the security-relevant invariant — a write to a brand-new path under
// root is not mistaken for an escape.
func TestEvalExistingPrefixFullyMissingResolvesAtRoot(t *testing.T) {
	p := filepath.Join(string(filepath.Separator)+"vornik-nonexistent-7f3a9c", "x", "y", "z.txt")
	if _, statErr := os.Lstat(string(filepath.Separator) + "vornik-nonexistent-7f3a9c"); statErr == nil {
		t.Skip("collision: the sentinel top-level dir unexpectedly exists")
	}
	resolved, ok, err := evalExistingPrefix(p)
	if err != nil {
		t.Fatalf("evalExistingPrefix err = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("evalExistingPrefix ok = false, want true (resolves at /)")
	}
	if resolved != filepath.Clean(p) {
		t.Fatalf("evalExistingPrefix = %q, want %q (tail re-appended verbatim)", resolved, p)
	}
}

// Sanity: the package's error sentinels behave as os errors expect,
// guarding against an accidental swap of errors.Is semantics in
// evalExistingPrefix.
func TestEvalExistingPrefixErrorIsNotErrNotExist(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "g"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := evalExistingPrefix(filepath.Join(root, "g", "h"))
	if err == nil {
		t.Fatalf("want error")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error should NOT be ErrNotExist (it is ENOTDIR), got %v", err)
	}
}
