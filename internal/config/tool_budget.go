package config

import (
	"time"

	"vornik.io/vornik/internal/toolbudget"
)

// ToolBudgetConfig is the daemon-level tool_budget block. It is the YAML
// surface for dynamic per-role tool-use limits; Resolved() applies the
// design defaults for any omitted field and converts to the toolbudget.Config
// the executor hands to toolbudget.Resolve. See
// https://docs.vornik.io §7.
type ToolBudgetConfig struct {
	// Enabled gates the whole feature. False (the default) makes
	// toolbudget.Resolve a passthrough — roles run on their static
	// VORNIK_MAX_TOOL_ITERATIONS exactly as before.
	Enabled bool `yaml:"enabled"`
	// Factors maps a complexity tier to a multiplier on the role's base
	// budget. Omitted tiers fall back to the design defaults below.
	Factors map[string]float64 `yaml:"factors"`
	// MaxFactor is the hard ceiling on any factor regardless of tier.
	// 0 → default 2.0.
	MaxFactor float64 `yaml:"max_factor"`
	// AutonomyMaxFactor is the tighter ceiling applied to unattended
	// (non-operator) tasks. 0 → default 1.5.
	AutonomyMaxFactor float64 `yaml:"autonomy_max_factor"`
	// MinStepTimeoutSeconds floors the coupled step-timeout scaling
	// (toolbudget.ScaleTimeout) so a downscaled small task still gets
	// enough wall-clock for container startup + a few iterations. 0 →
	// default 300 (5m). The floor never raises a step above its native
	// configured timeout.
	MinStepTimeoutSeconds int `yaml:"min_step_timeout_seconds"`
}

// defaultToolBudgetFactors is the LLD §7 factor table. Recalibrated
// 2026-06-09: the role's configured budget is the 'complex' (1.0) reference;
// standard tasks scale to half and trivial to a quarter (most tasks are
// small), while open_ended still scales up to 2x for genuinely large work.
// The same factors drive both the iteration budget (Resolve) and the coupled
// step timeout (ScaleTimeout).
var defaultToolBudgetFactors = map[toolbudget.Tier]float64{
	toolbudget.TierTrivial:   0.25,
	toolbudget.TierStandard:  0.5,
	toolbudget.TierComplex:   1.0,
	toolbudget.TierOpenEnded: 2.0,
}

// Resolved converts the YAML block into a toolbudget.Config, filling any
// omitted field with its design default. Operator-supplied factors override
// the default for that tier only; tiers the operator does not mention keep
// their defaults.
func (c ToolBudgetConfig) Resolved() toolbudget.Config {
	factors := make(map[toolbudget.Tier]float64, len(defaultToolBudgetFactors))
	for tier, f := range defaultToolBudgetFactors {
		factors[tier] = f
	}
	for tier, f := range c.Factors {
		factors[toolbudget.Tier(tier)] = f
	}

	maxFactor := c.MaxFactor
	if maxFactor == 0 {
		maxFactor = 2.0
	}
	autonomyMaxFactor := c.AutonomyMaxFactor
	if autonomyMaxFactor == 0 {
		autonomyMaxFactor = 1.5
	}
	minStepTimeout := time.Duration(c.MinStepTimeoutSeconds) * time.Second
	if minStepTimeout == 0 {
		minStepTimeout = 5 * time.Minute
	}

	return toolbudget.Config{
		Enabled:           c.Enabled,
		Factors:           factors,
		MaxFactor:         maxFactor,
		AutonomyMaxFactor: autonomyMaxFactor,
		MinStepTimeout:    minStepTimeout,
	}
}
