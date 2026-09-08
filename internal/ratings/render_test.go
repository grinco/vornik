package ratings

import (
	"regexp"
	"strings"
	"testing"
)

// reFigure matches a RENDERED number — "-42 pp", "60%", "5/20" — rather than
// the bare letters "pp", which occur inside ordinary words such as
// "approving". The first version of this test matched the letters and failed
// against a line that contained no figure at all.
var reFigure = regexp.MustCompile(`[-+]?\d+ pp|\d+%|\d+/\d+`)

// The renderer is the one place the "no number without its counterfactual"
// rule lives (LLD 2026-09-08-execution-ratings-approval-paths-design.md §2.1).
// Phase 2 had the rule in two places — internal/ui and internal/cli — and
// phase 3 adds three more surfaces; these tests are what stops the fourth copy
// from being the one that is wrong.

// Every verdict must say something. An empty string renders as a blank column,
// which reports "examined and clean" and means "never examined" — the failure
// ENGINEERING-TENETS.md names directly (design §2.2).
func TestVerdictSentenceIsNeverEmpty(t *testing.T) {
	cases := []struct {
		verdict string
		cause   Cause
	}{
		{VerdictHelping, CauseNone},
		{VerdictLowLift, CauseNone},
		{VerdictUnknown, CauseNone},
		{VerdictNotComparable, CauseNone},
		{VerdictNotMeasurable, CauseAbsence},
		{VerdictNotMeasurable, CauseConstruction},
		{VerdictNotMeasurable, CauseUnknownProvenance},
		{VerdictNotMeasurable, CauseSupersededBody},
	}
	for _, c := range cases {
		if got := VerdictSentence(c.verdict, c.cause); strings.TrimSpace(got) == "" {
			t.Errorf("VerdictSentence(%q, %q) is empty; every verdict must say what it means",
				c.verdict, c.cause)
		}
	}
}

// The four not_measurable causes imply four different operator actions —
// wait, do not wait, the evidence cannot be tied to a body, the evidence
// judged a different body — so they must not collapse to one sentence.
//
// Asserted pairwise-different rather than merely non-empty: all four reduce to
// "you are approving without evidence", so a refactor that collapsed them
// would pass a non-emptiness check while destroying the distinction (design
// §2.2, round-1 review finding TWO).
func TestNotMeasurableCausesArePairwiseDistinct(t *testing.T) {
	causes := []Cause{CauseAbsence, CauseConstruction, CauseUnknownProvenance, CauseSupersededBody}
	seen := map[string]Cause{}
	for _, c := range causes {
		s := VerdictSentence(VerdictNotMeasurable, c)
		if prev, dup := seen[s]; dup {
			t.Errorf("causes %q and %q render the same sentence %q; they mean different things",
				prev, c, s)
		}
		seen[s] = c
	}
}

// The suppression rule itself. Only the two verdicts that stand behind a
// number may show one.
func TestShowsDifference(t *testing.T) {
	cases := map[string]bool{
		VerdictLowLift:       true,
		VerdictHelping:       true,
		VerdictUnknown:       false,
		VerdictNotComparable: false,
		VerdictNotMeasurable: false,
	}
	for verdict, want := range cases {
		if got := ShowsDifference(verdict); got != want {
			t.Errorf("ShowsDifference(%q) = %v, want %v", verdict, got, want)
		}
	}
}

// An unrecognised verdict must not be treated as one that earned a number.
// Fail toward refusal: a future verdict added to the evaluator and not to the
// renderer should print no figure rather than an unqualified one.
func TestShowsDifferenceRefusesUnknownVerdict(t *testing.T) {
	if ShowsDifference("some_future_verdict") {
		t.Fatal("an unrecognised verdict must not show a difference")
	}
}

// The rendered line carries both arms always, and the difference only when the
// verdict earned it.
func TestRenderContextSuppressesTheNumber(t *testing.T) {
	base := ContextResult{
		ProjectID:  "acme",
		WorkflowID: "digest",
		Result: Result{
			Lift:              -0.42,
			TreatmentCoverage: 0.5,
			BaselineCoverage:  0.5,
			TreatmentN:        10, TreatmentUp: 4,
			BaselineN: 10, BaselineUp: 8,
		},
	}

	withNumber := base
	withNumber.Result.Verdict = VerdictLowLift
	got := RenderContext(withNumber)
	if !strings.Contains(got, "pp") {
		t.Errorf("low_lift must render the difference, got %q", got)
	}

	// not_comparable has arms and a computable lift, and is exactly the case
	// the phase-2 review found operators would misread: they ignore the badge
	// and quote the number. So the number must not be there to quote.
	suppressed := base
	suppressed.Result.Verdict = VerdictNotComparable
	got = RenderContext(suppressed)
	if strings.Contains(got, "pp") {
		t.Errorf("not_comparable must not render a difference, got %q", got)
	}
	// The arms still render — suppressing the difference must not suppress the
	// evidence it was computed from.
	if !strings.Contains(got, "4/10") || !strings.Contains(got, "8/10") {
		t.Errorf("both arms must render even when the difference is suppressed, got %q", got)
	}
}

// A superseded body is the one case where a real, well-evidenced figure exists
// and must still be withheld: the ratings judged a body the operator is not
// approving (design §2.2, round-2 review finding F2).
func TestRenderContextWithholdsASupersededBodysNumber(t *testing.T) {
	c := ContextResult{
		ProjectID:  "acme",
		WorkflowID: "digest",
		Result: Result{
			Verdict:           VerdictNotMeasurable,
			Cause:             CauseSupersededBody,
			Lift:              -0.42,
			TreatmentCoverage: 0.6, BaselineCoverage: 0.6,
			TreatmentN: 20, TreatmentUp: 5,
			BaselineN: 20, BaselineUp: 17,
		},
	}
	got := RenderContext(c)
	// Matches a rendered figure — "-42 pp", "60%", "5/20" — rather than the
	// letters "pp", which occur in ordinary words ("approving").
	if reFigure.MatchString(got) {
		t.Errorf("a superseded body must render no figure at all, got %q", got)
	}
	if !strings.Contains(got, VerdictSentence(VerdictNotMeasurable, CauseSupersededBody)) {
		t.Errorf("a superseded body must say why the number is withheld, got %q", got)
	}
}

// Contested executions were excluded from both arms; saying so is what keeps
// the exclusion from being silent (phase 2 §4.1).
func TestRenderContextSurfacesContested(t *testing.T) {
	c := ContextResult{
		ProjectID: "acme", WorkflowID: "digest",
		Result: Result{
			Verdict:    VerdictHelping,
			TreatmentN: 10, TreatmentUp: 9, TreatmentContested: 2,
			BaselineN: 10, BaselineUp: 9, BaselineContested: 1,
		},
	}
	got := RenderContext(c)
	if !strings.Contains(got, "3 contested") {
		t.Errorf("contested executions must be named, got %q", got)
	}
}
