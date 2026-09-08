package ratings

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type fakeInstinctArms struct {
	arms   persistence.InstinctRatingArms
	err    error
	called int
}

func (f *fakeInstinctArms) InstinctRecoveryRatingArms(_ context.Context, _, _, _, _ string, _ time.Time) (persistence.InstinctRatingArms, error) {
	f.called++
	return f.arms, f.err
}

// Budget and architect record no execution_id on their applications, so there
// is nothing for a per-execution rating to attach to — ever. That must reach
// the operator as its own sentence, and must not cost a query.
func TestInstinctRollupRefusesTheDomainsThatCannotBeMeasured(t *testing.T) {
	for _, domain := range []string{"cost", "quality", "workflow", "retrieval"} {
		repo := &fakeInstinctArms{}
		got, err := InstinctRollup(context.Background(), repo, "inst-1", domain,
			"proj", "coder", "context_timeout", time.Hour, DefaultConfig())
		if err != nil {
			t.Fatalf("InstinctRollup(%s): %v", domain, err)
		}
		if got.Result.Verdict != VerdictNotMeasurable {
			t.Errorf("domain %s: Verdict = %q, want %q", domain, got.Result.Verdict, VerdictNotMeasurable)
		}
		if got.Result.Cause != CauseConstruction {
			t.Errorf("domain %s: Cause = %q, want %q — this is a property of the "+
				"schema, not a shortage of ratings", domain, got.Result.Cause, CauseConstruction)
		}
		if repo.called != 0 {
			t.Errorf("domain %s: queried the database for a question the schema already answers", domain)
		}
	}
}

// An empty context is absence, not construction: recovery CAN be rated, and
// this one simply has not been.
func TestInstinctRollupDistinguishesAbsenceFromConstruction(t *testing.T) {
	got, err := InstinctRollup(context.Background(), &fakeInstinctArms{},
		"inst-1", "recovery", "proj", "coder", "context_timeout", time.Hour, DefaultConfig())
	if err != nil {
		t.Fatalf("InstinctRollup: %v", err)
	}
	if got.Result.Cause != CauseAbsence {
		t.Errorf("Cause = %q, want %q", got.Result.Cause, CauseAbsence)
	}
}

func TestInstinctRollupEvaluatesRecoveryArms(t *testing.T) {
	repo := &fakeInstinctArms{arms: persistence.InstinctRatingArms{
		Treatment: persistence.RatingArm{EligibleN: 20, RatedN: 10, UpN: 3},
		Baseline:  persistence.RatingArm{EligibleN: 20, RatedN: 10, UpN: 9},
	}}
	got, err := InstinctRollup(context.Background(), repo, "inst-1", "recovery",
		"proj", "coder", "context_timeout", time.Hour, DefaultConfig())
	if err != nil {
		t.Fatalf("InstinctRollup: %v", err)
	}
	if got.Result.Verdict != VerdictLowLift {
		t.Errorf("Verdict = %q, want %q", got.Result.Verdict, VerdictLowLift)
	}
	if got.Result.Cause != CauseNone {
		t.Errorf("Cause = %q, want empty — a measured verdict explains itself", got.Result.Cause)
	}
}

func TestInstinctRollupPropagatesReadErrors(t *testing.T) {
	_, err := InstinctRollup(context.Background(), &fakeInstinctArms{err: errors.New("boom")},
		"inst-1", "recovery", "proj", "coder", "context_timeout", time.Hour, DefaultConfig())
	if err == nil {
		t.Fatal("a failed arm read must be an error, not a silent not_measurable")
	}
}

// The disagreement gate. Only two verdicts stand behind a number, so only they
// can disagree.
func TestDisagrees(t *testing.T) {
	cases := []struct {
		measured, reported string
		want               bool
	}{
		// The case the retire proposal exists to surface: the machine says
		// retire, the people who saw the output did not object.
		{VerdictLowLift, VerdictHelping, true},
		{VerdictHelping, VerdictLowLift, true},
		// Agreement is not a disagreement.
		{VerdictLowLift, VerdictLowLift, false},
		{VerdictHelping, VerdictHelping, false},
		// not_comparable HAS arms and a computable lift, and is still not a
		// disagreement: the two sides were watched at different rates, so
		// there is nothing to set against anything.
		{VerdictLowLift, VerdictNotComparable, false},
		{VerdictNotComparable, VerdictLowLift, false},
		// Nothing to disagree with.
		{VerdictLowLift, VerdictUnknown, false},
		{VerdictLowLift, VerdictNotMeasurable, false},
	}
	for _, c := range cases {
		if got := Disagrees(c.measured, c.reported); got != c.want {
			t.Errorf("Disagrees(%q, %q) = %v, want %v", c.measured, c.reported, got, c.want)
		}
	}
}
