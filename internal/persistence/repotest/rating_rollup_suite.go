package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RatingRollupSeeder lets the suite build fixtures without knowing the backend.
// The rollup query reads executions, execution_injected_skills and
// execution_ratings together, which no single repository owns.
type RatingRollupSeeder interface {
	SeedExecution(ctx context.Context, id, projectID, workflowID string, createdAt time.Time) error
	// SeedInjectedSkill records an injection. An empty bodySHA256 must land as
	// NULL, so the suite can assert the pre-migration-180 row behaves as
	// unknown provenance rather than as a match.
	SeedInjectedSkill(ctx context.Context, executionID, skillID, bodySHA256 string) error
	SeedRating(ctx context.Context, executionID, raterID, verdict string) error
}

// RunRatingRollupSuite is the backend-agnostic contract for
// persistence.RatingRollupRepository.
//
// The grain rules are the point. The query collapses per-(execution, rater)
// ratings to a per-execution verdict, and the two ways to get that wrong —
// counting a twice-rated execution twice, and silently picking a winner when
// raters disagree — are both asserted here, because the SQL that gets them
// right differs between the backends (Postgres FILTER versus portable
// SUM/CASE) and a divergence would be invisible in a round-trip test.
func RunRatingRollupSuite(t *testing.T, repo persistence.RatingRollupRepository, seed RatingRollupSeeder) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)

	t.Run("Treatment_and_baseline_share_a_context_and_window", func(t *testing.T) {
		ratingRollupTreatmentAndBaseline(ctx, t, repo, seed, now, since)
	})
	t.Run("An_execution_rated_twice_the_same_way_counts_once", func(t *testing.T) {
		ratingRollupAgreeingRatersCountOnce(ctx, t, repo, seed, now, since)
	})
	t.Run("An_execution_whose_raters_disagree_is_contested", func(t *testing.T) {
		ratingRollupDisagreementIsContested(ctx, t, repo, seed, now, since)
	})
	t.Run("A_skill_never_injected_returns_no_rows", func(t *testing.T) {
		got, err := repo.SkillRatingArms(ctx, "skill-never-injected", since)
		if err != nil {
			t.Fatalf("SkillRatingArms: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d rows for a skill that was never injected", len(got))
		}
	})
}

func ratingRollupTreatmentAndBaseline(ctx context.Context, t *testing.T,
	repo persistence.RatingRollupRepository, seed RatingRollupSeeder, now, since time.Time) {
	const (
		skill = "skill-rollup-a"
		proj  = "p-rollup-a"
		wf    = "digest"
	)
	// Two treated executions, one rated up; two untreated in the SAME context,
	// one rated down. Eligible counts both rated and unrated, which is what
	// makes coverage computable at all.
	mustSeedExec(ctx, t, seed, "exec-rr-t1", proj, wf, now)
	mustSeedExec(ctx, t, seed, "exec-rr-t2", proj, wf, now)
	mustSeedExec(ctx, t, seed, "exec-rr-b1", proj, wf, now)
	mustSeedExec(ctx, t, seed, "exec-rr-b2", proj, wf, now)
	// A different workflow, treated — must be its OWN row, not folded in: a
	// digest and a code review are not comparable outputs.
	mustSeedExec(ctx, t, seed, "exec-rr-other", proj, "review", now)

	for _, id := range []string{"exec-rr-t1", "exec-rr-t2", "exec-rr-other"} {
		if err := seed.SeedInjectedSkill(ctx, id, skill, ""); err != nil {
			t.Fatalf("seed injected skill %s: %v", id, err)
		}
	}
	mustSeedRating(ctx, t, seed, "exec-rr-t1", "op-a", "up")
	mustSeedRating(ctx, t, seed, "exec-rr-b1", "op-a", "down")

	rows, err := repo.SkillRatingArms(ctx, skill, since)
	if err != nil {
		t.Fatalf("SkillRatingArms: %v", err)
	}
	row := findContext(t, rows, proj, wf)
	if row.Treatment.EligibleN != 2 || row.Treatment.RatedN != 1 || row.Treatment.UpN != 1 {
		t.Errorf("treatment = %+v, want eligible 2 rated 1 up 1", row.Treatment)
	}
	if row.Baseline.EligibleN != 2 || row.Baseline.RatedN != 1 || row.Baseline.UpN != 0 {
		t.Errorf("baseline = %+v, want eligible 2 rated 1 up 0", row.Baseline)
	}
	if findContextOrNil(rows, proj, "review") == nil {
		t.Error("the other workflow was folded into this context instead of being its own row")
	}
}

