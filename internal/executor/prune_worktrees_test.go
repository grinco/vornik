package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// TestPruneAllWorktrees_NoWorkspacePath — guard: when the config
// has no ProjectWorkspacePath set, the helper short-circuits
// silently. This is the test-deployment default.
func TestPruneAllWorktrees_NoWorkspacePath(t *testing.T) {
	e := &Executor{config: &Config{ProjectWorkspacePath: ""}, logger: zerolog.Nop()}
	require.NotPanics(t, func() {
		e.pruneAllWorktrees(context.Background(), nil)
	})
}

// TestPruneAllWorktrees_WorkspacePathDoesNotExist — when the
// directory doesn't exist, os.ReadDir errors and the helper
// returns silently. Used in CI where the workspace volume
// hasn't been mounted yet.
func TestPruneAllWorktrees_NonExistentDir(t *testing.T) {
	e := &Executor{
		config: &Config{ProjectWorkspacePath: "/path/that/definitely/does/not/exist"},
		logger: zerolog.Nop(),
	}
	require.NotPanics(t, func() {
		e.pruneAllWorktrees(context.Background(), nil)
	})
}

// TestPruneAllWorktrees_WalksProjectDirs — when the workspace
// has multiple project subdirectories, the helper visits each
// one. Non-directory entries (stray files) are skipped per the
// IsDir guard.
func TestPruneAllWorktrees_WalksDirsSkipsFiles(t *testing.T) {
	dir := t.TempDir()
	// Create two project subdirs + one stray file.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "proj-a"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "proj-b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644))

	e := &Executor{
		config: &Config{ProjectWorkspacePath: dir},
		logger: zerolog.Nop(),
	}
	require.NotPanics(t, func() {
		e.pruneAllWorktrees(context.Background(), map[string]struct{}{"t1": {}})
	})
	// No assertions on the inner pruneWorktrees call (it bails out
	// on non-git dirs); the relevant coverage is the outer loop
	// hitting each subdir.
}

// TestPruneOrphanWorktreeDirs_NoWorktreesDir — when the project
// has no .worktrees/ subdir, the helper returns silently
// (os.IsNotExist branch). The vast majority of projects sit in
// this state until a task spawns a worktree.
func TestPruneOrphanWorktreeDirs_NoWorktreesDir(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	require.NotPanics(t, func() {
		pruneOrphanWorktreeDirs(context.Background(), dir, zerolog.Nop())
	})
}

// TestPruneOrphanWorktreeDirs_EmptyWorktreesDir — .worktrees/
// exists but is empty. The helper reads the dir, finds nothing
// to evaluate, returns.
func TestPruneOrphanWorktreeDirs_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".worktrees"), 0o755))
	require.NotPanics(t, func() {
		pruneOrphanWorktreeDirs(context.Background(), dir, zerolog.Nop())
	})
}
