package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// newTestRepo builds a tiny git repo in a temp dir with enough commits to
// exercise generatePlanChanges. Returns (repoDir, firstSHA, thirdSHA).
// Set user.name/email scoped to the repo so the test passes in CI without
// depending on global git config.
func newTestRepo(t *testing.T) (dir, firstSHA, thirdSHA string) {
	t.Helper()
	dir = t.TempDir()

	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v failed: %s", args, out)
		return out
	}
	// commit writes content to notes.md and creates a commit. Pass a
	// single subject for subject-only commits, or subject + body lines
	// to exercise multi-line commit messages (body lines become -m args
	// so git concatenates them into the commit body).
	commit := func(content string, messageLines ...string) string {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.md"), []byte(content), 0o644))
		run("add", "notes.md")
		args := []string{"commit"}
		for _, line := range messageLines {
			args = append(args, "-m", line)
		}
		run(args...)
		sha := strings.TrimSpace(string(run("rev-parse", "HEAD")))
		return sha
	}

	run("init", "-q", "-b", "main")
	run("config", "user.name", "test")
	run("config", "user.email", "test@example.com")

	firstSHA = commit("v1\n", "initial commit")
	_ = commit("v2\n", "add second note")
	thirdSHA = commit("v3\n",
		"third commit with body",
		"more detail here\nacross multiple lines")

	return dir, firstSHA, thirdSHA
}

// TestGeneratePlanChangesProducesPatchesAndSummary exercises the full
// flow: git format-patch file count, CHANGES.md contents, commit-subject
// lines. Also confirms the OutputDir is caller-owned (exists on return,
// doesn't leak state on subsequent calls).
func TestGeneratePlanChangesProducesPatchesAndSummary(t *testing.T) {
	repo, first, third := newTestRepo(t)

	changes, err := generatePlanChanges(context.Background(), repo, first, third)
	require.NoError(t, err)
	require.NotNil(t, changes)
	t.Cleanup(func() { _ = os.RemoveAll(changes.OutputDir) })

	require.Equal(t, first, changes.FromSHA)
	require.Equal(t, third, changes.ToSHA)

	// Two commits between first and third (third is inclusive, first is not).
	require.Len(t, changes.Patches, 2,
		"expected one .patch per commit in range; got: %v", changes.Patches)
	require.Len(t, changes.Commits, 2)

	// Each patch file must exist on disk with mbox headers so git am
	// could replay it. Smoke-test: the first few bytes must start with
	// the standard format-patch prefix.
	for _, p := range changes.Patches {
		body, readErr := os.ReadFile(p)
		require.NoError(t, readErr, "patch %s unreadable", p)
		require.True(t, strings.HasPrefix(string(body), "From "),
			"patch %s does not look like mbox format; first 64 bytes: %q",
			p, string(body[:min(64, len(body))]))
	}

	// The summary must mention both commit subjects and include the
	// extended body text for the third commit.
	require.Contains(t, changes.Summary, "add second note")
	require.Contains(t, changes.Summary, "third commit with body")
	require.Contains(t, changes.Summary, "more detail here",
		"summary should include commit bodies, not just subjects")

	// CHANGES.md must be written inside OutputDir so the caller can
	// persist it through the artifact store.
	summaryPath := filepath.Join(changes.OutputDir, "CHANGES.md")
	stored, readErr := os.ReadFile(summaryPath)
	require.NoError(t, readErr)
	require.Equal(t, changes.Summary, string(stored))
}

// TestGeneratePlanChangesNoopsForEmptyRange ensures the helper returns
// nil (not an error) when from==to. This is the common case for plans
// that never committed — the caller should leave the last role's
// result in place rather than overwriting it with a "0 commits"
// envelope.
func TestGeneratePlanChangesNoopsForEmptyRange(t *testing.T) {
	repo, _, third := newTestRepo(t)

	changes, err := generatePlanChanges(context.Background(), repo, third, third)
	require.NoError(t, err)
	require.Nil(t, changes)

	// Also skips when any input is empty.
	changes, err = generatePlanChanges(context.Background(), "", "abc", "def")
	require.NoError(t, err)
	require.Nil(t, changes)
}

// TestGeneratePlanChangesErrorsOnBadRepo ensures we surface the git error
// instead of silently pretending things are fine when the worktree isn't a
// valid repository.
func TestGeneratePlanChangesErrorsOnBadRepo(t *testing.T) {
	nonRepo := t.TempDir()

	changes, err := generatePlanChanges(context.Background(), nonRepo, "abcd", "efgh")
	require.Error(t, err)
	require.Nil(t, changes)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
