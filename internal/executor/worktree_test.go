package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// TestProjectRootFromWorktree locks in the small helper that drives the
// ProjectGitDir bind mount on agent containers. Regressions here silently
// break every git-using agent in a worktree-enabled project because the
// bind mount would simply not be emitted.
func TestProjectRootFromWorktree(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "typical worktree path",
			in:   "/var/lib/vornik/workspaces/proj/.worktrees/task_123",
			want: "/var/lib/vornik/workspaces/proj",
		},
		{
			name: "relative worktree path",
			in:   "workspaces/proj/.worktrees/task_123",
			want: "workspaces/proj",
		},
		{
			name: "not a worktree — normal project dir",
			in:   "/var/lib/vornik/workspaces/proj",
			want: "",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
		{
			name: "parent directory isn't .worktrees",
			in:   "/var/lib/vornik/workspaces/proj/foo/task_123",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := projectRootFromWorktree(tc.in)
			if got != tc.want {
				t.Errorf("projectRootFromWorktree(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// initGitRepo creates a fresh git repository at dir with one seed commit
// and configures a local identity. Returns the initial commit's SHA and
// a cleanup function. Isolated from the developer's global git config by
// setting user.name/user.email at the repo level.
func initGitRepo(t *testing.T, dir string) string {
	t.Helper()

	must := func(name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
		}
	}

	must("git", "init", "--initial-branch=master")
	must("git", "config", "user.email", "test@vornik.local")
	must("git", "config", "user.name", "Vornik Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	must("git", "add", "-A")
	must("git", "commit", "-m", "seed")

	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestWorkspaceDirty_IgnoresWorktreesPrefix pins the regression
// where ` D .worktrees/foo` lines from `git status --porcelain`
// were leaking through the .worktrees/ filter and refusing the
// merge. The bug: TrimSpace stripped the leading status space,
// then path[3:] consumed one character of the actual path,
// turning ".worktrees/foo" into "worktrees/foo" — which fails
// the HasPrefix(path, ".worktrees/") check.
//
// Regressed the snake project on 2026-05-03: a historical
// worktree directory had been committed to master, was later
// removed on disk, and showed up as ` D .worktrees/...` deleted
// lines. Every subsequent task failed at merge with "main
// workspace has uncommitted changes" until the dirty paths were
// committed away.
func TestWorkspaceDirty_IgnoresWorktreesPrefix(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	// Stage + commit a file under .worktrees/ to mirror snake's
	// historical state: a worktree dir got accidentally
	// committed to master.
	staleDir := filepath.Join(projectDir, ".worktrees", "task_stale", "subpath")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	stalePath := filepath.Join(staleDir, "FILE.md")
	if err := os.WriteFile(stalePath, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	if out, err := exec.Command("git", "-C", projectDir, "add", "-f", ".worktrees/task_stale/subpath/FILE.md").CombinedOutput(); err != nil {
		t.Fatalf("git add stale: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", projectDir, "commit", "-m", "seed stale worktree").CombinedOutput(); err != nil {
		t.Fatalf("git commit stale: %v: %s", err, out)
	}

	// Now delete the file on disk so `git status --porcelain`
	// shows ` D .worktrees/task_stale/subpath/FILE.md`.
	if err := os.Remove(stalePath); err != nil {
		t.Fatalf("remove stale: %v", err)
	}

	dirty, detail := workspaceDirty(context.Background(), projectDir)
	if dirty {
		t.Fatalf("workspaceDirty returned dirty for a .worktrees/ deletion that should be filtered: %s", detail)
	}
}

// TestWorkspaceDirty_FlagsRealChanges — counterpart to the
// previous test: changes OUTSIDE .worktrees/ must still mark
// the workspace as dirty so the merge correctly refuses.
func TestWorkspaceDirty_FlagsRealChanges(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	if err := os.WriteFile(filepath.Join(projectDir, "operator-edit.md"), []byte("operator's hand-edit\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	dirty, detail := workspaceDirty(context.Background(), projectDir)
	if !dirty {
		t.Fatalf("workspaceDirty must flag a real working-tree change as dirty")
	}
	if !strings.Contains(detail, "operator-edit.md") {
		t.Fatalf("detail must reference the dirty path: %s", detail)
	}
}

// TestRemoveWorktree_SurvivesCancelledParentContext pins the
// regression that motivated the detached-cleanup-context refactor:
// removeWorktree is called with the EXECUTION context, which is
// cancelled BEFORE this cleanup runs (cancellation is the most
// common reason removeWorktree fires). With the previous
// implementation, every `git` subprocess used CommandContext(ctx)
// → instantly killed by ctx cancellation → empty stdout/stderr,
// branch never deleted, orphan branch silently accumulating with
// only "git branch delete failed twice" + two empty diagnostic
// fields in the log. Now: removeWorktree derives a detached
// 30s context internally and the cleanup actually runs.
func TestRemoveWorktree_SurvivesCancelledParentContext(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	taskID := "task_test_cancel_cleanup_1"
	logger := zerolog.Nop()

	// Set up a worktree with the same shape removeWorktree expects.
	if _, err := createWorktree(context.Background(), projectDir, taskID, logger); err != nil {
		t.Fatalf("createWorktree: %v", err)
	}

	// Simulate task cancellation: pass an already-cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	removeWorktree(ctx, projectDir, worktreePath(projectDir, taskID), taskID, logger)

	// Branch must be gone.
	branchOut, err := exec.Command("git", "-C", projectDir, "branch", "--list", worktreeBranch(taskID)).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if got := strings.TrimSpace(string(branchOut)); got != "" {
		t.Fatalf("orphan branch survived removeWorktree under cancelled ctx: %q", got)
	}

	// Worktree dir must be gone.
	wtDir := worktreePath(projectDir, taskID)
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("worktree dir survived removeWorktree under cancelled ctx: %v", err)
	}
}

// TestMergeWorktree_AutoCommitsLeftoverChanges is the end-to-end check that
// ties our work-persistence contract together: when a role (scout,
// researcher, etc.) writes files into the worktree but never runs git
// commit, the executor auto-commits on its behalf and the merge back to
// master produces a real merge commit carrying that work. The fix for the
// "task completes but nothing reached master" failure mode lives here.
func TestMergeWorktree_AutoCommitsLeftoverChanges(t *testing.T) {
	projectDir := t.TempDir()
	seedSHA := initGitRepo(t, projectDir)

	ctx := context.Background()
	logger := zerolog.Nop()
	taskID := "task_test_autocommit_1"

	wtDir, err := createWorktree(ctx, projectDir, taskID, logger)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}

	// Simulate a scout-style role: write a new file into the worktree,
	// do NOT commit. This reproduces the exact state that was silently
	// losing scout's PROJECT_CONTEXT.md in production.
	payload := []byte("# project context\n\nwritten by the fake scout\n")
	if err := os.WriteFile(filepath.Join(wtDir, "PROJECT_CONTEXT.md"), payload, 0o644); err != nil {
		t.Fatalf("write PROJECT_CONTEXT.md: %v", err)
	}

	if err := mergeWorktree(ctx, projectDir, wtDir, taskID, logger); err != nil {
		t.Fatalf("mergeWorktree: %v", err)
	}

	// master must have advanced past the seed commit.
	headOut, err := exec.Command("git", "-C", projectDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse after merge: %v", err)
	}
	newHead := strings.TrimSpace(string(headOut))
	if newHead == seedSHA {
		t.Fatalf("master HEAD did not advance after merge — scout's work was lost (HEAD still at seed %s)", seedSHA)
	}

	// The file must now be tracked on master with the scout's content.
	fileOut, err := exec.Command("git", "-C", projectDir, "show", "HEAD:PROJECT_CONTEXT.md").Output()
	if err != nil {
		t.Fatalf("git show HEAD:PROJECT_CONTEXT.md: %v", err)
	}
	if string(fileOut) != string(payload) {
		t.Errorf("HEAD file content mismatch:\n got: %q\nwant: %q", string(fileOut), string(payload))
	}

	// Commit log must show both: the merge and the underlying auto-commit.
	logOut, err := exec.Command("git", "-C", projectDir, "log", "--format=%s", "-5").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	log := string(logOut)
	if !strings.Contains(log, "merge: worktree task "+taskID) {
		t.Errorf("expected merge commit for %s; log was:\n%s", taskID, log)
	}
	if !strings.Contains(log, "auto-commit: leftover work from "+taskID) {
		t.Errorf("expected auto-commit for %s; log was:\n%s", taskID, log)
	}

	// The auto-commit identity must be the fixed vornik-agent one so the
	// provenance is unambiguous in git log.
	authorOut, err := exec.Command("git", "-C", projectDir, "log",
		"--format=%an <%ae>", "--grep=auto-commit: leftover work", "-1").Output()
	if err != nil {
		t.Fatalf("git log --grep: %v", err)
	}
	if got := strings.TrimSpace(string(authorOut)); got != "vornik-agent <agent@vornik.io>" {
		t.Errorf("auto-commit identity = %q, want %q", got, "vornik-agent <agent@vornik.io>")
	}

	// Worktree must be cleaned up (directory removed, branch deleted) so
	// the second run doesn't collide. This is the existing contract we're
	// not changing, but we verify it here because the fix shouldn't
	// regress cleanup.
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists: %v", err)
	}
	branchOut, _ := exec.Command("git", "-C", projectDir, "branch", "--list", worktreeBranch(taskID)).Output()
	if strings.TrimSpace(string(branchOut)) != "" {
		t.Errorf("worktree branch still exists after merge: %q", branchOut)
	}
}

// TestMergeWorktree_ResolvesAddAddConflictTakingTheirs — regression for
// task …582f (2026-06-13). Two concurrent assistant tasks each create the same
// canonical output path (artifacts/out/research.md) off a base that lacked it;
// the first lands it on master, the second's merge hits an add/add conflict.
// add/add = no merge-base version, so there is no divergent edit to lose — the
// completing task's fresh output must win (theirs), not fail the task.
func TestMergeWorktree_ResolvesAddAddConflictTakingTheirs(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir) // seed has NO artifacts/out/research.md

	ctx := context.Background()
	logger := zerolog.Nop()
	taskID := "task_test_addadd_1"

	wtDir, err := createWorktree(ctx, projectDir, taskID, logger) // branches off the seed
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	// This task writes its output (uncommitted — mergeWorktree auto-commits it).
	outDir := filepath.Join(wtDir, "artifacts", "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir worktree out: %v", err)
	}
	taskVersion := []byte("# research\n\nthis task's fresh output\n")
	if err := os.WriteFile(filepath.Join(outDir, "research.md"), taskVersion, 0o644); err != nil {
		t.Fatalf("write worktree research.md: %v", err)
	}
	// A concurrent task already landed a DIFFERENT research.md on master.
	masterOut := filepath.Join(projectDir, "artifacts", "out")
	if err := os.MkdirAll(masterOut, 0o755); err != nil {
		t.Fatalf("mkdir master out: %v", err)
	}
	if err := os.WriteFile(filepath.Join(masterOut, "research.md"), []byte("# research\n\nstale prior-task output\n"), 0o644); err != nil {
		t.Fatalf("write master research.md: %v", err)
	}
	mustGit := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", projectDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	mustGit("add", "-A")
	mustGit("commit", "-m", "prior task output")

	// add/add must auto-resolve (take theirs), not fail.
	if err := mergeWorktree(ctx, projectDir, wtDir, taskID, logger); err != nil {
		t.Fatalf("mergeWorktree must auto-resolve add/add conflict, got: %v", err)
	}
	got, err := exec.Command("git", "-C", projectDir, "show", "HEAD:artifacts/out/research.md").Output()
	if err != nil {
		t.Fatalf("git show after merge: %v", err)
	}
	if string(got) != string(taskVersion) {
		t.Errorf("add/add must resolve to the task's (theirs) version:\n got: %q\nwant: %q", got, taskVersion)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("worktree dir must be cleaned up after a resolved merge: %v", err)
	}
}

// TestMergeWorktree_PreservesGenuineContentConflict — the safety boundary: a
// modify/modify conflict (both branches edited a file that EXISTED at the merge
// base) is a real divergence and must NOT be silently auto-resolved. The task
// fails and the worktree is preserved for manual recovery.
func TestMergeWorktree_PreservesGenuineContentConflict(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	ctx := context.Background()
	logger := zerolog.Nop()
	mustGit := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", projectDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	// code.txt exists at the base both branches diverge from.
	if err := os.WriteFile(filepath.Join(projectDir, "code.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base code.txt: %v", err)
	}
	mustGit("add", "-A")
	mustGit("commit", "-m", "base code")

	taskID := "task_test_modmod_1"
	wtDir, err := createWorktree(ctx, projectDir, taskID, logger) // base HAS code.txt
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "code.txt"), []byte("worktree edit\n"), 0o644); err != nil {
		t.Fatalf("write worktree code.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "code.txt"), []byte("master edit\n"), 0o644); err != nil {
		t.Fatalf("write master code.txt: %v", err)
	}
	mustGit("add", "-A")
	mustGit("commit", "-m", "master edit")

	err = mergeWorktree(ctx, projectDir, wtDir, taskID, logger)
	if err == nil {
		t.Fatal("mergeWorktree must FAIL on a genuine modify/modify conflict, not auto-resolve it")
	}
	if !strings.Contains(err.Error(), "merge of worktree branch") {
		t.Errorf("error must identify the merge failure, got: %v", err)
	}
	// master must NOT have been silently overwritten with the worktree's edit.
	got, _ := exec.Command("git", "-C", projectDir, "show", "HEAD:code.txt").Output()
	if strings.TrimSpace(string(got)) != "master edit" {
		t.Errorf("master content must be untouched on a refused merge, got: %q", got)
	}
	// Worktree preserved for manual recovery.
	if _, statErr := os.Stat(wtDir); os.IsNotExist(statErr) {
		t.Error("worktree must be PRESERVED (not removed) when the merge genuinely conflicts")
	}
}

func TestMergeWorktree_RefusesDirtyMainWithoutResettingIt(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	ctx := context.Background()
	logger := zerolog.Nop()
	taskID := "task_test_dirty_main_1"

	wtDir, err := createWorktree(ctx, projectDir, taskID, logger)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "WORKTREE.md"), []byte("worktree side\n"), 0o644); err != nil {
		t.Fatalf("write worktree file: %v", err)
	}

	dirtyPath := filepath.Join(projectDir, "operator-note.md")
	dirtyBody := []byte("do not delete me\n")
	if err := os.WriteFile(dirtyPath, dirtyBody, 0o644); err != nil {
		t.Fatalf("write dirty main file: %v", err)
	}

	err = mergeWorktree(ctx, projectDir, wtDir, taskID, logger)
	if err == nil || !strings.Contains(err.Error(), "main workspace has uncommitted changes") {
		t.Fatalf("mergeWorktree error = %v, want dirty-main refusal", err)
	}

	got, readErr := os.ReadFile(dirtyPath)
	if readErr != nil {
		t.Fatalf("dirty main file was removed: %v", readErr)
	}
	if string(got) != string(dirtyBody) {
		t.Fatalf("dirty main file changed: got %q want %q", string(got), string(dirtyBody))
	}
	if _, statErr := os.Stat(wtDir); statErr != nil {
		t.Fatalf("worktree should be preserved after refused merge: %v", statErr)
	}
	branchOut, branchErr := exec.Command("git", "-C", projectDir, "branch", "--list", worktreeBranch(taskID)).Output()
	if branchErr != nil {
		t.Fatalf("git branch --list: %v", branchErr)
	}
	if strings.TrimSpace(string(branchOut)) == "" {
		t.Fatalf("worktree branch should be preserved after refused merge")
	}
}

// TestMergeWorktree_RescuesStrandedStagedTrackedChange — pins the
// 2026-05-06 self-perpetuating-trap fix. A previous task left the
// workspace-root index dirty with a STAGED change to a TRACKED file
// (the .autocoder/PROJECT_CONTEXT.md case from vornik-autocoder).
// Pre-fix every subsequent worktree merge refused with "main
// workspace has uncommitted changes" until an operator manually
// committed or stashed. Post-fix the workspace-prelude
// autoCommitTrackedChangesOnly captures the stranded change as a
// real commit on the workspace's current branch, the dirty check
// then sees a clean tree, and the merge proceeds.
//
// Critically the stranded data is preserved — operator can still
// inspect it via `git log` instead of having lost it to a manual
// `git restore` workaround.
func TestMergeWorktree_RescuesStrandedStagedTrackedChange(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	// Track a file in the workspace-root, then leave a staged
	// modification to it without committing — simulates a task
	// that ran `git add` then crashed before `git commit`.
	trackedPath := filepath.Join(projectDir, "STRANDED.md")
	if err := os.WriteFile(trackedPath, []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	for _, args := range [][]string{
		{"add", "STRANDED.md"},
		{"-c", "user.name=test", "-c", "user.email=test@test", "commit", "-m", "track stranded"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", projectDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(trackedPath, []byte("v2 — stranded edit\n"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	if out, err := exec.Command("git", "-C", projectDir, "add", "STRANDED.md").CombinedOutput(); err != nil {
		t.Fatalf("git add (staging stranded edit): %v: %s", err, out)
	}

	ctx := context.Background()
	logger := zerolog.Nop()
	taskID := "task_test_stranded_1"

	wtDir, err := createWorktree(ctx, projectDir, taskID, logger)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "WORKTREE_NEW.md"), []byte("worktree side\n"), 0o644); err != nil {
		t.Fatalf("write worktree file: %v", err)
	}

	// Pre-fix this would error with "main workspace has uncommitted
	// changes". Post-fix: auto-commit the stranded change, then
	// merge cleanly.
	if err := mergeWorktree(ctx, projectDir, wtDir, taskID, logger); err != nil {
		t.Fatalf("mergeWorktree must rescue stranded staged tracked change, got: %v", err)
	}

	// The stranded edit is now committed — verify it's present in
	// HEAD and that the file content reflects v2.
	got, err := os.ReadFile(trackedPath)
	if err != nil {
		t.Fatalf("read STRANDED.md: %v", err)
	}
	if string(got) != "v2 — stranded edit\n" {
		t.Errorf("stranded change lost or modified: got %q", string(got))
	}

	// Workspace-prelude commit should be in the log alongside the
	// merge commit.
	logOut, err := exec.Command("git", "-C", projectDir, "log", "--format=%s", "-3").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	logText := string(logOut)
	if !strings.Contains(logText, "auto-commit: workspace-root prelude") {
		t.Errorf("workspace-prelude commit missing from log:\n%s", logText)
	}

	// Worktree must be cleaned up — the merge ran successfully so
	// we expect the standard removeWorktree teardown.
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be removed after successful merge: %v", err)
	}
}

