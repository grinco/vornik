package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/quality"
)

// --- in-memory fakes -------------------------------------------------------

type fakeCanaryRepo struct {
	rows map[string]*persistence.CostTuningCanary
}

func newFakeCanaryRepo() *fakeCanaryRepo {
	return &fakeCanaryRepo{rows: map[string]*persistence.CostTuningCanary{}}
}
func (f *fakeCanaryRepo) Open(_ context.Context, c *persistence.CostTuningCanary) error {
	if _, ok := f.rows[c.ProposalID]; ok {
		return errors.New("duplicate canary")
	}
	cp := *c
	f.rows[c.ProposalID] = &cp
	return nil
}
func (f *fakeCanaryRepo) GetByProposalID(_ context.Context, id string) (*persistence.CostTuningCanary, error) {
	if c, ok := f.rows[id]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}
func (f *fakeCanaryRepo) ListOpen(context.Context) ([]*persistence.CostTuningCanary, error) {
	var out []*persistence.CostTuningCanary
	for _, c := range f.rows {
		if c.Status == persistence.CanaryStatusOpen {
			cp := *c
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (f *fakeCanaryRepo) Finalize(_ context.Context, id, status, reason string, closedAt time.Time) error {
	c, ok := f.rows[id]
	if !ok || c.Status != persistence.CanaryStatusOpen {
		return nil // only matches an open row (idempotent)
	}
	c.Status = status
	c.Reason = reason
	t := closedAt
	c.ClosedAt = &t
	return nil
}
func (f *fakeCanaryRepo) HasOpenForSwarmRole(_ context.Context, swarm, role string) (bool, error) {
	for _, c := range f.rows {
		if c.Status == persistence.CanaryStatusOpen && c.SwarmID == swarm && c.Role == role {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeCanaryRepo) HasActiveCooldown(_ context.Context, swarm, role, knob string, notBefore time.Time) (bool, error) {
	for _, c := range f.rows {
		if c.Status == persistence.CanaryStatusRegressed && c.SwarmID == swarm && c.Role == role &&
			c.Knob == knob && c.ClosedAt != nil && c.ClosedAt.After(notBefore) {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeCanaryRepo) LatestWindowUntil(_ context.Context, swarm, role string, before time.Time) (time.Time, bool, error) {
	var best time.Time
	found := false
	for _, c := range f.rows {
		if c.SwarmID == swarm && c.Role == role && !c.WindowUntil.After(before) {
			if !found || c.WindowUntil.After(best) {
				best = c.WindowUntil
				found = true
			}
		}
	}
	return best, found, nil
}

// CountPassedForKnob / LastApplyActorForKnob are cost-auto-apply trust reads;
// unused by the canary-guard tests but required to satisfy the interface.
func (f *fakeCanaryRepo) CountPassedForKnob(_ context.Context, swarm, role, knob string) (int, error) {
	n := 0
	for _, c := range f.rows {
		if c.SwarmID == swarm && c.Role == role && c.Knob == knob && c.Status == persistence.CanaryStatusPassed {
			n++
		}
	}
	return n, nil
}

func (f *fakeCanaryRepo) LastApplyActorForKnob(context.Context, string, string, string) (string, bool, error) {
	return "", false, nil
}

type guardProposals struct {
	rows               map[string]*persistence.ControlPlaneProposal
	markRegressedErr   error
	markRegressedCalls int
}

func newGuardProposals(ps ...*persistence.ControlPlaneProposal) *guardProposals {
	m := map[string]*persistence.ControlPlaneProposal{}
	for _, p := range ps {
		m[p.ID] = p
	}
	return &guardProposals{rows: m}
}
func (g *guardProposals) Create(context.Context, *persistence.ControlPlaneProposal) error { return nil }
func (g *guardProposals) GetByID(_ context.Context, id string) (*persistence.ControlPlaneProposal, error) {
	if p, ok := g.rows[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}
func (g *guardProposals) List(_ context.Context, f persistence.ProposalListFilter) ([]*persistence.ControlPlaneProposal, error) {
	var out []*persistence.ControlPlaneProposal
	for _, p := range g.rows {
		if len(f.Statuses) > 0 {
			match := false
			for _, s := range f.Statuses {
				if p.Status == s {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}
func (g *guardProposals) SetStatus(context.Context, string, string, string) error     { return nil }
func (g *guardProposals) StagePreApplySnapshot(context.Context, string, string) error { return nil }
func (g *guardProposals) MarkApplied(context.Context, string, string, string) error {
	return nil
}
func (g *guardProposals) MarkRolledBack(_ context.Context, id string) error {
	if p, ok := g.rows[id]; ok {
		p.Status = persistence.ProposalStatusRolledBack
	}
	return nil
}

// RefreshObservation is required by the repository interface (observations,
// 2026-08-10). The canary guard never files or refreshes observations, so this
// fake only needs to satisfy the contract.
func (g *guardProposals) RefreshObservation(_ context.Context, id, rationale, evidence string) error {
	p, ok := g.rows[id]
	if !ok || p.Kind != persistence.ProposalKindObservation {
		return persistence.ErrNotFound
	}
	p.Rationale, p.Evidence = rationale, evidence
	return nil
}

func (g *guardProposals) MarkRegressed(_ context.Context, id, _ string) error {
	g.markRegressedCalls++
	if g.markRegressedErr != nil {
		return g.markRegressedErr
	}
	if p, ok := g.rows[id]; ok {
		p.Status = persistence.ProposalStatusRegressed
	}
	return nil
}

type fakeQuality struct {
	// refresh returns the report for a window; nil => empty report.
	refresh func(from, to time.Time) (quality.Report, error)
}

func (q *fakeQuality) RefreshBetween(_ context.Context, from, to time.Time) (quality.Report, error) {
	if q.refresh == nil {
		return quality.Report{}, nil
	}
	return q.refresh(from, to)
}

// reportWith builds a Report with one A1 (swarm,role) series.
func reportA1(swarm, role string, rate, effcost float64, samples int64) quality.Report {
	return quality.Report{Steps: []quality.ScoredSwarmRole{{
		Swarm: swarm, Role: role,
		TierScore: quality.TierScore{Sufficient: true, QualityRate: rate, EffectiveCostTokens: effcost, SampleCount: samples},
	}}}
}

func appliedProposal(id, swarm, role, knob string, appliedAt time.Time) *persistence.ControlPlaneProposal {
	t := appliedAt
	return &persistence.ControlPlaneProposal{
		ID: id, Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeSwarm,
		Status: persistence.ProposalStatusApplied, ProposedBy: costQualityDetectorProposedBy,
		AppliedAt: &t,
		Evidence: `{"signal":"prompt_token_runaway","change":{"kind":"swarm_role_env","swarm":"` +
			swarm + `","role":"` + role + `","key":"` + knob + `","value":"50000"}}`,
	}
}

func newGuard(q canaryQuality, canaries persistence.CostTuningCanaryRepository, proposals persistence.ProposalRepository) *CanaryGuardWorker {
	return &CanaryGuardWorker{
		Quality: q, Canaries: canaries, Proposals: proposals,
		Enabled:      func() bool { return true },
		SwarmAllowed: func(string) bool { return true },
		IsTradingSwarm: func(s string) bool {
			return s == "trader-swarm"
		},
		MinSamples: 20, A2MinSamples: 10, A2Subwindows: 4,
		MarginA1: 0.05, MarginA2: 0.10, MarginCost: 0.15,
		Window: 168 * time.Hour, MaxCanaryAge: 336 * time.Hour,
	}
}

// --- pure predicate tests --------------------------------------------------

func TestA1Regress(t *testing.T) {
	base := persistence.CanaryBaseline{A1Rate: 0.90, A1Sufficient: true}
	if !a1Regress(base, 0.80, true, 0.05) {
		t.Error("0.80 < 0.90-0.05 should trip")
	}
	if a1Regress(base, 0.86, true, 0.05) {
		t.Error("0.86 is within margin, no trip")
	}
	if a1Regress(persistence.CanaryBaseline{A1Rate: 0.9, A1Sufficient: false}, 0.1, true, 0.05) {
		t.Error("insufficient baseline must not trip")
	}
	if a1Regress(base, 0.1, false, 0.05) {
		t.Error("insufficient post must not trip")
	}
}

func TestEffcostRegress(t *testing.T) {
	base := persistence.CanaryBaseline{EffCost: 1000, A1Sufficient: true}
	if !effcostRegress(base, 1200, true, 0.15) {
		t.Error("1200 > 1000*1.15 should trip")
	}
	if effcostRegress(base, 1100, true, 0.15) {
		t.Error("1100 within margin, no trip")
	}
	// Zero-baseline effcost is SKIPPED, not tripped (§4.1).
	if effcostRegress(persistence.CanaryBaseline{EffCost: 0, A1Sufficient: true}, 9999, true, 0.15) {
		t.Error("zero-baseline effcost must be skipped, not tripped")
	}
	// Insufficient post window must not trip regardless of the cost jump.
	if effcostRegress(base, 5000, false, 0.15) {
		t.Error("insufficient post must not trip effcost")
	}
}

func TestA2WorkflowRegress(t *testing.T) {
	base := persistence.CanaryA2Series{Rate: 0.90, Sufficient: true}
	allBad := []a2Sub{{0.7, true}, {0.7, true}, {0.7, true}, {0.7, true}}
	if !a2WorkflowRegress(base, allBad, 0.10) {
		t.Error("all sub-windows below baseline-margin should trip")
	}
	// A single non-regressed sub-window blocks the trip (sustained rule).
	oneGood := []a2Sub{{0.7, true}, {0.7, true}, {0.85, true}, {0.7, true}}
	if a2WorkflowRegress(base, oneGood, 0.10) {
		t.Error("one healthy sub-window must block the A2 trip")
	}
	// A single insufficient (thin) sub-window blocks the trip.
	oneThin := []a2Sub{{0.7, true}, {0.7, false}, {0.7, true}, {0.7, true}}
	if a2WorkflowRegress(base, oneThin, 0.10) {
		t.Error("one thin sub-window must block the A2 trip")
	}
	// Insufficient baseline never trips.
	if a2WorkflowRegress(persistence.CanaryA2Series{Rate: 0.9, Sufficient: false}, allBad, 0.10) {
		t.Error("insufficient baseline must not trip")
	}
}

func TestParseSwarmRoleEnvChange(t *testing.T) {
	s, r, k, ok := parseSwarmRoleEnvChange(`{"change":{"kind":"swarm_role_env","swarm":"a","role":"b","key":"K","value":"1"}}`)
	if !ok || s != "a" || r != "b" || k != "K" {
		t.Fatalf("parse = %q,%q,%q,%v", s, r, k, ok)
	}
	if _, _, _, ok := parseSwarmRoleEnvChange(`{"change":{"kind":"swarm_role_model","swarm":"a","role":"b"}}`); ok {
		t.Error("wrong kind must not parse")
	}
	if _, _, _, ok := parseSwarmRoleEnvChange(``); ok {
		t.Error("empty must not parse")
	}
	if _, _, _, ok := parseSwarmRoleEnvChange(`{"change":{"kind":"swarm_role_env","swarm":"a","role":"b"}}`); ok {
		t.Error("missing key must not parse")
	}
}

// --- discovery + open ------------------------------------------------------

func TestDiscover_OpensCanaryWithBaseline(t *testing.T) {
	applied := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	p := appliedProposal("cpp-1", "assistant-swarm", "researcher", "VORNIK_STEP_PROMPT_TOKEN_BUDGET", applied)
	canaries := newFakeCanaryRepo()
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		// baseline window ends at appliedAt
		return reportA1("assistant-swarm", "researcher", 0.9, 1000, 100), nil
	}}
	g := newGuard(q, canaries, newGuardProposals(p))
	g.Now = func() time.Time { return applied.Add(time.Hour) }

	g.discover(context.Background())

	c, err := canaries.GetByProposalID(context.Background(), "cpp-1")
	if err != nil {
		t.Fatalf("canary not opened: %v", err)
	}
	if c.Status != persistence.CanaryStatusOpen || c.Knob != "VORNIK_STEP_PROMPT_TOKEN_BUDGET" {
		t.Fatalf("bad canary: %+v", c)
	}
	if !c.BaselineStart.Equal(applied.Add(-168 * time.Hour)) {
		t.Fatalf("baselineStart = %v, want appliedAt-168h", c.BaselineStart)
	}
	if !c.WindowUntil.Equal(applied.Add(168 * time.Hour)) {
		t.Fatalf("windowUntil = %v, want appliedAt+168h", c.WindowUntil)
	}
	if c.Baseline.A1Rate != 0.9 || c.Baseline.A1Samples != 100 || !c.Baseline.A1Sufficient || c.Baseline.EffCost != 1000 {
		t.Fatalf("baseline not captured: %+v", c.Baseline)
	}
}

func TestDiscover_BaselineClampToPriorWindow(t *testing.T) {
	applied := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	// Prior canary on the same (swarm,role) whose window_until is 100h before
	// appliedAt (inside the 168h look-back) — the clamp anchor (I3).
	prior := applied.Add(-100 * time.Hour)
	canaries.rows["old"] = &persistence.CostTuningCanary{
		ProposalID: "old", SwarmID: "assistant-swarm", Role: "researcher",
		WindowUntil: prior, Status: persistence.CanaryStatusPassed,
	}
	p := appliedProposal("cpp-2", "assistant-swarm", "researcher", "K", applied)
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		return reportA1("assistant-swarm", "researcher", 0.9, 1000, 100), nil
	}}
	g := newGuard(q, canaries, newGuardProposals(p))
	g.Now = func() time.Time { return applied.Add(time.Hour) }

	g.discover(context.Background())
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-2")
	if !c.BaselineStart.Equal(prior) {
		t.Fatalf("baselineStart = %v, want clamped to prior window_until %v (I3)", c.BaselineStart, prior)
	}
}

func TestDiscover_CoverageGapOnUnparseableChange(t *testing.T) {
	p := appliedProposal("cpp-x", "s", "r", "K", time.Now())
	p.Evidence = `{"signal":"something","change":{"kind":"swarm_role_model","swarm":"s","role":"r"}}`
	canaries := newFakeCanaryRepo()
	g := newGuard(&fakeQuality{}, canaries, newGuardProposals(p))
	g.discover(context.Background())
	if _, err := canaries.GetByProposalID(context.Background(), "cpp-x"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("no canary should be opened for a non-env change; got %v", err)
	}
}

// --- trip sequence ---------------------------------------------------------

// openTestCanary seeds an open canary directly (bypassing discovery) so a trip
// test controls the baseline + windows precisely.
func openTestCanary(canaries *fakeCanaryRepo, swarm, role string, applied time.Time, base persistence.CanaryBaseline) {
	const id, knob = "cpp-1", "K"
	canaries.rows[id] = &persistence.CostTuningCanary{
		ProposalID: id, SwarmID: swarm, Role: role, Knob: knob,
		AppliedAt: applied, BaselineStart: applied.Add(-168 * time.Hour),
		WindowUntil: applied.Add(168 * time.Hour), Baseline: base,
		Status: persistence.CanaryStatusOpen, OpenedAt: applied,
	}
}

func TestEvaluate_A1RegressionTripsRollback(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	openTestCanary(canaries, "assistant-swarm", "researcher", applied,
		persistence.CanaryBaseline{A1Rate: 0.90, A1Samples: 100, A1Sufficient: true, EffCost: 1000})
	p := appliedProposal("cpp-1", "assistant-swarm", "researcher", "K", applied)
	props := newGuardProposals(p)
	rolledBack := false
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		return reportA1("assistant-swarm", "researcher", 0.70, 1000, 100), nil // big A1 drop
	}}
	g := newGuard(q, canaries, props)
	g.Rollback = func(_ context.Context, id string) error {
		rolledBack = true
		_ = props.MarkRolledBack(context.Background(), id) // engine flips status
		return nil
	}
	g.Now = func() time.Time { return applied.Add(200 * time.Hour) } // past window_until

	g.evaluate(context.Background())

	if !rolledBack {
		t.Fatal("A1 regression must trigger Rollback")
	}
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-1")
	if c.Status != persistence.CanaryStatusRegressed || c.ClosedAt == nil {
		t.Fatalf("canary should be regressed+closed, got %+v", c)
	}
	// Cooldown is active (the regressed canary row IS the cooldown record).
	if ok, _ := canaries.HasActiveCooldown(context.Background(), "assistant-swarm", "researcher", "K", applied); !ok {
		t.Fatal("cooldown must be active after a regressed rollback")
	}
	// Best-effort badge applied (AFTER the canary write).
	if props.rows["cpp-1"].Status != persistence.ProposalStatusRegressed {
		t.Fatalf("proposal badge = %s, want REGRESSED", props.rows["cpp-1"].Status)
	}
	if props.markRegressedCalls != 1 {
		t.Fatalf("MarkRegressed calls = %d, want 1", props.markRegressedCalls)
	}
}

func TestEvaluate_EffCostRegressionTrips(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	openTestCanary(canaries, "s", "r", applied,
		persistence.CanaryBaseline{A1Rate: 0.90, A1Samples: 100, A1Sufficient: true, EffCost: 1000})
	p := appliedProposal("cpp-1", "s", "r", "K", applied)
	props := newGuardProposals(p)
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		return reportA1("s", "r", 0.90, 1500, 100), nil // A1 fine, cost blew up
	}}
	g := newGuard(q, canaries, props)
	tripped := false
	g.Rollback = func(_ context.Context, id string) error {
		tripped = true
		_ = props.MarkRolledBack(context.Background(), id)
		return nil
	}
	g.Now = func() time.Time { return applied.Add(200 * time.Hour) }
	g.evaluate(context.Background())
	if !tripped {
		t.Fatal("effective-cost regression must trip")
	}
}

func TestEvaluate_CleanPasses(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	openTestCanary(canaries, "s", "r", applied,
		persistence.CanaryBaseline{A1Rate: 0.90, A1Samples: 100, A1Sufficient: true, EffCost: 1000})
	props := newGuardProposals(appliedProposal("cpp-1", "s", "r", "K", applied))
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		return reportA1("s", "r", 0.91, 1000, 100), nil // improved, no regression
	}}
	g := newGuard(q, canaries, props)
	g.Rollback = func(context.Context, string) error { t.Fatal("clean canary must NOT roll back"); return nil }
	g.Now = func() time.Time { return applied.Add(200 * time.Hour) } // past window
	g.evaluate(context.Background())
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-1")
	if c.Status != persistence.CanaryStatusPassed {
		t.Fatalf("status = %s, want passed", c.Status)
	}
}

func TestEvaluate_ThinPostBecomesInsufficientDataAndDoesNotBlockDetector(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	openTestCanary(canaries, "s", "r", applied,
		persistence.CanaryBaseline{A1Rate: 0.90, A1Samples: 100, A1Sufficient: true, EffCost: 1000})
	props := newGuardProposals(appliedProposal("cpp-1", "s", "r", "K", applied))
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		return reportA1("s", "r", 0.5, 1000, 5), nil // only 5 samples < floor 20
	}}
	g := newGuard(q, canaries, props)
	g.Rollback = func(context.Context, string) error { t.Fatal("thin data must not roll back"); return nil }
	g.Now = func() time.Time { return applied.Add(200 * time.Hour) } // window over
	g.evaluate(context.Background())
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-1")
	if c.Status != persistence.CanaryStatusInsufficientData {
		t.Fatalf("status = %s, want insufficient_data", c.Status)
	}
	// insufficient_data must NOT block the detector (only `open` blocks, §7).
	if ok, _ := canaries.HasOpenForSwarmRole(context.Background(), "s", "r"); ok {
		t.Fatal("insufficient_data canary must not read as open")
	}
	if ok, _ := canaries.HasActiveCooldown(context.Background(), "s", "r", "K", applied.Add(-time.Hour)); ok {
		t.Fatal("insufficient_data must not open a cooldown")
	}
}

