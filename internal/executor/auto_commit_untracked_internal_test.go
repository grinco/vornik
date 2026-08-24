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

// A vornik-written workspace-root file on its FIRST write is untracked, and
// `git add -u` cannot stage a path git has never seen. Before 2026-08-20 that
// left it uncommitted, `workspaceDirty` (which filters only .worktrees/ and
// .autonomy/) saw it, and mergeWorktree refused — for that task and every task
// after it, until a human intervened.
//
// That is a STANDING block, not the race the backlog originally described: the
// per-project workspace lock has serialised dispatch and cleanup since d2491e9a
// (2026-06-21). Nine merge failures in one evening is what this looks like when
// nine tasks run.
//
// Evidence: agentbench 2026-08-14, where an agent diagnosed it unaided and
// committed a .gitignore whose message records the failure — and the gold
// builder recorded the environmental failure as agent incapability.
// See https://docs.vornik.io §4.4a.

func gitStatusPorcelain(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput()
	require.NoError(t, err, "git status: %s", out)
	return string(out)
}

// The confirmed blocker: an untracked backlog file.
func TestAutoCommitTrackedChangesOnly_StagesUntrackedBacklogFile(t *testing.T) {
	dir := initGitRepoForTest(t)
	backlog := filepath.Join(dir, "BACKLOG.md")
	require.NoError(t, os.WriteFile(backlog, []byte("- [ ] item\n"), 0o644))

	autoCommitTrackedChangesOnly(context.Background(), dir, "task-1", backlog, zerolog.Nop())

	assert.Empty(t, strings.TrimSpace(gitStatusPorcelain(t, dir)),
		"an untracked backlog file must be checkpointed, not left to block every later merge")
	dirty, detail := workspaceDirty(context.Background(), dir)
	assert.False(t, dirty, "workspace must be mergeable afterwards; still dirty: %s", detail)
}

// The same mechanism for the other vornik-internal workspace-root paths. Only
// BACKLOG.md was observed failing; these differ solely in not having been seen
// in a failure report yet, so they are pinned explicitly rather than implied.
func TestAutoCommitTrackedChangesOnly_StagesOtherUntrackedInternalPaths(t *testing.T) {
	for _, name := range []string{"CURRENT_TASK.md", "COVERAGE_REPORT.md"} {
		t.Run(name, func(t *testing.T) {
			dir := initGitRepoForTest(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644))

			autoCommitTrackedChangesOnly(context.Background(), dir, "task-1", "", zerolog.Nop())

			dirty, detail := workspaceDirty(context.Background(), dir)
			assert.False(t, dirty, "%s must not block the merge; dirty: %s", name, detail)
		})
	}
}

// The bound that makes this safe: an operator's own untracked file still blocks.
// §4.2's rule is that a merge must never destroy or absorb unrelated operator
// work, so only vornik's OWN named paths are staged — never `git add -A`.
func TestAutoCommitTrackedChangesOnly_LeavesOperatorUntrackedFiles(t *testing.T) {
	dir := initGitRepoForTest(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "BACKLOG.md"), []byte("- [ ] item\n"), 0o644))

	autoCommitTrackedChangesOnly(context.Background(), dir, "task-1",
		filepath.Join(dir, "BACKLOG.md"), zerolog.Nop())

	status := gitStatusPorcelain(t, dir)
	assert.Contains(t, status, "notes.txt",
		"an operator's untracked file must be left alone — absorbing it would be exactly the "+
			"destruction the dirty-check exists to prevent")
	assert.NotContains(t, status, "BACKLOG.md", "vornik's own file should have been checkpointed")
}

