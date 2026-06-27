package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// forgeCheckout is the loosely-parsed subset of a task's forge_job the worktree
// code needs to pick a pre-work checkout target — kept decoupled from the forge
// package (the github channel stamps forge_job top-level, or it lands under
// context). DefaultBranch drives the issue-fix rebase; HeadRef +
// IsChangeRequest drive the PR-review head checkout.
type forgeCheckout struct {
	DefaultBranch   string
	HeadRef         string
	IsChangeRequest bool
}

// forgeCheckoutSpec returns the forge checkout spec when a task carries a
// forge_job and ok=false for non-forge tasks. A task is recognizably a forge
// task once it has EITHER a default branch (issue-fix) OR a change-request head
// ref (review) — both empty means a non-forge task.
func forgeCheckoutSpec(payload []byte) (forgeCheckout, bool) {
	if len(payload) == 0 {
		return forgeCheckout{}, false
	}
	type fj struct {
		DefaultBranch   string `json:"default_branch"`
		HeadRef         string `json:"head_ref"`
		IsChangeRequest bool   `json:"is_change_request"`
	}
	var p struct {
		ForgeJob *fj `json:"forge_job"`
		Context  struct {
			ForgeJob *fj `json:"forge_job"`
		} `json:"context"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return forgeCheckout{}, false
	}
	j := p.ForgeJob
	if j == nil {
		j = p.Context.ForgeJob
	}
	if j == nil {
		return forgeCheckout{}, false
	}
	spec := forgeCheckout{
		DefaultBranch:   strings.TrimSpace(j.DefaultBranch),
		HeadRef:         strings.TrimSpace(j.HeadRef),
		IsChangeRequest: j.IsChangeRequest,
	}
	if spec.DefaultBranch == "" && spec.HeadRef == "" {
		return forgeCheckout{}, false
	}
	return spec, true
}

// checkoutForgeChangeRequest materializes a change request's head in the project
// clone's working tree before a worktree is branched off it, so a reviewer agent
// sees the PR's ACTUAL files (incident 2026-06-13: github-review ran
// rebaseProjectToOrigin, which reset the clone to origin/<default_branch>, so the
// reviewer's working tree lacked every file the PR added and it "couldn't locate
// any new files"). It fetches the provider-supplied head ref (GitHub:
// refs/pull/<n>/head, which resolves even for fork PRs) and `git reset --hard
// FETCH_HEAD`.
//
// Best-effort, same contract as rebaseProjectToOrigin (no origin / non-git dir →
// logged + skipped, task runs off local HEAD). A missing headRef OR a failed
// head fetch falls back to the default-branch rebase, so a review never silently
// runs against an arbitrary tree — the worst case matches the prior behavior.
func checkoutForgeChangeRequest(ctx context.Context, projectDir, headRef, defaultBranch string, logger zerolog.Logger) {
	if !isGitRepo(projectDir) || strings.TrimSpace(headRef) == "" {
		rebaseProjectToOrigin(ctx, projectDir, defaultBranch, logger)
		return
	}
	if out, err := gitExec.combined(ctx, "-C", projectDir, "remote", "get-url", "origin"); err != nil {
		logger.Debug().Str("project_dir", projectDir).Str("detail", strings.TrimSpace(string(out))).
			Msg("forge CR checkout: no origin remote — skipping (task runs off local HEAD)")
		return
	}
	if out, err := gitExec.combined(ctx, "-C", projectDir, "fetch", "origin", headRef); err != nil {
		logger.Warn().Err(err).Str("project_dir", projectDir).Str("head_ref", headRef).Str("detail", strings.TrimSpace(string(out))).
			Msg("forge CR checkout: fetch head ref failed — falling back to default-branch rebase")
		rebaseProjectToOrigin(ctx, projectDir, defaultBranch, logger)
		return
	}
	if out, err := gitExec.combined(ctx, "-C", projectDir, "reset", "--hard", "FETCH_HEAD"); err != nil {
		logger.Warn().Err(err).Str("project_dir", projectDir).Str("head_ref", headRef).Str("detail", strings.TrimSpace(string(out))).
			Msg("forge CR checkout: reset --hard FETCH_HEAD failed — continuing off local HEAD")
		return
	}
	logger.Info().Str("project_dir", projectDir).Str("head_ref", headRef).
		Msg("forge CR checkout: project clone reset to change-request head before worktree")
}

// rebaseProjectToOrigin makes the project clone track current upstream before a
// worktree is branched off it: `git fetch origin <branch>` then `git reset --hard
// origin/<branch>`. This is the deterministic pre-work rebase — it replaces the
// agent-side `git fetch` that the read-only `.git` mount made impossible, so a
// forge task's code work starts from HEAD of the default branch (not the local
// clone's accumulated, possibly-stale state).
//
// Best-effort by design: a project with no `origin` remote (local-only repos),
// an unreachable origin, or a missing branch is logged and skipped — the task
// still runs off the current local HEAD. Only called for forge tasks (those
// carrying a forge_job), where the workspace IS a clone of origin and resetting
// to upstream is the intended per-issue starting point.
func rebaseProjectToOrigin(ctx context.Context, projectDir, branch string, logger zerolog.Logger) {
	if !isGitRepo(projectDir) || strings.TrimSpace(branch) == "" {
		return
	}
	if out, err := gitExec.combined(ctx, "-C", projectDir, "remote", "get-url", "origin"); err != nil {
		logger.Debug().Str("project_dir", projectDir).Str("detail", strings.TrimSpace(string(out))).
			Msg("pre-work rebase: no origin remote — skipping (task runs off local HEAD)")
		return
	}
	if out, err := gitExec.combined(ctx, "-C", projectDir, "fetch", "origin", branch); err != nil {
		logger.Warn().Err(err).Str("project_dir", projectDir).Str("branch", branch).Str("detail", strings.TrimSpace(string(out))).
			Msg("pre-work rebase: git fetch origin failed — continuing off local HEAD")
		return
	}

	// Best-effort backup: save any local-only commits before they are discarded.
	backupLocalOnlyCommits(ctx, projectDir, branch, logger)

	if out, err := gitExec.combined(ctx, "-C", projectDir, "reset", "--hard", "origin/"+branch); err != nil {
		logger.Warn().Err(err).Str("project_dir", projectDir).Str("branch", branch).Str("detail", strings.TrimSpace(string(out))).
			Msg("pre-work rebase: git reset --hard origin/<branch> failed — continuing off local HEAD")
		return
	}
	logger.Info().Str("project_dir", projectDir).Str("branch", branch).
		Msg("pre-work rebase: project clone reset to current upstream before worktree")
}

// backupLocalOnlyCommits is the best-effort pre-reset step for
// rebaseProjectToOrigin: if local HEAD has commits not reachable from
// origin/<branch> (i.e. an operator pushed to the workspace but not to the
// forge origin), it creates a refs/vornik/discarded/<ts>-<shortHEAD> ref so
// those commits can be recovered after the reset. Any failure is logged and
// silently skipped — this must never block the task.
func backupLocalOnlyCommits(ctx context.Context, projectDir, branch string, logger zerolog.Logger) {
	countOut, err := gitExec.output(ctx, "-C", projectDir, "rev-list", "--count", "origin/"+branch+"..HEAD")
	if err != nil {
		logger.Debug().Err(err).Str("project_dir", projectDir).Str("branch", branch).
			Msg("pre-work rebase: rev-list ahead-count failed — continuing to reset")
		return
	}
	count := 0
	if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(countOut)), "%d", &count); scanErr != nil || count == 0 {
		return // no local-only commits — nothing to back up
	}
	shortOut, shortErr := gitExec.output(ctx, "-C", projectDir, "rev-parse", "--short", "HEAD")
	if shortErr != nil {
		logger.Debug().Err(shortErr).Str("project_dir", projectDir).
			Msg("pre-work rebase: could not resolve short HEAD for backup ref — continuing anyway")
		return
	}
	backupRef := fmt.Sprintf("refs/vornik/discarded/%d-%s", time.Now().Unix(), strings.TrimSpace(string(shortOut)))
	if _, refErr := gitExec.combined(ctx, "-C", projectDir, "update-ref", backupRef, "HEAD"); refErr != nil {
		logger.Debug().Err(refErr).Str("project_dir", projectDir).Str("branch", branch).
			Msg("pre-work rebase: could not create backup ref for local-only commits — continuing anyway")
		return
	}
	logger.Warn().
		Str("project_dir", projectDir).
		Str("branch", branch).
		Int("count", count).
		Str("backup_ref", backupRef).
		Msg("pre-work rebase: local HEAD has commits not present on origin (operator/local push not yet pushed to forge) — resetting to origin; commits saved to backup ref and are recoverable from it")
}

