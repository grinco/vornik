// Package quality is the read-model behind the cost/quality auto-tuning
// control loop (https://docs.vornik.io
// design.md). It turns per-window aggregates over the existing audit spine
// (execution_step_outcomes + tasks + task_llm_usage) into a two-tier quality
// metric that guards cost-knob proposals. This file holds the deterministic
// scoring core — no DB, no LLM — so it is fully unit-testable.
package quality

// TierInput is the raw per-window aggregate for one quality tier (A1 step-level
// or A2 task-level). Total is the number of units observed (steps for A1, tasks
// for A2); Passing is how many met the tier's quality bar; PromptTokens is the
// summed prompt tokens over the Passing units; MinSample is the tier's
// min-sample floor below which the score is not trusted.
type TierInput struct {
	Total        int64
	Passing      int64
	PromptTokens int64
	MinSample    int64
}

// TierScore is the computed quality signal for one tier. When Sufficient is
// false the rate/cost fields are not trustworthy and callers must exclude the
// tier from proposals and the regression guard (never treat it as passing).
type TierScore struct {
	Sufficient          bool
	QualityRate         float64 // Passing / Total
	EffectiveCostTokens float64 // PromptTokens / Passing (tokens per quality-passing unit)
	SampleCount         int64
}

// ScoreTier computes a TierScore from a TierInput.
func ScoreTier(in TierInput) TierScore {
	s := TierScore{SampleCount: in.Total}
	s.Sufficient = in.Total >= in.MinSample && in.Total > 0
	if in.Total > 0 {
		s.QualityRate = float64(in.Passing) / float64(in.Total)
	}
	if in.Passing > 0 {
		s.EffectiveCostTokens = float64(in.PromptTokens) / float64(in.Passing)
	}
	return s
}
