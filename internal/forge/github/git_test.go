package github

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// run executes a git command in dir, failing the test on error.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestGitPushToOrigin exercises the real git push path against a temp bare
// remote: a fresh branch lands, a re-push is an idempotent no-op success, and a
// divergent (non-fast-forward) push is rejected rather than force-overwritten.
func TestGitPushToOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	clone := filepath.Join(root, "clone")
	run(t, root, "git", "init", "--bare", "-b", "main", bare)
	run(t, root, "git", "clone", bare, clone)
	run(t, clone, "git", "commit", "--allow-empty", "-m", "base")
	run(t, clone, "git", "push", "origin", "main")
	baseSha := run(t, clone, "git", "rev-parse", "HEAD")

	// New feature commit on a worktree branch (still local).
	run(t, clone, "git", "commit", "--allow-empty", "-m", "fix")
	sha := run(t, clone, "git", "rev-parse", "HEAD")

	// First push lands the branch. Token is irrelevant for a local file remote.
	if err := gitPushToOrigin(context.Background(), clone, "fix/issue-1", sha, "tok"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	got := run(t, bare, "git", "rev-parse", "refs/heads/fix/issue-1")
	if got != sha {
		t.Fatalf("remote ref %s != pushed %s", got, sha)
	}

	// Re-push of the same sha is an idempotent no-op (up-to-date), not an error.
	if err := gitPushToOrigin(context.Background(), clone, "fix/issue-1", sha, "tok"); err != nil {
		t.Fatalf("idempotent re-push should succeed: %v", err)
	}

	// A divergent history (branched from base, NOT containing the pushed "fix"
	// commit) pushed to the same branch is rejected (non-ff), proving PushBranch
	// never force-overwrites.
	run(t, clone, "git", "checkout", "-q", "-B", "diverge", baseSha)
	run(t, clone, "git", "commit", "--allow-empty", "-m", "other")
	divSha := run(t, clone, "git", "rev-parse", "HEAD")
	if err := gitPushToOrigin(context.Background(), clone, "fix/issue-1", divSha, "tok"); err == nil {
		t.Fatal("divergent non-fast-forward push must be rejected, not forced")
	}
}

func TestGitPushToOrigin_Validation(t *testing.T) {
	if err := gitPushToOrigin(context.Background(), "", "b", "s", "t"); err == nil {
		t.Error("empty gitDir should error")
	}
	if err := gitPushToOrigin(context.Background(), "/tmp", "", "s", "t"); err == nil {
		t.Error("empty branch should error")
	}
}
