package ratings

import "testing"

// The evaluator is a pure function of two arms and a config, so every case in
// the design's test plan is a table row with no database.
//
// Design: https://docs.vornik.io

func arm(eligible, rated, up int) Arm {
	return Arm{EligibleN: eligible, RatedN: rated, UpN: up}
}

func TestEvaluate_Verdicts(t *testing.T) {
	cfg := DefaultConfig()

	cases := []struct {
		name       string
		treatment  Arm
		baseline   Arm
		want       string
		wantReason string
	}{
		{
			// The case the design exists for. A large raw difference, and the
			// treatment arm was watched four times as hard — so the difference
			// is about who was looking, not about the skill.
			name:      "coverage skew refuses even a large difference",
			treatment: arm(100, 40, 10), // 40% coverage, 25% up
			baseline:  arm(100, 10, 9),  // 10% coverage, 90% up
			want:      VerdictNotComparable,
		},
		{
			// Equal observation, real difference: this one is allowed to speak.
			name:      "equal coverage with a large difference is low_lift",
			treatment: arm(100, 20, 4),  // 20% coverage, 20% up
			baseline:  arm(100, 20, 18), // 20% coverage, 90% up
			want:      VerdictLowLift,
		},
		{
			name:      "equal coverage with no difference is helping",
			treatment: arm(100, 20, 16),
			baseline:  arm(100, 20, 16),
			want:      VerdictHelping,
		},
		{
			// Below the min-rated floor, skew is never consulted: too few
			// ratings is a different fact from incomparable ones.
			name:      "too few ratings is unknown before skew is considered",
			treatment: arm(100, 3, 0), // 3 rated, all down, 3% coverage
			baseline:  arm(100, 40, 36),
			want:      VerdictUnknown,
		},
		{
			// Infinite ratio, negligible absolute coverage. The interesting
			// fact is that nobody rated anything.
			name:      "negligible absolute coverage is unknown, not not_comparable",
			treatment: arm(1000, 20, 5), // 2% coverage — under the 5% floor
			baseline:  arm(1000, 20, 18),
			want:      VerdictUnknown,
		},
		{
			// A treatment arm that is BETTER must not be flagged as harmful:
			// the sign convention is instinct lift's, so low_lift is one-sided.
			name:      "treatment better than baseline is helping, not low_lift",
			treatment: arm(100, 20, 19),
			baseline:  arm(100, 20, 10),
			want:      VerdictHelping,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.treatment, tc.baseline, cfg)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q (lift %.3f, coverage %.3f/%.3f)",
					got.Verdict, tc.want, got.Lift,
					got.TreatmentCoverage, got.BaselineCoverage)
			}
		})
	}
}

// Sign parity with instinct lift: lift is an UP-rate delta, so negative means
// worse under treatment. An earlier draft of the design wrote it as a down-rate
// delta, which is equivalent arithmetic with the opposite sign and would have
// fired low_lift on the wrong side of zero.
func TestEvaluate_LiftIsAnUpRateDeltaSoNegativeMeansWorse(t *testing.T) {
	got := Evaluate(arm(100, 20, 4), arm(100, 20, 18), DefaultConfig())
	if got.Lift >= 0 {
		t.Fatalf("lift = %.3f, want negative when treatment is worse", got.Lift)
	}
	if want := 0.20 - 0.90; !almost(got.Lift, want) {
		t.Fatalf("lift = %.3f, want %.3f", got.Lift, want)
	}
}

// Coverage is reported whatever the verdict — it is the evidence for the
// verdict, and a row that refuses without showing why is not much better than
// a number without a baseline.
func TestEvaluate_ReportsCoverageOnEveryVerdict(t *testing.T) {
	for _, tc := range []struct{ treatment, baseline Arm }{
		{arm(100, 40, 10), arm(100, 10, 9)},  // not_comparable
		{arm(100, 20, 16), arm(100, 20, 16)}, // helping
		{arm(100, 2, 1), arm(100, 40, 36)},   // unknown
	} {
		got := Evaluate(tc.treatment, tc.baseline, DefaultConfig())
		if got.TreatmentCoverage <= 0 || got.BaselineCoverage <= 0 {
			t.Fatalf("verdict %s reported no coverage: %+v", got.Verdict, got)
		}
	}
}

// An arm with no eligible executions must not divide by zero, and must not
// report a coverage of 1.0 for having rated none of nothing.
func TestEvaluate_EmptyArmsAreUnknownNotADivideByZero(t *testing.T) {
	got := Evaluate(Arm{}, Arm{}, DefaultConfig())
	if got.Verdict != VerdictUnknown {
		t.Fatalf("verdict = %q, want unknown", got.Verdict)
	}
	if got.TreatmentCoverage != 0 || got.BaselineCoverage != 0 {
		t.Fatalf("empty arms reported coverage %+v", got)
	}
}

// Contested executions are carried through rather than dropped: if a second
// rater ever becomes common, this number is what says so before the verdicts
// get thin.
func TestEvaluate_CarriesContestedCounts(t *testing.T) {
	tr := arm(100, 20, 16)
	tr.ContestedN = 3
	bl := arm(100, 20, 16)
	bl.ContestedN = 1
	got := Evaluate(tr, bl, DefaultConfig())
	if got.TreatmentContested != 3 || got.BaselineContested != 1 {
		t.Fatalf("contested counts lost: %+v", got)
	}
}

// The floor is instinct lift's 8, not a relaxed number. A rating is a MORE
// biased observation than an automatic outcome, so scarcity argues for a higher
// bar, not a lower one.
func TestDefaultConfig_MatchesInstinctLiftsFloor(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MinRated != 8 {
		t.Errorf("MinRated = %d, want 8 to match instinct lift", cfg.MinRated)
	}
	if cfg.Margin != 0.05 {
		t.Errorf("Margin = %.3f, want 0.05 to match instinct lift", cfg.Margin)
	}
}

func almost(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