// TestEnsureGitRepo covers the bootstrap contract: every project workspace
// becomes a git repo with a committed seed so worktree isolation + auto-
// commit-on-merge apply uniformly. Three scenarios matter:
//
//  1. Missing directory — must be created and initialized.
//  2. Empty existing directory — must be initialized with a seed marker.
//  3. Existing directory with files (the production regression: the
//     assistant workspace had 600+ dossier .md files sitting on a
//     non-repo filesystem) — must be initialized AND those files must
//     be committed in the bootstrap commit, not orphaned as untracked.
func TestEnsureGitRepo(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("creates missing directory and initializes repo", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "new-project")

		if err := ensureGitRepo(ctx, dir, logger); err != nil {
			t.Fatalf("ensureGitRepo: %v", err)
		}
		if !isGitRepo(dir) {
			t.Fatalf("dir is not a git repo after bootstrap: %s", dir)
		}

		// Seed commit must exist and have a HEAD we can branch from.
		if _, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output(); err != nil {
			t.Errorf("no HEAD after bootstrap: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".vornik", "bootstrap.md")); err != nil {
			t.Errorf("bootstrap marker not created: %v", err)
		}
	})

	t.Run("no-op when already a git repo", func(t *testing.T) {
		dir := t.TempDir()
		initGitRepo(t, dir) // creates repo + seed commit
		headBefore, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()

		if err := ensureGitRepo(ctx, dir, logger); err != nil {
			t.Fatalf("ensureGitRepo: %v", err)
		}
		headAfter, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()

		if string(headBefore) != string(headAfter) {
			t.Errorf("existing repo's HEAD changed — bootstrap should be a no-op")
		}
	})

	t.Run("captures pre-existing files in the bootstrap commit", func(t *testing.T) {
		// Reproduces the assistant project's exact state: a workspace
		// full of agent-written .md files sitting on a non-git filesystem
		// because the bootstrap step never ran. These files must survive
		// — and must end up committed, not orphaned as untracked — so
		// future tasks see them and don't accidentally overwrite or
		// re-generate their content.
		dir := t.TempDir()
		for _, name := range []string{"alice-dossier.md", "bob-dossier.md"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("# "+name+"\n"), 0o644); err != nil {
				t.Fatalf("seed existing file: %v", err)
			}
		}

		if err := ensureGitRepo(ctx, dir, logger); err != nil {
			t.Fatalf("ensureGitRepo: %v", err)
		}

		// Every pre-existing file should be tracked on HEAD.
		lsOut, err := exec.Command("git", "-C", dir, "ls-tree", "-r", "--name-only", "HEAD").Output()
		if err != nil {
			t.Fatalf("ls-tree: %v", err)
		}
		tracked := string(lsOut)
		for _, name := range []string{"alice-dossier.md", "bob-dossier.md"} {
			if !strings.Contains(tracked, name) {
				t.Errorf("expected %s in bootstrap commit; tracked:\n%s", name, tracked)
			}
		}

		// Working tree must be clean — no orphaned untracked leftovers.
		statusOut, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
		if strings.TrimSpace(string(statusOut)) != "" {
			t.Errorf("working tree not clean after bootstrap:\n%s", statusOut)
		}

		// The bootstrap marker is NOT added when the dir had content of
		// its own — avoid polluting projects that already have structure.
		if _, err := os.Stat(filepath.Join(dir, ".vornik", "bootstrap.md")); !os.IsNotExist(err) {
			t.Errorf("bootstrap marker should not be created when dir has pre-existing files")
		}
	})

	t.Run("empty dir is a no-op", func(t *testing.T) {
		if err := ensureGitRepo(ctx, "", logger); err != nil {
			t.Errorf("ensureGitRepo(\"\") should be a no-op, got: %v", err)
		}
	})
}

