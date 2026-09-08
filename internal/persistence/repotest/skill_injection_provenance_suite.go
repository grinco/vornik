package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunSkillInjectionProvenanceSuite is the backend-agnostic contract for
// ExecutionInjectedSkillRepository.SkillInjectionProvenance — the query that
// tells an approval surface WHICH kind of "no usable evidence" it is looking
// at (LLD 2026-09-08-execution-ratings-approval-paths-design.md §4).
//
// The load-bearing case is the NULL row. Rows written before Postgres
// migration 180 have no recorded body, and counting one as a match for
// whatever is being approved today would make every historical rating look
// like evidence about the body under review. Both backends must count it as
// unknown, and they get there differently — Postgres NULLs versus SQLite's
// untyped NULL through an `any` parameter — so a round-trip test on one
// backend would not catch a divergence.
func RunSkillInjectionProvenanceSuite(t *testing.T,
	repo persistence.ExecutionInjectedSkillRepository, seed RatingRollupSeeder) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)

	const (
		skill   = "skill-prov"
		shaNew  = "sha-body-under-review"
		shaOld  = "sha-earlier-body"
		project = "proj-prov"
		flow    = "wf-prov"
	)

	// Three executions in the window, one per provenance class, plus one
	// outside it to prove the window is applied.
	for _, f := range []struct {
		exec string
		age  time.Duration
		sha  string
	}{
		{"exec-prov-match", time.Hour, shaNew},
		{"exec-prov-other", 2 * time.Hour, shaOld},
		{"exec-prov-null", 3 * time.Hour, ""},       // a row predating migration 180
		{"exec-prov-stale", 72 * time.Hour, shaNew}, // outside the window
	} {
		if err := seed.SeedExecution(ctx, f.exec, project, flow, now.Add(-f.age)); err != nil {
			t.Fatalf("SeedExecution %s: %v", f.exec, err)
		}
		if err := seed.SeedInjectedSkill(ctx, f.exec, skill, f.sha); err != nil {
			t.Fatalf("SeedInjectedSkill %s: %v", f.exec, err)
		}
	}

	t.Run("Splits_injections_by_how_the_body_relates_to_the_one_under_review", func(t *testing.T) {
		got, err := repo.SkillInjectionProvenance(ctx, skill, shaNew, since)
		if err != nil {
			t.Fatalf("SkillInjectionProvenance: %v", err)
		}
		if got.MatchingN != 1 {
			t.Errorf("MatchingN = %d, want 1 (only the in-window row on this body)", got.MatchingN)
		}
		if got.OtherBodyN != 1 {
			t.Errorf("OtherBodyN = %d, want 1", got.OtherBodyN)
		}
		// The assertion this suite exists for.
		if got.UnknownBodyN != 1 {
			t.Errorf("UnknownBodyN = %d, want 1 — a row with no recorded body must "+
				"count as unknown, never as a match for the body being approved",
				got.UnknownBodyN)
		}
	})

	t.Run("The_window_is_applied_on_the_executions_clock", func(t *testing.T) {
		// exec-prov-stale carries shaNew but sits outside the window. If the
		// window were applied to injected_at (always "now") instead of the
		// execution's created_at, it would be counted and MatchingN would be 2
		// — and the provenance counts would describe a different set of runs
		// than the arms they sit beside.
		got, err := repo.SkillInjectionProvenance(ctx, skill, shaNew, since)
		if err != nil {
			t.Fatalf("SkillInjectionProvenance: %v", err)
		}
		if got.MatchingN != 1 {
			t.Errorf("MatchingN = %d, want 1; an execution older than the window "+
				"must not be counted", got.MatchingN)
		}
	})

	t.Run("A_skill_never_injected_is_all_zeroes_not_an_error", func(t *testing.T) {
		got, err := repo.SkillInjectionProvenance(ctx, "skill-prov-absent", shaNew, since)
		if err != nil {
			t.Fatalf("SkillInjectionProvenance: %v", err)
		}
		if got != (persistence.InjectionProvenance{}) {
			t.Errorf("got %+v, want the zero value — never injected is a fact, not a failure", got)
		}
	})
}
