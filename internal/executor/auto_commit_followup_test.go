package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAutoCommitLeftoverChanges_EmptyDir — guard short-circuits
// before any git invocation when dir is empty.
func TestAutoCommitLeftoverChanges_EmptyDir(t *testing.T) {
	require.NotPanics(t, func() {
		autoCommitLeftoverChanges(context.Background(), "", "task1", zerolog.Nop())
	})
}

// TestAutoCommitLeftoverChanges_NotARepo — non-repo path also
// short-circuits without spawning git.
func TestAutoCommitLeftoverChanges_NotARepo(t *testing.T) {
	require.NotPanics(t, func() {
		autoCommitLeftoverChanges(context.Background(), t.TempDir(), "task1", zerolog.Nop())
	})
}

// TestAutoCommitLeftoverChanges_NoChanges — empty `git status`
// output means no leftover changes; the helper returns without
// committing.
func TestAutoCommitLeftoverChanges_NoChanges(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// HEAD captured before the call should equal HEAD after — no
	// commit was made because the working tree is clean.
	beforeOut, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	autoCommitLeftoverChanges(context.Background(), dir, "task1", zerolog.Nop())
	afterOut, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(string(beforeOut)), strings.TrimSpace(string(afterOut)),
		"HEAD must not advance on a clean working tree")
}

// TestAutoCommitLeftoverChanges_NewFileCommitted — a new
// untracked file is captured by `git add -A` and committed.
// HEAD advances; commit message contains the task ID.
func TestAutoCommitLeftoverChanges_CommitsUntracked(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// Drop an untracked file in the worktree.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "NEW.md"), []byte("draft\n"), 0o644))

	beforeOut, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	autoCommitLeftoverChanges(context.Background(), dir, "task-leftover-1", zerolog.Nop())
	afterOut, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()

	assert.NotEqual(t, strings.TrimSpace(string(beforeOut)), strings.TrimSpace(string(afterOut)),
		"HEAD must advance after auto-commit captures the untracked file")

	// Commit message includes the task ID per the helper's format.
	logOut, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%s").Output()
	require.NoError(t, err)
	assert.Contains(t, string(logOut), "task-leftover-1")
}

// TestAutoCommitTrackedChangesOnly_EmptyDir — guard mirrors the
// leftover-changes helper's short-circuit.
func TestAutoCommitTrackedChangesOnly_EmptyDir(t *testing.T) {
	require.NotPanics(t, func() {
		autoCommitTrackedChangesOnly(context.Background(), "", "task1", zerolog.Nop())
	})
}

// TestAutoCommitTrackedChangesOnly_NotARepo — non-repo also
// silently no-ops.
func TestAutoCommitTrackedChangesOnly_NotARepo(t *testing.T) {
	require.NotPanics(t, func() {
		autoCommitTrackedChangesOnly(context.Background(), t.TempDir(), "task1", zerolog.Nop())
	})
}

// TestAutoCommitTrackedChangesOnly_NoTrackedChanges — clean
// tracked surface (no staged, no working-tree diff to tracked
// files): the helper detects this via two `git diff --quiet`
// runs and returns without committing.
func TestAutoCommitTrackedChangesOnly_NoTrackedChanges(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// Drop an UNTRACKED file. Tracked surface is clean.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "UNTRACKED.md"), []byte("x"), 0o644))

	beforeOut, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	autoCommitTrackedChangesOnly(context.Background(), dir, "task1", zerolog.Nop())
	afterOut, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	assert.Equal(t, strings.TrimSpace(string(beforeOut)), strings.TrimSpace(string(afterOut)),
		"untracked-only changes must NOT trigger a commit (this is the tracked-only helper)")
}

// TestAutoCommitTrackedChangesOnly_CapturesTrackedModification —
// modify an already-tracked file. `git add -u` stages it, the
// helper commits with the workspace-prelude message.
func TestAutoCommitTrackedChangesOnly_CapturesTrackedModification(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// README.md was created by initGitRepo and is tracked. Modify it.
	readme := filepath.Join(dir, "README.md")
	require.NoError(t, os.WriteFile(readme, []byte("# changed\n"), 0o644))

	beforeOut, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	autoCommitTrackedChangesOnly(context.Background(), dir, "task-tracked-1", zerolog.Nop())
	afterOut, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	assert.NotEqual(t, strings.TrimSpace(string(beforeOut)), strings.TrimSpace(string(afterOut)),
		"tracked-file modification must trigger an auto-commit")

	logOut, _ := exec.Command("git", "-C", dir, "log", "-1", "--format=%s").Output()
	assert.Contains(t, string(logOut), "workspace-root prelude")
	assert.Contains(t, string(logOut), "task-tracked-1")
}