// TestMergeWorktree_SucceedsWithoutRepoIdentity locks in the fix for
// task_20260419194623_6b319f72664e632b, where the editor successfully
// wrote PROJECT_CONTEXT.md into the worktree but the merge-back failed
// with "Committer identity unknown" because this deployment has no
// global git config. The fix is to inline user.name/user.email via -c
// on the git merge command itself; this test proves the merge now
// succeeds on a repo with NO local or global identity, which is the
// real-world failure mode.
func TestMergeWorktree_SucceedsWithoutRepoIdentity(t *testing.T) {
	projectDir := t.TempDir()

	must := func(name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = projectDir
		// Isolate from the developer's global git config so we reproduce
		// the bare-host / rootless-container scenario faithfully.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
		}
	}
	must("git", "init", "--initial-branch=master")
	// Deliberately DO NOT configure user.email / user.name on the repo.
	// Seed commit uses inline identity just to bootstrap the repo.
	if err := os.WriteFile(filepath.Join(projectDir, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	must("git", "add", "-A")
	must("git", "-c", "user.email=seed@local", "-c", "user.name=seed", "commit", "-m", "seed")

	ctx := context.Background()
	logger := zerolog.Nop()
	taskID := "task_test_noidentity_1"

	wtDir, err := createWorktree(ctx, projectDir, taskID, logger)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}

	// Simulate the editor writing a file into the worktree.
	payload := []byte("# updated by editor\n\nnew content\n")
	if err := os.WriteFile(filepath.Join(wtDir, "EDITOR_OUTPUT.md"), payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The fix: mergeWorktree passes -c user.name -c user.email inline,
	// so the merge commit succeeds even though the repo has no configured
	// identity.
	if err := mergeWorktree(ctx, projectDir, wtDir, taskID, logger); err != nil {
		t.Fatalf("mergeWorktree returned error despite inline identity: %v", err)
	}

	// File must be on master.
	show, err := exec.Command("git", "-C", projectDir, "show", "HEAD:EDITOR_OUTPUT.md").Output()
	if err != nil {
		t.Fatalf("git show HEAD:EDITOR_OUTPUT.md: %v", err)
	}
	if string(show) != string(payload) {
		t.Errorf("merged content mismatch: got %q, want %q", string(show), string(payload))
	}
}

// TestMergeWorktree_ErrorSignalsFailureToCaller locks in the other half
// of the fix: when a merge genuinely fails (e.g. a conflict we can't
// recover from), mergeWorktree returns a non-nil error so the caller
// can promote it to a task-level FAILURE rather than silently
// swallowing the lost work.
func TestMergeWorktree_ErrorSignalsFailureToCaller(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	ctx := context.Background()
	logger := zerolog.Nop()
	taskID := "task_test_mergefail_1"

	must := func(dir, name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s in %s: %v: %s", name, strings.Join(args, " "), dir, err, out)
		}
	}

	// A genuine modify/modify conflict (NOT add/add — add/add now
	// auto-resolves by design, see TestMergeWorktree_ResolvesAddAddConflict…).
	// CONFLICT.md must exist at the merge base so both sides' edits diverge from
	// a common version; that's what makes the merge unresolvable and exercises
	// the silent-loss regression this test guards (a failed merge MUST surface
	// an error to the caller, never a silent COMPLETED).
	if err := os.WriteFile(filepath.Join(projectDir, "CONFLICT.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base CONFLICT.md: %v", err)
	}
	must(projectDir, "git", "add", "-A")
	must(projectDir, "git", "commit", "-m", "base CONFLICT.md")

	wtDir, err := createWorktree(ctx, projectDir, taskID, logger) // base HAS CONFLICT.md
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "CONFLICT.md"), []byte("worktree side\n"), 0o644); err != nil {
		t.Fatalf("write worktree CONFLICT.md: %v", err)
	}
	must(wtDir, "git", "add", "-A")
	must(wtDir, "git", "commit", "-m", "worktree edit")

	if err := os.WriteFile(filepath.Join(projectDir, "CONFLICT.md"), []byte("master side\n"), 0o644); err != nil {
		t.Fatalf("write master CONFLICT.md: %v", err)
	}
	must(projectDir, "git", "add", "-A")
	must(projectDir, "git", "commit", "-m", "master edit")

	if err := mergeWorktree(ctx, projectDir, wtDir, taskID, logger); err == nil {
		t.Fatalf("expected merge to fail on conflict, but got nil error")
	}

	// After a failed merge the worktree must be preserved (NOT removed)
	// so the operator can salvage the commits.
	if _, err := os.Stat(wtDir); err != nil {
		t.Errorf("worktree was removed after failed merge; operator has no salvage path: %v", err)
	}
}

