//go:build integration
// +build integration

package integration_test

// Integration test for the admin workflow-proposals review surface
// (Slice 3a) against real postgres. The handler tests pin the gate
// matrix; this test pins the SQL semantics — specifically that
// Decide() reads back the updated row with decided_at + decided_by
// stamped, and that the list filters compose correctly against
// real indexes.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
)

func TestAdminWorkflowProposals_Decide_E2E(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowID := "wfprop-admin-" + suffix
	proposalID := "wpr-admin-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE workflow_id = $1`, workflowID)
	})

	ctx := context.Background()
	require.NoError(t, repo.Insert(ctx, &persistence.WorkflowProposal{
		ID:             proposalID,
		WorkflowID:     workflowID,
		Status:         persistence.WorkflowProposalStatusPending,
		ProposalYAML:   "yaml-body",
		Motivation:     "research findings",
		EvidenceRunIDs: []string{"r-1", "r-2", "r-3"},
		Confidence:     0.72,
		ArchitectModel: "test-model",
		CreatedAt:      time.Now().UTC(),
	}))

	// Approve. The decided_by + notes survive the round-trip.
	require.NoError(t, repo.Decide(ctx, proposalID,
		persistence.WorkflowProposalStatusApproved, "operator-x", "looks correct"))

	got, err := repo.Get(ctx, proposalID)
	require.NoError(t, err)
	require.Equal(t, persistence.WorkflowProposalStatusApproved, got.Status)
	require.Equal(t, "operator-x", got.DecidedBy)
	require.Equal(t, "looks correct", got.Notes)
	require.NotNil(t, got.DecidedAt)

	// Second decide on already-approved row → ErrInvalidProposalTransition.
	err = repo.Decide(ctx, proposalID,
		persistence.WorkflowProposalStatusRejected, "operator-y", "racing")
	require.Error(t, err)
	require.ErrorIs(t, err, persistence.ErrInvalidProposalTransition)
}

func TestAdminWorkflowProposals_List_StatusFilter_E2E(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	wfA := "wfprop-listA-" + suffix
	wfB := "wfprop-listB-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE workflow_id IN ($1, $2)`, wfA, wfB)
	})

	ctx := context.Background()
	// wfA: insert + reject so it's no longer pending.
	require.NoError(t, repo.Insert(ctx, &persistence.WorkflowProposal{
		ID:             "wpr-a-" + suffix,
		WorkflowID:     wfA,
		ProposalYAML:   "y",
		Motivation:     "m",
		EvidenceRunIDs: []string{"r-1"},
		Confidence:     0.7,
		ArchitectModel: "m",
		CreatedAt:      time.Now().UTC(),
	}))
	require.NoError(t, repo.Decide(ctx, "wpr-a-"+suffix,
		persistence.WorkflowProposalStatusRejected, "v", ""))

	// wfB: insert and leave pending.
	require.NoError(t, repo.Insert(ctx, &persistence.WorkflowProposal{
		ID:             "wpr-b-" + suffix,
		WorkflowID:     wfB,
		ProposalYAML:   "y",
		Motivation:     "m",
		EvidenceRunIDs: []string{"r-1"},
		Confidence:     0.7,
		ArchitectModel: "m",
		CreatedAt:      time.Now().UTC(),
	}))

	// Filter by status=approved,rejected → only wfA matches.
	got, err := repo.List(ctx, persistence.WorkflowProposalFilter{
		Statuses: []persistence.WorkflowProposalStatus{
			persistence.WorkflowProposalStatusApproved,
			persistence.WorkflowProposalStatusRejected,
		},
	})
	require.NoError(t, err)
	foundA := false
	for _, p := range got {
		require.NotEqual(t, "wpr-b-"+suffix, p.ID,
			"wfB pending row should NOT appear in approved|rejected filter")
		if p.ID == "wpr-a-"+suffix {
			foundA = true
		}
	}
	require.True(t, foundA, "wfA rejected row should appear in approved|rejected filter")

	// Filter by workflow=wfB → only wfB regardless of status.
	got, err = repo.List(ctx, persistence.WorkflowProposalFilter{WorkflowID: wfB})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "wpr-b-"+suffix, got[0].ID)
}
