package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestForgeCheckoutSpec(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantOK   bool
		wantBase string
		wantHead string
		wantCR   bool
	}{
		{name: "none", payload: `{"context":{"prompt":"x"}}`, wantOK: false},
		{name: "issue top-level", payload: `{"forge_job":{"default_branch":"main"}}`, wantOK: true, wantBase: "main"},
		{name: "issue under context", payload: `{"context":{"forge_job":{"default_branch":"trunk"}}}`, wantOK: true, wantBase: "trunk"},
		{name: "empty payload", payload: ``, wantOK: false},
		{name: "garbage", payload: `not json`, wantOK: false},
		{
			// A change-request (PR review) job carries the head ref to check out.
			name:     "change request with head ref",
			payload:  `{"forge_job":{"default_branch":"main","head_ref":"refs/pull/20/head","is_change_request":true}}`,
			wantOK:   true,
			wantBase: "main",
			wantHead: "refs/pull/20/head",
			wantCR:   true,
		},
		{
			// A head ref alone (no default branch) is still a recognizable forge task.
			name:     "head ref only",
			payload:  `{"context":{"forge_job":{"head_ref":"refs/pull/9/head","is_change_request":true}}}`,
			wantOK:   true,
			wantHead: "refs/pull/9/head",
			wantCR:   true,
		},
		{name: "neither branch nor head", payload: `{"forge_job":{"is_change_request":true}}`, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := forgeCheckoutSpec([]byte(tc.payload))
			if ok != tc.wantOK {
				t.Fatalf("forgeCheckoutSpec(%s) ok=%v want %v", tc.payload, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.DefaultBranch != tc.wantBase || got.HeadRef != tc.wantHead || got.IsChangeRequest != tc.wantCR {
				t.Errorf("forgeCheckoutSpec(%s) = %+v want base=%q head=%q cr=%v",
					tc.payload, got, tc.wantBase, tc.wantHead, tc.wantCR)
			}
		})
	}
}

