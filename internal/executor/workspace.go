package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"
)

// snapshotWorkspaceRef returns the current git HEAD SHA of the project directory,
// or an empty string if the directory is not a git repository or git is unavailable.
func snapshotWorkspaceRef(dir string) string {
	if dir == "" {
		return ""
	}
	// Fast check: skip git entirely if .git doesn't exist.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return ""
	}
	out, err := gitExec.output(context.Background(), "-C", dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// resetWorkspace runs `git reset --hard <ref> && git clean -fdx` in dir.
// It is a no-op when dir or ref is empty.
// Untracked files (including gitignored ones) are removed so the workspace
// exactly matches the snapshot state; this prevents contamination of retry
// attempts. .worktrees/ is excluded to avoid disturbing sibling worktrees.
func resetWorkspace(ctx context.Context, dir, ref string, logger zerolog.Logger) error {
	if dir == "" || ref == "" {
		return nil
	}

	logger.Info().
		Str("project_dir", dir).
		Str("ref", ref).
		Msg("resetting workspace to snapshot ref")

	if out, err := gitExec.combined(ctx, "-C", dir, "reset", "--hard", ref); err != nil {
		return fmt.Errorf("git reset --hard %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
	}
	if out, err := gitExec.combined(ctx, "-C", dir, "clean", "-fdx", "--exclude=.worktrees"); err != nil {
		return fmt.Errorf("git clean -fdx: %w: %s", err, strings.TrimSpace(string(out)))
	}

	logger.Info().
		Str("project_dir", dir).
		Str("ref", ref).
		Msg("workspace reset complete")
	return nil
}

// cleanProjectDir removes untracked files from dir (including gitignored
// ones) without touching tracked files. Safe to call after worktree
// removal.
//
// Default excludes (always applied):
//   - .worktrees/   — sibling task worktrees must survive a cleanup
//     triggered by a different task's failure path.
//   - .autonomy/    — the daemon's per-project bookkeeping namespace
//     and the default home for operator-authored docs
//     (PROJECT_CONTEXT.md, USER_GUIDANCE.md, anti-bot
//     configs, portal source lists, etc.). Some are
//     committed; some are not. A bare-untracked
//     operator doc in this namespace MUST survive a
//     cleanup — wiping it silently breaks the
//     autonomy/USER-task split that depends on those
//     files being present. Live evidence: janka's
//     USER_GUIDANCE.md was wiped by this code path on
//     2026-05-10 before this exclusion landed.
//
// `extraExcludes` lets the caller add per-project exclusions for
// operator-configured paths outside the .autonomy/ default — e.g.
// when ProjectAutonomy.ContextFilePath or .UserContextFilePath
// points at a custom directory. Each entry is treated as a `git
// clean --exclude=<value>` argument; callers should pass directory
// names relative to the project root.
//
// Used when worktrees are enabled but a container may have written
// to the main project directory directly (e.g. worktree re-creation
// failed before a retry).
func cleanProjectDir(ctx context.Context, dir string, logger zerolog.Logger, extraExcludes ...string) {
	if dir == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return
	}
	args := []string{"-C", dir, "clean", "-fdx",
		"--exclude=.worktrees",
		"--exclude=.autonomy",
	}
	for _, ex := range extraExcludes {
		ex = strings.TrimSpace(ex)
		if ex == "" {
			continue
		}
		args = append(args, "--exclude="+ex)
	}
	out, err := gitExec.combined(ctx, args...)
	if err != nil {
		logger.Warn().Str("project_dir", dir).Str("output", strings.TrimSpace(string(out))).Msg("project dir cleanup failed")
	}
}

// projectCleanExcludeDir resolves a single context-file-path config
// string into the directory cleanProjectDir should preserve, or "" if
// the default excludes already cover it (or the input is unsafe).
//
// Used by callers to compute extraExcludes for cleanProjectDir when
// an operator points ProjectAutonomy.ContextFilePath or
// .UserContextFilePath outside the default .autonomy/ namespace —
// without this their untracked operator doc gets wiped on the next
// cleanup pass. For paths inside .autonomy/ the helper returns ""
// because the default exclude already covers them. For root-level
// paths ("X.md") it also returns "" — operators stashing docs at the
// workspace root should commit them; excluding "." would no-op the
// whole cleanup.
//
// Safety: rejects absolute paths and `..` traversal.
func projectCleanExcludeDir(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return ""
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return ""
	}
	dir := filepath.Dir(clean)
	if dir == "." || dir == "" {
		return ""
	}
	// .autonomy is already excluded by default; don't re-emit.
	if dir == ".autonomy" || strings.HasPrefix(dir, ".autonomy/") {
		return ""
	}
	return dir
}
