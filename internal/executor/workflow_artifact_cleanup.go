package executor

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// workflowEffectiveWorkspaceDir resolves the absolute workspace dir
// the executor's containers are mounting. When worktrees are
// enabled, plan.worktreeDir is the answer; otherwise it falls back
// to the project's shared workspace (config.ProjectWorkspacePath +
// projectID). Empty string when neither resolves — the cleanup
// helper degrades to a no-op in that case.
func workflowEffectiveWorkspaceDir(e *Executor, plan *executionPlan, task *persistence.Task) string {
	if plan == nil || task == nil {
		return ""
	}
	if plan.worktreeDir != "" {
		return plan.worktreeDir
	}
	if e == nil {
		return ""
	}
	if e.config.ProjectWorkspacePath == "" || task.ProjectID == "" {
		return ""
	}
	return filepath.Join(e.config.ProjectWorkspacePath, task.ProjectID)
}

// workflowArtifactCleanupResult is the per-call summary, exposed so
// tests can verify the cleanup ran without scraping logs.
type workflowArtifactCleanupResult struct {
	// Deleted lists workspace-relative paths whose os.Remove
	// succeeded. Order matches the workflow's CleanupArtifacts list.
	Deleted []string
	// Missing lists workspace-relative paths that did not exist at
	// cleanup time. NOT an error — the cleanup is opportunistic.
	Missing []string
	// Skipped lists workspace-relative paths the helper refused
	// (absolute paths, parent-traversal, empty after Clean).
	Skipped []string
	// Errored lists paths whose deletion failed for some reason
	// OTHER than "not found" — e.g. EACCES. Errored entries are
	// logged but never fail the workflow.
	Errored []string
}

// applyWorkflowArtifactCleanup deletes each path listed in the
// workflow's CleanupArtifacts field from the project workspace,
// BEFORE the workflow's entrypoint step runs. The cleanup is
// defense-in-depth: prompts already instruct producer agents to
// overwrite the canonical artifacts (artifacts/out/research.md,
// etc.) — this guarantees the file is gone if the producer crashes
// without writing.
//
// workspaceDir is the absolute path to the effective workspace root
// (worktreeDir when worktrees are enabled, otherwise the project
// directory). Empty workspaceDir or nil workflow returns a zero-
// value result so the executor can call this unconditionally.
//
// Safety: paths must stay inside workspaceDir. Absolute paths and
// `..` traversal are skipped (and logged). The helper deletes only
// regular files — directories are skipped to avoid a typo nuking a
// whole subtree.
//
// Error policy: per-file errors NEVER fail the workflow. The
// pre-clean is best-effort; a worst-case unwritable file still lets
// the workflow run and produce whatever the agent writes on top.
func applyWorkflowArtifactCleanup(workspaceDir string, wf *registry.Workflow, logger zerolog.Logger) workflowArtifactCleanupResult {
	res := workflowArtifactCleanupResult{}
	if workspaceDir == "" || wf == nil || len(wf.CleanupArtifacts) == 0 {
		return res
	}

	for _, rel := range wf.CleanupArtifacts {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			res.Skipped = append(res.Skipped, rel)
			continue
		}
		if !isSafeWorkspacePath(rel) {
			logger.Warn().
				Str("workflow_id", wf.ID).
				Str("path", rel).
				Msg("workflow cleanup: rejected unsafe path")
			res.Skipped = append(res.Skipped, rel)
			continue
		}
		abs := filepath.Join(workspaceDir, rel)
		info, err := os.Lstat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				res.Missing = append(res.Missing, rel)
				continue
			}
			logger.Warn().
				Err(err).
				Str("workflow_id", wf.ID).
				Str("path", abs).
				Msg("workflow cleanup: stat failed")
			res.Errored = append(res.Errored, rel)
			continue
		}
		// Skip directories — `cleanup_artifacts` is intended for
		// canonical file outputs, not bulk-tree removal.
		if info.IsDir() {
			logger.Warn().
				Str("workflow_id", wf.ID).
				Str("path", rel).
				Msg("workflow cleanup: refusing to delete a directory")
			res.Skipped = append(res.Skipped, rel)
			continue
		}
		if err := os.Remove(abs); err != nil {
			logger.Warn().
				Err(err).
				Str("workflow_id", wf.ID).
				Str("path", abs).
				Msg("workflow cleanup: delete failed")
			res.Errored = append(res.Errored, rel)
			continue
		}
		logger.Debug().
			Str("workflow_id", wf.ID).
			Str("path", rel).
			Msg("workflow cleanup: deleted prior-task artifact")
		res.Deleted = append(res.Deleted, rel)
	}
	return res
}

// isSafeWorkspacePath rejects absolute paths and `..` traversal so
// the cleanup helper can never reach outside the workspace dir.
// Empty / `.` paths are also rejected — both would resolve to the
// workspace root which the helper refuses to delete.
func isSafeWorkspacePath(rel string) bool {
	if rel == "" {
		return false
	}
	if filepath.IsAbs(rel) {
		return false
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == "" {
		return false
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	// Defensive: reject embedded `..` segments even after Clean
	// removes them in the common cases.
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		if seg == ".." {
			return false
		}
	}
	return true
}