// TestMergeWorktree_NoopMergeWhenNothingChanged confirms the honest-log
// behaviour: when the worktree branch has no commits ahead of master
// (nothing was written, nothing auto-committed), the merge still exits 0
// but produces no new commit, and we log "no commits to merge" rather
// than the misleading "worktree branch merged" that hid the earlier bug.
func TestMergeWorktree_NoopMergeWhenNothingChanged(t *testing.T) {
	projectDir := t.TempDir()
	seedSHA := initGitRepo(t, projectDir)

	ctx := context.Background()
	logger := zerolog.Nop()
	taskID := "task_test_noop_1"

	wtDir, err := createWorktree(ctx, projectDir, taskID, logger)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}

	// Do not touch the worktree — simulate a role that read some files
	// and returned without writing anything (e.g. a planner that output
	// only JSON to /app/output, never touched project/).
	if err := mergeWorktree(ctx, projectDir, wtDir, taskID, logger); err != nil {
		t.Fatalf("mergeWorktree (noop): %v", err)
	}

	// master HEAD must not have moved — no merge commit should exist
	// because there was nothing to merge.
	headOut, _ := exec.Command("git", "-C", projectDir, "rev-parse", "HEAD").Output()
	if got := strings.TrimSpace(string(headOut)); got != seedSHA {
		t.Errorf("HEAD advanced unexpectedly: got %s, want seed %s", got, seedSHA)
	}
}

