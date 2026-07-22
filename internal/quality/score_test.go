package quality

import "testing"

// A tier score turns raw per-window aggregates into a quality rate + an
// effective-cost figure (prompt tokens per quality-passing unit). This is the
// deterministic core of the cost/quality tuning loop's quality primitive
// (2026-07-21-cost-quality-tuning-loop-design.md §A).
func TestScoreTierComputesRateAndEffectiveCost(t *testing.T) {
	got := ScoreTier(TierInput{Total: 100, Passing: 80, PromptTokens: 800_000, MinSample: 20})

	if !got.Sufficient {
		t.Fatalf("Sufficient = false, want true (100 samples >= floor 20)")
	}
	if got.QualityRate != 0.8 {
		t.Errorf("QualityRate = %v, want 0.8", got.QualityRate)
	}
	if got.EffectiveCostTokens != 10_000 { // 800000 / 80 passing
		t.Errorf("EffectiveCostTokens = %v, want 10000", got.EffectiveCostTokens)
	}
	if got.SampleCount != 100 {
		t.Errorf("SampleCount = %v, want 100", got.SampleCount)
	}
}

// Below the min-sample floor the tier is not trusted: Sufficient=false so the
// caller excludes it from proposals AND the regression guard (design §A/§D —
// it must never silently read as "passing").
func TestScoreTierBelowMinSampleIsInsufficient(t *testing.T) {
	got := ScoreTier(TierInput{Total: 5, Passing: 5, PromptTokens: 50_000, MinSample: 20})
	if got.Sufficient {
		t.Errorf("Sufficient = true, want false (5 samples < floor 20)")
	}
}

// A tier with zero quality-passing units is worst-case quality, not a
// divide-by-zero: QualityRate 0 and EffectiveCostTokens 0 (undefined cost →
// 0), Sufficient still governed by the sample floor.
func TestScoreTierZeroPassingIsSafe(t *testing.T) {
	got := ScoreTier(TierInput{Total: 30, Passing: 0, PromptTokens: 900_000, MinSample: 20})
	if got.QualityRate != 0 {
		t.Errorf("QualityRate = %v, want 0", got.QualityRate)
	}
	if got.EffectiveCostTokens != 0 {
		t.Errorf("EffectiveCostTokens = %v, want 0 (no passing units)", got.EffectiveCostTokens)
	}
	if !got.Sufficient {
		t.Errorf("Sufficient = false, want true (30 samples >= floor 20)")
	}
}

// An empty window (no units observed) is insufficient and never divides by zero.
func TestScoreTierZeroTotalIsInsufficient(t *testing.T) {
	got := ScoreTier(TierInput{Total: 0, MinSample: 20})
	if got.Sufficient {
		t.Errorf("Sufficient = true, want false (no samples)")
	}
	if got.QualityRate != 0 || got.EffectiveCostTokens != 0 {
		t.Errorf("empty window should yield zero rate/cost, got rate=%v cost=%v", got.QualityRate, got.EffectiveCostTokens)
	}
}
