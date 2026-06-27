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

// TestPruneWorktrees_PreservesInFlightTasks pins the regression
// that surfaced as T-4ae6 (2026-05-10):
//
// During a daemon restart while an execution was RUNNING in the
// DB, the startup pruneWorktrees call deleted the worktree under
// the recovered goroutine. The recovered execution then tried to
// start a new podman container with the now-missing worktree as
// a bind-mount source — `Error: statfs ... no such file or
// directory` — and the task terminal-failed with MERGE_FAILED.
//
// The fix: pruneWorktrees takes a `preserve` set of task IDs.
// Worktrees for tasks in the set are left alone so recoverExecution
// can adopt them. This test creates two worktrees, marks one for
// preservation, runs prune, and asserts only the un-preserved one
// was deleted.
func TestPruneWorktrees_PreservesInFlightTasks(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	logger := zerolog.Nop()

	// Two worktrees: one will be preserved (in-flight execution),
	// one will be pruned (truly orphaned).
	preservedTask := "task_preserve_keep_me"
	prunedTask := "task_preserve_prune_me"

	if _, err := createWorktree(context.Background(), projectDir, preservedTask, logger); err != nil {
		t.Fatalf("createWorktree(preserved): %v", err)
	}
	if _, err := createWorktree(context.Background(), projectDir, prunedTask, logger); err != nil {
		t.Fatalf("createWorktree(pruned): %v", err)
	}

	preserve := map[string]struct{}{preservedTask: {}}
	pruneWorktrees(context.Background(), projectDir, logger, preserve)

	// Preserved worktree must survive: directory present + branch present.
	preservedDir := worktreePath(projectDir, preservedTask)
	if _, err := os.Stat(preservedDir); err != nil {
		t.Errorf("preserved worktree dir was deleted: %v", err)
	}
	branchOut, err := exec.Command("git", "-C", projectDir, "branch", "--list", worktreeBranch(preservedTask)).Output()
	if err != nil {
		t.Fatalf("git branch --list (preserved): %v", err)
	}
	if strings.TrimSpace(string(branchOut)) == "" {
		t.Errorf("preserved branch was deleted: prune ignored the preserve set")
	}

	// Pruned worktree must be gone.
	prunedDir := worktreePath(projectDir, prunedTask)
	if _, err := os.Stat(prunedDir); !os.IsNotExist(err) {
		t.Errorf("orphan worktree dir survived prune: %v", err)
	}
	branchOut2, err := exec.Command("git", "-C", projectDir, "branch", "--list", worktreeBranch(prunedTask)).Output()
	if err != nil {
		t.Fatalf("git branch --list (pruned): %v", err)
	}
	if got := strings.TrimSpace(string(branchOut2)); got != "" {
		t.Errorf("orphan branch survived prune: %q", got)
	}
}

// TestPruneWorktrees_NilPreserveSafe — passing a nil preserve set
// must not panic and must prune all worktree branches as before.
// Defensive: callers that don't have a DB to query (tests, reduced
// builds) shouldn't be required to allocate an empty map.
func TestPruneWorktrees_NilPreserveSafe(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)
	logger := zerolog.Nop()

	taskID := "task_nilpreserve_1"
	if _, err := createWorktree(context.Background(), projectDir, taskID, logger); err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	pruneWorktrees(context.Background(), projectDir, logger, nil)

	if _, err := os.Stat(worktreePath(projectDir, taskID)); !os.IsNotExist(err) {
		t.Errorf("nil-preserve set should still prune orphans; dir survived: %v", err)
	}
}

// TestRemoveWorktree_RemovesPodmanResurrectedDirectory pins the
// other half of the T-4ae6 leak: when `git worktree remove`
// returns 0 but the directory remains on disk (a known behaviour
// when a parallel process — typically a podman bind-mount in a
// late-dying agent container — is holding the inodes), the
// subsequent post-condition check in removeWorktree must force-
// remove the directory.
//
// Simulation: invoke removeWorktree on a directory that has the
// `.git` worktree pointer file but is NOT in git's worktree
// registry (the registry was already pruned but the directory
// survived — exactly the state T-4ae6 left on disk). The
// post-condition `os.Stat` check should detect the leftover and
// the fallback `os.RemoveAll` should clear it.
func TestRemoveWorktree_RemovesPodmanResurrectedDirectory(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)
	logger := zerolog.Nop()

	taskID := "task_resurrected_1"
	wtDir := worktreePath(projectDir, taskID)

	// Simulate the post-restart state: directory exists with some
	// content but git's worktree registry doesn't know about it
	// (the previous prune already cleared the registry entry).
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatalf("mkdir worktree dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "leftover-from-podman"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	removeWorktree(context.Background(), projectDir, wtDir, taskID, logger)

	// The directory MUST be gone after removeWorktree returns,
	// regardless of whether `git worktree remove` succeeded
	// (which it won't for a directory git doesn't know about).
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("removeWorktree must clean the directory even when git's registry doesn't know about it; got %v", err)
	}
}
