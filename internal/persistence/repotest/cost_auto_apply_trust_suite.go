package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunCostAutoApplyTrustSuite is the backend-agnostic contract for the cost-auto-apply
// trust primitives that span TWO repos (auto-apply design D1/D8): the canary
// LastApplyActorForKnob (a join canaries⋈proposals) and the proposal
// StagePreApplySnapshot. Both Postgres and SQLite run it.
func RunCostAutoApplyTrustSuite(t *testing.T, canaries persistence.CostTuningCanaryRepository, proposals persistence.ProposalRepository) {
	t.Helper()
	t.Run("LastApplyActorForKnob_most_recent_by_applied_at", func(t *testing.T) { lastApplyActor(t, canaries, proposals) })
	t.Run("StagePreApplySnapshot_writes_without_status_change", func(t *testing.T) { stageSnapshot(t, proposals) })
}

// mkAppliedProposal creates a DRAFT proposal, approves it (distinct actor — no
// self-approve), and marks it APPLIED with the given appliedBy actor.
func mkAppliedProposal(t *testing.T, proposals persistence.ProposalRepository, id, appliedBy string) {
	t.Helper()
	ctx := context.Background()
	p := &persistence.ControlPlaneProposal{
		ID: id, ProjectID: "", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeSwarm, Title: "t-" + id,
		ApplyTarget: "configs/swarms/x.md", ApplyContent: "content",
		Status: persistence.ProposalStatusDraft, ProposedBy: "cost-quality-detector",
	}
	if err := proposals.Create(ctx, p); err != nil {
		t.Fatalf("Create(%s): %v", id, err)
	}
	if err := proposals.SetStatus(ctx, id, persistence.ProposalStatusApproved, "operator"); err != nil {
		t.Fatalf("approve(%s): %v", id, err)
	}
	if err := proposals.MarkApplied(ctx, id, appliedBy, "snap"); err != nil {
		t.Fatalf("MarkApplied(%s): %v", id, err)
	}
}

func lastApplyActor(t *testing.T, canaries persistence.CostTuningCanaryRepository, proposals persistence.ProposalRepository) {
	ctx := context.Background()
	base := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	openCanaryAt := func(id, swarm, role, knob string, appliedAt time.Time) {
		c := newTestCanary(id, swarm, role, knob)
		c.AppliedAt = appliedAt
		mustOpenCanary(t, canaries, c)
	}
	// Human apply first (older), then an auto-apply (newer) on the same knob.
	mkAppliedProposal(t, proposals, "cpp_human", "operator")
	mkAppliedProposal(t, proposals, "cpp_auto", persistence.CostAutoApplyActor)
	openCanaryAt("cpp_human", "sw", "reviewer", "BUDGET", base)
	openCanaryAt("cpp_auto", "sw", "reviewer", "BUDGET", base.Add(48*time.Hour))

	actor, ok, err := canaries.LastApplyActorForKnob(ctx, "sw", "reviewer", "BUDGET")
	if err != nil {
		t.Fatalf("LastApplyActorForKnob: %v", err)
	}
	if !ok || actor != persistence.CostAutoApplyActor {
		t.Errorf("LastApplyActorForKnob = (%q,%v), want (%q,true) — most-recent canary is the auto-apply", actor, ok, persistence.CostAutoApplyActor)
	}
	// A knob with no canary → ok=false (never applied; first apply stays human-gated).
	if _, ok2, _ := canaries.LastApplyActorForKnob(ctx, "sw", "reviewer", "NOPE"); ok2 {
		t.Error("LastApplyActorForKnob(no canary) ok=true, want false")
	}
}

func stageSnapshot(t *testing.T, proposals persistence.ProposalRepository) {
	ctx := context.Background()
	// APPROVED proposal: stage succeeds, status unchanged, snapshot stored.
	p := &persistence.ControlPlaneProposal{
		ID: "cpp_stage", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeSwarm, Title: "stage",
		ApplyTarget: "configs/swarms/x.md", ApplyContent: "new",
		Status: persistence.ProposalStatusDraft, ProposedBy: "cost-quality-detector",
	}
	if err := proposals.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := proposals.SetStatus(ctx, "cpp_stage", persistence.ProposalStatusApproved, "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := proposals.StagePreApplySnapshot(ctx, "cpp_stage", "PREIMAGE"); err != nil {
		t.Fatalf("StagePreApplySnapshot: %v", err)
	}
	got, err := proposals.GetByID(ctx, "cpp_stage")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.PreApplySnapshot != "PREIMAGE" {
		t.Errorf("PreApplySnapshot = %q, want PREIMAGE", got.PreApplySnapshot)
	}
	if got.Status != persistence.ProposalStatusApproved {
		t.Errorf("status = %q, want APPROVED (StagePreApplySnapshot must not change status)", got.Status)
	}
	// A DRAFT (non-APPROVED) proposal: stage refused.
	d := &persistence.ControlPlaneProposal{
		ID: "cpp_draft", Kind: persistence.ProposalKindConfig, Title: "d",
		BlastRadius: persistence.ProposalScopeSwarm,
		ApplyTarget: "configs/swarms/y.md", ApplyContent: "z",
		Status: persistence.ProposalStatusDraft, ProposedBy: "cost-quality-detector",
	}
	if err := proposals.Create(ctx, d); err != nil {
		t.Fatalf("Create draft: %v", err)
	}
	if err := proposals.StagePreApplySnapshot(ctx, "cpp_draft", "x"); !errors.Is(err, persistence.ErrProposalNotApproved) {
		t.Errorf("StagePreApplySnapshot(DRAFT) err = %v, want ErrProposalNotApproved", err)
	}
}
