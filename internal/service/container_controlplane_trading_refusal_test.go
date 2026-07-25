package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/storage"
)

// TestIsTradingSwarm pins the single trading classifier used by BOTH the
// detector-side exclusion (costTuningSwarmMap) and the applier-side refusal
// (injected into the Actionizer). Regression: review-20260721-a7bf #6.
func TestIsTradingSwarm(t *testing.T) {
	trading := []string{"ibkr-trader-swarm", "trading-swarm", "broker-swarm", "TRADER", "MyBrokerX"}
	for _, s := range trading {
		if !isTradingSwarm(s) {
			t.Errorf("%q should classify as trading", s)
		}
	}
	benign := []string{"assistant-swarm", "vornik-marketing", "dev-swarm", ""}
	for _, s := range benign {
		if isTradingSwarm(s) {
			t.Errorf("%q should NOT classify as trading", s)
		}
	}
}

// TestNewActionizerWiresTradingClassifier asserts newActionizer injects the REAL
// isTradingSwarm (not nil), so the applier-side refusal shares the detector's
// single classifier and refuses a cost-quality-detector proposal targeting a
// trading swarm. This is the load-bearing wiring for the fail-open safety
// argument (review-20260724-3386 #2): a forgotten field here would be caught.
func TestNewActionizerWiresTradingClassifier(t *testing.T) {
	c := &Container{ConfigPath: "/etc/vornik/config.yaml"}
	a := c.newActionizer()
	if a == nil {
		t.Fatal("newActionizer returned nil")
	}
	if a.IsTradingSwarm == nil {
		t.Fatal("newActionizer must wire IsTradingSwarm (nil = applier-side refusal silently disabled)")
	}
	if !a.IsTradingSwarm("ibkr-trader-swarm") {
		t.Error("injected classifier must recognise a trading swarm")
	}
	ev := `{"change":{"kind":"swarm_role_env","swarm":"ibkr-trader-swarm","role":"strategist","key":"K","value":"v"}}`
	if err := a.RefuseTradingTarget("cost-quality-detector", ev); !errors.Is(err, controlplane.ErrTradingSwarmRefused) {
		t.Errorf("wired actionizer must refuse a detector proposal to a trading swarm, got %v", err)
	}
	// A non-trading target still passes through the wired classifier.
	evOK := `{"change":{"kind":"swarm_role_env","swarm":"assistant-swarm","role":"researcher","key":"K","value":"v"}}`
	if err := a.RefuseTradingTarget("cost-quality-detector", evOK); err != nil {
		t.Errorf("wired actionizer must pass a detector proposal to a non-trading swarm, got %v", err)
	}
}

// TestProposalApplierValidateChangeRefusesTrading drives the REAL
// newProposalApplier() ValidateChange closure end-to-end (not the actionizer
// directly, not a stubbed hook), so a regression that deletes the
// RefuseTradingTarget call OR reorders it behind RevalidateChange is caught.
// The trading swarm file is absent under the temp config dir, so RevalidateChange
// would ALSO error — asserting the trading sentinel therefore pins both the
// wiring (review-20260724-24a2 F1) and the refuse-before-revalidate ordering (F2).
func TestProposalApplierValidateChangeRefusesTrading(t *testing.T) {
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	c := &Container{
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		repos:      &storage.Repositories{Proposals: sqlite.NewProposalRepository(db.DB)},
	}
	engine := c.newProposalApplier()
	if engine == nil || engine.ValidateChange == nil {
		t.Fatal("newProposalApplier must build an engine with a ValidateChange hook")
	}
	ctx := context.Background()
	tradingEv := `{"change":{"kind":"swarm_role_env","swarm":"ibkr-trader-swarm","role":"strategist","key":"K","value":"v"}}`

	// Detector proposal to a trading swarm → the closure must return the trading
	// sentinel (refusal fires before RevalidateChange, which would also error on
	// the missing swarm file).
	det := &persistence.ControlPlaneProposal{ProjectID: "x", ProposedBy: "cost-quality-detector", Evidence: tradingEv}
	if err := engine.ValidateChange(ctx, det); !errors.Is(err, controlplane.ErrTradingSwarmRefused) {
		t.Errorf("closure must refuse a detector proposal to a trading swarm with the sentinel, got %v", err)
	}
	// Operator-authored proposal to the same swarm → NOT refused by the trading
	// guard; it falls through to RevalidateChange (which errors on the missing
	// file, but that error is NOT the trading sentinel).
	op := &persistence.ControlPlaneProposal{ProjectID: "x", ProposedBy: "operator", Evidence: tradingEv}
	if err := engine.ValidateChange(ctx, op); errors.Is(err, controlplane.ErrTradingSwarmRefused) {
		t.Errorf("operator proposal must not be blocked by the trading guard, got %v", err)
	}
}
