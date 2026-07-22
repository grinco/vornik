package quality

import "testing"

func baseInput() BudgetDetectorInput {
	return BudgetDetectorInput{
		P95PromptTokens:   500_000,
		P99PromptTokens:   2_000_000,
		CurrentBudget:     0,
		QualitySufficient: true,
		TailFactor:        1.5,
		Margin:            0.2,
		MinChangeFrac:     0.1,
	}
}

// A fat runaway tail (p99 >> p95) on a quality-sufficient locus yields a
// proposed budget of p95 × (1+margin) — the Phase-2 prompt_token_budget
// detector's core (design §B).
func TestDecidePromptTokenBudgetProposesOnRunawayTail(t *testing.T) {
	d := DecidePromptTokenBudget(baseInput())
	if !d.ShouldPropose {
		t.Fatalf("expected a proposal on a fat tail, got none (%s)", d.Reason)
	}
	if d.ProposedBudget != 600_000 { // 500000 * 1.2
		t.Errorf("ProposedBudget = %d, want 600000", d.ProposedBudget)
	}
}

// No quality signal → never propose (can't guarantee the cut is safe).
func TestDecidePromptTokenBudgetSkipsWhenQualityInsufficient(t *testing.T) {
	in := baseInput()
	in.QualitySufficient = false
	if d := DecidePromptTokenBudget(in); d.ShouldPropose {
		t.Errorf("proposed despite insufficient quality: %+v", d)
	}
}

// No runaway tail (p99 within TailFactor×p95) → nothing to clamp, no proposal.
func TestDecidePromptTokenBudgetSkipsWithoutTail(t *testing.T) {
	in := baseInput()
	in.P99PromptTokens = 600_000 // < 1.5 * 500000
	if d := DecidePromptTokenBudget(in); d.ShouldPropose {
		t.Errorf("proposed without a runaway tail: %+v", d)
	}
}

// A role below the absolute p95 floor is not a real token hog — no proposal
// even with a fat relative tail (avoids clamping cheap roles like lead).
func TestDecidePromptTokenBudgetSkipsBelowMinP95Floor(t *testing.T) {
	in := baseInput()
	in.P95PromptTokens = 20_000  // tiny (lead-like)
	in.P99PromptTokens = 200_000 // 10× tail, but absolute p95 is trivial
	in.MinP95Tokens = 100_000
	if d := DecidePromptTokenBudget(in); d.ShouldPropose {
		t.Errorf("proposed for a sub-floor role: %+v", d)
	}
}

// A current budget already within MinChangeFrac of the proposal → no churn.
func TestDecidePromptTokenBudgetSkipsWhenNearCurrent(t *testing.T) {
	in := baseInput()
	in.CurrentBudget = 620_000 // proposed 600000 is within 10% → skip
	if d := DecidePromptTokenBudget(in); d.ShouldPropose {
		t.Errorf("proposed a churn-only change: %+v", d)
	}
}