func TestEvaluate_ThinPostBeforeWindowWaits(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	openTestCanary(canaries, "s", "r", applied,
		persistence.CanaryBaseline{A1Rate: 0.90, A1Samples: 100, A1Sufficient: true, EffCost: 1000})
	props := newGuardProposals(appliedProposal("cpp-1", "s", "r", "K", applied))
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		return reportA1("s", "r", 0.5, 1000, 5), nil
	}}
	g := newGuard(q, canaries, props)
	g.Now = func() time.Time { return applied.Add(10 * time.Hour) } // still inside window
	g.evaluate(context.Background())
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-1")
	if c.Status != persistence.CanaryStatusOpen {
		t.Fatalf("thin data before window_until must stay open, got %s", c.Status)
	}
}

// Crash-recovery / F1 operator-manual-rollback: proposal already ROLLED_BACK
// while its canary is open → §4.5 step-1 finalizes regressed + cooldown, without
// re-evaluating data.
func TestEvaluate_CrashRecoveryFinalizesRegressed(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	openTestCanary(canaries, "s", "r", applied,
		persistence.CanaryBaseline{A1Rate: 0.90, A1Samples: 100, A1Sufficient: true, EffCost: 1000})
	p := appliedProposal("cpp-1", "s", "r", "K", applied)
	p.Status = persistence.ProposalStatusRolledBack // already rolled back (crash or operator)
	props := newGuardProposals(p)
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		t.Fatal("recovery branch must NOT re-evaluate data")
		return quality.Report{}, nil
	}}
	g := newGuard(q, canaries, props)
	g.Rollback = func(context.Context, string) error { t.Fatal("must not roll back again"); return nil }
	g.Now = func() time.Time { return applied.Add(10 * time.Hour) }
	g.evaluate(context.Background())
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-1")
	if c.Status != persistence.CanaryStatusRegressed {
		t.Fatalf("status = %s, want regressed (F1 conflation)", c.Status)
	}
	if ok, _ := canaries.HasActiveCooldown(context.Background(), "s", "r", "K", applied); !ok {
		t.Fatal("recovery must open a cooldown")
	}
	if props.rows["cpp-1"].Status != persistence.ProposalStatusRegressed {
		t.Fatalf("badge = %s, want REGRESSED", props.rows["cpp-1"].Status)
	}
}

