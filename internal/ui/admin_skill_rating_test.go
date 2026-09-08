package ui

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/ratings"
)

func ratingCtx(verdict string, lift float64, tUp, tN, bUp, bN int, cov, bcov float64) ratings.ContextResult {
	return ratings.ContextResult{
		ProjectID: "p1", WorkflowID: "digest",
		Result: ratings.Result{
			Verdict: verdict, Lift: lift,
			TreatmentUp: tUp, TreatmentN: tN, BaselineUp: bUp, BaselineN: bN,
			TreatmentCoverage: cov, BaselineCoverage: bcov,
		},
	}
}

// Both arms and both coverages, on every verdict. A badge is the easiest place
// to lose the counterfactual, which is the one thing this design refuses.
func TestRenderRatingContext_AlwaysCarriesBothArms(t *testing.T) {
	for _, verdict := range []string{"low_lift", "helping", "not_comparable", "unknown"} {
		got := renderRatingContext(ratingCtx(verdict, -0.7, 4, 20, 18, 20, 0.2, 0.2))
		for _, want := range []string{"4/20", "18/20", "coverage"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s line lost %q: %s", verdict, want, got)
			}
		}
	}
}

// The difference is rendered only by the verdicts that stand behind it.
func TestRenderRatingContext_ShowsTheDifferenceOnlyWhenTheVerdictEarnsIt(t *testing.T) {
	for _, tc := range []struct {
		verdict string
		wantPP  bool
	}{
		{"low_lift", true},
		{"helping", true},
		{"not_comparable", false},
		{"unknown", false},
	} {
		got := renderRatingContext(ratingCtx(tc.verdict, -0.7, 4, 20, 18, 20, 0.4, 0.1))
		if has := strings.Contains(got, "pp"); has != tc.wantPP {
			t.Errorf("%s: difference shown = %v, want %v: %s", tc.verdict, has, tc.wantPP, got)
		}
	}
}

// Contested executions are surfaced rather than silently dropped.
func TestRenderRatingContext_SurfacesContested(t *testing.T) {
	c := ratingCtx("low_lift", -0.7, 4, 20, 18, 20, 0.2, 0.2)
	c.Result.TreatmentContested = 2
	c.Result.BaselineContested = 1
	got := renderRatingContext(c)
	if !strings.Contains(got, "3 contested") {
		t.Errorf("contested not surfaced: %s", got)
	}
}
