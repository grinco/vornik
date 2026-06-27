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

// Task 2.5 — end-to-end git-over-HTTPS suite (File B: white-box merge-conflict
// + rebase semantics). These exercise the REAL unexported executor helpers
// (mergeWorktree, createWorktree, rebaseProjectToOrigin) against t.TempDir()
// repos, reusing the package's existing test helpers (mustGit, gitOut from
// worktree_rebase_test.go).
//
// TEST-ONLY: no production code is modified. The point is to PROVE and DOCUMENT
// the exact observed behavior — especially the same-file merge outcome
// (scenario 7) and whether a forge rebase discards an operator push (scenario 8).

// seedRepo inits a project repo on `main` with file X = base and the receive
// guards installed, mimicking a vornik-managed workspace. Returns the repo dir.
func seedRepo(t *testing.T, parent, name string) string {
	t.Helper()
	repo := filepath.Join(parent, name)
	mustGit(t, parent, "git", "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "X"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "git", "add", "-A")
	mustGit(t, repo, "git", "commit", "-q", "-m", "base")
	return repo
}

// Scenario 7a — operator push THEN worktree (the brief's literal ordering).
// The operator advances main (X=operator) BEFORE the worktree is created, so
// createWorktree branches from the operator commit. The agent's edit (X=agent)
// is therefore a linear descendant of the operator commit: NO merge-base
// divergence, so mergeWorktree merges cleanly and the agent's version lands.
//
// DOCUMENTED OUTCOME: no conflict; mergeWorktree returns nil; final X = "agent".
// The operator's content is superseded (not lost — it is the merge base), which
// is the intended last-writer-wins-within-a-lineage behavior.
func TestGitMergeE2E_S7a_OperatorThenWorktree_CleanMerge(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()
	log := zerolog.Nop()
	root := t.TempDir()
	repo := seedRepo(t, root, "proj")
	taskID := "task_s7a"

	// Operator push: advance main first (this is the state updateInstead leaves:
	// HEAD + working tree advanced).
	if err := os.WriteFile(filepath.Join(repo, "X"), []byte("operator\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "git", "add", "-A")
	mustGit(t, repo, "git", "commit", "-q", "-m", "operator push")

	// Worktree is created AFTER → branches from the operator commit.
	wt, err := createWorktree(ctx, repo, taskID, log)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "X"), []byte("agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt, "git", "add", "-A")
	mustGit(t, wt, "git", "commit", "-q", "-m", "agent edit")

	if err := mergeWorktree(ctx, repo, wt, taskID, log); err != nil {
		t.Fatalf("S7a: expected clean merge (agent descends from operator), got error: %v", err)
	}
	got := gitOut(t, repo, "git", "show", "main:X")
	if got != "agent" {
		t.Fatalf("S7a: final main:X = %q, want %q (agent supersedes operator in-lineage)", got, "agent")
	}
}

// Scenario 7b — TRUE modify/modify conflict (worktree branched from base BEFORE
// the operator push). The worktree is created at X=base, then the operator
// advances main to X=operator (divergent), then the agent commits X=agent. The
// merge has a real merge-base divergence on X. tryResolveAddAddConflictsTakeTheirs
// refuses (stage-1/base version present → not add/add), so mergeWorktree aborts
// the merge and returns an error.
//
// DOCUMENTED OUTCOME: mergeWorktree returns a non-nil error
// ("git merge of worktree branch ... failed"); after the internal `merge
// --abort`, the project working tree is CLEAN (no conflict markers left in X)
// and main:X is still the operator version; the worktree branch worktree/<task>
// is PRESERVED for manual recovery (NOT removed). No silent data loss — both
// sides survive, the task is meant to be failed by the caller.
func TestGitMergeE2E_S7b_SameFileTrueConflict_Aborts(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()
	log := zerolog.Nop()
	root := t.TempDir()
	repo := seedRepo(t, root, "proj")
	taskID := "task_s7b"

	// Worktree created FIRST, off X=base.
	wt, err := createWorktree(ctx, repo, taskID, log)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}

	// Operator push AFTER: advance main to X=operator (diverges from base).
	if err := os.WriteFile(filepath.Join(repo, "X"), []byte("operator\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "git", "add", "-A")
	mustGit(t, repo, "git", "commit", "-q", "-m", "operator push")

	// Agent edits the SAME file divergently.
	if err := os.WriteFile(filepath.Join(wt, "X"), []byte("agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt, "git", "add", "-A")
	mustGit(t, wt, "git", "commit", "-q", "-m", "agent edit")

	mergeErr := mergeWorktree(ctx, repo, wt, taskID, log)
	if mergeErr == nil {
		t.Fatal("S7b: expected modify/modify conflict to FAIL the merge, got nil error")
	}

	// After the internal merge --abort the project tree must be clean (no
	// lingering conflict markers / unmerged index entries).
	if dirty, detail := workspaceDirty(ctx, repo); dirty {
		t.Fatalf("S7b: project tree dirty after merge --abort (conflict not cleaned): %s", detail)
	}
	if got := gitOut(t, repo, "git", "show", "main:X"); got != "operator" {
		t.Fatalf("S7b: main:X = %q after abort, want %q (operator side preserved)", got, "operator")
	}

	// The worktree branch is PRESERVED for recovery (not removed on failure).
	if got := gitOut(t, repo, "git", "show", "worktree/"+taskID+":X"); got != "agent" {
		t.Fatalf("S7b: worktree branch not preserved with agent's work; show:X = %q", got)
	}
}

// Scenario 8 — forge rebase discards a local-only operator push but saves it
// to a backup ref under refs/vornik/discarded/ before the reset.
// A project clone tracks an origin remote. The operator pushes a commit to the
// LOCAL workspace default branch that is NOT on origin. The forge pre-work step
// rebaseProjectToOrigin does `git fetch origin` + `git reset --hard origin/main`,
// which DISCARDS the operator's local-only commit. The discard is non-silent:
// a backup ref is created first and a warning is logged.
//
// DOCUMENTED OUTCOME: HEAD is reset to origin (operator commit discarded from
// working tree), AND a refs/vornik/discarded/<ts>-<short> ref is created that
// points to the pre-reset HEAD (operator commit is recoverable).
func TestGitMergeE2E_S8_ForgeRebaseDiscardsOperatorPush(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()
	log := zerolog.Nop()
	root := t.TempDir()

	bare := filepath.Join(root, "origin.git")
	clone := filepath.Join(root, "clone")
	mustGit(t, root, "git", "init", "--bare", "-q", "-b", "main", bare)
	mustGit(t, root, "git", "clone", "-q", bare, clone)

	// Establish origin/main with a base commit.
	if err := os.WriteFile(filepath.Join(clone, "X"), []byte("origin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, clone, "git", "add", "-A")
	mustGit(t, clone, "git", "commit", "-q", "-m", "origin base")
	mustGit(t, clone, "git", "push", "-q", "origin", "main")
	originSHA := gitOut(t, clone, "git", "rev-parse", "HEAD")

	// Operator push: advance the LOCAL default branch with a commit NOT on origin.
	if err := os.WriteFile(filepath.Join(clone, "operator_only.txt"), []byte("operator local only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, clone, "git", "add", "-A")
	mustGit(t, clone, "git", "commit", "-q", "-m", "operator local-only push")
	localSHA := gitOut(t, clone, "git", "rev-parse", "HEAD")
	if localSHA == originSHA {
		t.Fatal("S8 setup: local HEAD should have advanced past origin")
	}

	// Forge pre-work rebase.
	rebaseProjectToOrigin(ctx, clone, "main", log)

	// The operator's local-only commit is GONE from the working tree — workspace
	// reset to origin/main.
	if got := gitOut(t, clone, "git", "rev-parse", "HEAD"); got != originSHA {
		t.Fatalf("S8: HEAD=%s after rebase, want origin %s (operator commit should be discarded)", got, originSHA)
	}
	if _, err := os.Stat(filepath.Join(clone, "operator_only.txt")); !os.IsNotExist(err) {
		t.Fatalf("S8: operator's local-only file should be GONE after reset --hard origin/main (err=%v)", err)
	}

	// BUT: a backup ref under refs/vornik/discarded/ must have been created that
	// points to the pre-reset HEAD (operator commit is recoverable).
	backupRefs := gitOut(t, clone, "git", "for-each-ref", "--format=%(refname)", "refs/vornik/discarded/")
	if backupRefs == "" {
		t.Fatal("S8: expected a backup ref under refs/vornik/discarded/ but none was created")
	}
	// There should be exactly one backup ref; use the first line.
	lines := strings.Split(strings.TrimSpace(backupRefs), "\n")
	backupRef := strings.TrimSpace(lines[0])
	// The backup ref must point to the operator's pre-reset commit SHA.
	backupSHA := gitOut(t, clone, "git", "rev-parse", backupRef)
	if backupSHA != localSHA {
		t.Fatalf("S8: backup ref %s points to %s, want pre-reset operator SHA %s", backupRef, backupSHA, localSHA)
	}
	// The operator's file must be recoverable via the backup ref.
	recoverOut := gitOut(t, clone, "git", "show", backupRef+":operator_only.txt")
	if recoverOut != "operator local only" {
		t.Fatalf("S8: operator_only.txt not recoverable from backup ref: %q", recoverOut)
	}
}

// TestRebaseProjectToOrigin_NoDivergence_NoBackupRef — when local HEAD equals
// origin (no ahead commits), rebaseProjectToOrigin must NOT create any
// refs/vornik/discarded/ backup ref. The reset still proceeds normally.
func TestRebaseProjectToOrigin_NoDivergence_NoBackupRef(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()
	log := zerolog.Nop()
	root := t.TempDir()

	bare := filepath.Join(root, "origin.git")
	clone := filepath.Join(root, "clone")
	mustGit(t, root, "git", "init", "--bare", "-q", "-b", "main", bare)
	mustGit(t, root, "git", "clone", "-q", bare, clone)

	// Establish origin/main + push so local == origin.
	if err := os.WriteFile(filepath.Join(clone, "Y"), []byte("synced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, clone, "git", "add", "-A")
	mustGit(t, clone, "git", "commit", "-q", "-m", "base")
	mustGit(t, clone, "git", "push", "-q", "origin", "main")
	syncedSHA := gitOut(t, clone, "git", "rev-parse", "HEAD")

	// Rebase: local HEAD == origin/main → no divergence.
	rebaseProjectToOrigin(ctx, clone, "main", log)

	// HEAD unchanged (still at synced commit).
	if got := gitOut(t, clone, "git", "rev-parse", "HEAD"); got != syncedSHA {
		t.Fatalf("NoDivergence: HEAD changed unexpectedly: got %s, want %s", got, syncedSHA)
	}

	// NO backup ref must have been created.
	backupRefs := gitOut(t, clone, "git", "for-each-ref", "--format=%(refname)", "refs/vornik/discarded/")
	if backupRefs != "" {
		t.Fatalf("NoDivergence: unexpected backup ref(s) created when no divergence: %s", backupRefs)
	}
}

// Scenario 9 — non-forge project uses the pushed HEAD as the base. With NO
// origin remote, rebaseProjectToOrigin early-returns (no-op), so the operator's
// pushed commit SURVIVES and a subsequent createWorktree branches from it.
//
// DOCUMENTED OUTCOME: for non-forge (local-only) projects an operator push is
// durable — it is the base the next task's worktree branches from. This is the
// counterpart to scenario 8: the discard only happens when an origin exists.
func TestGitMergeE2E_S9_NonForgeKeepsOperatorPush(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()
	log := zerolog.Nop()
	root := t.TempDir()
	repo := seedRepo(t, root, "proj") // no origin remote

	// Operator push: advance the default branch.
	if err := os.WriteFile(filepath.Join(repo, "operator_pushed.txt"), []byte("operator content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "git", "add", "-A")
	mustGit(t, repo, "git", "commit", "-q", "-m", "operator push")
	pushedSHA := gitOut(t, repo, "git", "rev-parse", "HEAD")

	// Forge pre-work rebase is a no-op without an origin remote.
	rebaseProjectToOrigin(ctx, repo, "main", log)
	if got := gitOut(t, repo, "git", "rev-parse", "HEAD"); got != pushedSHA {
		t.Fatalf("S9: rebase changed HEAD on a no-origin repo: %s != %s (must be no-op)", got, pushedSHA)
	}
	if _, err := os.Stat(filepath.Join(repo, "operator_pushed.txt")); err != nil {
		t.Fatalf("S9: operator's pushed file must survive on a non-forge repo: %v", err)
	}

	// A subsequent worktree branches FROM the operator-pushed HEAD.
	wt, err := createWorktree(ctx, repo, "task_s9", log)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "operator_pushed.txt")); err != nil {
		t.Fatalf("S9: worktree should contain the operator-pushed file (branched from pushed HEAD): %v", err)
	}
}

// requireGitExec skips when git is unavailable. (executor tests already gate on
// exec.LookPath; this is the local equivalent so this file is self-contained.)
func requireGitExec(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}