// .worktrees/ is the internal worktree host directory. It must never be
// committed — that is why the helper stages tracked-only in the first place.
func TestAutoCommitTrackedChangesOnly_NeverStagesWorktreesDir(t *testing.T) {
	dir := initGitRepoForTest(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".worktrees", "task-x"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".worktrees", "task-x", "f.txt"), []byte("x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "BACKLOG.md"), []byte("- [ ] item\n"), 0o644))

	autoCommitTrackedChangesOnly(context.Background(), dir, "task-1",
		filepath.Join(dir, "BACKLOG.md"), zerolog.Nop())

	out, err := exec.Command("git", "-C", dir, "ls-files").CombinedOutput()
	require.NoError(t, err)
	assert.NotContains(t, string(out), ".worktrees",
		"the worktree host directory must never be committed")
}

// A path excluded via .git/info/exclude (what excludeVornikInternalPaths does on
// forge clones) never appears in `git status`, so intersecting with status is
// what keeps this fix inert there — nothing vornik-internal is committed into a
// clone destined for a customer change request.
func TestAutoCommitTrackedChangesOnly_ExcludedPathStaysUncommitted(t *testing.T) {
	dir := initGitRepoForTest(t)
	excludePath := filepath.Join(dir, ".git", "info", "exclude")
	require.NoError(t, os.MkdirAll(filepath.Dir(excludePath), 0o755))
	require.NoError(t, os.WriteFile(excludePath, []byte("BACKLOG.md\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "BACKLOG.md"), []byte("- [ ] item\n"), 0o644))

	autoCommitTrackedChangesOnly(context.Background(), dir, "task-1",
		filepath.Join(dir, "BACKLOG.md"), zerolog.Nop())

	out, err := exec.Command("git", "-C", dir, "ls-files").CombinedOutput()
	require.NoError(t, err)
	assert.NotContains(t, string(out), "BACKLOG.md",
		"an excluded path must never be force-staged — forge clones depend on it")
}

// A backlogFilePath resolving outside the repo must stage nothing.
func TestAutoCommitTrackedChangesOnly_BacklogOutsideRepoStagesNothing(t *testing.T) {
	dir := initGitRepoForTest(t)
	outside := filepath.Join(t.TempDir(), "BACKLOG.md")
	require.NoError(t, os.WriteFile(outside, []byte("- [ ] item\n"), 0o644))

	assert.NotPanics(t, func() {
		autoCommitTrackedChangesOnly(context.Background(), dir, "task-1", outside, zerolog.Nop())
	})
	out, err := exec.Command("git", "-C", dir, "ls-files").CombinedOutput()
	require.NoError(t, err)
	assert.NotContains(t, string(out), "BACKLOG.md")
}

// The two-commit split (§4.3) must survive: the backlog marker is attributed to
// the checkpoint commit, other rescued residue to the rescue commit.
func TestAutoCommitTrackedChangesOnly_UntrackedBacklogLandsInCheckpointCommit(t *testing.T) {
	dir := initGitRepoForTest(t)
	backlog := filepath.Join(dir, "BACKLOG.md")
	require.NoError(t, os.WriteFile(backlog, []byte("- [ ] item\n"), 0o644))

	autoCommitTrackedChangesOnly(context.Background(), dir, "task-1", backlog, zerolog.Nop())

	out, err := exec.Command("git", "-C", dir, "log", "--format=%s", "-1").CombinedOutput()
	require.NoError(t, err, "git log: %s", out)
	assert.Contains(t, string(out), "backlog: marker checkpoint",
		"the backlog marker must be attributed to the checkpoint commit, not the rescue commit")
}

// A configured backlogFilePath is operator-supplied, so it can carry a space or
// a non-ASCII byte. Plain `git status --porcelain` QUOTES those paths
// (`?? "my file.md"`), which would silently fail the name match and leave the
// file blocking every merge with nothing in the log to explain it. The helper
// uses -z for exactly this reason.
func TestAutoCommitTrackedChangesOnly_StagesAwkwardBacklogNames(t *testing.T) {
	for _, name := range []string{"my backlog.md", "café-backlog.md"} {
		t.Run(name, func(t *testing.T) {
			dir := initGitRepoForTest(t)
			backlog := filepath.Join(dir, name)
			require.NoError(t, os.WriteFile(backlog, []byte("- [ ] item\n"), 0o644))

			autoCommitTrackedChangesOnly(context.Background(), dir, "task-1", backlog, zerolog.Nop())

			dirty, detail := workspaceDirty(context.Background(), dir)
			assert.False(t, dirty, "a quoted-path backlog file must still be checkpointed; dirty: %s", detail)
		})
	}
}

// A backlog file configured into a subdirectory must still be recognised.
func TestAutoCommitTrackedChangesOnly_StagesBacklogInSubdirectory(t *testing.T) {
	dir := initGitRepoForTest(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs"), 0o755))
	backlog := filepath.Join(dir, "docs", "BACKLOG.md")
	require.NoError(t, os.WriteFile(backlog, []byte("- [ ] item\n"), 0o644))

	autoCommitTrackedChangesOnly(context.Background(), dir, "task-1", backlog, zerolog.Nop())

	dirty, detail := workspaceDirty(context.Background(), dir)
	assert.False(t, dirty, "a backlog file in a subdirectory must be checkpointed; dirty: %s", detail)
}

// The two-commit split under the condition that actually exercises it: an
// untracked backlog marker AND a tracked modification present together. The
// marker must land in the checkpoint commit and the residue in the rescue
// commit — attribution is the point of the split (§4.3), and a single commit
// carrying both would claim the rescue reconciled the marker.
func TestAutoCommitTrackedChangesOnly_SplitsCheckpointAndRescueCommits(t *testing.T) {
	dir := initGitRepoForTest(t)
	tracked := filepath.Join(dir, "src.txt")
	require.NoError(t, os.WriteFile(tracked, []byte("v1\n"), 0o644))
	out, err := exec.Command("git", "-C", dir, "add", "src.txt").CombinedOutput()
	require.NoError(t, err, "%s", out)
	out, err = exec.Command("git", "-C", dir, "-c", "user.email=t@e", "-c", "user.name=t",
		"commit", "-qm", "add src").CombinedOutput()
	require.NoError(t, err, "%s", out)

	require.NoError(t, os.WriteFile(tracked, []byte("v2\n"), 0o644))
	backlog := filepath.Join(dir, "BACKLOG.md")
	require.NoError(t, os.WriteFile(backlog, []byte("- [ ] item\n"), 0o644))

	autoCommitTrackedChangesOnly(context.Background(), dir, "task-1", backlog, zerolog.Nop())

	subjects, err := exec.Command("git", "-C", dir, "log", "--format=%s", "-2").CombinedOutput()
	require.NoError(t, err, "%s", subjects)
	assert.Contains(t, string(subjects), "backlog: marker checkpoint")
	assert.Contains(t, string(subjects), "rescue: stranded tracked changes")

	filesFor := func(rev string) string {
		o, e := exec.Command("git", "-C", dir, "show", "--name-only", "--format=%s", rev).CombinedOutput()
		require.NoError(t, e, "%s", o)
		return string(o)
	}
	// The checkpoint commit is the older of the two; the rescue commit is HEAD.
	assert.Contains(t, filesFor("HEAD"), "src.txt", "the rescue commit carries the tracked residue")
	assert.NotContains(t, filesFor("HEAD"), "BACKLOG.md", "the rescue commit must not carry the marker")
	assert.Contains(t, filesFor("HEAD~1"), "BACKLOG.md", "the checkpoint commit carries the marker")
}

// backlogFilePath is operator-configurable, so the default name is not the only
// one that must be recognised.
func TestAutoCommitTrackedChangesOnly_StagesNonDefaultBacklogName(t *testing.T) {
	dir := initGitRepoForTest(t)
	backlog := filepath.Join(dir, "NOTES.md")
	require.NoError(t, os.WriteFile(backlog, []byte("- [ ] item\n"), 0o644))

	autoCommitTrackedChangesOnly(context.Background(), dir, "task-1", backlog, zerolog.Nop())

	dirty, detail := workspaceDirty(context.Background(), dir)
	assert.False(t, dirty, "a non-default backlog name must be checkpointed; dirty: %s", detail)
}

// Once the backlog file is tracked, `git add -u` already covers it. The by-name
// staging must not change that path's behaviour — it is additive for the
// untracked case only.
func TestAutoCommitTrackedChangesOnly_TrackedBacklogStillCheckpointed(t *testing.T) {
	dir := initGitRepoForTest(t)
	backlog := filepath.Join(dir, "BACKLOG.md")
	require.NoError(t, os.WriteFile(backlog, []byte("- [ ] one\n"), 0o644))
	out, err := exec.Command("git", "-C", dir, "add", "BACKLOG.md").CombinedOutput()
	require.NoError(t, err, "%s", out)
	out, err = exec.Command("git", "-C", dir, "-c", "user.email=t@e", "-c", "user.name=t",
		"commit", "-qm", "track backlog").CombinedOutput()
	require.NoError(t, err, "%s", out)

	require.NoError(t, os.WriteFile(backlog, []byte("- [x] one\n"), 0o644))
	autoCommitTrackedChangesOnly(context.Background(), dir, "task-1", backlog, zerolog.Nop())

	dirty, detail := workspaceDirty(context.Background(), dir)
	assert.False(t, dirty, "a tracked backlog modification must still be checkpointed; dirty: %s", detail)
	subj, err := exec.Command("git", "-C", dir, "log", "--format=%s", "-1").CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(subj), "backlog: marker checkpoint",
		"a tracked marker must keep its checkpoint attribution")
}
