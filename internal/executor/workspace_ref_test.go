package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSnapshotWorkspaceRef_EarlyReturns covers the three early-exit
// branches: empty dir, missing .git, and exec.Command failure on a
// non-repo directory. The happy path (a real git repo) is exercised
// by the broader workspace integration tests.
func TestSnapshotWorkspaceRef_EarlyReturns(t *testing.T) {
	t.Run("empty dir returns empty", func(t *testing.T) {
		assert.Equal(t, "", snapshotWorkspaceRef(""))
	})
	t.Run("missing .git returns empty", func(t *testing.T) {
		tmp := t.TempDir()
		// No .git/ — snapshotWorkspaceRef short-circuits on the
		// os.Stat check before invoking git.
		assert.Equal(t, "", snapshotWorkspaceRef(tmp))
	})
	t.Run("invalid .git file returns empty", func(t *testing.T) {
		tmp := t.TempDir()
		// .git as a regular file (a corrupt or sub-worktree pointer
		// without a valid gitdir) — git rev-parse will fail. Cover
		// the post-Stat exec branch by making .git a file.
		require := func(err error) {
			if err != nil {
				t.Fatalf("test setup failed: %v", err)
			}
		}
		require(os.WriteFile(filepath.Join(tmp, ".git"), []byte("gitdir: /nope"), 0o600))
		assert.Equal(t, "", snapshotWorkspaceRef(tmp))
	})
}

// TestResetWorkspace_EmptyInputsAreNoop covers the early-return
// branches: empty dir or empty ref both short-circuit before any
// git invocation.
func TestResetWorkspace_EmptyInputsAreNoop(t *testing.T) {
	ctx := context.Background()
	assert.NoError(t, resetWorkspace(ctx, "", "deadbeef", zerolog.Nop()))
	assert.NoError(t, resetWorkspace(ctx, "/tmp/some-dir", "", zerolog.Nop()))
	assert.NoError(t, resetWorkspace(ctx, "", "", zerolog.Nop()))
}

// TestResetWorkspace_NonGitDirErrors — passing a non-git dir with a
// non-empty ref drives git reset, which fails because the dir isn't
// a repo. Surface the wrapped error so callers see the underlying
// reason rather than a generic 500.
func TestResetWorkspace_NonGitDirErrors(t *testing.T) {
	tmp := t.TempDir()
	err := resetWorkspace(context.Background(), tmp, "deadbeef", zerolog.Nop())
	if err == nil {
		t.Fatalf("expected error from git reset against non-repo dir")
	}
	assert.Contains(t, err.Error(), "git reset")
}

// TestResetWorkspace_ResetsRepoToRefAndCleansUntracked exercises the
// real success path against a fresh git repo: a tracked file is
// modified + an untracked file is created, then resetWorkspace
// returns the worktree to the initial commit and cleans the
// untracked entry.
func TestResetWorkspace_ResetsRepoToRefAndCleansUntracked(t *testing.T) {
	dir := initGitRepoForTest(t)
	// Add + commit a tracked file at the initial state.
	tracked := filepath.Join(dir, "f.txt")
	require.NoError(t, os.WriteFile(tracked, []byte("v1"), 0o600))
	require.NoError(t, runGit(dir, "add", "f.txt"))
	require.NoError(t, runGit(dir, "-c", "user.email=t@e", "-c", "user.name=t",
		"commit", "-m", "initial"))
	headBefore, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").CombinedOutput()
	require.NoError(t, err)

	// Now dirty things up.
	require.NoError(t, os.WriteFile(tracked, []byte("v2"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("x"), 0o600))

	// Reset back to the captured HEAD.
	err = resetWorkspace(context.Background(), dir, strings.TrimSpace(string(headBefore)), zerolog.Nop())
	require.NoError(t, err)

	// The tracked file has v1 again.
	body, _ := os.ReadFile(tracked)
	assert.Equal(t, "v1", string(body))
	// The untracked file is gone.
	_, err = os.Stat(filepath.Join(dir, "untracked.txt"))
	assert.True(t, os.IsNotExist(err), "git clean -fdx must drop untracked entries")
}

// runGit is a small helper for the workspace tests.
func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %w (%s)", args, err, out)
	}
	return nil
}

// TestWorktreeInUseByContainer_EmptyPathFalse — the empty-path short
// circuit avoids touching podman.
func TestWorktreeInUseByContainer_EmptyPathFalse(t *testing.T) {
	assert.False(t, worktreeInUseByContainer(context.Background(), ""))
}

// TestWorktreeInUseByContainer_NoPodmanReturnsFalse — exec failure
// (podman not on PATH, container error) is documented as "no" so
// the cleanup path doesn't get stuck if podman is misconfigured.
func TestWorktreeInUseByContainer_NoPodmanReturnsFalse(t *testing.T) {
	t.Setenv("PATH", "") // make exec.LookPath fail for podman
	assert.False(t, worktreeInUseByContainer(context.Background(), "/some/wt"))
}

// TestProjectCleanExcludeDir_DefaultsAndRejections — pin every
// branch of the small helper.
func TestProjectCleanExcludeDir_AllBranches(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"absolute rejected", "/etc/notes.md", ""},
		{"parent traversal rejected", "../escape.md", ""},
		{"deeper traversal rejected", "../../etc/passwd", ""},
		{"root-level file returns empty", "PROJECT_CONTEXT.md", ""},
		{".autonomy subdir excluded by default → empty", ".autonomy/USER_GUIDANCE.md", ""},
		{".autonomy nested subdir excluded by default → empty", ".autonomy/foo/bar.md", ""},
		{"non-autonomy subdir returns its dir", "operator/notes.md", "operator"},
		{"deeply nested path returns full prefix", "operator/sub/notes.md", filepath.Join("operator", "sub")},
		{"whitespace trimmed then evaluated", "   operator/notes.md   ", "operator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, projectCleanExcludeDir(tc.in))
		})
	}
}
