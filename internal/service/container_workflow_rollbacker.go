package service

// Slice 5 wiring — service-layer adapters for the memetic
// rollbacker. Git revert against the source tree; same
// nil-safety conventions as Slice 4's applier.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memetic"
	"vornik.io/vornik/internal/persistence"
)

type workflowRollbackerAdapter struct {
	r *memetic.Rollbacker
}

func (w *workflowRollbackerAdapter) Rollback(ctx context.Context, proposalID, revertedBy string) (any, error) {
	if w == nil || w.r == nil {
		return nil, fmt.Errorf("workflow rollbacker not wired")
	}
	return w.r.Rollback(ctx, proposalID, revertedBy)
}

// gitReverter implements memetic.GitReverter via `git revert`.
// Uses --no-edit so the operator's revert lands without an
// interactive editor, and -m 1 to handle merge-commit reverts
// (no-op on regular commits).
type gitReverter struct {
	repoDir string
}

func (g *gitReverter) Revert(ctx context.Context, sha, message, authorName, authorEmail string) (string, error) {
	if g.repoDir == "" {
		return "", fmt.Errorf("gitReverter: repoDir not set")
	}
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
	// --no-edit: skip the editor.
	// -m 1: pick the first parent if the SHA is a merge commit;
	// no-op on regular commits.
	cmd := exec.CommandContext(ctx, "git", "-C", g.repoDir,
		"revert", "--no-edit", "-m", "1", sha)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git revert: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// If the operator-supplied message is non-default, amend
	// the commit with it.
	if message != "" {
		amend := exec.CommandContext(ctx, "git", "-C", g.repoDir,
			"commit", "--amend", "-m", message)
		amend.Env = env
		if out, err := amend.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git commit --amend: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	sha2 := exec.CommandContext(ctx, "git", "-C", g.repoDir, "rev-parse", "HEAD")
	out, err := sha2.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// newWorkflowRollbacker mirrors newWorkflowApplier — picks the
// source tree's git repo, falls back to deployed tree if it's
// a git repo. Returns nil when no git is available; the admin
// endpoint surfaces 503 in that case.
func newWorkflowRollbacker(
	proposals persistence.WorkflowProposalRepository,
	reloader *config.ConfigReloader,
	deployedConfigDir string,
) *memetic.Rollbacker {
	if proposals == nil {
		return nil
	}
	sourceDir := os.Getenv("VORNIK_CONFIGS_SOURCE_DIR")
	gitDir := sourceDir
	if gitDir == "" {
		gitDir = deployedConfigDir
	}
	if !isGitRepo(gitDir) {
		return nil
	}
	return memetic.NewRollbacker(
		proposals,
		&gitReverter{repoDir: gitDir},
		&configReloadAdapter{reloader: reloader},
		memetic.RollbackerConfig{
			AuthorName:  envOr("VORNIK_GIT_AUTHOR_NAME", "vornik-architect"),
			AuthorEmail: envOr("VORNIK_GIT_AUTHOR_EMAIL", "architect@vornik.local"),
		},
	)
}
