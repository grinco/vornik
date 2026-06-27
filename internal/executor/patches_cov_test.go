package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// patchesCov_git runs a git command in dir with deterministic identity.
func patchesCov_git(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v failed: %s", args, out)
	return out
}

// TestPatchesCov_BuildChangeSummary_EmptyRange covers the `raw == ""`
// early-return branch in buildChangeSummary: when from==to, `git log X..X`
// emits nothing, so the helper returns (nil, "", nil).
func TestPatchesCov_BuildChangeSummary_EmptyRange(t *testing.T) {
	dir := t.TempDir()
	patchesCov_git(t, dir, "init", "-q", "-b", "main")
	patchesCov_git(t, dir, "config", "user.name", "test")
	patchesCov_git(t, dir, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("a\n"), 0o644))
	patchesCov_git(t, dir, "add", ".")
	patchesCov_git(t, dir, "commit", "-q", "-m", "only")
	head := strings.TrimSpace(string(patchesCov_git(t, dir, "rev-parse", "HEAD")))

	commits, summary, err := buildChangeSummary(context.Background(), dir, head, head)
	require.NoError(t, err)
	require.Nil(t, commits)
	require.Equal(t, "", summary)
}

// TestPatchesCov_BuildChangeSummary_BadRepoErrors covers the git-log error
// branch (non-existent revisions in a real repo make `git log` exit non-zero).
func TestPatchesCov_BuildChangeSummary_BadRepoErrors(t *testing.T) {
	dir := t.TempDir()
	patchesCov_git(t, dir, "init", "-q", "-b", "main")
	patchesCov_git(t, dir, "config", "user.name", "test")
	patchesCov_git(t, dir, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("a\n"), 0o644))
	patchesCov_git(t, dir, "add", ".")
	patchesCov_git(t, dir, "commit", "-q", "-m", "only")

	// Reference two SHAs that don't exist → git log errors.
	commits, summary, err := buildChangeSummary(context.Background(), dir,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"cafecafecafecafecafecafecafecafecafecafe")
	require.Error(t, err)
	require.Nil(t, commits)
	require.Equal(t, "", summary)
}