func TestPruneWorktrees_RemovesBranchWhenWorktreeDirIsGone(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	ctx := context.Background()
	logger := zerolog.Nop()
	taskID := "task_test_crash_branch_left"

	wtDir, err := createWorktree(ctx, projectDir, taskID, logger)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}

	// Simulate a daemon/host crash after git registered the worktree and
	// branch, but after the worktree directory disappeared out-of-band.
	if err := os.RemoveAll(wtDir); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}

	pruneWorktrees(ctx, projectDir, logger, nil)

	branchOut, err := exec.Command("git", "-C", projectDir, "branch", "--list", worktreeBranch(taskID)).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if got := strings.TrimSpace(string(branchOut)); got != "" {
		t.Fatalf("orphan worktree branch survived startup prune: %q", got)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("worktree dir survived startup prune: %v", err)
	}
}

func TestPruneWorktrees_RemovesDirectoryWhenBranchIsGone(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	ctx := context.Background()
	logger := zerolog.Nop()
	taskID := "task_test_crash_dir_left"

	wtDir, err := createWorktree(ctx, projectDir, taskID, logger)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	if out, err := exec.Command("git", "-C", projectDir, "worktree", "remove", "--force", wtDir).CombinedOutput(); err != nil {
		t.Fatalf("git worktree remove: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", projectDir, "branch", "-D", worktreeBranch(taskID)).CombinedOutput(); err != nil {
		t.Fatalf("git branch -D: %v: %s", err, out)
	}
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatalf("recreate orphan worktree dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "orphan.txt"), []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write orphan marker: %v", err)
	}

	pruneWorktrees(ctx, projectDir, logger, nil)

	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("orphan worktree dir survived startup prune: %v", err)
	}
}

