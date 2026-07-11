package projectwizard

import (
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/rolelibrary"
)

// Grounded cost estimate (task 1.3; design §5.2 — "CostBand: rough
// per-run estimate, always labelled an estimate"; §9 Phase 3
// "cost-estimate calibration"). The LLM's own cost_band string is
// untrusted free text — it is OVERWRITTEN here with a deterministic,
// server-computed figure derived from the materialized bundle, before
// the plan ever reaches the preview/commit path (applyBundle calls
// estimateCostBand right after a successful materializeBundle).
//
// v1 heuristic (documented, not live-read from configs/pricing.yaml):
// there is no clean tier -> concrete-model resolution reachable from
// this package today — role-library archetypes declare a modelTier
// (trivial|standard|complex, §5.3) but the server's tier -> model
// routing is a daemon-level policy (open-weight-first) applied at
// EXECUTION time, not at composition time, and configs/pricing.yaml is
// keyed by concrete model ID, not tier. Rather than guess a specific
// model here (which would violate the open-weight-first, server-owned
// routing invariant), each tier gets a documented per-run token-cost
// constant approximating a representative model in that tier's price
// band from configs/pricing.yaml as of 2026-07 (see the band comments
// below). This is a v1 approximation flagged for later calibration
// against actual task_llm_usage percentiles of composed projects, per
// design §9/§11 Q2 — NOT a billing guarantee.
const (
	// trivialTierCostPerRunUSD approximates a small/cheap model
	// (configs/pricing.yaml band: nvidia.nemotron-nano-9b-v2-class,
	// ~$0.06/$0.23 per 1M in/out) at ~1,500 input / 400 output tokens
	// for a single trivial-role turn.
	trivialTierCostPerRunUSD = (1500*0.06 + 400*0.23) / 1_000_000
	// standardTierCostPerRunUSD approximates a mid-tier model
	// (configs/pricing.yaml band: minimax.minimax-m2-class, ~$0.30/
	// $1.20 per 1M in/out) at ~4,000 input / 1,200 output tokens.
	standardTierCostPerRunUSD = (4000*0.30 + 1200*1.20) / 1_000_000
	// complexTierCostPerRunUSD approximates a larger/reasoning-capable
	// model (configs/pricing.yaml band: deepseek.v3.2-class, ~$0.62/
	// $1.85 per 1M in/out) at ~9,000 input / 2,500 output tokens.
	complexTierCostPerRunUSD = (9000*0.62 + 2500*1.85) / 1_000_000
)

// approxMonthDuration is the fixed 30-day month used to translate a
// poll-interval cadence into an expected runs/month figure — an
// approximation (real months vary 28-31 days) that's precise enough
// for a labelled estimate.
const approxMonthDuration = 30 * 24 * time.Hour

// perRoleCostPerRunUSD returns the documented v1 per-run cost constant
// for a role's modelTier. Unknown/empty tiers (a role-library entry
// that predates the modelTier field, or a doctor-check gap) fall back
// to the standard band rather than the cheapest or most expensive —
// erring toward a representative middle estimate instead of silently
// under- or over-stating cost.
func perRoleCostPerRunUSD(tier string) float64 {
	switch tier {
	case rolelibrary.ModelTierTrivial:
		return trivialTierCostPerRunUSD
	case rolelibrary.ModelTierComplex:
		return complexTierCostPerRunUSD
	default:
		return standardTierCostPerRunUSD
	}
}

// estimateCostBand computes the grounded per-run/per-month cost
// estimate for a materialized tier-3 bundle. Returns "" for a nil/
// incomplete bundle (the caller leaves the LLM's original CostBand
// alone in that case — there is nothing grounded to say yet).
//
// Heuristic: sum each composed role's per-run tier cost (approximating
// "every role does roughly one LLM call per workflow run" — a
// deliberate v1 simplification; a role invoked multiple times per run,
// e.g. inside a loop, is under-counted, which is why this is always
// labelled an estimate, never a guarantee). Multiply by the expected
// number of runs/month derived from the bundle's autonomy schedule: a
// one-shot (autonomy disabled, or an unparseable cadence) bundle runs
// once; a recurring bundle runs approxMonthDuration / pollInterval
// times per month (floored at 1).
func estimateCostBand(mb *materializedBundle) string {
	if mb == nil || mb.Project == nil {
		return ""
	}
	var perRun float64
	for _, tier := range mb.RoleModelTiers {
		perRun += perRoleCostPerRunUSD(tier)
	}

	oneShot := true
	runsPerMonth := 1.0
	if mb.Project.Autonomy.Enabled {
		if d, err := time.ParseDuration(strings.TrimSpace(mb.Project.Autonomy.PollInterval)); err == nil && d > 0 {
			oneShot = false
			runsPerMonth = approxMonthDuration.Seconds() / d.Seconds()
			if runsPerMonth < 1 {
				runsPerMonth = 1
			}
		}
		// An unparseable/zero cadence on an "enabled" autonomy block
		// shouldn't happen past the registry validator, but if it does,
		// falling back to one-shot is the safe (never wildly-wrong)
		// direction rather than guessing a runs/month figure.
	}

	return formatCostBand(perRun, perRun*runsPerMonth, oneShot)
}

// formatCostBand renders the grounded estimate, always keeping the
// "(estimate)" label design §5.2 requires the preview UI to be able to
// rely on regardless of how the number itself is phrased.
func formatCostBand(perRun, perMonth float64, oneShot bool) string {
	if oneShot {
		return fmt.Sprintf("~$%.4f for this run (estimate; one-time run, not recurring)", perRun)
	}
	return fmt.Sprintf("~$%.4f per run, ~$%.2f per month at the confirmed schedule (estimate)", perRun, perMonth)
}
