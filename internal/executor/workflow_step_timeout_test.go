package executor

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/toolbudget"
)

// applyStepTimeoutBudget couples a step's wall-clock to the same complexity
// factor as its iteration budget (https://docs.vornik.io).
// It runs BEFORE the counterfactual cap so "override only lowers" holds against
// the scaled value, and exempts warm-pool roles (which run on their static
// iteration budget — scaling their time down would cause spurious timeouts).

func budgetCfg() toolbudget.Config {
	return toolbudget.Config{
		Enabled:           true,
		Factors:           map[toolbudget.Tier]float64{toolbudget.TierTrivial: 0.25, toolbudget.TierStandard: 0.5, toolbudget.TierComplex: 1.0, toolbudget.TierOpenEnded: 2.0},
		MaxFactor:         2.0,
		AutonomyMaxFactor: 1.5,
		MinStepTimeout:    5 * time.Minute,
	}
}

func TestApplyStepTimeoutBudget_ScalesEphemeralRole(t *testing.T) {
	role := &registry.SwarmRole{Name: "researcher", RuntimePolicy: "ephemeral"}
	// complex (1.0) leaves 45m unchanged; standard (0.5) halves it.
	if got := applyStepTimeoutBudget(45*time.Minute, role, "complex", false, budgetCfg()); got != 45*time.Minute {
		t.Errorf("complex = %s; want 45m", got)
	}
	if got := applyStepTimeoutBudget(45*time.Minute, role, "standard", false, budgetCfg()); got != 22*time.Minute+30*time.Second {
		t.Errorf("standard = %s; want 22m30s", got)
	}
}

func TestApplyStepTimeoutBudget_WarmRoleExempt(t *testing.T) {
	role := &registry.SwarmRole{Name: "lead", RuntimePolicy: "warm"}
	// Warm roles keep BOTH budgets static — no timeout scaling even when
	// the feature is enabled and the tier would downscale.
	if got := applyStepTimeoutBudget(45*time.Minute, role, "trivial", false, budgetCfg()); got != 45*time.Minute {
		t.Errorf("warm role = %s; want native 45m (exempt)", got)
	}
}

func TestApplyStepTimeoutBudget_DisabledIsPassthrough(t *testing.T) {
	role := &registry.SwarmRole{Name: "researcher", RuntimePolicy: "ephemeral"}
	cfg := budgetCfg()
	cfg.Enabled = false
	if got := applyStepTimeoutBudget(45*time.Minute, role, "trivial", false, cfg); got != 45*time.Minute {
		t.Errorf("disabled = %s; want native 45m", got)
	}
}

func TestApplyStepTimeoutBudget_NilRole(t *testing.T) {
	// Role not found (findSwarmRole error) → no scaling, native kept.
	if got := applyStepTimeoutBudget(45*time.Minute, nil, "open_ended", false, budgetCfg()); got != 45*time.Minute {
		t.Errorf("nil role = %s; want native 45m", got)
	}
}

func TestApplyStepTimeoutBudget_AutonomyCeiling(t *testing.T) {
	role := &registry.SwarmRole{Name: "researcher", RuntimePolicy: "ephemeral"}
	// open_ended (2.0) clamped to autonomy_max 1.5 for unattended tasks.
	if got := applyStepTimeoutBudget(45*time.Minute, role, "open_ended", true, budgetCfg()); got != 67*time.Minute+30*time.Second {
		t.Errorf("autonomous open_ended = %s; want 67m30s", got)
	}
	// Operator-initiated gets the full 2.0.
	if got := applyStepTimeoutBudget(45*time.Minute, role, "open_ended", false, budgetCfg()); got != 90*time.Minute {
		t.Errorf("operator open_ended = %s; want 90m", got)
	}
}
