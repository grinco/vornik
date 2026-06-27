package workflowhealing

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func TestGateThresholds_IsZeroAndDefault(t *testing.T) {
	if !(GateThresholds{}).IsZero() {
		t.Error("zero-value GateThresholds should report IsZero")
	}
	if DefaultGateThresholds().IsZero() {
		t.Error("DefaultGateThresholds must report configured")
	}
	// An explicitly-configured all-zero set is NOT zero.
	if (GateThresholds{}).WithConfigured().IsZero() {
		t.Error("WithConfigured must mark the set as configured")
	}
}

func TestGate_Evaluate_PassesWhenStrictlyBetter(t *testing.T) {
	g := DefaultGateThresholds()
	base := TrialSummary{Runs: 4, Successes: 2, AvgCostUSD: 0.20, AvgDurationSeconds: 30, HallucinationRate: 0.2}
	cand := TrialSummary{Runs: 4, Successes: 4, AvgCostUSD: 0.10, AvgDurationSeconds: 15, HallucinationRate: 0.0}
	sc := g.Evaluate(base, cand, "low")
	if sc.Verdict != string(persistence.HealingTrialPassed) {
		t.Fatalf("verdict = %q, want passed; reasons=%v", sc.Verdict, sc.Reasons)
	}
	if sc.SuccessDelta <= 0 {
		t.Errorf("success delta = %f, want positive", sc.SuccessDelta)
	}
	if sc.CostDeltaPct >= 0 {
		t.Errorf("cost delta = %f, want negative", sc.CostDeltaPct)
	}
}

func TestGate_Evaluate_FailsOnSuccessRegression(t *testing.T) {
	g := DefaultGateThresholds()
	base := TrialSummary{Runs: 4, Successes: 4}
	cand := TrialSummary{Runs: 4, Successes: 2}
	sc := g.Evaluate(base, cand, "low")
	if sc.Verdict != string(persistence.HealingTrialFailed) {
		t.Fatalf("verdict = %q, want failed", sc.Verdict)
	}
}

func TestGate_Evaluate_FailsOnCostOverTolerance(t *testing.T) {
	g := DefaultGateThresholds() // 10% cost tolerance
	base := TrialSummary{Runs: 2, Successes: 2, AvgCostUSD: 0.10}
	cand := TrialSummary{Runs: 2, Successes: 2, AvgCostUSD: 0.20} // +100%
	sc := g.Evaluate(base, cand, "low")
	if sc.Verdict != string(persistence.HealingTrialFailed) {
		t.Fatalf("verdict = %q, want failed (cost over tolerance)", sc.Verdict)
	}
}

func TestGate_Evaluate_AllowsCostWithinTolerance(t *testing.T) {
	g := DefaultGateThresholds() // 10% cost + latency tolerance
	base := TrialSummary{Runs: 2, Successes: 2, AvgCostUSD: 0.100, AvgDurationSeconds: 10}
	cand := TrialSummary{Runs: 2, Successes: 2, AvgCostUSD: 0.105, AvgDurationSeconds: 10} // +5%
	sc := g.Evaluate(base, cand, "low")
	if sc.Verdict != string(persistence.HealingTrialPassed) {
		t.Fatalf("verdict = %q, want passed (cost within tolerance); reasons=%v", sc.Verdict, sc.Reasons)
	}
}

func TestGate_Evaluate_FailsOnLatencyOverTolerance(t *testing.T) {
	g := DefaultGateThresholds() // 10% latency tolerance
	base := TrialSummary{Runs: 2, Successes: 2, AvgDurationSeconds: 10}
	cand := TrialSummary{Runs: 2, Successes: 2, AvgDurationSeconds: 30} // +200%
	sc := g.Evaluate(base, cand, "low")
	if sc.Verdict != string(persistence.HealingTrialFailed) {
		t.Fatalf("verdict = %q, want failed (latency over tolerance)", sc.Verdict)
	}
	if sc.LatencyDeltaPct <= g.LatencyTolerancePct {
		t.Errorf("latency delta %f should exceed tolerance %f", sc.LatencyDeltaPct, g.LatencyTolerancePct)
	}
}

func TestGate_Evaluate_FailsOnHallucinationIncrease(t *testing.T) {
	g := DefaultGateThresholds()
	base := TrialSummary{Runs: 2, Successes: 2, HallucinationRate: 0.0}
	cand := TrialSummary{Runs: 2, Successes: 2, HallucinationRate: 0.5}
	if sc := g.Evaluate(base, cand, "low"); sc.Verdict != string(persistence.HealingTrialFailed) {
		t.Fatalf("verdict = %q, want failed (hallucination regression)", sc.Verdict)
	}
}

func TestGate_Evaluate_FailsOnVerifierIncrease(t *testing.T) {
	g := DefaultGateThresholds()
	base := TrialSummary{Runs: 2, Successes: 2, VerifierFailureRate: 0.0}
	cand := TrialSummary{Runs: 2, Successes: 2, VerifierFailureRate: 0.5}
	sc := g.Evaluate(base, cand, "low")
	if sc.Verdict != string(persistence.HealingTrialFailed) {
		t.Fatalf("verdict = %q, want failed (verifier regression)", sc.Verdict)
	}
}

func TestGate_Evaluate_RespectsCustomUplift(t *testing.T) {
	// Require a +25% success uplift; a +25% improvement (0.5 -> 0.75)
	// exactly clears, a smaller one fails.
	g := GateThresholds{SuccessUplift: 0.25, CostTolerancePct: 1, LatencyTolerancePct: 1}.WithConfigured()
	base := TrialSummary{Runs: 4, Successes: 2}      // 0.5
	candClear := TrialSummary{Runs: 4, Successes: 3} // 0.75 → +0.25
	if sc := g.Evaluate(base, candClear, "low"); sc.Verdict != string(persistence.HealingTrialPassed) {
		t.Errorf("exact uplift should pass; got %q reasons=%v", sc.Verdict, sc.Reasons)
	}
	candShort := TrialSummary{Runs: 8, Successes: 5} // 0.625 → +0.125 < 0.25
	if sc := g.Evaluate(base, candShort, "low"); sc.Verdict != string(persistence.HealingTrialFailed) {
		t.Errorf("sub-uplift should fail; got %q", sc.Verdict)
	}
}

func TestPctDelta_ZeroBaseline(t *testing.T) {
	if got := pctDelta(0, 0); got != 0 {
		t.Errorf("pctDelta(0,0) = %f, want 0", got)
	}
	if got := pctDelta(0, 0.5); got != 1.0 {
		t.Errorf("pctDelta(0,>0) = %f, want 1.0", got)
	}
	if got := pctDelta(0.10, 0.20); got != 1.0 {
		t.Errorf("pctDelta(0.10,0.20) = %f, want 1.0", got)
	}
}

func TestTrialSummary_SuccessRate(t *testing.T) {
	if got := (TrialSummary{}).SuccessRate(); got != 0 {
		t.Errorf("empty SuccessRate = %f, want 0", got)
	}
	if got := (TrialSummary{Runs: 4, Successes: 1}).SuccessRate(); got != 0.25 {
		t.Errorf("SuccessRate = %f, want 0.25", got)
	}
}