// TestCheckoutForgeChangeRequest: a PR-review forge task must materialize the
// change-request's head in the working tree so the reviewer sees the PR's actual
// files — NOT the default branch (incident 2026-06-13: github-review reset the
// clone to origin/<default_branch> via rebaseProjectToOrigin, so the reviewer's
// working tree lacked every file the PR added and it "couldn't locate any new
// files"). On a fetch failure it must fall back to the default-branch rebase
// rather than silently review a stale tree.
func TestCheckoutForgeChangeRequest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	log := zerolog.Nop()

	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	clone := filepath.Join(root, "clone")

	mustGit(t, root, "git", "init", "--bare", "-b", "main", bare)
	mustGit(t, root, "git", "clone", bare, work)

	// Base commit on main (no PR file yet).
	if err := os.WriteFile(filepath.Join(work, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "git", "add", "-A")
	mustGit(t, work, "git", "commit", "-m", "base")
	mustGit(t, work, "git", "push", "origin", "main")
	baseSHA := gitOut(t, work, "git", "rev-parse", "HEAD")

	// PR head: a branch that ADDS a new file, published under the synthetic pull
	// ref the GitHub provider emits (refs/pull/<n>/head).
	mustGit(t, work, "git", "checkout", "-b", "pr-branch")
	if err := os.WriteFile(filepath.Join(work, "newfile.txt"), []byte("added by PR\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "git", "add", "-A")
	mustGit(t, work, "git", "commit", "-m", "pr work")
	prSHA := gitOut(t, work, "git", "rev-parse", "HEAD")
	mustGit(t, work, "git", "push", "origin", "pr-branch:refs/pull/20/head")

	// Fresh clone is on main → newfile.txt absent.
	mustGit(t, root, "git", "clone", bare, clone)
	if _, err := os.Stat(filepath.Join(clone, "newfile.txt")); !os.IsNotExist(err) {
		t.Fatalf("setup: clone should not have the PR file yet (err=%v)", err)
	}

	// Checking out the change-request head brings the PR's new file into the tree.
	checkoutForgeChangeRequest(ctx, clone, "refs/pull/20/head", "main", log)
	if got := gitOut(t, clone, "git", "rev-parse", "HEAD"); got != prSHA {
		t.Errorf("after checkout HEAD=%s, want PR head %s", got, prSHA)
	}
	if _, err := os.Stat(filepath.Join(clone, "newfile.txt")); err != nil {
		t.Errorf("PR's new file should be present after checkout: %v", err)
	}

	// Fallback: an unresolvable head ref falls back to the default-branch rebase,
	// so the working tree lands on origin/main (PR file gone) rather than staying
	// on a half-fetched / arbitrary state.
	checkoutForgeChangeRequest(ctx, clone, "refs/pull/999/head", "main", log)
	if got := gitOut(t, clone, "git", "rev-parse", "HEAD"); got != baseSHA {
		t.Errorf("after fallback HEAD=%s, want origin/main %s", got, baseSHA)
	}
	if _, err := os.Stat(filepath.Join(clone, "newfile.txt")); !os.IsNotExist(err) {
		t.Errorf("PR file should be gone after fallback to default branch (err=%v)", err)
	}
}

// TestRebaseProjectToOrigin: with a real origin, the clone is reset to upstream;
// with no origin (or non-git), it's a safe no-op.
func TestRebaseProjectToOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	log := zerolog.Nop()

	// no-op on a non-git dir (must not panic / error out).
	rebaseProjectToOrigin(ctx, t.TempDir(), "main", log)

	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	clone := filepath.Join(root, "clone")
	mustGit(t, root, "git", "init", "--bare", "-b", "main", bare)
	mustGit(t, root, "git", "clone", bare, clone)
	mustGit(t, clone, "git", "commit", "--allow-empty", "-m", "upstream-1")
	mustGit(t, clone, "git", "push", "origin", "main")
	upstream := gitOut(t, clone, "git", "rev-parse", "HEAD")

	// Local clone drifts ahead with a commit that was never pushed.
	mustGit(t, clone, "git", "commit", "--allow-empty", "-m", "local-only drift")
	if gitOut(t, clone, "git", "rev-parse", "HEAD") == upstream {
		t.Fatal("setup: local HEAD should have drifted")
	}

	// Rebase resets local HEAD back to origin/main.
	rebaseProjectToOrigin(ctx, clone, "main", log)
	if got := gitOut(t, clone, "git", "rev-parse", "HEAD"); got != upstream {
		t.Errorf("after rebase HEAD=%s, want upstream %s", got, upstream)
	}

	// no-op on a git repo without an origin remote.
	noOrigin := filepath.Join(root, "noorigin")
	mustGit(t, root, "git", "init", "-b", "main", noOrigin)
	mustGit(t, noOrigin, "git", "commit", "--allow-empty", "-m", "x")
	before := gitOut(t, noOrigin, "git", "rev-parse", "HEAD")
	rebaseProjectToOrigin(ctx, noOrigin, "main", log) // must not fail/change
	if gitOut(t, noOrigin, "git", "rev-parse", "HEAD") != before {
		t.Error("no-origin repo HEAD should be unchanged")
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v: %s", args, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return string(trimNL(out))
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func TestExcludeVornikInternalPaths(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	log := zerolog.Nop()

	// Non-git dir → no-op (no panic, no file).
	excludeVornikInternalPaths(t.TempDir(), log)

	dir := t.TempDir()
	mustGit(t, dir, "git", "init")
	excludeStr := func() string {
		b, _ := os.ReadFile(filepath.Join(dir, ".git", "info", "exclude"))
		return string(b)
	}
	excludeVornikInternalPaths(dir, log)
	got := excludeStr()
	for _, p := range vornikInternalPaths {
		if !strings.Contains(got, p) {
			t.Errorf("exclude missing %q; have:\n%s", p, got)
		}
	}
	// Idempotent: a second call must not duplicate the entries.
	excludeVornikInternalPaths(dir, log)
	got2 := excludeStr()
	if strings.Count(got2, ".autonomy/") != 1 {
		t.Errorf("not idempotent — .autonomy/ appears %d times", strings.Count(got2, ".autonomy/"))
	}
	// A real .autonomy/ file in the worktree must now be git-ignored.
	if err := os.MkdirAll(filepath.Join(dir, ".autonomy"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, ".autonomy", "CURRENT_TASK.md"), []byte("x"), 0o644)
	out, _ := exec.Command("git", "-C", dir, "status", "--porcelain", "--ignored").Output()
	if !strings.Contains(string(out), "!! .autonomy/") {
		t.Errorf(".autonomy/ should be ignored; git status:\n%s", out)
	}
}
