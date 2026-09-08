package ratings

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SkillRollupForBody is what an approval prompt calls. It answers a narrower
// question than SkillRollup: not "how is this skill doing" but "what do we
// know about the BODY in front of you" (LLD 2026-09-08-execution-ratings-
// approval-paths-design.md §4).

type fakeBodyArms struct {
	arms    []persistence.SkillRatingArms
	err     error
	gotSHA  string
	gotCall int
}

func (f *fakeBodyArms) SkillRatingArmsForBody(_ context.Context, _, sha string, _ time.Time) ([]persistence.SkillRatingArms, error) {
	f.gotSHA = sha
	f.gotCall++
	return f.arms, f.err
}

type fakeProvenance struct {
	prov persistence.InjectionProvenance
	err  error
}

func (f fakeProvenance) SkillInjectionProvenance(_ context.Context, _, _ string, _ time.Time) (persistence.InjectionProvenance, error) {
	return f.prov, f.err
}

// The four causes, decided from the provenance split. Each implies a different
// operator action, which is why the rollup must return which one it is rather
// than a bare not_measurable.
func TestSkillRollupForBodyDecidesTheCause(t *testing.T) {
	cases := []struct {
		name string
		prov persistence.InjectionProvenance
		want Cause
	}{
		{
			name: "never injected at all",
			prov: persistence.InjectionProvenance{},
			want: CauseAbsence,
		},
		{
			name: "injected, but every recorded body is a different one",
			prov: persistence.InjectionProvenance{OtherBodyN: 12},
			want: CauseSupersededBody,
		},
		{
			name: "injected, but no body was ever recorded",
			prov: persistence.InjectionProvenance{UnknownBodyN: 7},
			want: CauseUnknownProvenance,
		},
		{
			// A superseded body outranks unknown provenance: knowing the
			// ratings judged a DIFFERENT body is a stronger, more actionable
			// statement than knowing they cannot be placed at all.
			name: "both a superseded body and unrecorded ones",
			prov: persistence.InjectionProvenance{OtherBodyN: 3, UnknownBodyN: 9},
			want: CauseSupersededBody,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SkillRollupForBody(context.Background(),
				&fakeBodyArms{}, fakeProvenance{prov: tc.prov},
				"skill-1", "sha-under-review", time.Hour, DefaultConfig())
			if err != nil {
				t.Fatalf("SkillRollupForBody: %v", err)
			}
			if got.Summary != VerdictNotMeasurable {
				t.Fatalf("Summary = %q, want %q", got.Summary, VerdictNotMeasurable)
			}
			if got.Cause != tc.want {
				t.Errorf("Cause = %q, want %q", got.Cause, tc.want)
			}
		})
	}
}

// When the body under review HAS been rated, the arms are measured and the
// normal verdict machinery applies — scoped to that body.
func TestSkillRollupForBodyMeasuresTheBodyUnderReview(t *testing.T) {
	arms := &fakeBodyArms{arms: []persistence.SkillRatingArms{{
		SkillID: "skill-1", ProjectID: "acme", WorkflowID: "digest",
		Treatment: persistence.RatingArm{EligibleN: 20, RatedN: 10, UpN: 3},
		Baseline:  persistence.RatingArm{EligibleN: 20, RatedN: 10, UpN: 9},
	}}}
	got, err := SkillRollupForBody(context.Background(), arms,
		fakeProvenance{prov: persistence.InjectionProvenance{MatchingN: 10}},
		"skill-1", "sha-under-review", time.Hour, DefaultConfig())
	if err != nil {
		t.Fatalf("SkillRollupForBody: %v", err)
	}
	if arms.gotSHA != "sha-under-review" {
		t.Errorf("arms were read with sha %q; the approval prompt must be scoped "+
			"to the body being approved", arms.gotSHA)
	}
	if got.Summary != VerdictLowLift {
		t.Errorf("Summary = %q, want %q", got.Summary, VerdictLowLift)
	}
	if got.Cause != CauseNone {
		t.Errorf("Cause = %q, want empty — a measured verdict has no cause to explain", got.Cause)
	}
}

// A provenance read that fails must not be reported as "no evidence". Silence
// caused by a broken query and silence caused by an unrated skill look
// identical on the surface, and only one of them is a fact.
func TestSkillRollupForBodyPropagatesProvenanceErrors(t *testing.T) {
	_, err := SkillRollupForBody(context.Background(), &fakeBodyArms{},
		fakeProvenance{err: errors.New("boom")},
		"skill-1", "sha", time.Hour, DefaultConfig())
	if err == nil {
		t.Fatal("a failed provenance read must be an error, not an empty result")
	}
}

// The provenance query is only consulted when there is nothing to measure.
// A skill with usable arms should not pay for a second query.
func TestSkillRollupForBodySkipsProvenanceWhenThereAreArms(t *testing.T) {
	arms := &fakeBodyArms{arms: []persistence.SkillRatingArms{{
		SkillID: "skill-1", ProjectID: "acme", WorkflowID: "digest",
		Treatment: persistence.RatingArm{EligibleN: 20, RatedN: 10, UpN: 9},
		Baseline:  persistence.RatingArm{EligibleN: 20, RatedN: 10, UpN: 9},
	}}}
	got, err := SkillRollupForBody(context.Background(), arms,
		fakeProvenance{err: errors.New("must not be called")},
		"skill-1", "sha", time.Hour, DefaultConfig())
	if err != nil {
		t.Fatalf("SkillRollupForBody: %v", err)
	}
	if got.Summary != VerdictHelping {
		t.Errorf("Summary = %q, want %q", got.Summary, VerdictHelping)
	}
}

// An approval prompt must never render blank. Blank reports "examined and
// clean" and means "never examined".
func TestSkillApprovalLineNeverBlank(t *testing.T) {
	if got := SkillApprovalLine(context.Background(), nil, nil, "s", "sha"); got == "" {
		t.Error("with no repositories wired the line must still say something")
	}

	got := SkillApprovalLine(context.Background(), &fakeBodyArms{},
		fakeProvenance{err: errors.New("boom")}, "s", "sha")
	if got == "" {
		t.Fatal("a failed read must still say something")
	}
	// And it must say the evidence could not be READ, not that there is none.
	if got == VerdictSentence(VerdictNotMeasurable, CauseAbsence) {
		t.Error("a failed read must not be reported as an absence of ratings")
	}
}

// The line an operator sees for a re-approved skill whose ratings judged the
// previous body: the sentence, and no figure.
func TestSkillApprovalLineOnASupersededBody(t *testing.T) {
	got := SkillApprovalLine(context.Background(), &fakeBodyArms{},
		fakeProvenance{prov: persistence.InjectionProvenance{OtherBodyN: 40}}, "s", "sha-new")
	want := VerdictSentence(VerdictNotMeasurable, CauseSupersededBody)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