// C2: if MarkRegressed fails, the cooldown is STILL set (canary regressed) and
// no flip-flop occurs — the badge is best-effort.
func TestEvaluate_MarkRegressedFailureStillSetsCooldown(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	openTestCanary(canaries, "s", "r", applied,
		persistence.CanaryBaseline{A1Rate: 0.90, A1Samples: 100, A1Sufficient: true, EffCost: 1000})
	p := appliedProposal("cpp-1", "s", "r", "K", applied)
	props := newGuardProposals(p)
	props.markRegressedErr = errors.New("db down")
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		return reportA1("s", "r", 0.70, 1000, 100), nil
	}}
	g := newGuard(q, canaries, props)
	g.Rollback = func(_ context.Context, id string) error {
		_ = props.MarkRolledBack(context.Background(), id)
		return nil
	}
	g.Now = func() time.Time { return applied.Add(200 * time.Hour) }
	g.evaluate(context.Background())
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-1")
	if c.Status != persistence.CanaryStatusRegressed || c.ClosedAt == nil {
		t.Fatalf("canary must be regressed+closed despite badge failure, got %+v", c)
	}
	if ok, _ := canaries.HasActiveCooldown(context.Background(), "s", "r", "K", applied); !ok {
		t.Fatal("cooldown must be set even when MarkRegressed fails (C2)")
	}
	// The proposal stays ROLLED_BACK (badge lost, F3) — NOT re-opened.
	if props.rows["cpp-1"].Status != persistence.ProposalStatusRolledBack {
		t.Fatalf("proposal = %s, want ROLLED_BACK (badge lost, no flip-flop)", props.rows["cpp-1"].Status)
	}
}

