package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunCostTuningCanarySuite is the backend-agnostic contract for
// persistence.CostTuningCanaryRepository (cost/quality canary guard, design
// 2026-07-24 §4.3). Both Postgres and SQLite run it — a failure here means a
// backend has diverged from the protocol contract.
func RunCostTuningCanarySuite(t *testing.T, repo persistence.CostTuningCanaryRepository) {
	t.Helper()
	t.Run("Open_then_Get_round_trips", func(t *testing.T) { canaryRoundTrip(t, repo) })
	t.Run("Get_unknown_is_ErrNotFound", func(t *testing.T) { canaryGetUnknown(t, repo) })
	t.Run("ListOpen_only_open_oldest_first", func(t *testing.T) { canaryListOpen(t, repo) })
	t.Run("Finalize_each_terminal_status", func(t *testing.T) { canaryFinalizeStatuses(t, repo) })
	t.Run("Finalize_only_matches_open", func(t *testing.T) { canaryFinalizeIdempotent(t, repo) })
	t.Run("HasOpenForSwarmRole", func(t *testing.T) { canaryHasOpen(t, repo) })
	t.Run("HasActiveCooldown_keyed_swarm_role_knob", func(t *testing.T) { canaryCooldown(t, repo) })
	t.Run("LatestWindowUntil_baseline_clamp", func(t *testing.T) { canaryLatestWindowUntil(t, repo) })
	t.Run("baseline_jsonb_round_trips", func(t *testing.T) { canaryBaselineRoundTrip(t, repo) })
	t.Run("CountPassedForKnob_exact_and_passed_only", func(t *testing.T) { canaryCountPassed(t, repo) })
}

// canaryCountPassed verifies CountPassedForKnob counts ONLY terminal
// status='passed' canaries and ONLY the exact (swarm,role,knob) — the
// cost-auto-apply track-record trust signal (auto-apply design D1).
func canaryCountPassed(t *testing.T, repo persistence.CostTuningCanaryRepository) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	seed := func(id, swarm, role, knob, status string) {
		mustOpenCanary(t, repo, newTestCanary(id, swarm, role, knob))
		if status != persistence.CanaryStatusOpen {
			if err := repo.Finalize(ctx, id, status, "", base.Add(200*time.Hour)); err != nil {
				t.Fatalf("Finalize(%s,%s): %v", id, status, err)
			}
		}
	}
	// Target locus: 2 passed + 1 regressed + 1 still-open.
	seed("cpp_p1", "sw", "reviewer", "BUDGET", persistence.CanaryStatusPassed)
	seed("cpp_p2", "sw", "reviewer", "BUDGET", persistence.CanaryStatusPassed)
	seed("cpp_r1", "sw", "reviewer", "BUDGET", persistence.CanaryStatusRegressed)
	seed("cpp_o1", "sw", "reviewer", "BUDGET", persistence.CanaryStatusOpen)
	// Other knob / role / swarm — must NOT be counted.
	seed("cpp_x1", "sw", "reviewer", "OTHERKNOB", persistence.CanaryStatusPassed)
	seed("cpp_x2", "sw", "analyst", "BUDGET", persistence.CanaryStatusPassed)
	seed("cpp_x3", "sw2", "reviewer", "BUDGET", persistence.CanaryStatusPassed)

	n, err := repo.CountPassedForKnob(ctx, "sw", "reviewer", "BUDGET")
	if err != nil {
		t.Fatalf("CountPassedForKnob: %v", err)
	}
	if n != 2 {
		t.Errorf("CountPassedForKnob = %d, want 2 (passed-only, exact locus)", n)
	}
	if n0, _ := repo.CountPassedForKnob(ctx, "sw", "reviewer", "NEVER"); n0 != 0 {
		t.Errorf("CountPassedForKnob(unknown knob) = %d, want 0", n0)
	}
}