// vornikInternalPaths are working/bookkeeping artifacts vornik's own pipelines
// (autonomy, dev-pipeline) write into a project workspace — they must NEVER land
// in a customer's change request (incident 2026-06-13: PR #18 carried 192 lines
// of .autonomy/CURRENT_TASK.md + BACKLOG.md noise). For forge tasks we add them
// to the clone's .git/info/exclude so an agent's writes stay untracked and the
// executor's auto-commit/merge never picks them up — defence-in-depth alongside
// the issue-fix workflow prompt telling the agent not to write them.
var vornikInternalPaths = []string{".autonomy/", "CURRENT_TASK.md", "BACKLOG.md", "COVERAGE_REPORT.md"}

// excludeVornikInternalPaths appends vornikInternalPaths to the project clone's
// .git/info/exclude (idempotently). Daemon-side (the executor has rw on the
// clone's .git); covers worktrees too since they share the main repo's exclude.
func excludeVornikInternalPaths(projectDir string, logger zerolog.Logger) {
	if !isGitRepo(projectDir) {
		return
	}
	excludePath := filepath.Join(projectDir, ".git", "info", "exclude")
	existing, _ := os.ReadFile(excludePath)
	body := string(existing)
	var add []string
	for _, p := range vornikInternalPaths {
		if !strings.Contains("\n"+body, "\n"+p) {
			add = append(add, p)
		}
	}
	if len(add) == 0 {
		return
	}
	if len(body) > 0 && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += "# vornik-internal — never publish to a forge:\n" + strings.Join(add, "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(excludePath, []byte(body), 0o644); err != nil {
		logger.Debug().Err(err).Str("project_dir", projectDir).Msg("forge: could not write .git/info/exclude")
		return
	}
	logger.Debug().Str("project_dir", projectDir).Strs("patterns", add).
		Msg("forge: excluded vornik-internal paths from git tracking")
}