// TestEnsureGitRepo_ReceiveGuards verifies that ensureGitRepo (and the
// extracted ensureReceiveGuards helper) installs push-protection guards on
// every call — fresh repos, already-existing repos, and second+ calls
// (idempotency). It also exercises the installed hook script directly by
// piping synthetic stdin lines and asserting accept/reject behaviour.
func TestEnsureGitRepo_ReceiveGuards(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("fresh repo gets config + hook", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "fresh-repo")

		if err := ensureGitRepo(ctx, dir, logger); err != nil {
			t.Fatalf("ensureGitRepo: %v", err)
		}

		// receive.denyCurrentBranch must be set to updateInstead.
		out, err := exec.Command("git", "-C", dir, "config", "--get", "receive.denyCurrentBranch").Output()
		if err != nil {
			t.Fatalf("git config --get receive.denyCurrentBranch: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != "updateInstead" {
			t.Errorf("receive.denyCurrentBranch = %q, want %q", got, "updateInstead")
		}

		// .git/hooks/pre-receive must exist and be executable (0o755).
		hookPath := filepath.Join(dir, ".git", "hooks", "pre-receive")
		info, err := os.Stat(hookPath)
		if err != nil {
			t.Fatalf("pre-receive hook not found: %v", err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("pre-receive hook is not executable: mode=%o", info.Mode().Perm())
		}

		// Hook script must contain the worktree/* guard.
		body, err := os.ReadFile(hookPath)
		if err != nil {
			t.Fatalf("read pre-receive: %v", err)
		}
		bodyStr := string(body)
		if !strings.Contains(bodyStr, "refs/heads/worktree/") {
			t.Errorf("hook does not contain worktree/* guard; body:\n%s", bodyStr)
		}
		if !strings.Contains(bodyStr, "refs/vornik/") {
			t.Errorf("hook does not contain refs/vornik/* guard; body:\n%s", bodyStr)
		}
		// Non-ff guard requires merge-base check.
		if !strings.Contains(bodyStr, "merge-base") {
			t.Errorf("hook does not contain non-ff merge-base check; body:\n%s", bodyStr)
		}
	})

	t.Run("idempotent on existing repo", func(t *testing.T) {
		dir := t.TempDir()
		// Use ensureGitRepo to create the repo.
		if err := ensureGitRepo(ctx, dir, logger); err != nil {
			t.Fatalf("first ensureGitRepo: %v", err)
		}
		// Call again — must not error, config must still be correct.
		if err := ensureGitRepo(ctx, dir, logger); err != nil {
			t.Fatalf("second ensureGitRepo (idempotency): %v", err)
		}

		out, err := exec.Command("git", "-C", dir, "config", "--get", "receive.denyCurrentBranch").Output()
		if err != nil {
			t.Fatalf("git config after second call: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != "updateInstead" {
			t.Errorf("receive.denyCurrentBranch after idempotent call = %q, want %q", got, "updateInstead")
		}

		hookPath := filepath.Join(dir, ".git", "hooks", "pre-receive")
		info, err := os.Stat(hookPath)
		if err != nil {
			t.Fatalf("pre-receive hook missing after idempotent call: %v", err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("pre-receive hook not executable after idempotent call: mode=%o", info.Mode().Perm())
		}
	})

	t.Run("guards applied to pre-existing repo (no early-return bypass)", func(t *testing.T) {
		// Simulate a repo that existed before this change shipped — it has no
		// guards; calling ensureGitRepo on it must install them.
		dir := t.TempDir()
		initGitRepo(t, dir) // sets up repo manually, no ensureGitRepo involved

		// Confirm the guards are NOT yet present.
		if _, err := exec.Command("git", "-C", dir, "config", "--get", "receive.denyCurrentBranch").Output(); err == nil {
			t.Skip("test setup: receive.denyCurrentBranch unexpectedly already set; skip")
		}

		if err := ensureGitRepo(ctx, dir, logger); err != nil {
			t.Fatalf("ensureGitRepo on pre-existing repo: %v", err)
		}

		out, err := exec.Command("git", "-C", dir, "config", "--get", "receive.denyCurrentBranch").Output()
		if err != nil {
			t.Fatalf("guards not installed on pre-existing repo: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != "updateInstead" {
			t.Errorf("receive.denyCurrentBranch on pre-existing repo = %q, want %q", got, "updateInstead")
		}

		hookPath := filepath.Join(dir, ".git", "hooks", "pre-receive")
		if _, err := os.Stat(hookPath); err != nil {
			t.Fatalf("pre-receive hook not installed on pre-existing repo: %v", err)
		}
	})

	t.Run("hook rejects push to refs/heads/worktree/*", func(t *testing.T) {
		dir := t.TempDir()
		if err := ensureGitRepo(ctx, dir, logger); err != nil {
			t.Fatalf("ensureGitRepo: %v", err)
		}
		hookPath := filepath.Join(dir, ".git", "hooks", "pre-receive")

		// Pipe a fake worktree/* update line to the hook.
		// Format: "<old-sha> <new-sha> <ref>"
		fakeOld := strings.Repeat("a", 40)
		fakeNew := strings.Repeat("b", 40)
		stdin := fakeOld + " " + fakeNew + " refs/heads/worktree/task_123\n"

		cmd := exec.Command("sh", hookPath)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("hook must reject refs/heads/worktree/* push (exit non-zero), but exited 0; output: %s", out)
		}
		if !strings.Contains(string(out), "worktree") {
			t.Errorf("hook rejection message must mention 'worktree'; got: %s", out)
		}
	})

	t.Run("hook rejects push to refs/vornik/*", func(t *testing.T) {
		dir := t.TempDir()
		if err := ensureGitRepo(ctx, dir, logger); err != nil {
			t.Fatalf("ensureGitRepo: %v", err)
		}
		hookPath := filepath.Join(dir, ".git", "hooks", "pre-receive")

		fakeOld := strings.Repeat("a", 40)
		fakeNew := strings.Repeat("b", 40)
		stdin := fakeOld + " " + fakeNew + " refs/vornik/internal\n"

		cmd := exec.Command("sh", hookPath)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("hook must reject refs/vornik/* push, but exited 0; output: %s", out)
		}
	})

	t.Run("hook allows create (old all-zeros) on default branch", func(t *testing.T) {
		dir := t.TempDir()
		if err := ensureGitRepo(ctx, dir, logger); err != nil {
			t.Fatalf("ensureGitRepo: %v", err)
		}
		hookPath := filepath.Join(dir, ".git", "hooks", "pre-receive")

		// old = all zeros means branch creation — must always be allowed.
		zeroSHA := strings.Repeat("0", 40)
		newSHA := strings.Repeat("b", 40)
		stdin := zeroSHA + " " + newSHA + " refs/heads/master\n"

		cmd := exec.Command("sh", hookPath)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("hook must allow create (old=zeros) on default branch, got error: %v; output: %s", err, out)
		}
	})

	t.Run("hook allows fast-forward on default branch", func(t *testing.T) {
		// Build a real two-commit chain so merge-base --is-ancestor can resolve.
		dir := t.TempDir()
		initGitRepo(t, dir) // creates the first commit

		// Create a second commit.
		if err := os.WriteFile(filepath.Join(dir, "second.txt"), []byte("second\n"), 0o644); err != nil {
			t.Fatalf("write second.txt: %v", err)
		}
		if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
			t.Fatalf("git add: %v: %s", err, out)
		}
		if out, err := exec.Command("git", "-C", dir,
			"-c", "user.name=test", "-c", "user.email=test@test",
			"commit", "-m", "second").CombinedOutput(); err != nil {
			t.Fatalf("git commit: %v: %s", err, out)
		}
		// Now install the guards.
		if err := ensureGitRepo(ctx, dir, logger); err != nil {
			t.Fatalf("ensureGitRepo: %v", err)
		}

		// Get the two SHAs: parent (old) and HEAD (new).
		newSHAOut, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		oldSHAOut, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD~1").Output()
		newSHA := strings.TrimSpace(string(newSHAOut))
		oldSHA := strings.TrimSpace(string(oldSHAOut))

		hookPath := filepath.Join(dir, ".git", "hooks", "pre-receive")
		stdin := oldSHA + " " + newSHA + " refs/heads/master\n"

		cmd := exec.Command("sh", hookPath)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("hook must allow fast-forward push, got: %v; output: %s", err, out)
		}
	})

	t.Run("hook rejects non-fast-forward on default branch", func(t *testing.T) {
		// Build two diverging commits from a common base.
		dir := t.TempDir()
		initGitRepo(t, dir) // base commit

		// Commit A (master goes here).
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
			t.Fatalf("write a.txt: %v", err)
		}
		mustGit := func(args ...string) string {
			out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		mustGit("add", "-A")
		mustGit("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "A")
		shaA := mustGit("rev-parse", "HEAD")

		// Commit B off base (simulate a rewrite: go back to initial commit and add B).
		baseSHA := mustGit("rev-parse", "HEAD~1")
		// Create a branch at the base.
		mustGit("checkout", "-b", "rewrite-branch", baseSHA)
		if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644); err != nil {
			t.Fatalf("write b.txt: %v", err)
		}
		mustGit("add", "-A")
		mustGit("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "B")
		shaB := mustGit("rev-parse", "HEAD")
		// Go back to master.
		mustGit("checkout", "master")

		// Install guards.
		if err := ensureGitRepo(ctx, dir, logger); err != nil {
			t.Fatalf("ensureGitRepo: %v", err)
		}

		hookPath := filepath.Join(dir, ".git", "hooks", "pre-receive")
		// old=shaA (current master tip), new=shaB (diverged commit — NOT ancestor of A)
		stdin := shaA + " " + shaB + " refs/heads/master\n"

		cmd := exec.Command("sh", hookPath)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("hook must reject non-fast-forward push to default branch, but exited 0; output: %s", out)
		}
		if !strings.Contains(string(out), "non-fast-forward") && !strings.Contains(string(out), "fast-forward") {
			t.Errorf("hook rejection must mention fast-forward; got: %s", out)
		}
	})
}

