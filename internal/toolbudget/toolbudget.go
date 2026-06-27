// Package toolbudget resolves a role's effective tool-iteration budget
// (VORNIK_MAX_TOOL_ITERATIONS) from a coarse complexity tier emitted by the
// planner, bounded by config caps. See
// https://docs.vornik.io
//
// The resolver is a pure function with no I/O so the daemon can call it at
// every worker spawn and every behaviour is unit-testable in isolation.
package toolbudget

import (
	"math"
	"strings"
	"time"
)

// Tier is the coarse complexity verdict a planner emits for a task.
type Tier string

const (
	TierTrivial   Tier = "trivial"
	TierStandard  Tier = "standard" // the fallback / zero-value tier (factor 1.0)
	TierComplex   Tier = "complex"
	TierOpenEnded Tier = "open_ended"
)

// Config is the daemon-level tool_budget block (LLD §7). The zero value has
// Enabled=false, which makes Resolve a passthrough — the feature is inert
// until an operator opts in.
type Config struct {
	Enabled           bool
	Factors           map[Tier]float64
	MaxFactor         float64
	AutonomyMaxFactor float64
	// MinStepTimeout floors ScaleTimeout so an aggressive downscale can't
	// starve a step of the time it needs for container startup + a few
	// iterations. 0 = no floor. Never raises a step above its native base.
	MinStepTimeout time.Duration
}

// factorFor returns the effective multiplier for a tier under the config caps.
// Shared by Resolve (iteration budget) and ScaleTimeout (time budget) so the
// two budgets always scale by the identical factor; autonomous work is held to
// the tighter AutonomyMaxFactor ceiling.
//
// An EMPTY or UNRECOGNISED tier resolves to 1.0 (no scaling) — the role's
// configured budget is the reference (== the `complex` 1.0x anchor). This is
// deliberate: when no producer classified the task we must NOT silently
// downscale it. (Before this, an absent tier fell through to `standard`, which
// the 2026-06-09 recalibration set to 0.5x — so every task whose planner emitted
// no tier, e.g. the dev-pipeline analyst, was silently halved on BOTH iterations
// and step timeout, starving real work. Incident 2026-06-13: implement's 15m
// step ran 15m×0.5=7m30s and timed out.) Only an EXPLICIT trivial/standard
// verdict downscales.
func factorFor(tier Tier, autonomous bool, cfg Config) float64 {
	factor := 1.0
	if t := Tier(strings.TrimSpace(string(tier))); t != "" {
		if f, ok := cfg.Factors[t]; ok {
			factor = f
		}
	}
	if cfg.MaxFactor > 0 && factor > cfg.MaxFactor {
		factor = cfg.MaxFactor
	}
	if autonomous && cfg.AutonomyMaxFactor > 0 && factor > cfg.AutonomyMaxFactor {
		factor = cfg.AutonomyMaxFactor
	}
	return factor
}

// ScaleTimeout scales a step's base timeout by the same complexity factor and
// caps as Resolve, so a step's time budget tracks its iteration budget — raise
// the iterations and you raise the wall-clock; downscale a small task and it
// gets proportionally less of both. When cfg.Enabled is false (or base is
// non-positive) base is returned unchanged. The result is floored at
// cfg.MinStepTimeout, but the floor never raises a step above its native base
// (an operator's explicit short step timeout wins over the floor).
func ScaleTimeout(base time.Duration, tier Tier, autonomous bool, cfg Config) time.Duration {
	if !cfg.Enabled || base <= 0 {
		return base
	}
	scaled := time.Duration(math.Round(float64(base) * factorFor(tier, autonomous, cfg)))
	floor := cfg.MinStepTimeout
	if floor > base {
		floor = base
	}
	if scaled < floor {
		scaled = floor
	}
	return scaled
}

// Resolve returns the effective tool-iteration budget for a worker.
//
//   - roleBase is the role's configured VORNIK_MAX_TOOL_ITERATIONS (the
//     caller substitutes the daemon default when the role pins none).
//   - tier is the planner's verdict; an empty or unrecognised tier resolves to
//     factor 1.0 (the role base, unscaled) — never a silent downscale.
//   - autonomous holds the budget to AutonomyMaxFactor — the tighter ceiling
//     for unattended work.
//
// When cfg.Enabled is false, roleBase is returned unchanged.
func Resolve(roleBase int, tier Tier, autonomous bool, cfg Config) int {
	if !cfg.Enabled {
		return roleBase
	}
	effective := int(math.Round(float64(roleBase) * factorFor(tier, autonomous, cfg)))
	if effective < 1 {
		effective = 1
	}
	return effective
}
