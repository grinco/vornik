package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/quality"
)

type fakeProposals struct {
	created []*persistence.ControlPlaneProposal
}

func (f *fakeProposals) Create(_ context.Context, p *persistence.ControlPlaneProposal) error {
	f.created = append(f.created, p)
	return nil
}
func (f *fakeProposals) List(context.Context, persistence.ProposalListFilter) ([]*persistence.ControlPlaneProposal, error) {
	return nil, nil
}
func (f *fakeProposals) GetByID(context.Context, string) (*persistence.ControlPlaneProposal, error) {
	return nil, nil
}
func (f *fakeProposals) SetStatus(context.Context, string, string, string) error     { return nil }
func (f *fakeProposals) MarkApplied(context.Context, string, string, string) error   { return nil }
func (f *fakeProposals) StagePreApplySnapshot(context.Context, string, string) error { return nil }
func (f *fakeProposals) MarkRolledBack(context.Context, string) error                { return nil }
func (f *fakeProposals) MarkRegressed(context.Context, string, string) error         { return nil }

type fakeQual struct{ rep quality.Report }

func (f fakeQual) Refresh(context.Context, time.Time) (quality.Report, error) { return f.rep, nil }

type fakePct struct{ rows []quality.SwarmRolePercentile }

func (f fakePct) RolePercentiles(context.Context, time.Time, []string, []string) ([]quality.SwarmRolePercentile, error) {
	return f.rows, nil
}

const cqSwarmFixture = `swarmId: "assistant-swarm"
roles:
    - name: "researcher"
      runtime:
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "25"
`

func newCQWorker(fp *fakeProposals, sufficient bool, enabled bool) *CostQualityWorker {
	return &CostQualityWorker{
		Quality: fakeQual{rep: quality.Report{Steps: []quality.ScoredSwarmRole{
			{Swarm: "assistant-swarm", Role: "researcher", TierScore: quality.TierScore{Sufficient: sufficient}},
		}}},
		Percentiles: fakePct{rows: []quality.SwarmRolePercentile{
			{Swarm: "assistant-swarm", Role: "researcher", N: 800, P95: 500_000, P99: 2_000_000},
		}},
		Actionize: testActionizer(map[string]string{"configs/swarms/assistant-swarm.md": cqSwarmFixture}),
		Proposals: fp,
		SwarmMap:  func() ([]string, []string) { return []string{"assistant"}, []string{"assistant-swarm"} },
		Enabled:   enabled,
		Logger:    zerolog.Nop(),
	}
}

// End-to-end tick: a runaway tail on a quality-sufficient locus files ONE
// applyable config DRAFT proposal that clamps the prompt-token budget.
func TestCostQualityWorkerFilesApplyableProposalOnRunaway(t *testing.T) {
	fp := &fakeProposals{}
	newCQWorker(fp, true, true).tick(context.Background())

	if len(fp.created) != 1 {
		t.Fatalf("created %d proposals, want 1", len(fp.created))
	}
	p := fp.created[0]
	if p.Kind != persistence.ProposalKindConfig {
		t.Errorf("Kind = %q, want config", p.Kind)
	}
	if p.BlastRadius != persistence.ProposalScopeSwarm {
		t.Errorf("BlastRadius = %q, want swarm", p.BlastRadius)
	}
	if p.ApplyTarget != "configs/swarms/assistant-swarm.md" {
		t.Errorf("ApplyTarget = %q", p.ApplyTarget)
	}
	if p.Title != costQualityBudgetTitle("assistant-swarm", "researcher") {
		t.Errorf("Title = %q", p.Title)
	}
	// proposed = p95×1.2 = 600000, inserted into the researcher envVars
	if want := `VORNIK_STEP_PROMPT_TOKEN_BUDGET: "600000"`; !strings.Contains(p.ApplyContent, want) {
		t.Errorf("ApplyContent missing %q:\n%s", want, p.ApplyContent)
	}
}

