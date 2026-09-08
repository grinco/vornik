package ratings

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type stubArmRepo struct {
	rows []persistence.SkillRatingArms
	err  error
}

func (s stubArmRepo) SkillRatingArms(context.Context, string, time.Time) ([]persistence.SkillRatingArms, error) {
	return s.rows, s.err
}

// ctxArmsEligible is the eligible count both arms carry in these cases.
//
// Fixed rather than a parameter: every case here varies coverage through how
// much was RATED, not through how much happened, because that is the axis the
// design's checks turn on. A hundred makes each rated count read directly as a
// percentage.
const ctxArmsEligible = 100

// ctxArms builds one context with both arms over ctxArmsEligible executions.
func ctxArms(project, workflow string, tRat, tUp, bRat, bUp int) persistence.SkillRatingArms {
	return persistence.SkillRatingArms{
		ProjectID:  project,
		WorkflowID: workflow,
		Treatment:  persistence.RatingArm{EligibleN: ctxArmsEligible, RatedN: tRat, UpN: tUp},
		Baseline:   persistence.RatingArm{EligibleN: ctxArmsEligible, RatedN: bRat, UpN: bUp},
	}
}

// One result per context, never pooled. A digest and a code review are not
// comparable outputs, so folding them together would put the difference between
// two workflows into the skill's lift.
func TestSkillRollup_OneResultPerContext(t *testing.T) {
	// Two workflows in one project, and the same workflow in a SECOND project.
	// Neither pair may pool: a digest and a review are different outputs, and
	// two projects' digests are different work by different people.
	repo := stubArmRepo{rows: []persistence.SkillRatingArms{
		ctxArms("p1", "digest", 20, 4, 20, 18),
		ctxArms("p1", "review", 20, 16, 20, 16),
		ctxArms("p2", "digest", 20, 17, 20, 16),
	}}
	got, err := SkillRollup(context.Background(), repo, "skill-x", 24*time.Hour, DefaultConfig())
	if err != nil {
		t.Fatalf("SkillRollup: %v", err)
	}
	if len(got.Contexts) != 3 {
		t.Fatalf("got %d contexts, want 3", len(got.Contexts))
	}
	if got.Contexts[2].ProjectID != "p2" || got.Contexts[2].WorkflowID != "digest" {
		t.Fatalf("the second project's digest was folded in: %+v", got.Contexts)
	}
	if got.Contexts[2].Result.Verdict != VerdictHelping {
		t.Errorf("p2 digest verdict = %q, want helping — it must not inherit p1's",
			got.Contexts[2].Result.Verdict)
	}
	if got.Contexts[0].Result.Verdict != VerdictLowLift {
		t.Errorf("digest verdict = %q, want low_lift", got.Contexts[0].Result.Verdict)
	}
	if got.Contexts[1].Result.Verdict != VerdictHelping {
		t.Errorf("review verdict = %q, want helping", got.Contexts[1].Result.Verdict)
	}
}

// The summary badge is the WORST verdict across contexts, because the operator's
// question is "is this hurting anywhere", not "on average".
func TestSkillRollup_SummaryIsTheWorstVerdict(t *testing.T) {
	cases := []struct {
		name string
		rows []persistence.SkillRatingArms
		want string
	}{
		{
			name: "one harmful context outranks a healthy one",
			rows: []persistence.SkillRatingArms{
				ctxArms("p1", "review", 20, 16, 20, 16), // helping
				ctxArms("p1", "digest", 20, 4, 20, 18),  // low_lift
			},
			want: VerdictLowLift,
		},
		{
			name: "an unusable context outranks a healthy one",
			rows: []persistence.SkillRatingArms{
				ctxArms("p1", "review", 20, 16, 20, 16), // helping
				ctxArms("p1", "digest", 40, 10, 10, 9),  // not_comparable
			},
			want: VerdictNotComparable,
		},
		{
			name: "not_comparable outranks unknown — there IS data, it is not usable",
			rows: []persistence.SkillRatingArms{
				ctxArms("p1", "a", 2, 1, 40, 36),  // unknown
				ctxArms("p1", "b", 40, 10, 10, 9), // not_comparable
			},
			want: VerdictNotComparable,
		},
		{
			name: "all healthy stays healthy",
			rows: []persistence.SkillRatingArms{
				ctxArms("p1", "a", 20, 16, 20, 16),
				ctxArms("p1", "b", 20, 17, 20, 16),
			},
			want: VerdictHelping,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SkillRollup(context.Background(), stubArmRepo{rows: tc.rows},
				"skill-x", 24*time.Hour, DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			if got.Summary != tc.want {
				t.Fatalf("summary = %q, want %q", got.Summary, tc.want)
			}
		})
	}
}

// A skill never injected in the window has no arms, and that is
// not_measurable — there is no attribution surface to join on, which is a
// different fact from "too few ratings".
func TestSkillRollup_NeverInjectedIsNotMeasurable(t *testing.T) {
	got, err := SkillRollup(context.Background(), stubArmRepo{}, "skill-x", 24*time.Hour, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != VerdictNotMeasurable {
		t.Fatalf("summary = %q, want not_measurable", got.Summary)
	}
	if len(got.Contexts) != 0 {
		t.Fatalf("got %d contexts", len(got.Contexts))
	}
}

func TestSkillRollup_PropagatesRepoErrors(t *testing.T) {
	boom := errors.New("query failed")
	_, err := SkillRollup(context.Background(), stubArmRepo{err: boom}, "skill-x", 24*time.Hour, DefaultConfig())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the repo's error", err)
	}
}

// The window is turned into an absolute cutoff by the service, so every context
// in one rollup is measured over the same span — arms compared across different
// windows are not arms.
func TestSkillRollup_UsesOneCutoffForEveryContext(t *testing.T) {
	var seen []time.Time
	repo := recordingRepo{fn: func(_ string, since time.Time) {
		seen = append(seen, since)
	}}
	if _, err := SkillRollup(context.Background(), repo, "skill-x", time.Hour, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("repo called %d times, want 1", len(seen))
	}
	if d := time.Since(seen[0]); d < 55*time.Minute || d > 65*time.Minute {
		t.Fatalf("cutoff was %v ago, want ~1h", d)
	}
}

type recordingRepo struct{ fn func(string, time.Time) }

func (r recordingRepo) SkillRatingArms(_ context.Context, skillID string, since time.Time) ([]persistence.SkillRatingArms, error) {
	r.fn(skillID, since)
	return nil, nil
}