// isGitRepo returns true if dir contains a .git directory.
func isGitRepo(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// preReceiveHookScript is the POSIX-sh hook installed at
// <dir>/.git/hooks/pre-receive by ensureReceiveGuards. It reads stdin lines
// of "<old> <new> <ref>" and enforces three rules:
//
//  1. Reject any update to refs/heads/worktree/* (daemon-reserved per-task branches).
//  2. Reject any update to refs/vornik/* (daemon-internal refs, defensive).
//  3. For the repo's default branch (resolved via `git symbolic-ref HEAD`),
//     reject a non-fast-forward update — i.e. when old is non-zero and old
//     is NOT an ancestor of new. Creates (old == all-zeros) and true
//     fast-forwards are allowed.
const preReceiveHookScript = `#!/bin/sh
# vornik push guard — installed by ensureReceiveGuards; do not edit manually.
set -e

default_branch=$(git symbolic-ref HEAD 2>/dev/null | sed 's|refs/heads/||')

while read old new ref; do
    case "$ref" in
        refs/heads/worktree/*)
            echo >&2 "vornik: push rejected — '$ref' is a reserved worktree branch managed by vornik; do not push to it directly."
            exit 1
            ;;
        refs/vornik/*)
            echo >&2 "vornik: push rejected — '$ref' is a reserved daemon-internal ref; direct pushes are not allowed."
            exit 1
            ;;
    esac

    # Non-fast-forward guard for the default branch only.
    if [ -n "$default_branch" ] && [ "$ref" = "refs/heads/$default_branch" ]; then
        # old == all-zeros means branch creation; always allow.
        zero40="0000000000000000000000000000000000000000"
        if [ "$old" != "$zero40" ]; then
            if ! git merge-base --is-ancestor "$old" "$new" 2>/dev/null; then
                echo >&2 "vornik: push rejected — non-fast-forward update to default branch '$default_branch' is not allowed."
                echo >&2 "vornik: force-pushes rewrite shared history. Use a new branch and a merge/PR instead."
                exit 1
            fi
        fi
    fi
done

exit 0
`

// EnsureReceiveGuards idempotently installs server-side push guards on the
// project repo at dir:
//
//  1. Sets git config receive.denyCurrentBranch=updateInstead so a push to
//     the checked-out branch updates the working tree (when clean) instead of
//     erroring.
//  2. Writes an executable pre-receive hook that rejects pushes to
//     refs/heads/worktree/* and refs/vornik/*, and rejects non-fast-forward
//     updates to the default branch.
//
// Both operations are idempotent: setting the config a second time is
// harmless, and rewriting the hook file is safe (same content, same mode).
// Task 2.4's git-over-HTTPS push handler calls this directly (via the
// api.WithGitReceiveGuards seam) immediately before invoking receive-pack to
// re-assert guards on repos that predate this change. Exported so the api
// package can wire it without importing the executor's private surface.
func EnsureReceiveGuards(ctx context.Context, dir string, logger zerolog.Logger) error {
	// 1. Set receive.denyCurrentBranch=updateInstead.
	if out, err := gitExec.combined(ctx, "-C", dir, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		return fmt.Errorf("git config receive.denyCurrentBranch: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// 2. Write the pre-receive hook script as an executable file.
	hooksDir := filepath.Join(dir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create .git/hooks dir: %w", err)
	}
	hookPath := filepath.Join(hooksDir, "pre-receive")
	if err := os.WriteFile(hookPath, []byte(preReceiveHookScript), 0o755); err != nil {
		return fmt.Errorf("write pre-receive hook: %w", err)
	}

	logger.Debug().Str("project_dir", dir).Msg("push guards: receive.denyCurrentBranch=updateInstead + pre-receive hook installed")
	return nil
}

// ensureGitRepo guarantees that dir is a git repository with at least one
// commit, creating dir and initializing the repo if necessary. This is the
// bootstrap half of the deterministic workspace contract: every project
// workspace is a git repo, so worktree isolation + auto-commit on merge
// can always apply, regardless of project/swarm/workflow.
//
// Semantics:
//   - Empty dir argument is a no-op (no ProjectWorkspacePath configured).
//   - If dir doesn't exist, it's created with 0o755.
//   - If dir is already a git repo, this is a no-op.
//   - Otherwise: `git init`, then a single commit with any pre-existing
//     files under a fixed vornik-agent identity. If the directory was
//     empty, a `.vornik/bootstrap.md` marker is created so the commit
//     isn't empty (git init alone leaves no HEAD, which breaks
//     `git worktree add -b branch` downstream).
//
// Errors are returned so the caller can decide whether to fail the task
// or fall back to non-worktree mode — but the normal executor path
// treats failure here as fatal: without a committed seed there's no
// branch point for worktrees.
func ensureGitRepo(ctx context.Context, dir string, logger zerolog.Logger) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ensure project dir: %w", err)
	}
	if isGitRepo(dir) {
		// Guards must be applied on every call (idempotent): older repos
		// predate this change and a hand-tampered repo must be re-guarded.
		return EnsureReceiveGuards(ctx, dir, logger)
	}

	logger.Info().Str("project_dir", dir).Msg("bootstrap: project dir is not a git repo — initializing")

	if out, err := gitExec.combined(ctx, "-C", dir, "init"); err != nil {
		return fmt.Errorf("git init: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// If the directory was empty, seed it with a bootstrap marker so we
	// have something to commit. `git init` alone leaves no HEAD and
	// `git worktree add -b <branch>` refuses to run without one.
	entries, _ := os.ReadDir(dir)
	hasContent := false
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		hasContent = true
		break
	}
	if !hasContent {
		markerDir := filepath.Join(dir, ".vornik")
		if err := os.MkdirAll(markerDir, 0o755); err != nil {
			return fmt.Errorf("create .vornik dir: %w", err)
		}
		marker := filepath.Join(markerDir, "bootstrap.md")
		body := "# vornik-managed project\n\nThis workspace was initialized by vornik as an empty git repository.\nAll task outputs are committed here automatically.\n"
		if err := os.WriteFile(marker, []byte(body), 0o644); err != nil {
			return fmt.Errorf("write bootstrap marker: %w", err)
		}
	}

	if out, err := gitExec.combined(ctx, "-C", dir, "add", "-A"); err != nil {
		return fmt.Errorf("git add -A (bootstrap): %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := gitExec.combined(ctx, "-C", dir,
		"-c", "user.name=vornik-agent",
		"-c", "user.email=agent@vornik.io",
		"commit", "-m", "bootstrap: vornik-managed project"); err != nil {
		return fmt.Errorf("git commit (bootstrap): %w: %s", err, strings.TrimSpace(string(out)))
	}

	logger.Info().Str("project_dir", dir).Msg("bootstrap: git repo initialized with seed commit")
	return EnsureReceiveGuards(ctx, dir, logger)
}

// worktreePath returns the filesystem path for a task's isolated worktree.
func worktreePath(projectDir, taskID string) string {
	return filepath.Join(projectDir, ".worktrees", taskID)
}

// projectRootFromWorktree is the inverse of worktreePath: given a worktree
// directory like "<projectRoot>/.worktrees/<taskID>", it returns
// "<projectRoot>". Returns an empty string when the path does not follow
// that layout — callers use that to skip worktree-specific handling (e.g.
// "ProjectDir is not a worktree, no extra .git bind mount needed").
func projectRootFromWorktree(worktreeDir string) string {
	parent := filepath.Dir(worktreeDir) // .../.worktrees
	if filepath.Base(parent) != ".worktrees" {
		return ""
	}
	return filepath.Dir(parent)
}

// worktreeBranch returns the git branch name used for a task's worktree.
func worktreeBranch(taskID string) string {
	return "worktree/" + taskID
}

// createWorktree creates an isolated git worktree for taskID branched from the
// project's current HEAD. Returns the worktree directory path.
// Caller must call mergeWorktree (success) or removeWorktree (failure/cancel).
func createWorktree(ctx context.Context, projectDir, taskID string, logger zerolog.Logger) (string, error) {
	wtDir := worktreePath(projectDir, taskID)
	branch := worktreeBranch(taskID)

	// Ensure parent .worktrees dir exists (git worktree add creates wtDir itself).
	if err := os.MkdirAll(filepath.Dir(wtDir), 0o755); err != nil {
		return "", fmt.Errorf("worktree parent dir: %w", err)
	}

	out, err := gitExec.combined(ctx, "-C", projectDir, "worktree", "add", wtDir, "-b", branch)
	if err != nil {
		// `git worktree add` routinely leaves partial state on failure:
		// the most common culprit is a post-checkout hook (git-lfs's
		// PATH check, husky, pre-commit) that aborts AFTER files have
		// been written to wtDir and the administrative dir has been
		// created under <projectDir>/.git/worktrees/<taskID>. git
		// reports exit status 2 but doesn't roll the filesystem back.
		//
		// A later `git worktree remove --force` on this state fails
		// validation with "gitdir file does not exist" because the
		// .git file inside wtDir was never written (hook aborted
		// mid-add). We observed this as 10+ orphan .worktrees/task_*
		// dirs piling up on a project with git-lfs enabled but
		// git-lfs not on the daemon's PATH.
		//
		// Best-effort clean every layer so the caller inherits a
		// clean slate (on-disk working tree, admin dir via `git
		// worktree prune`, branch via `branch -D`). All three are
		// intentionally non-fatal — we're already on the error path
		// and any step failing is no worse than the current state.
		_ = os.RemoveAll(wtDir)
		_, _ = gitExec.combined(ctx, "-C", projectDir, "worktree", "prune")
		_, _ = gitExec.combined(ctx, "-C", projectDir, "branch", "-D", "--", branch)
		return "", fmt.Errorf("git worktree add: %w: %s", err, strings.TrimSpace(string(out)))
	}

	logger.Info().
		Str("task_id", taskID).
		Str("worktree_dir", wtDir).
		Str("branch", branch).
		Msg("created git worktree")
	return wtDir, nil
}

// mergeWorktree merges the task's worktree branch into the project branch, then
// removes the worktree. Called on successful task completion. Merge failures are
// logged as warnings — the task already succeeded, so the execution is not failed.
// mergeWorktree merges the task's worktree branch back into projectDir's
// current branch. Returns a non-nil error when the merge fails for any
// reason that leaves the worktree's commits unmerged — callers should
// propagate this as a task failure rather than silently accepting the
// lost work (the failure mode that caused task_20260419194623's
// PROJECT_CONTEXT.md update to vanish: merge failed on missing committer
// identity, the warn log went unseen, and the task was marked COMPLETED
// despite no changes landing on master).
//
// Returns nil when the merge succeeds OR when the branch had no commits
// to merge (the no-op path — nothing was lost because there was nothing
// to merge in the first place).
func mergeWorktree(ctx context.Context, projectDir, worktreeDir, taskID string, logger zerolog.Logger) error {
	branch := worktreeBranch(taskID)

	// Auto-commit any leftover dirty state in the worktree. Some roles
	// (scout, researcher, tester, reviewer, writer, editor) are
	// instructed to file_write output but not to run git commit. Without
	// this step their work sits as an uncommitted modification that git
	// merge silently ignores ("Already up to date") and then
	// removeWorktree discards.
	autoCommitLeftoverChanges(ctx, worktreeDir, taskID, logger)

	// Auto-commit stale TRACKED-only changes in the WORKSPACE ROOT
	// before the dirty-check below. Handles the recovery path from a
	// previous task that failed mid-merge or mid-stage and left the
	// workspace-root index dirty (`M  .autocoder/PROJECT_CONTEXT.md`).
	// Pre-fix that stranded state caused every subsequent worktree
	// merge to refuse with "main workspace has uncommitted changes" —
	// a self-perpetuating trap requiring manual operator recovery
	// (observed vornik-autocoder, 2026-05-06).
	//
	// Tracked-only is critical: untracked workspace-root entries like
	// `.worktrees/` (the internal worktree-host directory) must NOT
	// be committed. Using `git add -u` instead of `-A` captures
	// modifications to already-tracked files while leaving untracked
	// state alone.
	autoCommitTrackedChangesOnly(ctx, projectDir, taskID+"-workspace-prelude", logger)

	// Refuse to merge into a dirty main checkout. Resetting here would
	// make the merge succeed by destroying unrelated operator changes in
	// the project workspace. Preserve both sides instead: the worktree
	// branch remains available for recovery and the caller marks the task
	// failed with an actionable error.
	if dirty, detail := workspaceDirty(ctx, projectDir); dirty {
		logger.Error().
			Str("task_id", taskID).
			Str("project_dir", projectDir).
			Str("dirty_status", detail).
			Msg("main workspace is dirty — refusing worktree merge")
		return fmt.Errorf("main workspace has uncommitted changes; refusing to merge worktree branch %s: %s", branch, detail)
	}

	// --no-ff always creates a merge commit, which requires a committer
	// identity. On hosts where git isn't globally configured (CI images,
	// containerised dev hosts, this deployment) the merge fails with
	// "Committer identity unknown" unless we pass inline identity via
	// -c user.* (same pattern autoCommitLeftoverChanges already uses).
	out, err := gitExec.combined(ctx, "-C", projectDir,
		"-c", "user.name=vornik-agent",
		"-c", "user.email=agent@vornik.io",
		"merge", "--no-ff", branch,
		"-m", "merge: worktree task "+taskID)
	if err != nil {
		// Hardening (2026-06-13, task …582f): the recurring merge-failure class
		// is an add/add conflict on regenerated OUTPUT artifacts
		// (artifacts/out/research.md, summary.txt, …) when concurrent tasks each
		// create the same canonical output path off a base that lacked it —
		// whichever merges first lands it, the next conflicts. add/add means
		// "both branches NEWLY created this path" (no merge-base version), so
		// there is no divergent edit to lose: the completing task's fresh output
		// is authoritative. Auto-resolve by taking the worktree's (theirs)
		// version IFF EVERY conflict is add/add. A genuine modify/modify conflict
		// (an edit to a file present at the merge base — typically code) still
		// fails loudly and preserves the worktree for manual recovery.
		if tryResolveAddAddConflictsTakeTheirs(ctx, projectDir, taskID, logger) {
			logger.Warn().
				Str("task_id", taskID).
				Str("branch", branch).
				Msg("worktree merge: auto-resolved add/add conflict(s) by taking the task's version (regenerated outputs) — merge completed")
			removeWorktree(ctx, projectDir, worktreeDir, taskID, logger)
			return nil
		}
		logger.Error().
			Str("task_id", taskID).
			Str("branch", branch).
			Str("output", strings.TrimSpace(string(out))).
			Msg("git merge failed — changes remain in worktree branch; task will be marked FAILED")
		// Abort any in-progress merge to leave projectDir clean.
		_, _ = gitExec.combined(ctx, "-C", projectDir, "merge", "--abort")
		// Preserve the worktree + branch for manual recovery. Don't call
		// removeWorktree here — that would destroy the commits the
		// operator needs to salvage.
		return fmt.Errorf("git merge of worktree branch %s failed: %s",
			branch, strings.TrimSpace(string(out)))
	}
	if strings.Contains(string(out), "Already up to date") {
		// Merge was a no-op — worktree branch had no commits ahead of the
		// project branch. Historically this was logged as "worktree branch
		// merged" which masked the silent-loss failure mode (see
		// autoCommitLeftoverChanges above). Log it honestly so it's
		// investigable if it recurs despite the auto-commit.
		logger.Info().
			Str("task_id", taskID).
			Str("branch", branch).
			Msg("worktree had no commits to merge — task produced no tracked changes")
	} else {
		logger.Info().Str("task_id", taskID).Str("branch", branch).Msg("worktree branch merged")
	}

	removeWorktree(ctx, projectDir, worktreeDir, taskID, logger)
	return nil
}

// tryResolveAddAddConflictsTakeTheirs completes an in-progress merge that failed
// ONLY on add/add conflicts — paths both branches newly created, with no
// merge-base version — by taking the merged-in branch's ("theirs") version and
// committing the merge. It returns false (leaving the merge in progress for the
// caller to abort) when there are no conflicts, when ANY unmerged path has a
// merge-base version (stage 1 → a genuine modify/modify divergence that must not
// be silently overwritten), or when any git step fails. See mergeWorktree's
// call site for the rationale (regenerated output artifacts racing between
// concurrent tasks — incident 2026-06-13, task …582f).
func tryResolveAddAddConflictsTakeTheirs(ctx context.Context, projectDir, taskID string, logger zerolog.Logger) bool {
	out, err := gitExec.output(ctx, "-C", projectDir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return false
	}
	conflicted := strings.Fields(strings.TrimSpace(string(out)))
	if len(conflicted) == 0 {
		return false // not a content-conflict failure (e.g. dirty tree, identity) — let the caller fail
	}

	// `git ls-files -u` lists every unmerged index entry as
	// "<mode> <sha> <stage>\t<path>". Stage 1 is the merge base; add/add has
	// only stages 2 (ours) + 3 (theirs). A single stage-1 entry anywhere means a
	// base version exists → a real divergent edit → refuse to auto-resolve.
	staged, err := gitExec.output(ctx, "-C", projectDir, "ls-files", "-u")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(staged)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[2] == "1" {
			return false // merge-base version present → not add/add
		}
	}

	// Take the worktree branch's version of every add/add path, stage, and
	// commit the merge under the fixed vornik identity (same as mergeWorktree).
	checkoutArgs := append([]string{"-C", projectDir, "checkout", "--theirs", "--"}, conflicted...)
	if out, err := gitExec.combined(ctx, checkoutArgs...); err != nil {
		logger.Warn().Str("task_id", taskID).Str("output", strings.TrimSpace(string(out))).
			Msg("worktree merge: add/add checkout --theirs failed — falling back to abort")
		return false
	}
	addArgs := append([]string{"-C", projectDir, "add", "--"}, conflicted...)
	if out, err := gitExec.combined(ctx, addArgs...); err != nil {
		logger.Warn().Str("task_id", taskID).Str("output", strings.TrimSpace(string(out))).
			Msg("worktree merge: add/add stage failed — falling back to abort")
		return false
	}
	if out, err := gitExec.combined(ctx, "-C", projectDir,
		"-c", "user.name=vornik-agent", "-c", "user.email=agent@vornik.io",
		"commit", "--no-edit"); err != nil {
		logger.Warn().Str("task_id", taskID).Str("output", strings.TrimSpace(string(out))).
			Msg("worktree merge: add/add resolve-commit failed — falling back to abort")
		return false
	}
	logger.Info().Str("task_id", taskID).Strs("files", conflicted).
		Msg("worktree merge: resolved add/add conflict(s) by taking the task's (theirs) version")
	return true
}

func workspaceDirty(ctx context.Context, dir string) (bool, string) {
	if dir == "" || !isGitRepo(dir) {
		return false, ""
	}
	out, err := gitExec.output(ctx, "-C", dir, "status", "--porcelain")
	if err != nil {
		return true, "git status failed: " + err.Error()
	}
	// Porcelain format is exactly "XY path" (2-char status + one
	// space + path) for additions/modifications, with the X or Y
	// position being a space when only one side has a change
	// (e.g. " D foo" for unstaged-deleted). DO NOT TrimSpace the
	// raw line before extracting the path — the leading space is
	// part of the status code, and stripping it shifts the path
	// extraction by one character. Specifically: " D .worktrees/x"
	// → TrimSpace → "D .worktrees/x" → line[3:] → "worktrees/x"
	// (lost the leading dot), and the .worktrees/ prefix filter
	// misses it. The fix is to slice from index 3 of the RAW
	// line, treating the first 3 chars as the canonical
	// "XY " status prefix.
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		path := ""
		if len(line) > 3 {
			path = line[3:]
		}
		// Renames look like "R old -> new"; the cleanup filter
		// should ignore the .worktrees prefix in either side.
		// Take whichever side appears in the path field; the
		// rename arrow is rare enough that prefix-checking the
		// whole substring is adequate.
		if path == ".worktrees" || strings.HasPrefix(path, ".worktrees/") || strings.Contains(path, " -> .worktrees/") {
			continue
		}
		// .autonomy/ is an internal vornik directory (PROJECT_CONTEXT.md,
		// CURRENT_TASK.md, etc.) that agents write during execution. Like
		// .worktrees/, it must not block the post-task merge.
		if path == ".autonomy" || strings.HasPrefix(path, ".autonomy/") {
			continue
		}
		lines = append(lines, line)
	}
	status := strings.Join(lines, "\n")
	return status != "", status
}

// autoCommitLeftoverChanges commits any uncommitted state in the worktree
// under a fixed vornik identity. Called immediately before mergeWorktree
// attempts the merge so that work produced by roles that don't run git
// commit themselves (scout writing PROJECT_CONTEXT.md, etc.) is not
// silently discarded when the worktree is removed.
//
// Skips silently when the working tree is clean (porcelain empty) or when
// the directory is not a git repo. Logs at warn (not error) on git
// failures: the caller treats this as a best-effort safety net and the
// following merge will still run.
//
// A fixed identity is used because the vornik daemon process runs outside
// any human author context; the task ID in the commit message links the
// auto-commit back to the execution that produced it.
func autoCommitLeftoverChanges(ctx context.Context, worktreeDir, taskID string, logger zerolog.Logger) {
	if worktreeDir == "" || !isGitRepo(worktreeDir) {
		return
	}

	statusOut, err := gitExec.output(ctx, "-C", worktreeDir, "status", "--porcelain")
	if err != nil {
		logger.Warn().
			Str("task_id", taskID).
			Str("worktree_dir", worktreeDir).
			Err(err).
			Msg("auto-commit: git status failed — leftover changes may be lost")
		return
	}
	if len(bytes.TrimSpace(statusOut)) == 0 {
		return
	}

	if out, err := gitExec.combined(ctx, "-C", worktreeDir, "add", "-A"); err != nil {
		logger.Warn().
			Str("task_id", taskID).
			Str("worktree_dir", worktreeDir).
			Str("output", strings.TrimSpace(string(out))).
			Err(err).
			Msg("auto-commit: git add -A failed — leftover changes may be lost")
		return
	}

	msg := "auto-commit: leftover work from " + taskID
	if out, err := gitExec.combined(ctx, "-C", worktreeDir,
		"-c", "user.name=vornik-agent",
		"-c", "user.email=agent@vornik.io",
		"commit", "-m", msg); err != nil {
		logger.Warn().
			Str("task_id", taskID).
			Str("worktree_dir", worktreeDir).
			Str("output", strings.TrimSpace(string(out))).
			Err(err).
			Msg("auto-commit: git commit failed — leftover changes may be lost")
		return
	}

	logger.Info().
		Str("task_id", taskID).
		Str("worktree_dir", worktreeDir).
		Msg("auto-commit: committed leftover worktree changes before merge")
}

// autoCommitTrackedChangesOnly is autoCommitLeftoverChanges's tracked-
// only sibling. Used for the workspace-root cleanup pass where we
// want to rescue stranded modifications to TRACKED files (a previous
// task's stale staged change blocking the dirty check) without
// pulling untracked workspace-root entries — most importantly the
// `.worktrees/` directory, which is the executor's own per-task
// scratch space and would otherwise become a runaway commit.
//
// `git add -u` is the load-bearing difference vs the standard helper:
// it stages modifications and deletions of already-tracked paths
// only. New files (untracked) stay unstaged.
func autoCommitTrackedChangesOnly(ctx context.Context, dir, taskID string, logger zerolog.Logger) {
	if dir == "" || !isGitRepo(dir) {
		return
	}

	// Quick check: if there's no tracked change either staged or in
	// the working tree, return without attempting to commit. Avoids
	// a no-op commit attempt that would surface as a confusing
	// "nothing to commit" warn line.
	if _, headDirty := gitExec.combined(ctx, "-C", dir, "diff", "--quiet", "HEAD"); headDirty == nil {
		if _, cachedDirty := gitExec.combined(ctx, "-C", dir, "diff", "--cached", "--quiet"); cachedDirty == nil {
			return
		}
	}

	if out, err := gitExec.combined(ctx, "-C", dir, "add", "-u"); err != nil {
		logger.Warn().
			Str("task_id", taskID).
			Str("dir", dir).
			Str("output", strings.TrimSpace(string(out))).
			Err(err).
			Msg("workspace-prelude auto-commit: git add -u failed — stranded stage may persist")
		return
	}

	msg := "auto-commit: workspace-root prelude before " + taskID
	out, err := gitExec.combined(ctx, "-C", dir,
		"-c", "user.name=vornik-agent",
		"-c", "user.email=agent@vornik.io",
		"commit", "-m", msg)
	if err != nil {
		// If commit failed because there was nothing to commit
		// (race between the diff check and add — e.g. file was
		// only modified in the working tree but `git add -u`
		// staged nothing because it was already at HEAD), that's
		// a benign no-op. Anything else is logged.
		text := strings.TrimSpace(string(out))
		if !strings.Contains(text, "nothing to commit") {
			logger.Warn().
				Str("task_id", taskID).
				Str("dir", dir).
				Str("output", text).
				Err(err).
				Msg("workspace-prelude auto-commit: git commit failed")
		}
		return
	}
	logger.Info().
		Str("task_id", taskID).
		Str("dir", dir).
		Msg("workspace-prelude auto-commit: rescued stranded tracked changes before merge")
}

// removeWorktree forcibly removes the worktree directory and deletes the branch.
// Called on task failure or cancellation (no merge).
//
// Idempotent and tolerant of partial prior cleanup: safe to call when the
// worktree dir is already gone or the branch is already deleted. This is
// the key property that prevents orphan worktree/task_* branches from
// accumulating across daemon restarts or interrupted executions.
//
// **Detached cleanup context.** The caller's `ctx` is typically the
// execution context, which gets CANCELLED on task cancellation — and
// task cancellation is the most common reason removeWorktree is
// called. With the caller's ctx, every `git` subprocess gets killed
// immediately with empty stdout+stderr, the branch never gets deleted,
// and an orphan accumulates with no diagnostic. Derive a detached
// 30-second context internally so cleanup actually runs.
//
// 30s is generous: a clean `git worktree remove` is < 1s, the
// fallback path adds maybe another second. The big margin tolerates
// a slow disk or a worktree containing many large files.
func removeWorktree(ctx context.Context, projectDir, worktreeDir, taskID string, logger zerolog.Logger) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	branch := worktreeBranch(taskID)

	if out, err := gitExec.combined(cleanupCtx, "-C", projectDir, "worktree", "remove", "--force", worktreeDir); err != nil {
		logger.Warn().
			Str("task_id", taskID).
			Str("output", strings.TrimSpace(string(out))).
			Err(err).
			Msg("git worktree remove failed — falling back to os.RemoveAll")
		_ = os.RemoveAll(worktreeDir)
		// Sync git's own worktree registry with the on-disk state so the
		// subsequent branch delete doesn't fail with "worktree still in use".
		_, _ = gitExec.combined(cleanupCtx, "-C", projectDir, "worktree", "prune")
	}

	// Post-condition check: verify the directory is actually gone.
	// `git worktree remove --force` can return exit 0 while leaving
	// the directory behind (observed when a previous step's podman
	// container had a bind mount on the path; the kernel's mount
	// namespace held the inodes open and git's removal was a no-op
	// despite the success exit code). Worse: if a later container
	// step fails to start because the directory was already cleaned,
	// podman re-creates an empty .worktrees/<id>/ directory just to
	// satisfy its --volume bind, which then leaks indefinitely.
	// T-4ae6 (2026-05-10) hit exactly this — the directory remained
	// on disk with podman-recreated content until manual cleanup.
	// Force-removing here closes both holes: the success-but-dir-
	// remains case AND the podman-resurrection case both end with a
	// truly absent directory.
	if _, err := os.Stat(worktreeDir); err == nil {
		if rmErr := os.RemoveAll(worktreeDir); rmErr != nil {
			logger.Error().
				Str("task_id", taskID).
				Str("worktree_dir", worktreeDir).
				Err(rmErr).
				Msg("worktree directory still present after git worktree remove; os.RemoveAll fallback also failed — manual cleanup required")
		} else {
			logger.Warn().
				Str("task_id", taskID).
				Str("worktree_dir", worktreeDir).
				Msg("worktree directory still present after git worktree remove (likely podman bind-mount or kernel namespace held it); os.RemoveAll fallback succeeded")
		}
	}

	// `--` separates options from the branch name so git can never
	// reinterpret the branch arg as a flag. The argument is always
	// `worktree/<taskID>` today (taskID is server-generated and the
	// `worktree/` prefix already protects it), but the separator
	// removes the dependence on that prefix never being dropped in a
	// future refactor.
	if out, err := gitExec.combined(cleanupCtx, "-C", projectDir, "branch", "-D", "--", branch); err != nil {
		// One retry after prune covers the "branch still associated with a
		// pruned worktree" race where the first branch -D fails but works
		// once the worktree registry is clean. If it still fails after
		// that, escalate to an ERROR log so the orphan is visible rather
		// than silently surviving as WARN noise.
		firstErr := err
		_, _ = gitExec.combined(cleanupCtx, "-C", projectDir, "worktree", "prune")
		if out2, err2 := gitExec.combined(cleanupCtx, "-C", projectDir, "branch", "-D", "--", branch); err2 != nil {
			// Log the underlying err values too — git emits its real
			// reason on stderr (captured in `out`), but if subprocess
			// invocation itself failed (binary missing, ctx hit
			// timeout), `out` is empty and only Err carries the
			// diagnostic. Logging both prevents silent loss seen
			// during the cancelled-task incident.
			logger.Error().
				Str("task_id", taskID).
				Str("branch", branch).
				Str("first_attempt", strings.TrimSpace(string(out))).
				Str("retry_attempt", strings.TrimSpace(string(out2))).
				AnErr("first_err", firstErr).
				AnErr("retry_err", err2).
				Msg("git branch delete failed twice — orphan branch may remain until startup prune")
		}
	}

	logger.Info().Str("task_id", taskID).Msg("git worktree removed")
}