func TestEvaluate_SupersededNoCooldown(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	openTestCanary(canaries, "s", "r", applied,
		persistence.CanaryBaseline{A1Rate: 0.90, A1Samples: 100, A1Sufficient: true, EffCost: 1000})
	props := newGuardProposals(appliedProposal("cpp-1", "s", "r", "K", applied))
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		return reportA1("s", "r", 0.70, 1000, 100), nil
	}}
	g := newGuard(q, canaries, props)
	g.Rollback = func(context.Context, string) error { return ErrRollbackTargetDrifted }
	g.Now = func() time.Time { return applied.Add(200 * time.Hour) }
	g.evaluate(context.Background())
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-1")
	if c.Status != persistence.CanaryStatusSuperseded {
		t.Fatalf("status = %s, want superseded", c.Status)
	}
	if ok, _ := canaries.HasActiveCooldown(context.Background(), "s", "r", "K", applied); ok {
		t.Fatal("superseded must NOT open a cooldown")
	}
}

func TestEvaluate_ApplyInProgressLeavesOpen(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	openTestCanary(canaries, "s", "r", applied,
		persistence.CanaryBaseline{A1Rate: 0.90, A1Samples: 100, A1Sufficient: true, EffCost: 1000})
	props := newGuardProposals(appliedProposal("cpp-1", "s", "r", "K", applied))
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		return reportA1("s", "r", 0.70, 1000, 100), nil
	}}
	g := newGuard(q, canaries, props)
	g.Rollback = func(context.Context, string) error { return ErrApplyInProgress }
	g.Now = func() time.Time { return applied.Add(200 * time.Hour) }
	g.evaluate(context.Background())
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-1")
	if c.Status != persistence.CanaryStatusOpen {
		t.Fatalf("apply-in-progress must leave the canary OPEN for retry, got %s", c.Status)
	}
}

