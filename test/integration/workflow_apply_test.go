//go:build integration
// +build integration

package integration_test

// End-to-end test for the memetic apply path (Slice 4) against a
// real postgres + a sandbox git repo + a temp config tree. Pins
// the contract that:
//   - Approve → Apply transitions the row to status=applied
//     and records the actual git commit SHA.
//   - The new WORKFLOW.md lands on disk in the deployed tree.
//   - The git repo in the source tree shows a new commit
//     mentioning the proposal_id.
//   - Apply on a pending (not-yet-approved) row errors with the
//     "must be approved" sentinel.

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

// itWorkflowWriter is a minimal copy of the service-package
// fsWorkflowWriter, kept inline so the integration test doesn't
// pull in the service package's heavy dependency graph.
type itWorkflowWriter struct {
	sourceDir   string
	deployedDir string
}

func (w *itWorkflowWriter) Write(_ context.Context, workflowID string, body []byte) (string, error) {
	if strings.ContainsAny(workflowID, "/\\") || strings.Contains(workflowID, "..") {
		return "", fmt.Errorf("workflowID escapes")
	}
	for _, dir := range []string{w.deployedDir, w.sourceDir} {
		if dir == "" {
			continue
		}
		wfDir := filepath.Join(dir, "workflows")
		if err := os.MkdirAll(wfDir, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(wfDir, workflowID+".md"), body, 0o644); err != nil {
			return "", err
		}
	}
	if w.sourceDir == "" {
		return "", nil
	}
	return filepath.Join(w.sourceDir, "workflows", workflowID+".md"), nil
}

// itGitCommitter mirrors the service-package gitCommitter inline.
type itGitCommitter struct {
	repoDir string
}

func (g *itGitCommitter) Commit(ctx context.Context, path, message, _, _ string) (string, error) {
	add := exec.CommandContext(ctx, "git", "-C", g.repoDir, "add", "--", path)
	if out, err := add.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git add: %w: %s", err, out)
	}
	commit := exec.CommandContext(ctx, "git", "-C", g.repoDir,
		"commit", "-m", message, "--only", "--", path)
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=vornik-it",
		"GIT_AUTHOR_EMAIL=it@vornik.test",
		"GIT_COMMITTER_NAME=vornik-it",
		"GIT_COMMITTER_EMAIL=it@vornik.test",
	)
	if out, err := commit.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %w: %s", err, out)
	}
	sha := exec.CommandContext(ctx, "git", "-C", g.repoDir, "rev-parse", "HEAD")
	out, err := sha.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

type stubReloader struct{ called int }

func (s *stubReloader) Reload() error { s.called++; return nil }

