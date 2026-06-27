package executor

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestWorkflowArtifactCleanupCov_EmptyEntryAfterTrimSkipped covers the
// blank-entry skip branch: a whitespace-only CleanupArtifacts element is
// recorded as Skipped rather than acted upon.
func TestWorkflowArtifactCleanupCov_EmptyEntryAfterTrimSkipped(t *testing.T) {
	ws := t.TempDir()
	wf := &registry.Workflow{ID: "wf", CleanupArtifacts: []string{"   "}}
	res := applyWorkflowArtifactCleanup(ws, wf, zerolog.Nop())
	assert.Equal(t, []string{""}, res.Skipped)
	assert.Empty(t, res.Deleted)
}

// TestWorkflowArtifactCleanupCov_RemoveErrorRecorded covers the Errored
// branch: a file whose parent directory is read-only makes os.Remove fail
// with EACCES (the file still stat's fine, so it passes the IsDir/missing
// guards and reaches the Remove call). Skipped when run as root, where
// permission bits don't block removal.
func TestWorkflowArtifactCleanupCov_RemoveErrorRecorded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory-permission removal semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permission; cannot force EACCES")
	}

	ws := t.TempDir()
	lockedDir := filepath.Join(ws, "locked")
	require.NoError(t, os.MkdirAll(lockedDir, 0o755))
	target := filepath.Join(lockedDir, "victim.md")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))

	// Make the parent dir read+execute but not writable → os.Remove fails.
	require.NoError(t, os.Chmod(lockedDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(lockedDir, 0o755) })

	wf := &registry.Workflow{ID: "wf", CleanupArtifacts: []string{"locked/victim.md"}}
	res := applyWorkflowArtifactCleanup(ws, wf, zerolog.Nop())
	assert.Equal(t, []string{"locked/victim.md"}, res.Errored)
	assert.Empty(t, res.Deleted)
}

// TestWorkflowArtifactCleanupCov_EffectiveWorkspaceDir covers the
// project-workspace fallback branches of workflowEffectiveWorkspaceDir.
func TestWorkflowArtifactCleanupCov_EffectiveWorkspaceDir(t *testing.T) {
	// nil plan / nil task → "".
	assert.Equal(t, "", workflowEffectiveWorkspaceDir(&Executor{}, nil, &persistence.Task{}))
	assert.Equal(t, "", workflowEffectiveWorkspaceDir(&Executor{}, &executionPlan{}, nil))

	// worktreeDir set → returned verbatim.
	plan := &executionPlan{worktreeDir: "/wt/task-1"}
	assert.Equal(t, "/wt/task-1",
		workflowEffectiveWorkspaceDir(&Executor{}, plan, &persistence.Task{ProjectID: "p"}))

	// nil executor with no worktree → "".
	assert.Equal(t, "",
		workflowEffectiveWorkspaceDir(nil, &executionPlan{}, &persistence.Task{ProjectID: "p"}))

	// Project-workspace fallback when worktree empty and config provides a path.
	e := &Executor{config: &Config{ProjectWorkspacePath: "/srv/ws"}}
	got := workflowEffectiveWorkspaceDir(e, &executionPlan{}, &persistence.Task{ProjectID: "proj-7"})
	assert.Equal(t, filepath.Join("/srv/ws", "proj-7"), got)

	// Missing config path → "".
	eNoPath := &Executor{config: &Config{}}
	assert.Equal(t, "",
		workflowEffectiveWorkspaceDir(eNoPath, &executionPlan{}, &persistence.Task{ProjectID: "p"}))
}
