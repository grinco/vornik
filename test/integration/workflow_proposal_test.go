//go:build integration
// +build integration

package integration_test

// Integration test for the WorkflowProposalRepository against a
// real postgres. sqlmock can't exercise the DB-layer guarantees
// the architect agent leans on: the partial unique index that
// caps "one pending proposal per workflow", and the state-machine
// WHERE clauses that refuse out-of-order transitions.
//
// These are the things that go wrong silently if the migration
// drifts from the repo's assumptions — exactly the class of bug
// the post-mortem-driven test discipline (CLAUDE.md) is meant to
// catch before it ships.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
)

func newProposalFixture(id, workflowID string) *persistence.WorkflowProposal {
	return &persistence.WorkflowProposal{
		ID:             id,
		WorkflowID:     workflowID,
		Status:         persistence.WorkflowProposalStatusPending,
		ProposalYAML:   "steps:\n  - id: a\n    role: researcher\n",
		Motivation:     "judge-fail 32% over 9 runs",
		EvidenceRunIDs: []string{"run-1", "run-2", "run-3"},
		Confidence:     0.74,
		ArchitectModel: "qwen3.6:35b",
		CreatedAt:      time.Now().UTC(),
	}
}

func TestWorkflowProposalRepository_PartialUniqueIndex_BlocksSecondPending(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowID := "wfprop-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE workflow_id = $1`, workflowID)
	})

	ctx := context.Background()
	first := newProposalFixture("wpr-a-"+suffix, workflowID)
	require.NoError(t, repo.Insert(ctx, first), "first pending should insert")

	// Second pending proposal for the same workflow MUST hit the
	// partial unique index and surface as ErrProposalRateLimited.
	second := newProposalFixture("wpr-b-"+suffix, workflowID)
	err := repo.Insert(ctx, second)
	require.True(t, errors.Is(err, persistence.ErrProposalRateLimited),
		"want ErrProposalRateLimited, got %v", err)

	// After deciding the first one (→ rejected), a new pending row
	// IS allowed. This is the architect's retry path: operator says
	// "nope" and the agent can re-propose later.
	require.NoError(t, repo.Decide(ctx, first.ID,
		persistence.WorkflowProposalStatusRejected, "test-operator", "trying again"))
	third := newProposalFixture("wpr-c-"+suffix, workflowID)
	require.NoError(t, repo.Insert(ctx, third),
		"new pending should insert after the prior was rejected")
}

func TestWorkflowProposalRepository_StateMachine_EndToEnd(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowID := "wfprop-sm-" + suffix
	proposalID := "wpr-sm-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE workflow_id = $1`, workflowID)
	})

	ctx := context.Background()
	require.NoError(t, repo.Insert(ctx, newProposalFixture(proposalID, workflowID)))

	// MarkApplied refuses a pending row (must be approved first).
	err := repo.MarkApplied(ctx, proposalID, "deadbeef")
	require.True(t, errors.Is(err, persistence.ErrInvalidProposalTransition),
		"MarkApplied on pending should error, got %v", err)

	// MarkRolledBack refuses a pending row (must be applied first).
	err = repo.MarkRolledBack(ctx, proposalID, "cafebabe")
	require.True(t, errors.Is(err, persistence.ErrInvalidProposalTransition),
		"MarkRolledBack on pending should error, got %v", err)

	// Approve → apply → roll back. Each transition stamps the
	// expected columns; Get returns the threaded values.
	require.NoError(t, repo.Decide(ctx, proposalID,
		persistence.WorkflowProposalStatusApproved, "test-operator", "ok"))
	require.NoError(t, repo.MarkApplied(ctx, proposalID, "abc1234"))
	require.NoError(t, repo.MarkRolledBack(ctx, proposalID, "def5678"))

	got, err := repo.Get(ctx, proposalID)
	require.NoError(t, err)
	require.Equal(t, persistence.WorkflowProposalStatusRolledBack, got.Status)
	require.Equal(t, "abc1234", got.AppliedCommit)
	require.Equal(t, "def5678", got.RollbackCommit)
	require.Equal(t, "test-operator", got.DecidedBy)
	require.NotNil(t, got.DecidedAt)
	require.NotNil(t, got.AppliedAt)
	require.Equal(t, []string{"run-1", "run-2", "run-3"}, got.EvidenceRunIDs)
}

func TestWorkflowProposalRepository_Decide_NotFound(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	err := repo.Decide(context.Background(),
		fmt.Sprintf("missing-%d", time.Now().UnixNano()),
		persistence.WorkflowProposalStatusApproved, "v", "")
	require.True(t, errors.Is(err, persistence.ErrNotFound),
		"want ErrNotFound, got %v", err)
}

func TestWorkflowProposalRepository_List_FiltersByStatusAndWorkflow(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	wfA := "wfprop-list-a-" + suffix
	wfB := "wfprop-list-b-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE workflow_id IN ($1, $2)`, wfA, wfB)
	})

	ctx := context.Background()
	require.NoError(t, repo.Insert(ctx, newProposalFixture("wpr-la-"+suffix, wfA)))
	require.NoError(t, repo.Insert(ctx, newProposalFixture("wpr-lb-"+suffix, wfB)))
	// Decide wfA so it isn't pending anymore.
	require.NoError(t, repo.Decide(ctx, "wpr-la-"+suffix,
		persistence.WorkflowProposalStatusRejected, "v", ""))

	// Filter on workflow + status=pending → only wfB.
	got, err := repo.List(ctx, persistence.WorkflowProposalFilter{
		Statuses: []persistence.WorkflowProposalStatus{persistence.WorkflowProposalStatusPending},
	})
	require.NoError(t, err)
	foundB := false
	for _, p := range got {
		require.NotEqual(t, "wpr-la-"+suffix, p.ID, "rejected row should not appear in pending filter")
		if p.ID == "wpr-lb-"+suffix {
			foundB = true
		}
	}
	require.True(t, foundB, "wfB pending row should appear")
}
