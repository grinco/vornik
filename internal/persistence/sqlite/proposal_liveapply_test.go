package sqlite

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestProposalRepo_LiveApplyRoundTrip locks in that the live_apply column
// added for control-plane live-apply round-trips through Create → GetByID →
// List, and defaults to false for proposals that don't set it.
func TestProposalRepo_LiveApplyRoundTrip(t *testing.T) {
	db, err := Connect(context.Background(), DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewProposalRepository(db.DB)
	ctx := context.Background()

	live := &persistence.ControlPlaneProposal{
		ID: "cpp-live", Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeDaemon,
		Title: "mcp add", ApplyTarget: "config.yaml", ApplyContent: "x",
		Status: persistence.ProposalStatusDraft, ProposedBy: "operator-ui", LiveApply: true,
	}
	gated := &persistence.ControlPlaneProposal{
		ID: "cpp-gated", ProjectID: "janka", Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeProject,
		Title: "tune", ApplyTarget: "config.yaml", ApplyContent: "y",
		Status: persistence.ProposalStatusDraft, ProposedBy: "tune-detector", // LiveApply defaults false
	}
	for _, p := range []*persistence.ControlPlaneProposal{live, gated} {
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("create %s: %v", p.ID, err)
		}
	}

	got, err := repo.GetByID(ctx, "cpp-live")
	if err != nil {
		t.Fatalf("get live: %v", err)
	}
	if !got.LiveApply {
		t.Error("LiveApply must round-trip as true")
	}

	got, err = repo.GetByID(ctx, "cpp-gated")
	if err != nil {
		t.Fatalf("get gated: %v", err)
	}
	if got.LiveApply {
		t.Error("unset LiveApply must default to false")
	}

	all, err := repo.List(ctx, persistence.ProposalListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := map[string]bool{}
	for _, p := range all {
		seen[p.ID] = p.LiveApply
	}
	if !seen["cpp-live"] || seen["cpp-gated"] {
		t.Errorf("List must carry LiveApply per row: live=%v gated=%v", seen["cpp-live"], seen["cpp-gated"])
	}
}