// F2: an open canary past max_canary_age force-closes to insufficient_data even
// when RefreshBetween errors every tick — the detector is never frozen.
func TestEvaluate_MaxCanaryAgeFailsafe(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	c0 := &persistence.CostTuningCanary{
		ProposalID: "cpp-1", SwarmID: "s", Role: "r", Knob: "K",
		AppliedAt: applied, WindowUntil: applied.Add(168 * time.Hour),
		Status: persistence.CanaryStatusOpen, OpenedAt: applied,
	}
	canaries.rows["cpp-1"] = c0
	props := newGuardProposals(appliedProposal("cpp-1", "s", "r", "K", applied))
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		return quality.Report{}, errors.New("refresh always fails")
	}}
	g := newGuard(q, canaries, props)
	g.MaxCanaryAge = 336 * time.Hour
	g.Now = func() time.Time { return applied.Add(400 * time.Hour) } // > max_canary_age
	g.evaluate(context.Background())
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-1")
	if c.Status != persistence.CanaryStatusInsufficientData {
		t.Fatalf("status = %s, want insufficient_data (failsafe)", c.Status)
	}
	if ok, _ := canaries.HasActiveCooldown(context.Background(), "s", "r", "K", applied); ok {
		t.Fatal("failsafe must NOT open a cooldown")
	}
}

