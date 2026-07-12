package autonomy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// TestExecGitRefresh_ResetsToOriginTip — the workspace picks up external
// commits merged to origin and drops a stale local commit. This is the
// mechanism that keeps backlog-mode autonomy from working against a stale tree.
func TestExecGitRefresh_ResetsToOriginTip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")
	contrib := filepath.Join(root, "contrib")

	git(t, root, "init", "--bare", "-b", "main", origin)
	git(t, root, "clone", origin, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "-A")
	git(t, work, "commit", "-m", "base")
	git(t, work, "push", "origin", "main")

	// External contribution lands on origin via a separate clone.
	git(t, root, "clone", origin, contrib)
	if err := os.WriteFile(filepath.Join(contrib, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, contrib, "add", "-A")
	git(t, contrib, "commit", "-m", "external contribution")
	git(t, contrib, "push", "origin", "main")

	// The workspace makes a STALE local commit (simulating a worktree-merge).
	if err := os.WriteFile(filepath.Join(work, "stale.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "-A")
	git(t, work, "commit", "-m", "stale local")

	if err := ExecGitRefresh(context.Background(), work); err != nil {
		t.Fatalf("ExecGitRefresh: %v", err)
	}

	// Workspace HEAD must now equal origin/main: has the external b.txt,
	// dropped the stale local commit.
	if _, err := os.Stat(filepath.Join(work, "b.txt")); err != nil {
		t.Errorf("external contribution not picked up: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("stale local commit not discarded (stale.txt still present)")
	}
}

// TestExecGitRefresh_NonRepoIsNoOp — a non-git dir must not error the tick.
func TestExecGitRefresh_NonRepoIsNoOp(t *testing.T) {
	if err := ExecGitRefresh(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("non-repo dir must be a no-op, got %v", err)
	}
}
