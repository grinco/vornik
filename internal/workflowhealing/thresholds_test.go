package workflowhealing

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// fakeOverrideRepo is a minimal in-memory
// HealingTriggerOverrideRepository for the resolver tests.
type fakeOverrideRepo struct {
	rows   map[string]*persistence.HealingTriggerOverride
	getErr error
}

func newFakeOverrideRepo() *fakeOverrideRepo {
	return &fakeOverrideRepo{rows: map[string]*persistence.HealingTriggerOverride{}}
}

func okey(p, w string, c persistence.HealingTriggerClass) string {
	return p + "|" + w + "|" + string(c)
}

func (f *fakeOverrideRepo) Upsert(ctx context.Context, o *persistence.HealingTriggerOverride) error {
	f.rows[okey(o.ProjectID, o.WorkflowID, o.TriggerClass)] = o
	return nil
}

func (f *fakeOverrideRepo) Get(ctx context.Context, projectID, workflowID string, class persistence.HealingTriggerClass) (*persistence.HealingTriggerOverride, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	o, ok := f.rows[okey(projectID, workflowID, class)]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return o, nil
}

func (f *fakeOverrideRepo) List(ctx context.Context, pageSize int) ([]*persistence.HealingTriggerOverride, error) {
	return nil, nil
}

func (f *fakeOverrideRepo) Delete(ctx context.Context, projectID, workflowID string, class persistence.HealingTriggerClass) error {
	return nil
}

func fptr(v float64) *float64 { return &v }

func TestResolve_NilRepoReturnsDefaults(t *testing.T) {
	r := NewGateThresholdResolver(nil, zerolog.Nop())
	g := r.Resolve(context.Background(), "p", "w", persistence.HealingTriggerCostRegression)
	if g.IsZero() {
		t.Fatal("resolver must return configured defaults, not the zero value")
	}
	if g.SuccessUplift != DefaultGateThresholds().SuccessUplift {
		t.Errorf("SuccessUplift = %f, want default %f", g.SuccessUplift, DefaultGateThresholds().SuccessUplift)
	}
}

func TestResolve_NilResolverReturnsDefaults(t *testing.T) {
	var r *GateThresholdResolver
	g := r.Resolve(context.Background(), "p", "w", persistence.HealingTriggerCostRegression)
	if g.IsZero() {
		t.Fatal("nil resolver must still return configured defaults")
	}
}

func TestResolve_NoRowReturnsDefaults(t *testing.T) {
	r := NewGateThresholdResolver(newFakeOverrideRepo(), zerolog.Nop())
	g := r.Resolve(context.Background(), "p", "w", persistence.HealingTriggerFailureRateSpike)
	if g.SuccessUplift != DefaultGateThresholds().SuccessUplift {
		t.Errorf("no-row SuccessUplift = %f, want default", g.SuccessUplift)
	}
	if g.CostTolerancePct != DefaultGateThresholds().CostTolerancePct {
		t.Errorf("no-row CostTolerancePct = %f, want default", g.CostTolerancePct)
	}
}

func TestResolve_ThresholdOverrideBecomesSuccessUplift(t *testing.T) {
	repo := newFakeOverrideRepo()
	_ = repo.Upsert(context.Background(), &persistence.HealingTriggerOverride{
		ProjectID:         "p",
		WorkflowID:        "w",
		TriggerClass:      persistence.HealingTriggerFailureRateSpike,
		ThresholdOverride: fptr(0.50),
	})
	r := NewGateThresholdResolver(repo, zerolog.Nop())
	g := r.Resolve(context.Background(), "p", "w", persistence.HealingTriggerFailureRateSpike)
	if g.SuccessUplift != 0.50 {
		t.Errorf("SuccessUplift = %f, want 0.50 (from override)", g.SuccessUplift)
	}
	if g.IsZero() {
		t.Error("override-derived thresholds must report configured")
	}
	// Cost/latency keep defaults (override row doesn't carry them).
	if g.CostTolerancePct != DefaultGateThresholds().CostTolerancePct {
		t.Errorf("CostTolerancePct = %f, want unchanged default", g.CostTolerancePct)
	}
}

func TestResolve_NilThresholdOverrideKeepsDefault(t *testing.T) {
	repo := newFakeOverrideRepo()
	// Row exists but only mutes — no threshold override.
	_ = repo.Upsert(context.Background(), &persistence.HealingTriggerOverride{
		ProjectID:    "p",
		WorkflowID:   "w",
		TriggerClass: persistence.HealingTriggerCostRegression,
	})
	r := NewGateThresholdResolver(repo, zerolog.Nop())
	g := r.Resolve(context.Background(), "p", "w", persistence.HealingTriggerCostRegression)
	if g.SuccessUplift != DefaultGateThresholds().SuccessUplift {
		t.Errorf("SuccessUplift = %f, want default (nil override)", g.SuccessUplift)
	}
}

func TestResolve_RepoErrorFallsBackToDefaults(t *testing.T) {
	repo := newFakeOverrideRepo()
	repo.getErr = errors.New("db down")
	r := NewGateThresholdResolver(repo, zerolog.Nop())
	g := r.Resolve(context.Background(), "p", "w", persistence.HealingTriggerCostRegression)
	// Must fail CLOSED to the stricter defaults, never fail-open.
	if g.SuccessUplift != DefaultGateThresholds().SuccessUplift {
		t.Errorf("on repo error SuccessUplift = %f, want default", g.SuccessUplift)
	}
	if g.IsZero() {
		t.Error("on repo error must still return configured defaults")
	}
}

// End-to-end: a resolved override that demands a +50% uplift fails a
// candidate that only improves +25%, proving threshold-from-overrides
// actually feeds the gate.
func TestResolve_FeedsGate(t *testing.T) {
	repo := newFakeOverrideRepo()
	_ = repo.Upsert(context.Background(), &persistence.HealingTriggerOverride{
		ProjectID:         "p",
		WorkflowID:        "w",
		TriggerClass:      persistence.HealingTriggerFailureRateSpike,
		ThresholdOverride: fptr(0.50),
	})
	g := NewGateThresholdResolver(repo, zerolog.Nop()).
		Resolve(context.Background(), "p", "w", persistence.HealingTriggerFailureRateSpike)

	base := TrialSummary{Runs: 4, Successes: 2} // 0.50
	cand := TrialSummary{Runs: 4, Successes: 3} // 0.75 → +0.25 < 0.50
	if sc := g.Evaluate(base, cand, "low"); sc.Verdict != string(persistence.HealingTrialFailed) {
		t.Fatalf("verdict = %q, want failed (uplift below the 50%% override)", sc.Verdict)
	}
}
