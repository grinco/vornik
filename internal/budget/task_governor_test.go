package budget

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// stubTaskSpend is a TaskSpendRepo double returning a fixed spend / error.
type stubTaskSpend struct {
	spent float64
	err   error
	calls int
}

func (s *stubTaskSpend) SumCostByTask(_ context.Context, _ string) (float64, error) {
	s.calls++
	return s.spent, s.err
}

func fptr(v float64) *float64 { return &v }

func projWithTaskBudget(def, maxUSD, soft float64) *registry.Project {
	return &registry.Project{ID: "p", Budget: registry.ProjectBudget{
		DefaultTaskBudgetUSD: def,
		MaxTaskBudgetUSD:     maxUSD,
		TaskSoftFraction:     soft,
	}}
}

func TestEffectiveTaskBudgetUSD_Ladder(t *testing.T) {
	proj := projWithTaskBudget(3.0, 0, 0)
	// override wins when non-nil & positive
	if got := EffectiveTaskBudgetUSD(proj, fptr(7.5)); got != 7.5 {
		t.Fatalf("override should win: got %v", got)
	}
	// nil override falls back to project default
	if got := EffectiveTaskBudgetUSD(proj, nil); got != 3.0 {
		t.Fatalf("nil override → project default: got %v", got)
	}
	// no project default + nil override → 0 (disabled)
	if got := EffectiveTaskBudgetUSD(projWithTaskBudget(0, 0, 0), nil); got != 0 {
		t.Fatalf("no default → 0: got %v", got)
	}
	// nil project → 0
	if got := EffectiveTaskBudgetUSD(nil, nil); got != 0 {
		t.Fatalf("nil project → 0: got %v", got)
	}
}

func TestTaskGovernor_BudgetZeroAlwaysOK(t *testing.T) {
	repo := &stubTaskSpend{spent: 1000} // huge spend, but no budget configured
	g := NewTaskGovernor(repo)
	dec, err := g.Check(context.Background(), projWithTaskBudget(0, 0, 0), &persistence.Task{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if dec.Tier != TierOK {
		t.Fatalf("budget 0 must be TierOK, got %v", dec.Tier)
	}
	if repo.calls != 0 {
		t.Fatalf("disabled governor must not read spend, calls=%d", repo.calls)
	}
}

func TestTaskGovernor_TierMatrix(t *testing.T) {
	proj := projWithTaskBudget(10.0, 0, 0) // soft defaults to 0.80
	cases := []struct {
		name  string
		spent float64
		want  TaskBudgetTier
	}{
		{"well under", 1.0, TierOK},
		{"just under soft", 7.99, TierOK},
		{"at soft", 8.0, TierSoft},
		{"between soft and hard", 9.5, TierSoft},
		{"just under hard", 9.99, TierSoft},
		{"at hard", 10.0, TierHard},
		{"over hard", 25.0, TierHard},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewTaskGovernor(&stubTaskSpend{spent: tc.spent})
			dec, err := g.Check(context.Background(), proj, &persistence.Task{ID: "t"})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if dec.Tier != tc.want {
				t.Fatalf("spent %v: tier=%v want %v (fraction=%v)", tc.spent, dec.Tier, tc.want, dec.Fraction)
			}
		})
	}
}

func TestTaskGovernor_SoftFractionRespected(t *testing.T) {
	proj := projWithTaskBudget(10.0, 0, 0.5) // soft at 50%
	g := NewTaskGovernor(&stubTaskSpend{spent: 5.0})
	dec, _ := g.Check(context.Background(), proj, &persistence.Task{ID: "t"})
	if dec.Tier != TierSoft {
		t.Fatalf("custom soft fraction 0.5 at spend 5/10 → Soft, got %v", dec.Tier)
	}
	// Below the custom fraction stays OK.
	g2 := NewTaskGovernor(&stubTaskSpend{spent: 4.9})
	dec2, _ := g2.Check(context.Background(), proj, &persistence.Task{ID: "t"})
	if dec2.Tier != TierOK {
		t.Fatalf("4.9/10 under 0.5 → OK, got %v", dec2.Tier)
	}
}

func TestTaskGovernor_OverrideBudgetUsed(t *testing.T) {
	proj := projWithTaskBudget(2.0, 0, 0) // project default 2
	// Task override of 100: spend 5 is well under → OK even though > default.
	g := NewTaskGovernor(&stubTaskSpend{spent: 5.0})
	dec, _ := g.Check(context.Background(), proj, &persistence.Task{ID: "t", BudgetUSD: fptr(100)})
	if dec.Tier != TierOK {
		t.Fatalf("override 100 spend 5 → OK, got %v (budget %v)", dec.Tier, dec.BudgetUSD)
	}
	if dec.BudgetUSD != 100 {
		t.Fatalf("decision must reflect override budget, got %v", dec.BudgetUSD)
	}
}

func TestTaskGovernor_DBErrorSurfaced(t *testing.T) {
	sentinel := errors.New("db down")
	g := NewTaskGovernor(&stubTaskSpend{err: sentinel})
	_, err := g.Check(context.Background(), projWithTaskBudget(10, 0, 0), &persistence.Task{ID: "t"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected repo error surfaced, got %v", err)
	}
}

func TestClampTaskBudget(t *testing.T) {
	proj := projWithTaskBudget(0, 20.0, 0) // max 20
	if got := ClampTaskBudget(proj, 50); got != 20 {
		t.Fatalf("clamp to max: got %v", got)
	}
	if got := ClampTaskBudget(proj, 5); got != 5 {
		t.Fatalf("under max unchanged: got %v", got)
	}
	// no max → unchanged
	if got := ClampTaskBudget(projWithTaskBudget(0, 0, 0), 999); got != 999 {
		t.Fatalf("no max → unchanged: got %v", got)
	}
	if got := ClampTaskBudget(nil, 999); got != 999 {
		t.Fatalf("nil project → unchanged: got %v", got)
	}
}

func TestTaskGovernor_NilSafe(t *testing.T) {
	var g *TaskGovernor
	if dec, err := g.Check(context.Background(), nil, &persistence.Task{ID: "t"}); err != nil || dec.Tier != TierOK {
		t.Fatalf("nil governor must be OK/no-err, got %v %v", dec.Tier, err)
	}
	g2 := NewTaskGovernor(&stubTaskSpend{})
	if dec, err := g2.Check(context.Background(), projWithTaskBudget(5, 0, 0), nil); err != nil || dec.Tier != TierOK {
		t.Fatalf("nil task must be OK/no-err, got %v %v", dec.Tier, err)
	}
}
