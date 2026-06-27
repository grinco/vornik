//go:build integration
// +build integration

package integration_test

// End-to-end test for the memetic rollback path (Slice 5) against
// a real postgres + sandbox git repo. Pins:
//   - apply then rollback transitions the row applied →
//     rolled_back and stamps the revert commit SHA.
//   - the working tree's WORKFLOW.md is restored to the pre-apply
//     version (verifying the git revert actually changed the file).
//   - rollback on a not-applied row errors with ErrProposalNotApplied.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/memetic"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
)

// itGitReverter mirrors the service-package gitReverter inline.
type itGitReverter struct {
	repoDir string
}

func (g *itGitReverter) Revert(ctx context.Context, sha, msg, _, _ string) (string, error) {
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=vornik-it",
		"GIT_AUTHOR_EMAIL=it@vornik.test",
		"GIT_COMMITTER_NAME=vornik-it",
		"GIT_COMMITTER_EMAIL=it@vornik.test",
	)
	cmd := exec.CommandContext(ctx, "git", "-C", g.repoDir,
		"revert", "--no-edit", "-m", "1", sha)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git revert: %w: %s", err, out)
	}
	if msg != "" {
		amend := exec.CommandContext(ctx, "git", "-C", g.repoDir,
			"commit", "--amend", "-m", msg)
		amend.Env = env
		if out, err := amend.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git amend: %w: %s", err, out)
		}
	}
	sha2 := exec.CommandContext(ctx, "git", "-C", g.repoDir, "rev-parse", "HEAD")
	out, err := sha2.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func TestRollbacker_E2E_AppliesThenReverts(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowID := "research-rb-" + suffix
	proposalID := "wpr-rb-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE workflow_id = $1`, workflowID)
	})

	sourceDir, deployedDir := setupSourceRepoForWorkflow(t, workflowID)
	writer := &itWorkflowWriter{sourceDir: sourceDir, deployedDir: deployedDir}
	git := &itGitCommitter{repoDir: sourceDir}
	reloader := &stubReloader{}

	ctx := context.Background()
	require.NoError(t, repo.Insert(ctx, &persistence.WorkflowProposal{
		ID:             proposalID,
		WorkflowID:     workflowID,
		Status:         persistence.WorkflowProposalStatusPending,
		ProposalYAML:   "---\nworkflowId: research\nversion: 2.0.0\n---\nnew body\n",
		Motivation:     "tighten step3",
		EvidenceRunIDs: []string{"r-1", "r-2", "r-3"},
		Confidence:     0.8,
		ArchitectModel: "test",
		CreatedAt:      time.Now().UTC(),
	}))
	require.NoError(t, repo.Decide(ctx, proposalID,
		persistence.WorkflowProposalStatusApproved, "operator-x", "ok"))

	// Apply first so we have a real commit to revert.
	applier := memetic.NewApplier(repo, writer, git, reloader,
		memetic.ApplierConfig{AuthorName: "vornik-architect", AuthorEmail: "architect@vornik.test"})
	applied, err := applier.Apply(ctx, proposalID, "operator-x")
	require.NoError(t, err)
	require.NotEmpty(t, applied.AppliedCommit)

	// File now contains the new YAML. The proposal's YAML overrides
	// workflowId to "research" via the YAML body — fine; the file
	// path on disk uses our test-unique workflowID.
	sourcePath := filepath.Join(sourceDir, "workflows", workflowID+".md")
	body, _ := os.ReadFile(sourcePath)
	require.Contains(t, string(body), "version: 2.0.0")

	// Roll back. Use a real git reverter against the same source
	// tree.
	rollbacker := memetic.NewRollbacker(repo, &itGitReverter{repoDir: sourceDir}, reloader,
		memetic.RollbackerConfig{AuthorName: "vornik-it", AuthorEmail: "it@vornik.test"})
	got, err := rollbacker.Rollback(ctx, proposalID, "operator-y")
	require.NoError(t, err)
	require.Equal(t, persistence.WorkflowProposalStatusRolledBack, got.Status)
	require.NotEmpty(t, got.RollbackCommit)
	require.NotEqual(t, applied.AppliedCommit, got.RollbackCommit)

	// File should be back to the baseline (pre-apply) content.
	body2, _ := os.ReadFile(sourcePath)
	require.Equal(t, "baseline", strings.TrimSpace(string(body2)),
		"git revert should restore the pre-apply file content")

	// Reloader fired twice — once on apply, once on rollback.
	require.Equal(t, 2, reloader.called)
}

func TestRollbacker_E2E_NotApplied(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowID := "research-rb-na-" + suffix
	proposalID := "wpr-rb-na-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE workflow_id = $1`, workflowID)
	})

	ctx := context.Background()
	require.NoError(t, repo.Insert(ctx, &persistence.WorkflowProposal{
		ID: proposalID, WorkflowID: workflowID,
		Status:       persistence.WorkflowProposalStatusPending,
		ProposalYAML: "y", Motivation: "m",
		EvidenceRunIDs: []string{"r-1"}, Confidence: 0.7,
		ArchitectModel: "m", CreatedAt: time.Now().UTC(),
	}))

	sourceDir, _ := setupSourceRepoForWorkflow(t, workflowID)
	rollbacker := memetic.NewRollbacker(repo, &itGitReverter{repoDir: sourceDir}, &stubReloader{},
		memetic.RollbackerConfig{})
	_, err := rollbacker.Rollback(ctx, proposalID, "operator-x")
	require.Error(t, err)
	require.ErrorIs(t, err, memetic.ErrProposalNotApplied)
}