// Kill-switch re-checked at trip time (I7): disabling mid-tick suppresses a
// pending rollback and leaves the canary open.
func TestEvaluate_KillSwitchAtTripTimeSuppresses(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	openTestCanary(canaries, "s", "r", applied,
		persistence.CanaryBaseline{A1Rate: 0.90, A1Samples: 100, A1Sufficient: true, EffCost: 1000})
	props := newGuardProposals(appliedProposal("cpp-1", "s", "r", "K", applied))
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		return reportA1("s", "r", 0.70, 1000, 100), nil
	}}
	g := newGuard(q, canaries, props)
	g.Enabled = func() bool { return false } // braked
	g.Rollback = func(context.Context, string) error { t.Fatal("braked guard must not roll back"); return nil }
	g.Now = func() time.Time { return applied.Add(200 * time.Hour) }
	g.evaluate(context.Background())
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-1")
	if c.Status != persistence.CanaryStatusOpen {
		t.Fatalf("braked canary must stay open, got %s", c.Status)
	}
}

func TestEvaluate_TradingRecheckSuppresses(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	openTestCanary(canaries, "trader-swarm", "r", applied,
		persistence.CanaryBaseline{A1Rate: 0.90, A1Samples: 100, A1Sufficient: true, EffCost: 1000})
	props := newGuardProposals(appliedProposal("cpp-1", "trader-swarm", "r", "K", applied))
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		return reportA1("trader-swarm", "r", 0.70, 1000, 100), nil
	}}
	g := newGuard(q, canaries, props)
	g.Rollback = func(context.Context, string) error { t.Fatal("trading swarm must never roll back"); return nil }
	g.Now = func() time.Time { return applied.Add(200 * time.Hour) }
	g.evaluate(context.Background())
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-1")
	if c.Status != persistence.CanaryStatusOpen {
		t.Fatalf("trading canary must stay open (re-check), got %s", c.Status)
	}
}