func newTestCanary(id, swarm, role, knob string) *persistence.CostTuningCanary {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	return &persistence.CostTuningCanary{
		ProposalID:    id,
		SwarmID:       swarm,
		Role:          role,
		Knob:          knob,
		ProjectIDs:    []string{"p1", "p2"},
		WorkflowIDs:   []string{"wf1"},
		AppliedAt:     base,
		BaselineStart: base.Add(-168 * time.Hour),
		WindowUntil:   base.Add(168 * time.Hour),
		Baseline: persistence.CanaryBaseline{
			A1Rate: 0.9, A1Samples: 100, A1Sufficient: true, EffCost: 1200,
			A2: map[string]persistence.CanaryA2Series{"wf1": {Rate: 0.85, Samples: 40, Sufficient: true}},
		},
		Status:   persistence.CanaryStatusOpen,
		OpenedAt: base.Add(time.Hour),
	}
}

func mustOpenCanary(t *testing.T, repo persistence.CostTuningCanaryRepository, c *persistence.CostTuningCanary) {
	t.Helper()
	if err := repo.Open(context.Background(), c); err != nil {
		t.Fatalf("Open(%s): %v", c.ProposalID, err)
	}
}

func canaryRoundTrip(t *testing.T, repo persistence.CostTuningCanaryRepository) {
	ctx := context.Background()
	c := newTestCanary("rt-1", "assistant", "researcher", "VORNIK_STEP_PROMPT_TOKEN_BUDGET")
	mustOpenCanary(t, repo, c)
	got, err := repo.GetByProposalID(ctx, "rt-1")
	if err != nil {
		t.Fatalf("GetByProposalID: %v", err)
	}
	if got.SwarmID != "assistant" || got.Role != "researcher" || got.Knob != "VORNIK_STEP_PROMPT_TOKEN_BUDGET" {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if got.Status != persistence.CanaryStatusOpen {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if len(got.ProjectIDs) != 2 || len(got.WorkflowIDs) != 1 || got.WorkflowIDs[0] != "wf1" {
		t.Fatalf("slice round-trip mismatch: projects=%v workflows=%v", got.ProjectIDs, got.WorkflowIDs)
	}
	if !got.AppliedAt.Equal(c.AppliedAt) || !got.WindowUntil.Equal(c.WindowUntil) || !got.BaselineStart.Equal(c.BaselineStart) {
		t.Fatalf("time round-trip mismatch: %+v", got)
	}
	if got.ClosedAt != nil {
		t.Fatalf("closed_at should be nil until terminal, got %v", got.ClosedAt)
	}
}

func canaryGetUnknown(t *testing.T, repo persistence.CostTuningCanaryRepository) {
	if _, err := repo.GetByProposalID(context.Background(), "does-not-exist"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func canaryListOpen(t *testing.T, repo persistence.CostTuningCanaryRepository) {
	ctx := context.Background()
	older := newTestCanary("lo-old", "s", "r", "k")
	older.OpenedAt = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	newer := newTestCanary("lo-new", "s", "r2", "k")
	newer.OpenedAt = time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	closed := newTestCanary("lo-closed", "s", "r3", "k")
	mustOpenCanary(t, repo, older)
	mustOpenCanary(t, repo, newer)
	mustOpenCanary(t, repo, closed)
	if err := repo.Finalize(ctx, "lo-closed", persistence.CanaryStatusPassed, "clean", time.Now().UTC()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	open, err := repo.ListOpen(ctx)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	// The suite shares one repo across subtests, so assert on the rows THIS
	// subtest created rather than an exact global count: both open rows present,
	// the closed one excluded (partial index parity, I5), and oldest-first order.
	var idxOld, idxNew = -1, -1
	for i, c := range open {
		switch c.ProposalID {
		case "lo-closed":
			t.Fatalf("ListOpen must exclude the finalized (passed) canary (partial index parity, I5)")
		case "lo-old":
			idxOld = i
		case "lo-new":
			idxNew = i
		}
	}
	if idxOld < 0 || idxNew < 0 {
		t.Fatalf("ListOpen missing an open row: lo-old idx=%d lo-new idx=%d", idxOld, idxNew)
	}
	if idxOld > idxNew {
		t.Fatalf("ListOpen not oldest-first: lo-old at %d, lo-new at %d", idxOld, idxNew)
	}
}

func canaryFinalizeStatuses(t *testing.T, repo persistence.CostTuningCanaryRepository) {
	ctx := context.Background()
	closedAt := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	for i, st := range []string{
		persistence.CanaryStatusPassed,
		persistence.CanaryStatusRegressed,
		persistence.CanaryStatusInsufficientData,
		persistence.CanaryStatusSuperseded,
	} {
		id := "fin-" + st
		c := newTestCanary(id, "s", "r", "k")
		mustOpenCanary(t, repo, c)
		reason := "reason-" + st
		if err := repo.Finalize(ctx, id, st, reason, closedAt); err != nil {
			t.Fatalf("[%d] Finalize(%s): %v", i, st, err)
		}
		got, err := repo.GetByProposalID(ctx, id)
		if err != nil {
			t.Fatalf("[%d] Get: %v", i, err)
		}
		if got.Status != st {
			t.Fatalf("[%d] status = %q, want %q", i, got.Status, st)
		}
		if got.Reason != reason {
			t.Fatalf("[%d] reason = %q, want %q", i, got.Reason, reason)
		}
		if got.ClosedAt == nil || !got.ClosedAt.Equal(closedAt) {
			t.Fatalf("[%d] closed_at = %v, want %v", i, got.ClosedAt, closedAt)
		}
	}
}

func canaryFinalizeIdempotent(t *testing.T, repo persistence.CostTuningCanaryRepository) {
	ctx := context.Background()
	c := newTestCanary("idem-1", "s", "r", "k")
	mustOpenCanary(t, repo, c)
	first := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	if err := repo.Finalize(ctx, "idem-1", persistence.CanaryStatusRegressed, "first", first); err != nil {
		t.Fatalf("Finalize 1: %v", err)
	}
	// A second finalize (e.g. a re-run tick) must NOT overwrite — it only matches
	// an open row. No error either way; the status stays regressed.
	if err := repo.Finalize(ctx, "idem-1", persistence.CanaryStatusPassed, "second", first.Add(time.Hour)); err != nil {
		t.Fatalf("Finalize 2: %v", err)
	}
	got, err := repo.GetByProposalID(ctx, "idem-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != persistence.CanaryStatusRegressed || got.Reason != "first" {
		t.Fatalf("second finalize overwrote a terminal row: status=%q reason=%q", got.Status, got.Reason)
	}
}

func canaryHasOpen(t *testing.T, repo persistence.CostTuningCanaryRepository) {
	ctx := context.Background()
	mustOpenCanary(t, repo, newTestCanary("ho-1", "swarmA", "roleA", "k"))
	if ok, err := repo.HasOpenForSwarmRole(ctx, "swarmA", "roleA"); err != nil || !ok {
		t.Fatalf("HasOpenForSwarmRole(open) = %v,%v; want true,nil", ok, err)
	}
	if ok, err := repo.HasOpenForSwarmRole(ctx, "swarmA", "otherRole"); err != nil || ok {
		t.Fatalf("HasOpenForSwarmRole(other role) = %v,%v; want false,nil", ok, err)
	}
	// A finalized canary does NOT count as open.
	if err := repo.Finalize(ctx, "ho-1", persistence.CanaryStatusPassed, "", time.Now().UTC()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if ok, err := repo.HasOpenForSwarmRole(ctx, "swarmA", "roleA"); err != nil || ok {
		t.Fatalf("HasOpenForSwarmRole after passed = %v,%v; want false,nil", ok, err)
	}
}

func canaryCooldown(t *testing.T, repo persistence.CostTuningCanaryRepository) {
	ctx := context.Background()
	closedAt := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	c := newTestCanary("cd-1", "swarmC", "roleC", "KNOB_A")
	mustOpenCanary(t, repo, c)
	if err := repo.Finalize(ctx, "cd-1", persistence.CanaryStatusRegressed, "regressed", closedAt); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	// Active while closed_at is after notBefore.
	if ok, err := repo.HasActiveCooldown(ctx, "swarmC", "roleC", "KNOB_A", closedAt.Add(-time.Hour)); err != nil || !ok {
		t.Fatalf("HasActiveCooldown(within) = %v,%v; want true,nil", ok, err)
	}
	// Expired once notBefore passes closed_at.
	if ok, err := repo.HasActiveCooldown(ctx, "swarmC", "roleC", "KNOB_A", closedAt.Add(time.Hour)); err != nil || ok {
		t.Fatalf("HasActiveCooldown(expired) = %v,%v; want false,nil", ok, err)
	}
	// A DIFFERENT knob is unaffected (cooldown keyed swarm,role,knob).
	if ok, err := repo.HasActiveCooldown(ctx, "swarmC", "roleC", "KNOB_B", closedAt.Add(-time.Hour)); err != nil || ok {
		t.Fatalf("HasActiveCooldown(other knob) = %v,%v; want false,nil", ok, err)
	}
	// An insufficient_data / passed canary does NOT create a cooldown.
	c2 := newTestCanary("cd-2", "swarmC", "roleD", "KNOB_A")
	mustOpenCanary(t, repo, c2)
	if err := repo.Finalize(ctx, "cd-2", persistence.CanaryStatusInsufficientData, "thin", closedAt); err != nil {
		t.Fatalf("Finalize insufficient: %v", err)
	}
	if ok, err := repo.HasActiveCooldown(ctx, "swarmC", "roleD", "KNOB_A", closedAt.Add(-time.Hour)); err != nil || ok {
		t.Fatalf("insufficient_data must not cool down: %v,%v", ok, err)
	}
}

func canaryLatestWindowUntil(t *testing.T, repo persistence.CostTuningCanaryRepository) {
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Two prior canaries on (swarmL, roleL) with different window_until values.
	c1 := newTestCanary("lw-1", "swarmL", "roleL", "k")
	c1.WindowUntil = base
	c2 := newTestCanary("lw-2", "swarmL", "roleL", "k")
	c2.WindowUntil = base.Add(72 * time.Hour)
	mustOpenCanary(t, repo, c1)
	mustOpenCanary(t, repo, c2)
	// before = base+100h → both qualify; MAX is c2's window_until.
	got, ok, err := repo.LatestWindowUntil(ctx, "swarmL", "roleL", base.Add(100*time.Hour))
	if err != nil || !ok {
		t.Fatalf("LatestWindowUntil = _,%v,%v; want _,true,nil", ok, err)
	}
	if !got.Equal(base.Add(72 * time.Hour)) {
		t.Fatalf("LatestWindowUntil = %v, want %v", got, base.Add(72*time.Hour))
	}
	// before = base-1h → none qualify (window_until > before).
	if _, ok, err := repo.LatestWindowUntil(ctx, "swarmL", "roleL", base.Add(-time.Hour)); err != nil || ok {
		t.Fatalf("LatestWindowUntil(none) = _,%v,%v; want _,false,nil", ok, err)
	}
	// Different (swarm,role) → no prior.
	if _, ok, err := repo.LatestWindowUntil(ctx, "swarmL", "otherRole", base.Add(100*time.Hour)); err != nil || ok {
		t.Fatalf("LatestWindowUntil(other role) = _,%v,%v; want _,false,nil", ok, err)
	}
}

func canaryBaselineRoundTrip(t *testing.T, repo persistence.CostTuningCanaryRepository) {
	ctx := context.Background()
	c := newTestCanary("bl-1", "s", "r", "k")
	c.Baseline = persistence.CanaryBaseline{
		A1Rate: 0.777, A1Samples: 55, A1Sufficient: true, EffCost: 3141.5,
		A2: map[string]persistence.CanaryA2Series{
			"wfA": {Rate: 0.6, Samples: 12, Sufficient: true},
			"wfB": {Rate: 0.0, Samples: 3, Sufficient: false},
		},
	}
	c.WorkflowIDs = []string{"wfA", "wfB"}
	mustOpenCanary(t, repo, c)
	got, err := repo.GetByProposalID(ctx, "bl-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	b := got.Baseline
	if b.A1Rate != 0.777 || b.A1Samples != 55 || !b.A1Sufficient || b.EffCost != 3141.5 {
		t.Fatalf("A1 baseline round-trip mismatch: %+v", b)
	}
	if len(b.A2) != 2 || b.A2["wfA"].Rate != 0.6 || b.A2["wfA"].Samples != 12 || !b.A2["wfA"].Sufficient {
		t.Fatalf("A2 baseline round-trip mismatch: %+v", b.A2)
	}
	if b.A2["wfB"].Sufficient {
		t.Fatalf("wfB should be insufficient")
	}
}