// TestEnsureReceiveGuards_GitConfigError covers the error path when
// `git config receive.denyCurrentBranch` fails (e.g. repo is corrupted).
// Uses the gitRunner seam so no real git repo is needed.
func TestEnsureReceiveGuards_GitConfigError(t *testing.T) {
	f := newFakeGitRunner()
	f.errs["config"] = errors.New("not a git repository")
	withGitRunner(t, f)

	err := EnsureReceiveGuards(context.Background(), "/fake/dir", zerolog.Nop())
	if err == nil {
		t.Fatal("ensureReceiveGuards must return error when git config fails")
	}
	if !strings.Contains(err.Error(), "git config receive.denyCurrentBranch") {
		t.Errorf("error must identify the failing step, got: %v", err)
	}
}

// TestEnsureReceiveGuards_HookWriteError covers the error path when the
// hook file cannot be written (e.g. .git/hooks is read-only).
func TestEnsureReceiveGuards_HookWriteError(t *testing.T) {
	// Use a real repo so git config succeeds, but make .git/hooks read-only.
	dir := t.TempDir()
	initGitRepo(t, dir)

	hooksDir := filepath.Join(dir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	// Make the hooks dir read-only so WriteFile fails.
	if err := os.Chmod(hooksDir, 0o555); err != nil {
		t.Fatalf("chmod hooks dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(hooksDir, 0o755) })

	err := EnsureReceiveGuards(context.Background(), dir, zerolog.Nop())
	if err == nil {
		// On some systems root ignores mode; skip rather than fail.
		if os.Getuid() == 0 {
			t.Skip("running as root — read-only dir test not meaningful")
		}
		t.Fatal("ensureReceiveGuards must return error when hook write fails")
	}
	if !strings.Contains(err.Error(), "pre-receive hook") {
		t.Errorf("error must identify hook write failure, got: %v", err)
	}
}

// TestEnsureReceiveGuards_StandaloneHelper verifies that ensureReceiveGuards
// can be called directly (Task 2.4's push handler will call it before
// invoking receive-pack).
func TestEnsureReceiveGuards_StandaloneHelper(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	dir := t.TempDir()
	initGitRepo(t, dir)

	if err := EnsureReceiveGuards(ctx, dir, logger); err != nil {
		t.Fatalf("ensureReceiveGuards: %v", err)
	}

	out, err := exec.Command("git", "-C", dir, "config", "--get", "receive.denyCurrentBranch").Output()
	if err != nil {
		t.Fatalf("config not set by ensureReceiveGuards: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "updateInstead" {
		t.Errorf("receive.denyCurrentBranch = %q, want updateInstead", got)
	}

	hookPath := filepath.Join(dir, ".git", "hooks", "pre-receive")
	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("hook not installed by ensureReceiveGuards: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("hook not executable: mode=%o", info.Mode().Perm())
	}

	// Second call must be idempotent.
	if err := EnsureReceiveGuards(ctx, dir, logger); err != nil {
		t.Fatalf("ensureReceiveGuards (second call): %v", err)
	}
}

// TestCreateWorktree_SelfCleansOnHookFailure reproduces the n8n-agents
// leak: a post-checkout hook (git-lfs, husky, etc.) returns non-zero,
// git reports exit status 2, but the working-tree dir, admin dir, and
// branch have all already been written. Prior to the self-clean patch
// these three layers accumulated — 10+ dirs across one day of task
// activity on a project with git-lfs configured but git-lfs not on
// the daemon's PATH. After the patch createWorktree removes all three
// on error so the caller gets a clean slate.
func TestCreateWorktree_SelfCleansOnHookFailure(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	// Wire a failing post-checkout hook via core.hookspath — same
	// mechanism git-lfs uses to install its own hooks. The hook
	// exits 1, which makes `git worktree add` report exit 2 after
	// it has created both the working tree dir and the admin dir.
	hooksDir := filepath.Join(projectDir, ".custom-hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	hookPath := filepath.Join(hooksDir, "post-checkout")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	if out, err := exec.Command("git", "-C", projectDir, "config", "core.hookspath", hooksDir).CombinedOutput(); err != nil {
		t.Fatalf("git config hookspath: %v: %s", err, out)
	}

	ctx := context.Background()
	logger := zerolog.Nop()
	taskID := "task_test_hook_fail"

	// Expect an error — the hook failed.
	if _, err := createWorktree(ctx, projectDir, taskID, logger); err == nil {
		t.Fatalf("expected createWorktree to surface the hook failure")
	}

	// All three layers must be gone afterwards.
	wtDir := worktreePath(projectDir, taskID)
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("working tree dir survived createWorktree error: %v", err)
	}
	adminDir := filepath.Join(projectDir, ".git", "worktrees", taskID)
	if _, err := os.Stat(adminDir); !os.IsNotExist(err) {
		t.Fatalf("admin dir survived createWorktree error: %v", err)
	}
	branchOut, err := exec.Command("git", "-C", projectDir, "branch", "--list", worktreeBranch(taskID)).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if got := strings.TrimSpace(string(branchOut)); got != "" {
		t.Fatalf("branch survived createWorktree error: %q", got)
	}
}
