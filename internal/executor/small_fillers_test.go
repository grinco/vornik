package executor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOneLine_NoNewlines — pass-through (length under cap).
func TestOneLine_NoNewlines(t *testing.T) {
	assert.Equal(t, "hello world", oneLine("hello world"))
	assert.Equal(t, "", oneLine(""))
}

// TestOneLine_CollapsesNewlines — newlines get replaced with the
// visible ⏎ sentinel so the prompt builder gets a one-line
// rendering that the model can still understand as "this was
// originally multi-line".
func TestOneLine_CollapsesNewlines(t *testing.T) {
	got := oneLine("a\nb\nc")
	assert.Equal(t, "a ⏎ b ⏎ c", got)
}

// TestOneLine_TruncatesAt500 — anything over 500 chars (after
// newline replacement) is clipped to 497 + "...". Length must
// always be ≤ 500 in the truncated branch.
func TestOneLine_Truncates(t *testing.T) {
	long := strings.Repeat("x", 600)
	got := oneLine(long)
	assert.True(t, len(got) == 500, "truncated form must be exactly 500 chars (497 body + 3 dots)")
	assert.True(t, strings.HasSuffix(got, "..."), "must end with ellipsis sentinel")
}

// TestPreviewJSON_ShortRaw — under 300 chars passes through.
func TestPreviewJSON_Short(t *testing.T) {
	got := previewJSON(json.RawMessage(`{"x":1}`))
	assert.Equal(t, `{"x":1}`, got)

	// nil/empty passes through as empty string
	assert.Equal(t, "", previewJSON(nil))
}

// TestPreviewJSON_TruncatesOver300 — clips at 300 + "...".
func TestPreviewJSON_LongTruncated(t *testing.T) {
	raw := json.RawMessage(`{"x":"` + strings.Repeat("a", 400) + `"}`)
	got := previewJSON(raw)
	assert.Equal(t, 303, len(got), "truncated form: 300 chars + '...'")
	assert.True(t, strings.HasSuffix(got, "..."))
}

// TestGitObjectExists_EmptySHA — guard short-circuits before
// invoking git. Used by verifyRoleClaims when the agent didn't
// claim a checked_commit.
func TestGitObjectExists_EmptySHA(t *testing.T) {
	assert.False(t, gitObjectExists(context.Background(), "/tmp", ""))
}

// TestGitObjectExists_NotARepo — pointing at a non-repo dir
// returns false (git cat-file exits non-zero). Doesn't panic.
func TestGitObjectExists_NotARepo(t *testing.T) {
	assert.False(t, gitObjectExists(context.Background(), t.TempDir(), "abc123"))
}

// TestGitObjectExists_RealRepo — initialise a tiny repo,
// commit, then verify that the HEAD sha is recognised and a
// bogus sha is not. Hits both true and false branches.
func TestGitObjectExists_RealRepo(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, string(out))
	}
	runGit("init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644))
	runGit("add", ".")
	runGit("commit", "-q", "-m", "init")

	headBytes, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	head := strings.TrimSpace(string(headBytes))
	assert.True(t, gitObjectExists(context.Background(), dir, head),
		"real HEAD sha must be recognised")
	assert.False(t, gitObjectExists(context.Background(), dir, "ffffffffffffffffffffffffffffffffffffffff"),
		"bogus sha must NOT be recognised")
}
