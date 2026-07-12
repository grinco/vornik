package autonomy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ExecGitRefresh is the default gitRefresh hook (wired via WithGitRefresh):
// fetch origin and hard-reset repoDir to origin/<current-branch>, so a backlog
// tick reads the workspace at the tip of main including external contributions
// merged since the last iteration.
//
// It only touches a real git checkout — a non-repo repoDir is a clean no-op so
// a mis-provisioned workspace never errors the tick. The hard reset discards
// stale local worktree-merge commits and any non-BACKLOG.md drift; the
// caller's RefreshFromOrigin runs this inside the backlog lock and re-applies
// the local consumption marks afterwards, so no [x]/[!] state is lost.
func ExecGitRefresh(ctx context.Context, repoDir string) error {
	if fi, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil || (!fi.IsDir() && fi.Size() == 0) {
		return nil // not a git checkout — nothing to refresh
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoDir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := run("fetch", "--quiet", "origin"); err != nil {
		return err
	}
	branch, err := run("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "" || branch == "HEAD" {
		branch = "main" // detached / unknown → assume the default branch
	}
	_, err = run("reset", "--hard", "origin/"+branch)
	return err
}
