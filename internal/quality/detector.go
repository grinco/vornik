package quality

import "math"

// SwarmRolePercentile carries prompt-tokens-per-step percentiles for one
// (swarm, role) over the window. Unlike the count aggregates, percentiles are
// NOT foldable across a shared swarm's projects (you cannot average p95s), so
// the repository computes them at (swarm, role) grain directly over the combined
// per-step distribution (the project→swarm map is passed into the query).
type SwarmRolePercentile struct {
	Swarm string
	Role  string
	N     int64
	P95   int64
	P99   int64
}

// BudgetDetectorInput is the per-(swarm, role) signal the prompt_token_budget
// detector reasons over (design §B). Percentiles are prompt-tokens-per-step
// over the window; QualitySufficient is the A1 tier's min-sample verdict.
type BudgetDetectorInput struct {
	P95PromptTokens int64
	P99PromptTokens int64
	CurrentBudget   int64 // 0 = unset (image default in effect)

	QualitySufficient bool

	// Tuning knobs (the detector emitter kit's margins).
	MinP95Tokens  int64   // skip roles whose p95 is below this absolute floor (not a real token hog)
	TailFactor    float64 // p99 must exceed TailFactor×p95 to count as a runaway tail
	Margin        float64 // proposed budget = p95 × (1 + Margin)
	MinChangeFrac float64 // skip if the proposal is within this fraction of CurrentBudget
}

// BudgetDecision is the detector's verdict for one locus.
type BudgetDecision struct {
	ShouldPropose  bool
	ProposedBudget int64
	Reason         string
}

// DecidePromptTokenBudget decides whether to propose a per-step prompt-token
// budget for one (swarm, role). It proposes only when the quality signal is
// trustworthy AND a fat runaway tail exists (p99 ≫ p95), clamping to
// p95 × (1+Margin); it declines churn-only changes near an existing budget.
func DecidePromptTokenBudget(in BudgetDetectorInput) BudgetDecision {
	if !in.QualitySufficient {
		return BudgetDecision{Reason: "quality signal below min-sample floor"}
	}
	if in.P95PromptTokens <= 0 {
		return BudgetDecision{Reason: "no p95 signal"}
	}
	if in.P95PromptTokens < in.MinP95Tokens {
		return BudgetDecision{Reason: "p95 below minimum floor (role is not a token hog)"}
	}
	if float64(in.P99PromptTokens) <= in.TailFactor*float64(in.P95PromptTokens) {
		return BudgetDecision{Reason: "no runaway tail (p99 within TailFactor×p95)"}
	}
	proposed := int64(math.Round(float64(in.P95PromptTokens) * (1 + in.Margin)))
	if in.CurrentBudget > 0 {
		delta := math.Abs(float64(proposed-in.CurrentBudget)) / float64(in.CurrentBudget)
		if delta < in.MinChangeFrac {
			return BudgetDecision{Reason: "proposal within MinChangeFrac of current budget"}
		}
	}
	return BudgetDecision{ShouldPropose: true, ProposedBudget: proposed, Reason: "runaway tail clamped to p95×(1+margin)"}
}
