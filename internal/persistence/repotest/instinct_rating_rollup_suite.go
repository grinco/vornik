package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// InstinctRatingRollupSeeder builds the fixtures the reported-arms query
// reads. Three tables no single repository owns: the step outcomes that define
// the (project, role, error_class) context, the applications that mark the
// treatment arm, and the ratings.
type InstinctRatingRollupSeeder interface {
	// SeedStepOutcome writes one step outcome. outcome "ok" means the step
	// succeeded; anything else is the failure that puts the execution into a
	// recovery context.
	SeedStepOutcome(ctx context.Context, executionID, stepID, projectID, role, errorClass, outcome string, at time.Time) error
	// SeedInstinctApplication writes one lead_recovery application.
	SeedInstinctApplication(ctx context.Context, instinctID, executionID, stepID, result string, at time.Time) error
	// SeedRating writes one human verdict.
	SeedRating(ctx context.Context, executionID, raterID, verdict string) error
}

// RunInstinctRatingRollupSuite is the backend-agnostic contract for
// persistence.InstinctRatingRollupRepository
// (LLD 2026-09-08-execution-ratings-approval-paths-design.md §3.1).
//
// What is worth asserting on BOTH backends is the same thing the skill rollup
// suite asserts: the per-execution grain. The query collapses per-(execution,
// rater) rows to one verdict per execution, and the two ways to get that wrong
// — counting a twice-rated execution twice, and silently picking a winner when
// raters disagree — are invisible in a round-trip test and differ between the
// planners.
//
// The arm split is asserted too, because it is the claim the whole design
// rests on: both arms drawn from ONE context, differing only in whether the
// instinct fired.
func RunInstinctRatingRollupSuite(t *testing.T,
	repo persistence.InstinctRatingRollupRepository, seed InstinctRatingRollupSeeder) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)

	const (
		inst  = "inst-rec-1"
		proj  = "proj-inst"
		role  = "coder"
		class = "context_timeout"
	)

	seedInstinctArmFixtures(ctx, t, seed, inst, proj, role, class, now)

	// One helper per rule, matching rating_rollup_suite.go's shape.
	t.Run("Both_arms_come_from_one_context_and_differ_only_in_the_intervention", func(t *testing.T) {
		instArmsSplit(ctx, t, repo, inst, proj, role, class, since)
	})
	t.Run("An_execution_rated_twice_the_same_way_counts_once", func(t *testing.T) {
		instArmsAgreeingRatersCountOnce(ctx, t, repo, inst, proj, role, class, since)
	})
	t.Run("An_execution_whose_raters_disagree_is_contested_and_in_neither_count", func(t *testing.T) {
		instArmsDisagreementIsContested(ctx, t, repo, inst, proj, role, class, since)
	})
	t.Run("A_different_error_class_is_a_different_context", func(t *testing.T) {
		instArmsContextConstrains(ctx, t, repo, inst, proj, role, since)
	})
	t.Run("An_empty_project_drops_the_constraint_for_a_global_instinct", func(t *testing.T) {
		instArmsGlobalScopeDropsProject(ctx, t, repo, inst, role, class, since)
	})
}

// mustInstArms reads the arms or fails the subtest.
func mustInstArms(ctx context.Context, t *testing.T, repo persistence.InstinctRatingRollupRepository,
	inst, proj, role, class string, since time.Time) persistence.InstinctRatingArms {
	t.Helper()
	got, err := repo.InstinctRecoveryRatingArms(ctx, inst, proj, role, class, since)
	if err != nil {
		t.Fatalf("InstinctRecoveryRatingArms: %v", err)
	}
	return got
}

func instArmsSplit(ctx context.Context, t *testing.T, repo persistence.InstinctRatingRollupRepository,
	inst, proj, role, class string, since time.Time) {
	t.Helper()
	got := mustInstArms(ctx, t, repo, inst, proj, role, class, since)
	if got.Treatment.EligibleN != 2 {
		t.Errorf("Treatment.EligibleN = %d, want 2", got.Treatment.EligibleN)
	}
	if got.Baseline.EligibleN != 2 {
		t.Errorf("Baseline.EligibleN = %d, want 2", got.Baseline.EligibleN)
	}
}

