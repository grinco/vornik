package membench

import "testing"

// Outcome taxonomy and run trustworthiness from design §5.9. Four outcomes kept
// distinct because collapsing them is how a benchmark lies, plus the hard
// ceilings that stop a degraded run being quoted as a valid score.

// TestOutcomeCounts_ScoreExcludesInvalidAndError — accuracy is correct over the
// JUDGED population. Counting an adapter timeout as a wrong answer would blame
// retrieval for an infrastructure fault.
func TestOutcomeCounts_ScoreExcludesInvalidAndError(t *testing.T) {
	c := OutcomeCounts{Correct: 6, Incorrect: 2, Invalid: 1, Error: 1}

	if got := c.Judged(); got != 8 {
		t.Errorf("Judged() = %d, want 8 (correct+incorrect only)", got)
	}
	if got := c.Accuracy(); got != 0.75 {
		t.Errorf("Accuracy() = %v, want 0.75 (6/8, not 6/10)", got)
	}
	if got := c.Total(); got != 10 {
		t.Errorf("Total() = %d, want 10", got)
	}
}

// TestOutcomeCounts_NoJudgedIsNaN — a run where nothing was gradeable has no
// accuracy. Reporting 0 would be indistinguishable from getting everything
// wrong.
func TestOutcomeCounts_NoJudgedIsNaN(t *testing.T) {
	c := OutcomeCounts{Invalid: 3, Error: 2}
	if got := c.Accuracy(); !isNaN(got) {
		t.Errorf("Accuracy() with nothing judged = %v, want NaN", got)
	}
}

// TestOutcomeCounts_DegradedRate — invalid + error over the total is the signal
// that decides trustworthiness.
func TestOutcomeCounts_DegradedRate(t *testing.T) {
	c := OutcomeCounts{Correct: 7, Incorrect: 1, Invalid: 1, Error: 1}
	if got := c.DegradedRate(); got != 0.2 {
		t.Errorf("DegradedRate() = %v, want 0.2", got)
	}
	if got := (OutcomeCounts{}).DegradedRate(); got != 0 {
		t.Errorf("DegradedRate() of an empty run = %v, want 0", got)
	}
}

// TestTrustThreshold_HardCeilingCannotBeRaised is the round-1 finding: a purely
// configurable threshold lets an operator set it to 50% and quote a half-broken
// run. The configurable part may only TIGHTEN.
func TestTrustThreshold_HardCeilingCannotBeRaised(t *testing.T) {
	if got := ResolveDegradedThreshold(0.5); got != MaxDegradedThreshold {
		t.Errorf("threshold 0.5 resolved to %v, want the %v ceiling",
			got, MaxDegradedThreshold)
	}
	if got := ResolveDegradedThreshold(0.99); got != MaxDegradedThreshold {
		t.Errorf("threshold 0.99 resolved to %v, want the %v ceiling",
			got, MaxDegradedThreshold)
	}
	if got := ResolveDegradedThreshold(0.05); got != 0.05 {
		t.Errorf("a tightening threshold was overridden: got %v, want 0.05", got)
	}
	// Unset (zero) means "use the ceiling", not "tolerate nothing" — a run with
	// one flaky HTTP call should not be stamped untrustworthy by default.
	if got := ResolveDegradedThreshold(0); got != MaxDegradedThreshold {
		t.Errorf("unset threshold = %v, want the %v default", got, MaxDegradedThreshold)
	}
	// Negative is nonsense; treat as unset rather than making everything
	// untrustworthy.
	if got := ResolveDegradedThreshold(-1); got != MaxDegradedThreshold {
		t.Errorf("negative threshold = %v, want the %v default", got, MaxDegradedThreshold)
	}
}

// TestMaxDegradedThreshold_Value pins the ceiling itself. Round-2 review
// accepted 20%; changing it is a deliberate act that should break a test.
func TestMaxDegradedThreshold_Value(t *testing.T) {
	if MaxDegradedThreshold != 0.20 {
		t.Errorf("MaxDegradedThreshold = %v, want 0.20 (design §5.9)", MaxDegradedThreshold)
	}
}

// TestAssessTrust_DegradedRateOverThreshold — the primary stamp.
func TestAssessTrust_DegradedRateOverThreshold(t *testing.T) {
	// 3 of 10 degraded = 30%, over the 20% ceiling.
	c := OutcomeCounts{Correct: 6, Incorrect: 1, Invalid: 2, Error: 1}
	tr := AssessTrust(c, 0, 0)

	if tr.Trustworthy {
		t.Error("a 30% degraded run was stamped trustworthy")
	}
	if tr.Reason == "" {
		t.Error("untrustworthy verdict carries no reason; an unexplained stamp " +
			"cannot be acted on")
	}
}

// TestAssessTrust_HaystackLossForcesUntrustworthy — an item that lost more than
// half its haystack is being scored against an EASIER task than the dataset
// poses, so reporting it as the dataset's score overstates us.
func TestAssessTrust_HaystackLossForcesUntrustworthy(t *testing.T) {
	clean := OutcomeCounts{Correct: 10}

	tr := AssessTrust(clean, 0.60, 0)
	if tr.Trustworthy {
		t.Error("60% haystack loss was stamped trustworthy")
	}

	// Just under the bar stays trustworthy — the guard is for gross loss, not
	// for the odd redacted secret.
	if tr := AssessTrust(clean, 0.10, 0); !tr.Trustworthy {
		t.Errorf("10%% haystack loss stamped untrustworthy: %s", tr.Reason)
	}
}

// TestAssessTrust_PartialComparabilityKeyIsRecorded — round-2 finding: when the
// external system won't report its config we cannot verify it is unchanged. That
// is not the same as verified-identical and must not be silently treated as
// such.
func TestAssessTrust_PartialComparabilityIsFlagged(t *testing.T) {
	tr := AssessTrust(OutcomeCounts{Correct: 10}, 0, 0)
	if !tr.Trustworthy {
		t.Fatalf("clean run stamped untrustworthy: %s", tr.Reason)
	}
	// A clean run is trustworthy for SCORING; partial comparability is a
	// separate axis, carried on the manifest, and must not silently pass as
	// full comparability.
	if tr.Reason != "" {
		t.Errorf("clean run carries reason %q, want empty", tr.Reason)
	}
}

// TestAssessTrust_CleanRun — the baseline case.
func TestAssessTrust_CleanRun(t *testing.T) {
	tr := AssessTrust(OutcomeCounts{Correct: 9, Incorrect: 1}, 0, 0)
	if !tr.Trustworthy {
		t.Errorf("clean run stamped untrustworthy: %s", tr.Reason)
	}
}

// TestAssessTrust_TighterThresholdHonoured — an operator may demand better than
// the ceiling.
func TestAssessTrust_TighterThresholdHonoured(t *testing.T) {
	// 1 of 10 degraded = 10%: under the 20% ceiling, over a 5% request.
	c := OutcomeCounts{Correct: 8, Incorrect: 1, Error: 1}

	if tr := AssessTrust(c, 0, 0); !tr.Trustworthy {
		t.Error("10% degraded should pass the default ceiling")
	}
	if tr := AssessTrust(c, 0, 0.05); tr.Trustworthy {
		t.Error("10% degraded should fail an explicit 5% threshold")
	}
}