// Insufficient A1 quality → never proposes (can't guarantee the cut is safe).
func TestCostQualityWorkerSkipsWhenQualityInsufficient(t *testing.T) {
	fp := &fakeProposals{}
	newCQWorker(fp, false, true).tick(context.Background())
	if len(fp.created) != 0 {
		t.Errorf("created %d proposals, want 0 (quality insufficient)", len(fp.created))
	}
}

// Disabled → no work (default-off gate).
func TestCostQualityWorkerDisabledIsNoop(t *testing.T) {
	fp := &fakeProposals{}
	newCQWorker(fp, true, false).tick(context.Background())
	if len(fp.created) != 0 {
		t.Errorf("created %d proposals, want 0 (disabled)", len(fp.created))
	}
}

// Detector skip (design §7): an OPEN canary on (swarm,role) suppresses a fresh
// proposal.
func TestCostQualityWorkerSkipsWhenCanaryOpen(t *testing.T) {
	fp := &fakeProposals{}
	w := newCQWorker(fp, true, true)
	canaries := newFakeCanaryRepo()
	canaries.rows["existing"] = &persistence.CostTuningCanary{
		ProposalID: "existing", SwarmID: "assistant-swarm", Role: "researcher",
		Knob: promptTokenBudgetEnvKey, Status: persistence.CanaryStatusOpen,
	}
	w.Canaries = canaries
	w.CooldownDuration = 336 * time.Hour
	w.tick(context.Background())
	if len(fp.created) != 0 {
		t.Fatalf("open canary must suppress proposals, got %d", len(fp.created))
	}
}

// Detector skip (design §7): an active cooldown on (swarm,role,knob) suppresses.
func TestCostQualityWorkerSkipsWhenCooldownActive(t *testing.T) {
	fp := &fakeProposals{}
	w := newCQWorker(fp, true, true)
	canaries := newFakeCanaryRepo()
	closed := time.Now()
	canaries.rows["regr"] = &persistence.CostTuningCanary{
		ProposalID: "regr", SwarmID: "assistant-swarm", Role: "researcher",
		Knob: promptTokenBudgetEnvKey, Status: persistence.CanaryStatusRegressed, ClosedAt: &closed,
	}
	w.Canaries = canaries
	w.CooldownDuration = 336 * time.Hour
	w.tick(context.Background())
	if len(fp.created) != 0 {
		t.Fatalf("active cooldown must suppress proposals, got %d", len(fp.created))
	}
}

// A cooldown on a DIFFERENT knob does NOT suppress this knob (§7: keyed
// swarm,role,knob).
func TestCostQualityWorkerCooldownDifferentKnobUnaffected(t *testing.T) {
	fp := &fakeProposals{}
	w := newCQWorker(fp, true, true)
	canaries := newFakeCanaryRepo()
	closed := time.Now()
	canaries.rows["regr"] = &persistence.CostTuningCanary{
		ProposalID: "regr", SwarmID: "assistant-swarm", Role: "researcher",
		Knob: "SOME_OTHER_KNOB", Status: persistence.CanaryStatusRegressed, ClosedAt: &closed,
	}
	w.Canaries = canaries
	w.CooldownDuration = 336 * time.Hour
	w.tick(context.Background())
	if len(fp.created) != 1 {
		t.Fatalf("a different-knob cooldown must NOT suppress this knob, got %d", len(fp.created))
	}
}

// insufficient_data / passed canaries do NOT block the detector (§7).
func TestCostQualityWorkerInsufficientDataDoesNotBlock(t *testing.T) {
	fp := &fakeProposals{}
	w := newCQWorker(fp, true, true)
	canaries := newFakeCanaryRepo()
	closed := time.Now()
	canaries.rows["insuf"] = &persistence.CostTuningCanary{
		ProposalID: "insuf", SwarmID: "assistant-swarm", Role: "researcher",
		Knob: promptTokenBudgetEnvKey, Status: persistence.CanaryStatusInsufficientData, ClosedAt: &closed,
	}
	w.Canaries = canaries
	w.CooldownDuration = 336 * time.Hour
	w.tick(context.Background())
	if len(fp.created) != 1 {
		t.Fatalf("insufficient_data must not block; got %d proposals", len(fp.created))
	}
}