// A2-alone trip: A1 fine, but a blast-radius workflow's task tier eroded across
// all sub-windows.
func TestEvaluate_A2AloneTrips(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	c0 := &persistence.CostTuningCanary{
		ProposalID: "cpp-1", SwarmID: "s", Role: "r", Knob: "K",
		AppliedAt: applied, BaselineStart: applied.Add(-168 * time.Hour),
		WindowUntil: applied.Add(168 * time.Hour),
		WorkflowIDs: []string{"wf1"},
		Baseline: persistence.CanaryBaseline{
			A1Rate: 0.90, A1Samples: 100, A1Sufficient: true, EffCost: 1000,
			A2: map[string]persistence.CanaryA2Series{"wf1": {Rate: 0.90, Samples: 40, Sufficient: true}},
		},
		Status: persistence.CanaryStatusOpen, OpenedAt: applied,
	}
	canaries.rows["cpp-1"] = c0
	props := newGuardProposals(appliedProposal("cpp-1", "s", "r", "K", applied))
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		// A1 healthy in every call; A2 wf1 eroded to 0.70 (< 0.90-0.10) in all
		// sub-windows.
		return quality.Report{
			Steps: []quality.ScoredSwarmRole{{Swarm: "s", Role: "r", TierScore: quality.TierScore{Sufficient: true, QualityRate: 0.90, EffectiveCostTokens: 1000, SampleCount: 100}}},
			Tasks: []quality.ScoredSwarmWorkflow{{Swarm: "s", Workflow: "wf1", TierScore: quality.TierScore{Sufficient: true, QualityRate: 0.70, EffectiveCostTokens: 1, SampleCount: 40}}},
		}, nil
	}}
	g := newGuard(q, canaries, props)
	tripped := false
	g.Rollback = func(_ context.Context, id string) error {
		tripped = true
		_ = props.MarkRolledBack(context.Background(), id)
		return nil
	}
	g.Now = func() time.Time { return applied.Add(200 * time.Hour) }
	g.evaluate(context.Background())
	if !tripped {
		t.Fatal("A2-alone erosion should trip")
	}
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-1")
	if c.Status != persistence.CanaryStatusRegressed {
		t.Fatalf("status = %s, want regressed", c.Status)
	}
}
