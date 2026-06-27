package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initGitRepoForTest creates a real local git repo with an initial
// commit so the auto-commit tests can run end-to-end without standing
// up a full executor.
func initGitRepoForTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "--allow-empty", "-m", "init"},
	} {
		// The `init -b main` form needs to run from inside dir; the rest from -C dir.
		var cmd *exec.Cmd
		if args[0] == "init" {
			cmd = exec.Command("git", append([]string{"-C", dir}, args...)...)
		} else {
			cmd = exec.Command("git", append([]string{"-C", dir}, args...)...)
		}
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	return dir
}

// TestIsGitRepo covers the empty-input + missing-.git branches.
func TestIsGitRepo(t *testing.T) {
	assert.False(t, isGitRepo(""))
	tmp := t.TempDir()
	assert.False(t, isGitRepo(tmp), "no .git directory → not a repo")

	// Create a .git directory → reported as a repo.
	require := func(err error) {
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	require(os.MkdirAll(filepath.Join(tmp, ".git"), 0o755))
	assert.True(t, isGitRepo(tmp))
}

// TestAutoCommitLeftoverChanges_NonRepoIsNoop — when the worktree dir
// isn't a git repo, the helper returns silently without attempting
// any git commands.
func TestAutoCommitLeftoverChanges_NonRepoIsNoop(t *testing.T) {
	tmp := t.TempDir()
	assert.NotPanics(t, func() {
		autoCommitLeftoverChanges(context.Background(), tmp, "task-1", zerolog.Nop())
	})
}

func TestAutoCommitLeftoverChanges_EmptyDirIsNoop(t *testing.T) {
	assert.NotPanics(t, func() {
		autoCommitLeftoverChanges(context.Background(), "", "task-1", zerolog.Nop())
	})
}

func TestAutoCommitTrackedChangesOnly_NonRepoIsNoop(t *testing.T) {
	tmp := t.TempDir()
	assert.NotPanics(t, func() {
		autoCommitTrackedChangesOnly(context.Background(), tmp, "task-1", zerolog.Nop())
	})
}

func TestAutoCommitTrackedChangesOnly_EmptyDirIsNoop(t *testing.T) {
	assert.NotPanics(t, func() {
		autoCommitTrackedChangesOnly(context.Background(), "", "task-1", zerolog.Nop())
	})
}

// TestAutoCommitLeftoverChanges_CommitsDirtyWorktree drives the full
// happy path: dirty worktree → git add -A → git commit.
func TestAutoCommitLeftoverChanges_CommitsDirtyWorktree(t *testing.T) {
	dir := initGitRepoForTest(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new-file.txt"), []byte("hello"), 0o600))
	autoCommitLeftoverChanges(context.Background(), dir, "task-X", zerolog.Nop())
	// Verify a commit landed with the task ID in the message.
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--pretty=%s").CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(out), "task-X")
}

// TestAutoCommitLeftoverChanges_CleanRepoIsNoop — when git status
// shows no diff, the helper returns without invoking git commit.
func TestAutoCommitLeftoverChanges_CleanRepoIsNoop(t *testing.T) {
	dir := initGitRepoForTest(t)
	// Capture initial HEAD.
	before, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").CombinedOutput()
	require.NoError(t, err)
	autoCommitLeftoverChanges(context.Background(), dir, "task-Y", zerolog.Nop())
	after, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").CombinedOutput()
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "no new commit on clean repo")
}

// TestAutoCommitTrackedChangesOnly_CommitsTrackedModification —
// modifying a tracked file should be auto-committed; an untracked
// new file must NOT be picked up by `git add -u`.
func TestAutoCommitTrackedChangesOnly_CommitsTrackedModification(t *testing.T) {
	dir := initGitRepoForTest(t)
	// Create + commit a tracked file.
	tracked := filepath.Join(dir, "tracked.txt")
	require.NoError(t, os.WriteFile(tracked, []byte("v1"), 0o600))
	out, err := exec.Command("git", "-C", dir,
		"-c", "user.email=t@e", "-c", "user.name=t",
		"add", "tracked.txt").CombinedOutput()
	require.NoError(t, err, "%s", out)
	out, err = exec.Command("git", "-C", dir,
		"-c", "user.email=t@e", "-c", "user.name=t",
		"commit", "-m", "initial").CombinedOutput()
	require.NoError(t, err, "%s", out)

	// Modify the tracked file + add an untracked file.
	require.NoError(t, os.WriteFile(tracked, []byte("v2"), 0o600))
	untracked := filepath.Join(dir, "untracked.txt")
	require.NoError(t, os.WriteFile(untracked, []byte("new"), 0o600))

	autoCommitTrackedChangesOnly(context.Background(), dir, "task-Z", zerolog.Nop())

	// Verify the commit landed.
	logOut, err := exec.Command("git", "-C", dir, "log", "-1", "--pretty=%s").CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(logOut), "task-Z")
	// Verify untracked.txt is still untracked (not in the commit).
	statusOut, err := exec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(statusOut), "untracked.txt",
		"untracked file must remain untracked after add -u commit")
}

// TestAutoCommitTrackedChangesOnly_CleanRepoIsNoop — when there's no
// tracked diff at all, the helper short-circuits without committing.
func TestAutoCommitTrackedChangesOnly_CleanRepoIsNoop(t *testing.T) {
	dir := initGitRepoForTest(t)
	before, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").CombinedOutput()
	require.NoError(t, err)
	autoCommitTrackedChangesOnly(context.Background(), dir, "task-W", zerolog.Nop())
	after, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").CombinedOutput()
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}
