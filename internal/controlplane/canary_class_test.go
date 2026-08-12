package controlplane

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/quality"
)

// Canary class registry (LLD 2026-08-10 §4). Replaces the two hardcoded
// constants in canary_guard.go (costQualityDetectorProposedBy /
// swarmRoleEnvChangeKind) that excluded the tune-detector's
// workflow_step_timeout proposals on BOTH axes and so let the 2026-08-10
// easeit-companion ingest regression run unwatched.

func costQualityProposal() *persistence.ControlPlaneProposal {
	return &persistence.ControlPlaneProposal{
		ID: "cpp_cq", ProposedBy: "cost-quality-detector",
		Status:   persistence.ProposalStatusApplied,
		Evidence: `{"change":{"kind":"swarm_role_env","swarm":"s1","role":"coder","key":"VORNIK_STEP_PROMPT_TOKEN_BUDGET"}}`,
	}
}

func stepTimeoutProposal() *persistence.ControlPlaneProposal {
	return &persistence.ControlPlaneProposal{
		ID: "cpp_st", ProposedBy: "tune-detector",
		Status:   persistence.ProposalStatusApplied,
		Evidence: `{"change":{"kind":"workflow_step_timeout","workflow":"ingest","step":"ingest","timeout":"259s"}}`,
	}
}

func TestCanaryRegistry_MatchesCostQualityProposal(t *testing.T) {
	cls := canaryClassFor(defaultCanaryClasses(), costQualityProposal())
	if cls == nil {
		t.Fatal("cost-quality proposal matched no class")
	}
	if cls.Name() != "cost_quality" {
		t.Fatalf("matched %q, want cost_quality", cls.Name())
	}
}

// The registry must not claim a proposal it has no evaluator for. Class B is
// not built yet (its locus needs a schema change), so a step-timeout proposal
// matching nothing is the CORRECT state — and it must be a clean no-match, not
// a wrong-class match, or the guard would open a canary it cannot evaluate.
func TestCanaryRegistry_DoesNotMisclaimStepTimeoutProposal(t *testing.T) {
	if cls := canaryClassFor(defaultCanaryClasses(), stepTimeoutProposal()); cls != nil {
		t.Fatalf("step-timeout proposal wrongly matched class %q", cls.Name())
	}
}

// Both axes still gate: the right proposer with the wrong change kind, and the
// right change kind from the wrong proposer, must each fail to match.
func TestCanaryRegistry_RequiresBothProposerAndChangeKind(t *testing.T) {
	wrongKind := costQualityProposal()
	wrongKind.Evidence = `{"change":{"kind":"workflow_step_timeout","workflow":"w","step":"s"}}`
	if cls := canaryClassFor(defaultCanaryClasses(), wrongKind); cls != nil {
		t.Fatalf("cost-quality proposer with a foreign change kind matched %q", cls.Name())
	}

	wrongProposer := costQualityProposal()
	wrongProposer.ProposedBy = "operator-ui"
	if cls := canaryClassFor(defaultCanaryClasses(), wrongProposer); cls != nil {
		t.Fatalf("swarm_role_env from a foreign proposer matched %q", cls.Name())
	}
}

func TestCostQualityClass_LocusMatchesShippedParse(t *testing.T) {
	cls := costQualityCanaryClass{}
	swarm, role, knob, ok := cls.Locus(costQualityProposal())
	if !ok {
		t.Fatal("Locus failed on a well-formed cost-quality proposal")
	}
	if swarm != "s1" || role != "coder" || knob != "VORNIK_STEP_PROMPT_TOKEN_BUDGET" {
		t.Fatalf("Locus = (%q, %q, %q), want (s1, coder, VORNIK_STEP_PROMPT_TOKEN_BUDGET)", swarm, role, knob)
	}
}

// Malformed evidence must report ok=false so discovery raises the coverage-gap
// metric rather than opening a canary on empty identity.
func TestCostQualityClass_LocusRejectsMalformedEvidence(t *testing.T) {
	cls := costQualityCanaryClass{}
	p := costQualityProposal()
	p.Evidence = `{"change":{"kind":"swarm_role_env","swarm":"","role":"coder","key":"k"}}`
	if _, _, _, ok := cls.Locus(p); ok {
		t.Fatal("Locus accepted evidence with an empty swarm")
	}
}

// Design D1 ("never close early on a pass") needs no per-class knob: the
// shipped guard already holds a canary to window_until even when the post
// window looks clean. Pinned behaviourally so a future early-close
// optimisation cannot silently reintroduce the hazard D1 exists to prevent —
// a regression latent behind a quiet period being stamped PASS before it
// appears, which is exactly what the 2026-08-10 five-day auth outage would
// have caused.
func TestCanaryGuard_HealthyCanaryStaysOpenBeforeWindowUntil(t *testing.T) {
	appliedAt := time.Date(2026, 8, 5, 8, 23, 0, 0, time.UTC)
	// Mid-window: 5 days into a 7-day watch — the exact position the ingest
	// canary would have been in on 2026-08-10 while the auth outage kept every
	// tick clean.
	now := appliedAt.Add(120 * time.Hour)

	canaries := newFakeCanaryRepo()
	p := appliedProposal("cpp_d1", "s1", "coder", "K", appliedAt)
	guard := newGuard(
		&fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
			return reportA1("s1", "coder", 0.95, 100, 50), nil // healthy, plenty of samples
		}},
		canaries, newGuardProposals(p))
	guard.Now = func() time.Time { return now }

	open := &persistence.CostTuningCanary{
		ProposalID: p.ID, SwarmID: "s1", Role: "coder", Knob: "K",
		AppliedAt: appliedAt, WindowUntil: appliedAt.Add(168 * time.Hour),
		Baseline: persistence.CanaryBaseline{A1Rate: 0.95, A1Sufficient: true, EffCost: 100},
		Status:   persistence.CanaryStatusOpen, OpenedAt: appliedAt,
	}
	if err := canaries.Open(context.Background(), open); err != nil {
		t.Fatalf("seed canary: %v", err)
	}

	guard.evaluateOne(context.Background(), open)

	got, err := canaries.GetByProposalID(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("reload canary: %v", err)
	}
	if got.Status != persistence.CanaryStatusOpen {
		t.Fatalf("canary finalized %q mid-window on a clean read; D1 (no early close) regressed", got.Status)
	}

	// Proof this test can distinguish: the SAME clean read past window_until
	// must finalize `passed`. Without this, the assertion above would also hold
	// if evaluateOne were bailing out for an unrelated reason.
	guard.Now = func() time.Time { return open.WindowUntil.Add(time.Minute) }
	guard.evaluateOne(context.Background(), open)
	got, err = canaries.GetByProposalID(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("reload canary: %v", err)
	}
	if got.Status != persistence.CanaryStatusPassed {
		t.Fatalf("canary status %q after window_until on a clean read; want passed", got.Status)
	}
}