// pruneWorktrees runs `git worktree prune` to clean up stale worktree metadata
// and then removes all orphaned `worktree/*` branches left from a previous
// daemon run (e.g. crash, restart, or un-merged task). Branches whose task
// ID is in `preserve` are retained — those represent in-flight executions
// the recovery loop is about to adopt; pruning their worktree under the
// recovered goroutine causes podman to fail the next step with `statfs: no
// such file or directory` (T-4ae6, 2026-05-10).
// Call once at executor startup before processing any tasks.
func pruneWorktrees(ctx context.Context, projectDir string, logger zerolog.Logger, preserve map[string]struct{}) {
	if !isGitRepo(projectDir) {
		return
	}
	out, err := gitExec.combined(ctx, "-C", projectDir, "worktree", "prune")
	if err != nil {
		logger.Warn().
			Str("project_dir", projectDir).
			Str("output", strings.TrimSpace(string(out))).
			Msg("git worktree prune failed")
	} else {
		logger.Debug().Str("project_dir", projectDir).Msg("git worktree prune complete")
	}

	// List all worktree/* branches and remove their worktrees + branches.
	// "At startup no tasks are running" was the design assumption; in
	// practice an orphaned agent container from a prior daemon process
	// can still hold a worktree dir as a bind mount when the new daemon
	// boots — wiping it under the live container produces cascading
	// "No such file or directory" errors and a dead task. The
	// worktreeInUseByContainer check below is the safety net.
	listOut, err := gitExec.output(ctx, "-C", projectDir, "branch", "--list", "worktree/*")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(listOut), "\n") {
		// `git branch --list` prefixes output: "* " for the current branch,
		// "+ " for branches checked out in another worktree, "  " otherwise.
		// Trim whichever is present before matching on the name.
		branch := strings.TrimSpace(line)
		branch = strings.TrimPrefix(branch, "* ")
		branch = strings.TrimPrefix(branch, "+ ")
		branch = strings.TrimSpace(branch)
		if branch == "" || !strings.HasPrefix(branch, "worktree/") {
			continue
		}
		taskID := strings.TrimPrefix(branch, "worktree/")
		// preserve-set guard: tasks with an in-flight execution row
		// in the DB are about to be adopted by recoverExecution.
		// Their worktree must survive the prune.
		if _, keep := preserve[taskID]; keep {
			logger.Info().
				Str("project_dir", projectDir).
				Str("branch", branch).
				Str("task_id", taskID).
				Msg("skipping worktree prune: in-flight execution will adopt this worktree")
			continue
		}
		wtDir := worktreePath(projectDir, taskID)
		if worktreeInUseByContainer(ctx, wtDir) {
			logger.Warn().
				Str("project_dir", projectDir).
				Str("branch", branch).
				Str("worktree_dir", wtDir).
				Msg("skipping worktree prune: a running container has it bind-mounted (orphan from prior daemon process); recoverExecution will adopt or replace it")
			continue
		}
		logger.Info().
			Str("project_dir", projectDir).
			Str("branch", branch).
			Msg("removing orphaned worktree branch at startup")
		removeWorktree(ctx, projectDir, wtDir, taskID, logger)
	}

	// Second pass: orphan .worktrees/<id>/ directories with no matching
	// branch. Branches get deleted first; a pre-existing directory without
	// a branch is stale and would otherwise linger forever.
	pruneOrphanWorktreeDirs(ctx, projectDir, logger)
}

