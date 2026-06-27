package service

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowhealing"
)

// ---- fakes for the two repos the adapter + resolver touch -------------

type fakeTriggersRepo struct {
	trig *persistence.HealingTrigger
}

func (f *fakeTriggersRepo) Insert(context.Context, *persistence.HealingTrigger) error { return nil }
func (f *fakeTriggersRepo) Get(_ context.Context, _ string) (*persistence.HealingTrigger, error) {
	if f.trig == nil {
		return nil, persistence.ErrNotFound
	}
	return f.trig, nil
}
func (f *fakeTriggersRepo) List(context.Context, persistence.HealingTriggerListFilter) ([]*persistence.HealingTrigger, error) {
	return nil, nil
}
func (f *fakeTriggersRepo) Dismiss(context.Context, string) error               { return nil }
func (f *fakeTriggersRepo) MarkGenerated(context.Context, string, string) error { return nil }

type fakeOverridesRepo struct {
	ov       *persistence.HealingTriggerOverride
	gotClass persistence.HealingTriggerClass
}

func (f *fakeOverridesRepo) Upsert(context.Context, *persistence.HealingTriggerOverride) error {
	return nil
}
func (f *fakeOverridesRepo) Get(_ context.Context, _, _ string, class persistence.HealingTriggerClass) (*persistence.HealingTriggerOverride, error) {
	f.gotClass = class
	if f.ov == nil {
		return nil, persistence.ErrNotFound
	}
	return f.ov, nil
}
func (f *fakeOverridesRepo) List(context.Context, int) ([]*persistence.HealingTriggerOverride, error) {
	return nil, nil
}
func (f *fakeOverridesRepo) Delete(context.Context, string, string, persistence.HealingTriggerClass) error {
	return nil
}

// ---- tests ------------------------------------------------------------

// The adapter must recover the trigger CLASS from the candidate's
// trigger and feed it to the resolver so the override is found.
func TestHealingGateResolverAdapter_ResolvesClassFromTrigger(t *testing.T) {
	uplift := 0.42
	ovRepo := &fakeOverridesRepo{ov: &persistence.HealingTriggerOverride{ThresholdOverride: &uplift}}
	trRepo := &fakeTriggersRepo{trig: &persistence.HealingTrigger{
		ID:           "trg1",
		TriggerClass: persistence.HealingTriggerFailureRateSpike,
	}}
	resolver := workflowhealing.NewGateThresholdResolver(ovRepo, zerolog.Nop())
	adapter := newHealingGateResolverAdapter(resolver, trRepo)

	g := adapter.ResolveForCandidate(context.Background(), &persistence.HealingCandidate{
		ProjectID: "p", WorkflowID: "w", TriggerID: "trg1",
	})
	if g.SuccessUplift != uplift {
		t.Errorf("SuccessUplift = %v, want %v (sourced from the override)", g.SuccessUplift, uplift)
	}
	// The override lookup must have used the trigger's class.
	if ovRepo.gotClass != persistence.HealingTriggerFailureRateSpike {
		t.Errorf("override looked up with class %q, want failure_rate_spike", ovRepo.gotClass)
	}
}

// No override row → conservative defaults.
func TestHealingGateResolverAdapter_NoOverrideDefaults(t *testing.T) {
	ovRepo := &fakeOverridesRepo{ov: nil} // Get → ErrNotFound
	trRepo := &fakeTriggersRepo{trig: &persistence.HealingTrigger{ID: "trg1", TriggerClass: persistence.HealingTriggerCostRegression}}
	adapter := newHealingGateResolverAdapter(workflowhealing.NewGateThresholdResolver(ovRepo, zerolog.Nop()), trRepo)

	g := adapter.ResolveForCandidate(context.Background(), &persistence.HealingCandidate{ProjectID: "p", WorkflowID: "w", TriggerID: "trg1"})
	if g != workflowhealing.DefaultGateThresholds() {
		t.Errorf("no override should yield DefaultGateThresholds; got %+v", g)
	}
}

func TestNewHealingGateResolverAdapter_NilResolver(t *testing.T) {
	if newHealingGateResolverAdapter(nil, &fakeTriggersRepo{}) != nil {
		t.Error("nil resolver should yield a nil adapter (runner uses its static gate)")
	}
}

// Nil candidate → defaults, no panic.
func TestHealingGateResolverAdapter_NilCandidate(t *testing.T) {
	adapter := newHealingGateResolverAdapter(workflowhealing.NewGateThresholdResolver(&fakeOverridesRepo{}, zerolog.Nop()), &fakeTriggersRepo{})
	if g := adapter.ResolveForCandidate(context.Background(), nil); g != workflowhealing.DefaultGateThresholds() {
		t.Errorf("nil candidate should yield defaults; got %+v", g)
	}
}
