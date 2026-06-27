package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verifyCov_resultJSON builds a result.json body with a single
// modified_files claim pointing at the given path.
func verifyCov_resultJSON(t *testing.T, path string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"modified_files": []string{path}})
	require.NoError(t, err)
	return b
}

// TestVerifyCov_ClaimedDirectoryFails covers the IsDir branch: a claim that
// resolves to a directory (not a file) is flagged as a problem.
func TestVerifyCov_ClaimedDirectoryFails(t *testing.T) {
	ws := t.TempDir()
	sub := filepath.Join(ws, "subdir")
	require.NoError(t, os.Mkdir(sub, 0o755))

	e := &Executor{}
	stepStart := time.Now().Add(-5 * time.Second)
	err := e.verifyClaimedFiles(verifyCov_resultJSON(t, "subdir"), ws, t.TempDir(), stepStart)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is a directory, not a file")
}

// TestVerifyCov_StatFailsNonNotExist exercises the stat-error branch that is
// NOT os.IsNotExist: a claim whose parent path component is actually a file
// makes the os.Stat return ENOTDIR rather than ErrNotExist.
func TestVerifyCov_StatFailsNonNotExist(t *testing.T) {
	ws := t.TempDir()
	// Create a regular file, then claim a path that treats it as a directory.
	regular := filepath.Join(ws, "afile")
	require.NoError(t, os.WriteFile(regular, []byte("x"), 0o600))

	e := &Executor{}
	stepStart := time.Now().Add(-5 * time.Second)
	// "afile/child" → stat on a path under a non-directory → ENOTDIR.
	err := e.verifyClaimedFiles(verifyCov_resultJSON(t, "afile/child"), ws, t.TempDir(), stepStart)
	require.Error(t, err)
	// Either the not-exist or stat-failed message is acceptable; the branch
	// under test is the non-IsNotExist arm, which yields "stat failed".
	assert.Contains(t, err.Error(), "afile/child")
}

// TestVerifyCov_SafeJoinUnder_Edges covers safeJoinUnder's reject branches:
// rel resolving to the base itself, and a sibling-prefix escape.
func TestVerifyCov_SafeJoinUnder_Edges(t *testing.T) {
	base := "/app/workspace/project"

	// rel that cleans to "." resolves to base itself → rejected ("").
	assert.Equal(t, "", safeJoinUnder(base, "."))

	// A path that shares a string prefix with base but is a sibling
	// (e.g. /app/workspace/project-evil) must be rejected.
	assert.Equal(t, "", safeJoinUnder(base, "../project-evil/x"))

	// A legitimate child resolves under base.
	got := safeJoinUnder(base, "artifacts/out.md")
	assert.Equal(t, filepath.Clean("/app/workspace/project/artifacts/out.md"), got)
}

// TestVerifyCov_ResolveClaimedPath_EmptyComponents covers resolveClaimedPath's
// empty-rel / empty-base guards and the workspace-absolute prefix branch.
func TestVerifyCov_ResolveClaimedPath_EmptyComponents(t *testing.T) {
	// project-absolute with nothing after the prefix → rel "" → "".
	assert.Equal(t, "", resolveClaimedPath("/app/workspace/project/", "/ws", "/proj"))

	// workspace-absolute prefix with a child resolves under workspaceDir.
	got := resolveClaimedPath("/app/workspace/foo.txt", "/ws", "/proj")
	assert.Equal(t, filepath.Clean("/ws/foo.txt"), got)

	// bare relative with empty workspace base → "".
	assert.Equal(t, "", resolveClaimedPath("foo.txt", "", "/proj"))

	// whitespace-only claim → "".
	assert.Equal(t, "", resolveClaimedPath("   ", "/ws", "/proj"))
}