// worktreeInUseByContainer reports whether any running podman container
// has worktreeDir bind-mounted. Used as a guard before removing a
// worktree at startup: an orphaned agent container from a prior daemon
// process can still hold the directory, and yanking the mount source
// out from under it produces cascading "No such file or directory"
// errors and a dead task with a missing result.json (regression
// observed 2026-05-07 after a daemon rebuild).
//
// Fail-open semantics: if podman isn't available or the inspection
// fails, returns false so cleanup proceeds. The original "always
// remove" behavior is preserved on hosts without podman, and on hosts
// with podman the worst case is the same — the user already asked
// for the worktree to be pruned.
//
// Substring match against the human-readable Mounts column is good
// enough here: worktreeDir is a project-internal absolute path
// (`<workspaces>/<project>/.worktrees/<taskID>`) and the taskID
// component is unique enough to never collide with another mount
// in practice.
func worktreeInUseByContainer(ctx context.Context, worktreeDir string) bool {
	if worktreeDir == "" {
		return false
	}
	out, err := exec.CommandContext(ctx, "podman", "ps",
		"--no-trunc",
		"--filter", "status=running",
		"--format", "{{.Mounts}}",
	).CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), worktreeDir)
}

// pruneOrphanWorktreeDirs walks <projectDir>/.worktrees/ and removes any
// subdirectory that has no matching `worktree/<name>` branch in the repo.
// The branch-list pass above is the primary cleanup; this handles the edge
// case where a crash between branch delete and directory unlink left the
// directory behind.
func pruneOrphanWorktreeDirs(ctx context.Context, projectDir string, logger zerolog.Logger) {
	worktreesDir := filepath.Join(projectDir, ".worktrees")
	entries, err := os.ReadDir(worktreesDir)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn().Err(err).Str("dir", worktreesDir).Msg("worktree orphan scan failed")
		}
		return
	}

	// Re-read the current branch list (now post-primary-cleanup) so we
	// only flag directories whose branches are really gone.
	listOut, err := gitExec.output(ctx, "-C", projectDir, "branch", "--list", "worktree/*")
	if err != nil {
		return
	}
	liveBranches := make(map[string]struct{})
	for _, line := range strings.Split(string(listOut), "\n") {
		b := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(line), "* "), "+ "))
		if strings.HasPrefix(b, "worktree/") {
			liveBranches[strings.TrimPrefix(b, "worktree/")] = struct{}{}
		}
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		taskID := entry.Name()
		if _, live := liveBranches[taskID]; live {
			continue
		}
		full := filepath.Join(worktreesDir, taskID)
		if err := os.RemoveAll(full); err != nil {
			logger.Warn().Err(err).Str("dir", full).Msg("failed to remove orphan worktree directory")
		} else {
			logger.Info().Str("dir", full).Msg("removed orphan worktree directory at startup")
		}
	}
}
