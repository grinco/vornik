package service

// Slice 4 wiring — service-layer adapters for the memetic
// applier. Filesystem writer (two-tree discipline), git committer,
// and config-reload trigger. Kept here so internal/memetic stays
// free of filesystem / exec / git dependencies.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memetic"
	"vornik.io/vornik/internal/persistence"
)

// workflowApplierAdapter bridges *memetic.Applier (returns the
// typed persistence.WorkflowProposal) to the api package's
// WorkflowApplier interface (returns any).
type workflowApplierAdapter struct {
	a *memetic.Applier
}

func (w *workflowApplierAdapter) Apply(ctx context.Context, proposalID, appliedBy string) (any, error) {
	if w == nil || w.a == nil {
		return nil, fmt.Errorf("workflow applier not wired")
	}
	return w.a.Apply(ctx, proposalID, appliedBy)
}

// fsWorkflowWriter implements memetic.WorkflowWriter against the
// two-tree config discipline: writes to both source (operator's
// vornik checkout) and deployed (daemon's read-target) trees. The
// source path is returned for git staging.
//
// Source-tree empty / not present is non-fatal: deployed-only
// deployments (production daemons without an operator checkout)
// get the file written to the deployed tree and no git commit.
type fsWorkflowWriter struct {
	sourceConfigDir   string // <root>/configs, contains workflows/
	deployedConfigDir string // ~/.config/vornik/configs, contains workflows/
}

func (w *fsWorkflowWriter) Write(_ context.Context, workflowID string, body []byte) (string, error) {
	if workflowID == "" {
		return "", fmt.Errorf("fsWorkflowWriter: empty workflowID")
	}
	if w.deployedConfigDir == "" {
		return "", fmt.Errorf("fsWorkflowWriter: deployedConfigDir not set")
	}
	// Defend against operator-supplied IDs containing path
	// separators. Same hardening as fsWorkflowSource.
	if strings.ContainsAny(workflowID, "/\\") || strings.Contains(workflowID, "..") {
		return "", fmt.Errorf("fsWorkflowWriter: workflowID contains path separators")
	}

	deployedPath, err := w.writeToTree(w.deployedConfigDir, workflowID, body)
	if err != nil {
		return "", fmt.Errorf("write deployed tree: %w", err)
	}

	// Source tree write is optional. When the source tree exists
	// AND has a workflows/ directory, we mirror the write so the
	// operator's git repo reflects the change. Otherwise we skip
	// silently — the applier handles the "no git commit" case.
	if w.sourceConfigDir == "" {
		_ = deployedPath
		return "", nil
	}
	sourceWorkflowsDir := filepath.Join(w.sourceConfigDir, "workflows")
	if info, err := os.Stat(sourceWorkflowsDir); err != nil || !info.IsDir() {
		return "", nil
	}
	sourcePath, err := w.writeToTree(w.sourceConfigDir, workflowID, body)
	if err != nil {
		return "", fmt.Errorf("write source tree: %w", err)
	}
	return sourcePath, nil
}

func (w *fsWorkflowWriter) writeToTree(configDir, workflowID string, body []byte) (string, error) {
	workflowsDir := filepath.Join(configDir, "workflows")
	candidate := filepath.Clean(filepath.Join(workflowsDir, workflowID+".md"))
	if !strings.HasPrefix(candidate, workflowsDir+string(filepath.Separator)) {
		return "", fmt.Errorf("workflowID escapes workflows directory")
	}
	// Best-effort atomic: write to <name>.md.tmp then rename.
	// os.Rename on the same filesystem is atomic on POSIX.
	tmp := candidate + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, candidate); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return candidate, nil
}

// gitCommitter implements memetic.GitCommitter via `git` binary
// calls. Stages one path (so the commit doesn't accidentally
// include unrelated working-tree changes) and commits with the
// operator-supplied message + identity.
type gitCommitter struct {
	repoDir string // the git repo root (or any subdirectory inside it)
}