func instArmsAgreeingRatersCountOnce(ctx context.Context, t *testing.T, repo persistence.InstinctRatingRollupRepository,
	inst, proj, role, class string, since time.Time) {
	t.Helper()
	got := mustInstArms(ctx, t, repo, inst, proj, role, class, since)
	// Both treatment executions are rated; the second carries two agreeing
	// verdicts and must not inflate the count.
	if got.Treatment.RatedN != 2 {
		t.Errorf("Treatment.RatedN = %d, want 2 — two agreeing raters on one "+
			"execution are one observation, not two", got.Treatment.RatedN)
	}
	if got.Treatment.UpN != 0 {
		t.Errorf("Treatment.UpN = %d, want 0", got.Treatment.UpN)
	}
}

func instArmsDisagreementIsContested(ctx context.Context, t *testing.T, repo persistence.InstinctRatingRollupRepository,
	inst, proj, role, class string, since time.Time) {
	t.Helper()
	got := mustInstArms(ctx, t, repo, inst, proj, role, class, since)
	if got.Baseline.ContestedN != 1 {
		t.Errorf("Baseline.ContestedN = %d, want 1", got.Baseline.ContestedN)
	}
	if got.Baseline.RatedN != 1 {
		t.Errorf("Baseline.RatedN = %d, want 1 — a contested execution is "+
			"evidence about the raters, not about the instinct", got.Baseline.RatedN)
	}
	if got.Baseline.UpN != 1 {
		t.Errorf("Baseline.UpN = %d, want 1", got.Baseline.UpN)
	}
}

func instArmsContextConstrains(ctx context.Context, t *testing.T, repo persistence.InstinctRatingRollupRepository,
	inst, proj, role string, since time.Time) {
	t.Helper()
	got := mustInstArms(ctx, t, repo, inst, proj, role, "some_other_class", since)
	if got.Treatment.EligibleN != 0 || got.Baseline.EligibleN != 0 {
		t.Errorf("got %+v, want both arms empty — the context keys are what "+
			"make the arms comparable, so they must actually constrain", got)
	}
}

func instArmsGlobalScopeDropsProject(ctx context.Context, t *testing.T, repo persistence.InstinctRatingRollupRepository,
	inst, role, class string, since time.Time) {
	t.Helper()
	got := mustInstArms(ctx, t, repo, inst, "", role, class, since)
	if got.Treatment.EligibleN != 2 {
		t.Errorf("Treatment.EligibleN = %d, want 2; a global-scope instinct "+
			"drops the project constraint, matching RecoveryComplementOutcomes",
			got.Treatment.EligibleN)
	}
}

// seedInstinctArmFixtures builds four executions failing the same way in the
// same context. Two had the instinct applied and two did not, so the arms
// differ ONLY in the intervention — which is what makes them arms.
//
// The rating shapes are the grain rules in fixture form: one execution rated
// twice the SAME way (must count once) and one whose raters disagree (must
// count in neither arm).
func seedInstinctArmFixtures(ctx context.Context, t *testing.T, seed InstinctRatingRollupSeeder,
	inst, proj, role, class string, now time.Time) {
	t.Helper()
	type rating struct{ rater, verdict string }
	for _, f := range []struct {
		exec    string
		applied bool
		ratings []rating
	}{
		{exec: "exec-inst-t1", applied: true, ratings: []rating{{"op-a", "down"}}},
		{exec: "exec-inst-t2", applied: true, ratings: []rating{{"op-a", "down"}, {"op-b", "down"}}},
		{exec: "exec-inst-b1", applied: false, ratings: []rating{{"op-a", "up"}}},
		{exec: "exec-inst-b2", applied: false, ratings: []rating{{"op-a", "up"}, {"op-b", "down"}}},
	} {
		if err := seed.SeedStepOutcome(ctx, f.exec, "step-1", proj, role, class, "failed", now.Add(-time.Hour)); err != nil {
			t.Fatalf("SeedStepOutcome %s: %v", f.exec, err)
		}
		if f.applied {
			if err := seed.SeedInstinctApplication(ctx, inst, f.exec, "step-1", "succeeded", now.Add(-time.Hour)); err != nil {
				t.Fatalf("SeedInstinctApplication %s: %v", f.exec, err)
			}
		}
		for _, r := range f.ratings {
			if err := seed.SeedRating(ctx, f.exec, r.rater, r.verdict); err != nil {
				t.Fatalf("SeedRating %s/%s: %v", f.exec, r.rater, err)
			}
		}
	}
}
