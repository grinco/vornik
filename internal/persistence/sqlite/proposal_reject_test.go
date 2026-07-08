package sqlite

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func rejectTestRepo(t *testing.T) (persistence.ProposalRepository, context.Context) {
	t.Helper()
	db, err := Connect(context.Background(), DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewProposalRepository(db.DB), context.Background()
}

func seedDraft(ctx context.Context, t *testing.T, repo persistence.ProposalRepository, id string) {
	t.Helper()
	if err := repo.Create(ctx, &persistence.ControlPlaneProposal{
		ID: id, Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeDaemon,
		Title: "x", ApplyTarget: "config.yaml", ApplyContent: "y",
		Status: persistence.ProposalStatusDraft, ProposedBy: "proposer",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
}

// TestSetStatus_WithdrawApproved is the fix for the stuck-proposal report
// (2026-07-08): an APPROVED-but-unappliable proposal (e.g. a daemon-scope MCP
// change superseded by a re-draft) could not be rejected — SetStatus only
// allowed DRAFT. Reject/withdraw is now allowed from APPROVED too.
func TestSetStatus_WithdrawApproved(t *testing.T) {
	repo, ctx := rejectTestRepo(t)
	seedDraft(ctx, t, repo, "cpp-approved")
	if err := repo.SetStatus(ctx, "cpp-approved", persistence.ProposalStatusApproved, "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// Withdraw the APPROVED proposal → REJECTED.
	if err := repo.SetStatus(ctx, "cpp-approved", persistence.ProposalStatusRejected, "operator"); err != nil {
		t.Fatalf("withdraw an APPROVED proposal must succeed, got %v", err)
	}
	got, _ := repo.GetByID(ctx, "cpp-approved")
	if got.Status != persistence.ProposalStatusRejected {
		t.Fatalf("status = %s, want REJECTED", got.Status)
	}
}

// TestSetStatus_RejectDraftStillWorks — the original path is unchanged.
func TestSetStatus_RejectDraftStillWorks(t *testing.T) {
	repo, ctx := rejectTestRepo(t)
	seedDraft(ctx, t, repo, "cpp-draft")
	if err := repo.SetStatus(ctx, "cpp-draft", persistence.ProposalStatusRejected, "operator"); err != nil {
		t.Fatalf("reject a DRAFT must succeed, got %v", err)
	}
}

// TestSetStatus_ApproveStillDraftOnly — approve must not work on a decided row.
func TestSetStatus_ApproveStillDraftOnly(t *testing.T) {
	repo, ctx := rejectTestRepo(t)
	seedDraft(ctx, t, repo, "cpp-reapprove")
	_ = repo.SetStatus(ctx, "cpp-reapprove", persistence.ProposalStatusApproved, "operator")
	// Second approve (now APPROVED) must be refused.
	if err := repo.SetStatus(ctx, "cpp-reapprove", persistence.ProposalStatusApproved, "operator2"); !errors.Is(err, persistence.ErrProposalNotDraft) {
		t.Fatalf("re-approve must fail ErrProposalNotDraft, got %v", err)
	}
}

// TestSetStatus_RejectTerminalRefused — a REJECTED (terminal) proposal can't be
// re-rejected.
func TestSetStatus_RejectTerminalRefused(t *testing.T) {
	repo, ctx := rejectTestRepo(t)
	seedDraft(ctx, t, repo, "cpp-terminal")
	_ = repo.SetStatus(ctx, "cpp-terminal", persistence.ProposalStatusRejected, "operator")
	if err := repo.SetStatus(ctx, "cpp-terminal", persistence.ProposalStatusRejected, "operator"); !errors.Is(err, persistence.ErrProposalNotPending) {
		t.Fatalf("rejecting a terminal proposal must fail ErrProposalNotPending, got %v", err)
	}
}