func (g *gitCommitter) Commit(ctx context.Context, path, message, authorName, authorEmail string) (string, error) {
	if g.repoDir == "" {
		return "", fmt.Errorf("gitCommitter: repoDir not set")
	}
	// Stage just this one path so unrelated working-tree changes
	// don't ride along.
	add := exec.CommandContext(ctx, "git", "-C", g.repoDir, "add", "--", path)
	if out, err := add.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Commit with author env vars overriding any local git
	// config so the architect identity is recorded consistently
	// even on shared boxes. --only restricts the commit to the
	// staged path even if the working tree has other changes.
	env := append([]string{}, os.Environ()...)
	if authorName != "" {
		env = append(env,
			"GIT_AUTHOR_NAME="+authorName,
			"GIT_COMMITTER_NAME="+authorName,
		)
	}
	if authorEmail != "" {
		env = append(env,
			"GIT_AUTHOR_EMAIL="+authorEmail,
			"GIT_COMMITTER_EMAIL="+authorEmail,
		)
	}
	commit := exec.CommandContext(ctx, "git", "-C", g.repoDir,
		"commit", "-m", message, "--only", "--", path)
	commit.Env = env
	if out, err := commit.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(string(out)))
	}

	sha := exec.CommandContext(ctx, "git", "-C", g.repoDir, "rev-parse", "HEAD")
	out, err := sha.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// configReloadAdapter bridges *config.ConfigReloader to
// memetic.ConfigReloadTrigger. The reloader's post-reload hook
// (installed by installConfigReloadBroadcast) handles the cross-
// instance NOTIFY automatically — we just call Reload() and the
// machinery downstream fires the broadcast.
type configReloadAdapter struct {
	reloader *config.ConfigReloader
}

func (a *configReloadAdapter) Reload() error {
	if a == nil || a.reloader == nil {
		return nil
	}
	return a.reloader.Reload()
}

// newWorkflowApplier wires the memetic.Applier out of the
// container's primitives. Returns nil if prerequisites are
// missing; the admin endpoint nil-checks and surfaces 503.
//
// The source-tree path is resolved from VORNIK_CONFIGS_SOURCE_DIR
// env (operator's vornik checkout root) — falls back to the
// deployed tree if unset. When source == deployed, the applier
// effectively writes once and (if the deployed tree is a git
// repo) commits there. Most dev deployments will land in this
// shape; production has a deployed-only tree and skips git.
func newWorkflowApplier(
	proposals persistence.WorkflowProposalRepository,
	reloader *config.ConfigReloader,
	deployedConfigDir string,
) *memetic.Applier {
	if proposals == nil || deployedConfigDir == "" {
		return nil
	}
	sourceDir := os.Getenv("VORNIK_CONFIGS_SOURCE_DIR")
	writer := &fsWorkflowWriter{
		sourceConfigDir:   sourceDir,
		deployedConfigDir: deployedConfigDir,
	}

	// Pick the git repo root: prefer the source tree (where the
	// operator's checkout lives). If unset, fall back to the
	// deployed tree iff it's a git repo. Otherwise leave git nil
	// — the applier records "no-git" as applied_commit.
	gitDir := sourceDir
	if gitDir == "" {
		gitDir = deployedConfigDir
	}
	var git memetic.GitCommitter
	if isGitRepo(gitDir) {
		git = &gitCommitter{repoDir: gitDir}
	}

	return memetic.NewApplier(
		proposals, writer, git,
		&configReloadAdapter{reloader: reloader},
		memetic.ApplierConfig{
			AuthorName:  envOr("VORNIK_GIT_AUTHOR_NAME", "vornik-architect"),
			AuthorEmail: envOr("VORNIK_GIT_AUTHOR_EMAIL", "architect@vornik.local"),
		},
	)
}

func isGitRepo(dir string) bool {
	if dir == "" {
		return false
	}
	// Walk up looking for a .git directory. `git rev-parse
	// --is-inside-work-tree` would be more correct but adds an
	// exec call to startup; the .git check is good enough for
	// the "should we wire a committer" decision.
	cur := dir
	for {
		info, err := os.Stat(filepath.Join(cur, ".git"))
		if err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}
