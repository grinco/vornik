package executor

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// output_contract.go — the require_output_glob step guard (incident
// task_20260712143854_429a3500d692d23c, 2026-07-12: 7 of 8 deep-research
// subtasks reported COMPLETED without writing their promised findings
// file, and the parent chain "succeeded" all the way into a publisher
// holding no deliverable). A step that declares an output-file contract
// must produce a matching file, freshly written, or the step fails.

// outputContractMtimeSlack mirrors verifyClaimedFiles' 1s allowance for
// sub-second mtime resolution on some filesystems.
const outputContractMtimeSlack = time.Second

// outputGlobSatisfied reports whether at least one file matching glob
// was written at-or-after stepStart. Globs prefixed "project/" resolve
// against effectiveProjectDir (the task's worktree, or the persistent
// project root in non-worktree mode) — the container's
// /app/workspace/project/ view; any other glob resolves against the
// step's ephemeral staging workspaceDir.
//
// A malformed glob (absolute, or containing "..") never matches — the
// contract is config-authored, and failing closed surfaces the config
// mistake as a loud step failure instead of silently skipping the
// guard.
func outputGlobSatisfied(workspaceDir, effectiveProjectDir, glob string, stepStart time.Time) bool {
	glob = strings.TrimSpace(glob)
	if glob == "" {
		return true
	}
	if filepath.IsAbs(glob) || strings.Contains(glob, "..") {
		return false
	}
	root := workspaceDir
	rel := glob
	if strings.HasPrefix(glob, "project/") {
		root = effectiveProjectDir
		rel = strings.TrimPrefix(glob, "project/")
	}
	if root == "" {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(root, rel))
	if err != nil {
		return false
	}
	floor := stepStart.Add(-outputContractMtimeSlack)
	for _, m := range matches {
		info, statErr := os.Stat(m)
		if statErr != nil || info.IsDir() {
			continue
		}
		// Freshness gate: the project tree persists across tasks, so
		// a file inherited from an earlier task must not satisfy THIS
		// step's contract.
		if stepStart.IsZero() || !info.ModTime().Before(floor) {
			return true
		}
	}
	return false
}
