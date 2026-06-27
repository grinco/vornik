package workflowhealing

// Promotion gate — Self-Healing Workflow Genome v1 (LLD § Promotion
// Gate). The gate turns a baseline-vs-candidate TrialSummary pair into
// a pass/fail verdict + an operator-facing scorecard. A candidate is
// promotable only if it clears EVERY configured gate:
//
//   - replay success rate >= baseline success rate + SuccessUplift
//   - average cost <= baseline cost * (1 + CostTolerancePct)
//   - average latency <= baseline latency * (1 + LatencyTolerancePct)
//   - hallucination rate does not increase
//   - verifier failure rate does not increase
//
// Thresholds are sourced from the per-(project,workflow,class)
// overrides repo by the caller and passed in here; the gate itself is
// pure (no I/O) so it is trivially unit-testable and produces a
// deterministic verdict. The gate NEVER promotes — it only scores.
// Promotion remains a manual operator action (LLD non-negotiable #1).

import (
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// GateThresholds carries the promotion-gate tolerances. All fields are
// fractions/deltas in their natural units. The zero value reports
// IsZero()==true so NewTrialRunner can substitute DefaultGateThresholds.
type GateThresholds struct {
	// SuccessUplift is the minimum improvement in success RATE the
	// candidate must show over baseline (e.g. 0.0 = "must not
	// regress", 0.05 = "must be 5 percentage points better").
	SuccessUplift float64
	// CostTolerancePct is the fraction by which the candidate's
	// average cost may EXCEED baseline and still pass (e.g. 0.10 =
	// "up to 10% more expensive is acceptable"). Negative tightens.
	CostTolerancePct float64
	// LatencyTolerancePct is the same tolerance for average duration.
	LatencyTolerancePct float64
	// AllowHallucinationIncrease, when false (default), fails any
	// candidate whose hallucination rate rose above baseline.
	AllowHallucinationIncrease bool
	// AllowVerifierFailureIncrease, when false (default), fails any
	// candidate whose verifier-failure rate rose above baseline.
	AllowVerifierFailureIncrease bool
	// configured is set by DefaultGateThresholds / explicit
	// construction so IsZero can tell "operator left it blank" from
	// "operator chose all-zero tolerances".
	configured bool
}

// DefaultGateThresholds returns the conservative v1 defaults: the
// candidate must not regress success, may cost/latency up to 10% more,
// and must not increase hallucination or verifier failures.
func DefaultGateThresholds() GateThresholds {
	return GateThresholds{
		SuccessUplift:                0.0,
		CostTolerancePct:             0.10,
		LatencyTolerancePct:          0.10,
		AllowHallucinationIncrease:   false,
		AllowVerifierFailureIncrease: false,
		configured:                   true,
	}
}

// IsZero reports whether the thresholds were left unconfigured (the
// Go zero value), so the runner can fall back to defaults.
func (g GateThresholds) IsZero() bool { return !g.configured }

// WithConfigured marks an explicitly-built threshold set as
// configured. Callers assembling thresholds from the overrides repo
// use this so an all-zero-but-deliberate config isn't treated as
// "unset".
func (g GateThresholds) WithConfigured() GateThresholds {
	g.configured = true
	return g
}

// Evaluate scores the candidate against the baseline and returns the
// scorecard. The verdict is "passed" only when every gate clears;
// otherwise "failed" with the failing reasons enumerated. The deltas
// on the scorecard are always populated so the operator sees the full
// picture regardless of verdict.
func (g GateThresholds) Evaluate(baseline, candidate TrialSummary, riskLevel string) HealingScorecard {
	sc := HealingScorecard{RiskLevel: riskLevel}

	sc.SuccessDelta = candidate.SuccessRate() - baseline.SuccessRate()
	sc.CostDeltaPct = pctDelta(baseline.AvgCostUSD, candidate.AvgCostUSD)
	sc.LatencyDeltaPct = pctDelta(baseline.AvgDurationSeconds, candidate.AvgDurationSeconds)
	sc.HallucinationDelta = candidate.HallucinationRate - baseline.HallucinationRate
	sc.VerifierDelta = candidate.VerifierFailureRate - baseline.VerifierFailureRate

	var reasons []string
	pass := true

	if sc.SuccessDelta < g.SuccessUplift {
		pass = false
		reasons = append(reasons, fmt.Sprintf(
			"success rate uplift %.1f%% is below the required +%.1f%%",
			sc.SuccessDelta*100, g.SuccessUplift*100))
	} else {
		reasons = append(reasons, fmt.Sprintf("success rate uplift %+.1f%% clears the +%.1f%% gate", sc.SuccessDelta*100, g.SuccessUplift*100))
	}

	if sc.CostDeltaPct > g.CostTolerancePct {
		pass = false
		reasons = append(reasons, fmt.Sprintf(
			"average cost rose %.1f%%, above the %.1f%% tolerance",
			sc.CostDeltaPct*100, g.CostTolerancePct*100))
	}

	if sc.LatencyDeltaPct > g.LatencyTolerancePct {
		pass = false
		reasons = append(reasons, fmt.Sprintf(
			"average latency rose %.1f%%, above the %.1f%% tolerance",
			sc.LatencyDeltaPct*100, g.LatencyTolerancePct*100))
	}

	if !g.AllowHallucinationIncrease && sc.HallucinationDelta > 0 {
		pass = false
		reasons = append(reasons, fmt.Sprintf(
			"hallucination rate rose %.1f percentage points (increase not allowed)",
			sc.HallucinationDelta*100))
	}

	if !g.AllowVerifierFailureIncrease && sc.VerifierDelta > 0 {
		pass = false
		reasons = append(reasons, fmt.Sprintf(
			"verifier-failure rate rose %.1f percentage points (increase not allowed)",
			sc.VerifierDelta*100))
	}

	if pass {
		sc.Verdict = string(persistence.HealingTrialPassed)
	} else {
		sc.Verdict = string(persistence.HealingTrialFailed)
	}
	sc.Reasons = reasons
	return sc
}

// pctDelta returns (candidate-baseline)/baseline as a fraction. When
// baseline is 0 the ratio is undefined: a candidate that introduces
// any cost/latency from a zero baseline returns +1.0 (a 100% increase
// signal) so the tolerance gate still bites; a candidate that is also
// 0 returns 0.
func pctDelta(baseline, candidate float64) float64 {
	if baseline == 0 {
		if candidate == 0 {
			return 0
		}
		return 1.0
	}
	return (candidate - baseline) / baseline
}
