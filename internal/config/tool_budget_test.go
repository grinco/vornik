package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/toolbudget"
)

// The tool_budget block (https://docs.vornik.io
// §7) is opt-in and off by default. Resolved() applies the LLD default factor
// table + caps when fields are omitted, and converts to a toolbudget.Config
// the executor can hand to toolbudget.Resolve.

func TestToolBudget_DefaultIsDisabled(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("server:\n  address: \":8080\"\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.ToolBudget.Enabled {
		t.Fatalf("tool_budget must be disabled by default")
	}
	// A disabled block resolves to a passthrough toolbudget.Config.
	if c.ToolBudget.Resolved().Enabled {
		t.Fatalf("disabled block must resolve to Enabled=false")
	}
}

func TestToolBudget_ResolvedFillsDefaultFactorsAndCaps(t *testing.T) {
	// enabled with no factors/caps specified → LLD defaults apply.
	y := "tool_budget:\n  enabled: true\n"
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rc := c.ToolBudget.Resolved()
	if !rc.Enabled {
		t.Fatalf("Resolved().Enabled = false; want true")
	}
	// Recalibrated 2026-06-09: the role's configured budget is the
	// 'complex' reference; standard tasks scale to half, trivial a
	// quarter, open_ended up to 2x. Both iteration AND (coupled) step
	// timeout scale by these. See dynamic-tool-budget-design.md §7.
	want := map[toolbudget.Tier]float64{
		toolbudget.TierTrivial:   0.25,
		toolbudget.TierStandard:  0.5,
		toolbudget.TierComplex:   1.0,
		toolbudget.TierOpenEnded: 2.0,
	}
	for tier, f := range want {
		if rc.Factors[tier] != f {
			t.Errorf("default factor[%q] = %v; want %v", tier, rc.Factors[tier], f)
		}
	}
	if rc.MaxFactor != 2.0 {
		t.Errorf("default MaxFactor = %v; want 2.0", rc.MaxFactor)
	}
	if rc.AutonomyMaxFactor != 1.5 {
		t.Errorf("default AutonomyMaxFactor = %v; want 1.5", rc.AutonomyMaxFactor)
	}
	if rc.MinStepTimeout != 5*time.Minute {
		t.Errorf("default MinStepTimeout = %v; want 5m", rc.MinStepTimeout)
	}
}

func TestToolBudget_ResolvedHonorsMinStepTimeoutOverride(t *testing.T) {
	y := "tool_budget:\n  enabled: true\n  min_step_timeout_seconds: 120\n"
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := c.ToolBudget.Resolved().MinStepTimeout; got != 2*time.Minute {
		t.Errorf("MinStepTimeout = %v; want 2m (operator override)", got)
	}
}

func TestToolBudget_ResolvedHonorsOperatorOverrides(t *testing.T) {
	y := "tool_budget:\n" +
		"  enabled: true\n" +
		"  max_factor: 3.0\n" +
		"  autonomy_max_factor: 2.0\n" +
		"  factors:\n" +
		"    complex: 1.8\n"
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rc := c.ToolBudget.Resolved()
	if rc.MaxFactor != 3.0 {
		t.Errorf("MaxFactor = %v; want 3.0 (operator override)", rc.MaxFactor)
	}
	if rc.AutonomyMaxFactor != 2.0 {
		t.Errorf("AutonomyMaxFactor = %v; want 2.0 (operator override)", rc.AutonomyMaxFactor)
	}
	if rc.Factors[toolbudget.TierComplex] != 1.8 {
		t.Errorf("factor[complex] = %v; want 1.8 (operator override)", rc.Factors[toolbudget.TierComplex])
	}
	// Tiers the operator didn't override still get their defaults.
	if rc.Factors[toolbudget.TierStandard] != 0.5 {
		t.Errorf("factor[standard] = %v; want 0.5 (default kept)", rc.Factors[toolbudget.TierStandard])
	}
}