// setupSourceRepoForWorkflow creates a temporary git repo with a
// baseline <workflowID>.md committed so subsequent `git commit
// --only` calls have something to compare against. The workflow ID
// is parametric so every test run uses a unique one and can't trip
// the partial unique index on workflow_proposals (one pending
// proposal per workflow). Regression: 2026-06-04 — both applier
// e2e tests used the fixed ID "research"; a killed run left a
// stale pending row behind (t.Cleanup never fired) and every
// later Insert failed with "workflow already has a pending
// proposal" until the row was deleted by hand.
func setupSourceRepoForWorkflow(t *testing.T, workflowID string) (sourceDir string, deployedDir string) {
	t.Helper()
	sourceDir = t.TempDir()
	deployedDir = t.TempDir()

	mustRun(t, "git", "-C", sourceDir, "init", "-q")
	mustRun(t, "git", "-C", sourceDir, "config", "user.email", "it@vornik.test")
	mustRun(t, "git", "-C", sourceDir, "config", "user.name", "vornik-it")
	require.NoError(t, os.MkdirAll(filepath.Join(sourceDir, "workflows"), 0o755))
	baseline := filepath.Join(sourceDir, "workflows", workflowID+".md")
	require.NoError(t, os.WriteFile(baseline, []byte("baseline"), 0o644))
	mustRun(t, "git", "-C", sourceDir, "add", "workflows/"+workflowID+".md")
	mustRun(t, "git", "-C", sourceDir, "commit", "-q", "-m", "baseline")
	return sourceDir, deployedDir
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

func TestApplier_E2E_HappyPath(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowID := "research-apply-" + suffix // unique per run; see setupSourceRepoForWorkflow doc
	proposalID := "wpr-apply-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE id = $1`, proposalID)
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
		ProposalYAML:   "---\nworkflowId: " + workflowID + "\nversion: 2.0.0\n---\nnew body\n",
		Motivation:     "tighten step3 gate after 32% failure",
		EvidenceRunIDs: []string{"r-1", "r-2", "r-3"},
		Confidence:     0.8,
		ArchitectModel: "test",
		CreatedAt:      time.Now().UTC(),
	}))
	// Approve before apply (mirrors the Slice 3 flow).
	require.NoError(t, repo.Decide(ctx, proposalID,
		persistence.WorkflowProposalStatusApproved, "operator-x", "looks good"))

	applier := memetic.NewApplier(repo, writer, git, reloader,
		memetic.ApplierConfig{AuthorName: "vornik-architect", AuthorEmail: "architect@vornik.test"})

	got, err := applier.Apply(ctx, proposalID, "operator-x")
	require.NoError(t, err)
	require.Equal(t, persistence.WorkflowProposalStatusApplied, got.Status)
	require.NotEmpty(t, got.AppliedCommit)
	require.NotEqual(t, "no-git", got.AppliedCommit)

	// Files exist in both trees.
	deployedPath := filepath.Join(deployedDir, "workflows", workflowID+".md")
	sourcePath := filepath.Join(sourceDir, "workflows", workflowID+".md")
	for _, p := range []string{deployedPath, sourcePath} {
		body, err := os.ReadFile(p)
		require.NoError(t, err, "read %s", p)
		require.Contains(t, string(body), "version: 2.0.0",
			"file %s should contain the new YAML", p)
	}

	// Git log shows our commit subject + body.
	out, err := exec.Command("git", "-C", sourceDir, "log", "-1", "--pretty=%B").Output()
	require.NoError(t, err)
	logBody := string(out)
	require.Contains(t, logBody, "workflow("+workflowID+"):", "commit subject")
	require.Contains(t, logBody, proposalID, "commit body should reference proposal_id")
	require.Contains(t, logBody, "operator-x", "commit body should reference operator")

	require.Equal(t, 1, reloader.called, "reloader should fire exactly once")

	// Round-trip via repo to confirm the row's applied_commit
	// matches what HEAD points at.
	roundTrip, err := repo.Get(ctx, proposalID)
	require.NoError(t, err)
	require.Equal(t, got.AppliedCommit, roundTrip.AppliedCommit)
	require.NotNil(t, roundTrip.AppliedAt)
}

func TestApplier_E2E_NotApproved(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	proposalID := "wpr-notapproved-" + suffix
	workflowID := "research-notapproved-" + suffix // unique per run; see setupSourceRepoForWorkflow doc
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE id = $1`, proposalID)
	})

	ctx := context.Background()
	require.NoError(t, repo.Insert(ctx, &persistence.WorkflowProposal{
		ID:             proposalID,
		WorkflowID:     workflowID,
		Status:         persistence.WorkflowProposalStatusPending,
		ProposalYAML:   "yaml",
		Motivation:     "m",
		EvidenceRunIDs: []string{"r-1"},
		Confidence:     0.7,
		ArchitectModel: "m",
		CreatedAt:      time.Now().UTC(),
	}))
	// Deliberately NO Decide — still pending.

	sourceDir, deployedDir := setupSourceRepoForWorkflow(t, workflowID)
	writer := &itWorkflowWriter{sourceDir: sourceDir, deployedDir: deployedDir}
	applier := memetic.NewApplier(repo, writer, &itGitCommitter{repoDir: sourceDir}, &stubReloader{},
		memetic.ApplierConfig{})

	_, err := applier.Apply(ctx, proposalID, "operator-x")
	require.Error(t, err)
	require.ErrorIs(t, err, memetic.ErrProposalNotApproved)

	// File must NOT have been written.
	if _, err := os.Stat(filepath.Join(deployedDir, "workflows", workflowID+".md")); err == nil {
		t.Error("apply on pending should not write the file")
	}
}
