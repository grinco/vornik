package executor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

// These tests exercise worktree.go's git-backed flow through the gitRunner
// seam (P2 increment 2) — previously this required a real git repository on
// disk. createWorktree is a representative case: it mixes a real filesystem
// op (MkdirAll) with git calls routed through gitExec.

func TestCreateWorktree_HappyPathReturnsWorktreeDir(t *testing.T) {
	f := newFakeGitRunner() // no scripted errors → `git worktree add` succeeds
	withGitRunner(t, f)

	projectDir := t.TempDir()
	wt, err := createWorktree(context.Background(), projectDir, "task_abc", zerolog.Nop())
	if err != nil {
		t.Fatalf("createWorktree: unexpected error %v", err)
	}
	if want := filepath.Join(projectDir, ".worktrees", "task_abc"); wt != want {
		t.Fatalf("worktree dir = %q, want %q", wt, want)
	}
	// It issued `git worktree add <wtDir> -b <branch>`.
	if len(f.calls) == 0 || f.subcmd(f.calls[0]) != "worktree" {
		t.Fatalf("expected a `git worktree add` call first, got %v", f.calls)
	}
}

func TestCreateWorktree_AddFailsCleansUpAndErrors(t *testing.T) {
	f := newFakeGitRunner()
	f.errs["worktree"] = errors.New("post-checkout hook aborted")
	withGitRunner(t, f)

	projectDir := t.TempDir()
	wt, err := createWorktree(context.Background(), projectDir, "task_xyz", zerolog.Nop())
	if err == nil {
		t.Fatal("createWorktree: expected error when `git worktree add` fails")
	}
	if wt != "" {
		t.Fatalf("worktree dir on failure = %q, want \"\"", wt)
	}
	// Best-effort cleanup must have run: a `branch -D` after the failed add.
	sawBranchCleanup := false
	for _, c := range f.calls {
		if f.subcmd(c) == "branch" {
			sawBranchCleanup = true
		}
	}
	if !sawBranchCleanup {
		t.Fatalf("expected a `git branch -D` cleanup call on the failure path, got %v", f.calls)
	}
}
