package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCanonicalContextCov_NonRegularFileSkipped covers tryReadCanonical's
// `!info.Mode().IsRegular()` branch: a *directory* named PROJECT_CONTEXT.md is
// neither a symlink nor a regular file, so it must be treated as absent rather
// than read.
func TestCanonicalContextCov_NonRegularFileSkipped(t *testing.T) {
	ws := t.TempDir()
	dotDir := filepath.Join(ws, ".autonomy")
	require.NoError(t, os.MkdirAll(dotDir, 0o755))
	// Create a *directory* where a file is expected.
	require.NoError(t, os.MkdirAll(filepath.Join(dotDir, "PROJECT_CONTEXT.md"), 0o755))

	ctx := resolveCanonicalContext(ws)
	assert.Empty(t, ctx.ProjectContext, "a directory must not be read as the context file")
	assert.True(t, ctx.Empty())
}

// TestCanonicalContextCov_TruncateWithMarker_NoTruncationPath covers the
// truncateWithMarker early-return (data already within budget returns the
// verbatim string).
func TestCanonicalContextCov_TruncateWithMarker_NoTruncationPath(t *testing.T) {
	small := []byte("short body")
	out := truncateWithMarker(small, 1024)
	assert.Equal(t, "short body", out)
	assert.NotContains(t, out, "truncated")
}

// TestCanonicalContextCov_UserGuidanceMixedSources covers the user-field
// contribution to the "mixed" source determination: project context comes
// from .autonomy/ while user guidance comes from autonomy/, so both usedDot
// and usedPlain are set via different fields.
func TestCanonicalContextCov_UserGuidanceMixedSources(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".autonomy"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "autonomy"), 0o755))

	// project from the dot dir, user from the plain dir.
	require.NoError(t, os.WriteFile(
		filepath.Join(ws, ".autonomy", "PROJECT_CONTEXT.md"), []byte("proj"), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(ws, "autonomy", "USER_GUIDANCE.md"), []byte("user"), 0o644))

	ctx := resolveCanonicalContext(ws)
	assert.Equal(t, "proj", ctx.ProjectContext)
	assert.Equal(t, "user", ctx.UserGuidance)
	assert.Equal(t, "mixed", ctx.Source)
}