func ratingRollupAgreeingRatersCountOnce(ctx context.Context, t *testing.T,
	repo persistence.RatingRollupRepository, seed RatingRollupSeeder, now, since time.Time) {
	const (
		skill = "skill-rollup-b"
		proj  = "p-rollup-b"
		wf    = "digest"
	)
	mustSeedExec(ctx, t, seed, "exec-rr-twice", proj, wf, now)
	mustSeedExec(ctx, t, seed, "exec-rr-base-b", proj, wf, now)
	if err := seed.SeedInjectedSkill(ctx, "exec-rr-twice", skill, ""); err != nil {
		t.Fatalf("seed injected skill: %v", err)
	}
	// Two operators, same verdict. One execution.
	mustSeedRating(ctx, t, seed, "exec-rr-twice", "op-a", "up")
	mustSeedRating(ctx, t, seed, "exec-rr-twice", "op-b", "up")

	rows, err := repo.SkillRatingArms(ctx, skill, since)
	if err != nil {
		t.Fatalf("SkillRatingArms: %v", err)
	}
	row := findContext(t, rows, proj, wf)
	if row.Treatment.RatedN != 1 || row.Treatment.UpN != 1 {
		t.Fatalf("treatment = %+v, want rated 1 up 1 — two agreeing raters must "+
			"not weight one execution twice", row.Treatment)
	}
	if row.Treatment.ContestedN != 0 {
		t.Errorf("agreeing raters were recorded as contested: %+v", row.Treatment)
	}
}

func ratingRollupDisagreementIsContested(ctx context.Context, t *testing.T,
	repo persistence.RatingRollupRepository, seed RatingRollupSeeder, now, since time.Time) {
	const (
		skill = "skill-rollup-c"
		proj  = "p-rollup-c"
		wf    = "digest"
	)
	mustSeedExec(ctx, t, seed, "exec-rr-split", proj, wf, now)
	mustSeedExec(ctx, t, seed, "exec-rr-base-c", proj, wf, now)
	if err := seed.SeedInjectedSkill(ctx, "exec-rr-split", skill, ""); err != nil {
		t.Fatalf("seed injected skill: %v", err)
	}
	mustSeedRating(ctx, t, seed, "exec-rr-split", "op-a", "up")
	mustSeedRating(ctx, t, seed, "exec-rr-split", "op-b", "down")

	rows, err := repo.SkillRatingArms(ctx, skill, since)
	if err != nil {
		t.Fatalf("SkillRatingArms: %v", err)
	}
	row := findContext(t, rows, proj, wf)
	if row.Treatment.ContestedN != 1 {
		t.Fatalf("treatment = %+v, want contested 1", row.Treatment)
	}
	// Excluded from BOTH, not silently resolved to one side. A disagreement is
	// evidence about the raters, not about the skill.
	if row.Treatment.RatedN != 0 || row.Treatment.UpN != 0 {
		t.Fatalf("treatment = %+v, want rated 0 up 0 — a contested execution "+
			"must not be resolved into an arm", row.Treatment)
	}
	// Still eligible: it happened, and coverage is about what was observable.
	if row.Treatment.EligibleN != 1 {
		t.Errorf("treatment = %+v, want eligible 1", row.Treatment)
	}
}

func mustSeedExec(ctx context.Context, t *testing.T, seed RatingRollupSeeder,
	id, project, workflow string, at time.Time) {
	t.Helper()
	if err := seed.SeedExecution(ctx, id, project, workflow, at); err != nil {
		t.Fatalf("seed execution %s: %v", id, err)
	}
}

func mustSeedRating(ctx context.Context, t *testing.T, seed RatingRollupSeeder,
	executionID, rater, verdict string) {
	t.Helper()
	if err := seed.SeedRating(ctx, executionID, rater, verdict); err != nil {
		t.Fatalf("seed rating %s/%s: %v", executionID, rater, err)
	}
}

func findContext(t *testing.T, rows []persistence.SkillRatingArms, project, workflow string) persistence.SkillRatingArms {
	t.Helper()
	if r := findContextOrNil(rows, project, workflow); r != nil {
		return *r
	}
	t.Fatalf("no row for (%s, %s) in %+v", project, workflow, rows)
	return persistence.SkillRatingArms{}
}

func findContextOrNil(rows []persistence.SkillRatingArms, project, workflow string) *persistence.SkillRatingArms {
	for i := range rows {
		if rows[i].ProjectID == project && rows[i].WorkflowID == workflow {
			return &rows[i]
		}
	}
	return nil
}
