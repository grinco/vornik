package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunExecutionRatingSuite is the backend-agnostic contract for
// persistence.ExecutionRatingRepository — the human verdict on what an
// execution produced (LLD 2026-09-04-execution-ratings-design).
//
// The properties here are the ones the design argues for rather than the ones
// an implementation happens to have. In particular the upsert must PRESERVE
// created_at: "when was this first judged" is the half of the record that
// survives a change of mind, and an implementation that rewrites it on every
// edit would still pass a naive round-trip test.
//
// Fixtures use the "exec-" hyphen prefix so a purge sweep can tell them from
// production "exec_" (underscore) ids, matching the sibling suites.
// suite's by construction — that shape is what gocognit requires here, and the
// twin dupl names (forge_pr_review_state_suite.go) is the same three lines of
// t.Run plumbing, not shared logic worth extracting.
//
//nolint:dupl // a flat t.Run dispatcher is structurally identical to every other
func RunExecutionRatingSuite(t *testing.T, repo persistence.ExecutionRatingRepository) {
	t.Helper()
	// One named function per case, so this dispatcher stays flat (gocognit) and
	// a failure names the behaviour — the shape skill_suite.go established.
	t.Run("Upsert_then_Get_round_trips", func(t *testing.T) { ratingUpsertThenGetRoundTrips(t, repo) })
	t.Run("Re_rating_replaces_the_verdict_and_preserves_created_at", func(t *testing.T) { ratingReRatingPreservesCreatedAt(t, repo) })
	t.Run("A_second_rater_is_a_second_row", func(t *testing.T) { ratingSecondRaterIsASecondRow(t, repo) })
	t.Run("Get_miss_contract", func(t *testing.T) { ratingGetMissContract(t, repo) })
	t.Run("Delete_removes_only_that_raters_row", func(t *testing.T) { ratingDeleteRemovesOnlyThatRater(t, repo) })
	t.Run("Delete_of_an_absent_rating_is_not_an_error", func(t *testing.T) { ratingDeleteOfAbsentIsNotAnError(t, repo) })
	t.Run("Rating_an_execution_that_does_not_exist_is_stored", func(t *testing.T) { ratingUnknownExecutionIsStored(t, repo) })
	t.Run("ListByExecution_is_empty_for_an_unrated_execution", func(t *testing.T) { ratingListIsEmptyWhenUnrated(t, repo) })
	t.Run("DeleteOlderThan_prunes_by_horizon_and_spares_the_rest", func(t *testing.T) { ratingDeleteOlderThanPrunesByHorizon(t, repo) })
}

func ratingUpsertThenGetRoundTrips(t *testing.T, repo persistence.ExecutionRatingRepository) {
	ctx := context.Background()
	want := &persistence.ExecutionRating{
		ExecutionID: "exec-rating-1",
		RaterID:     "op-a",
		Verdict:     persistence.VerdictUp,
		Reason:      "tight and on format",
	}
	if err := repo.Upsert(ctx, want); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := repo.Get(ctx, "exec-rating-1", "op-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Verdict != persistence.VerdictUp || got.Reason != "tight and on format" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not populated: %+v", got)
	}
}

func ratingReRatingPreservesCreatedAt(t *testing.T, repo persistence.ExecutionRatingRepository) {
	ctx := context.Background()
	first := &persistence.ExecutionRating{
		ExecutionID: "exec-rating-2",
		RaterID:     "op-a",
		Verdict:     persistence.VerdictDown,
		Reason:      "thin",
	}
	if err := repo.Upsert(ctx, first); err != nil {
		t.Fatalf("Upsert first: %v", err)
	}
	before, err := repo.Get(ctx, "exec-rating-2", "op-a")
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	// Just enough to guarantee a distinct instant. Both backends store
	// sub-second times (SQLite RFC3339Nano, Postgres microseconds), so this
	// does not need to be a second — an earlier draft slept 1.1s on the
	// false premise that one of them stored whole seconds, which would have
	// added 2.2s to every run of this suite for nothing.
	time.Sleep(5 * time.Millisecond)

	second := &persistence.ExecutionRating{
		ExecutionID: "exec-rating-2",
		RaterID:     "op-a",
		Verdict:     persistence.VerdictUp,
		Reason:      "reread it; it was fine",
	}
	if err := repo.Upsert(ctx, second); err != nil {
		t.Fatalf("Upsert second: %v", err)
	}
	after, err := repo.Get(ctx, "exec-rating-2", "op-a")
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}

	if after.Verdict != persistence.VerdictUp {
		t.Errorf("verdict = %q, want the replacement", after.Verdict)
	}
	if after.Reason != "reread it; it was fine" {
		t.Errorf("reason = %q, want the replacement", after.Reason)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("created_at moved on edit: %v -> %v; when a run was FIRST judged "+
			"is the half of the record that survives a change of mind",
			before.CreatedAt, after.CreatedAt)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("updated_at did not move: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}

	// One row, not two.
	all, err := repo.ListByExecution(ctx, "exec-rating-2")
	if err != nil {
		t.Fatalf("ListByExecution: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("re-rating created %d rows, want 1 upserted row", len(all))
	}
}

func ratingSecondRaterIsASecondRow(t *testing.T, repo persistence.ExecutionRatingRepository) {
	ctx := context.Background()
	for _, r := range []struct{ rater, verdict string }{
		{"op-a", persistence.VerdictUp},
		{"op-b", persistence.VerdictDown},
	} {
		if err := repo.Upsert(ctx, &persistence.ExecutionRating{
			ExecutionID: "exec-rating-3", RaterID: r.rater, Verdict: r.verdict,
		}); err != nil {
			t.Fatalf("Upsert %s: %v", r.rater, err)
		}
	}
	all, err := repo.ListByExecution(ctx, "exec-rating-3")
	if err != nil {
		t.Fatalf("ListByExecution: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d ratings, want one per rater", len(all))
	}
	byRater := map[string]string{}
	for _, r := range all {
		byRater[r.RaterID] = r.Verdict
	}
	if byRater["op-a"] != persistence.VerdictUp || byRater["op-b"] != persistence.VerdictDown {
		t.Fatalf("raters overwrote each other: %v", byRater)
	}
}

func ratingGetMissContract(t *testing.T, repo persistence.ExecutionRatingRepository) {
	// No ctx of its own: AssertMissRepo supplies one to the probe it drives.
	AssertMissRepo(t, "ExecutionRatingRepository.Get",
		func(ctx context.Context, id string) (*persistence.ExecutionRating, error) {
			return repo.Get(ctx, id, "op-nobody")
		})
}

func ratingDeleteRemovesOnlyThatRater(t *testing.T, repo persistence.ExecutionRatingRepository) {
	ctx := context.Background()
	for _, rater := range []string{"op-a", "op-b"} {
		if err := repo.Upsert(ctx, &persistence.ExecutionRating{
			ExecutionID: "exec-rating-4", RaterID: rater, Verdict: persistence.VerdictUp,
		}); err != nil {
			t.Fatalf("Upsert %s: %v", rater, err)
		}
	}
	if err := repo.Delete(ctx, "exec-rating-4", "op-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	all, err := repo.ListByExecution(ctx, "exec-rating-4")
	if err != nil {
		t.Fatalf("ListByExecution: %v", err)
	}
	if len(all) != 1 || all[0].RaterID != "op-b" {
		t.Fatalf("delete hit the wrong rows: %+v", all)
	}
}

func ratingDeleteOfAbsentIsNotAnError(t *testing.T, repo persistence.ExecutionRatingRepository) {
	ctx := context.Background()
	if err := repo.Delete(ctx, "exec-rating-never", "op-a"); err != nil {
		t.Fatalf("Delete of an absent rating: %v", err)
	}
}

func ratingUnknownExecutionIsStored(t *testing.T, repo persistence.ExecutionRatingRepository) {
	ctx := context.Background()
	if err := repo.Upsert(ctx, &persistence.ExecutionRating{
		ExecutionID: "exec-rating-pruned", RaterID: "op-a", Verdict: persistence.VerdictDown,
	}); err != nil {
		t.Fatalf("Upsert against an unknown execution: %v — a rating must not "+
			"depend on the run still existing", err)
	}
	if _, err := repo.Get(ctx, "exec-rating-pruned", "op-a"); err != nil {
		t.Fatalf("Get: %v", err)
	}
}

func ratingListIsEmptyWhenUnrated(t *testing.T, repo persistence.ExecutionRatingRepository) {
	ctx := context.Background()
	got, err := repo.ListByExecution(ctx, "exec-rating-unrated")
	if err != nil {
		t.Fatalf("ListByExecution: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d ratings for an unrated execution", len(got))
	}
}

func ratingDeleteOlderThanPrunesByHorizon(t *testing.T, repo persistence.ExecutionRatingRepository) {
	ctx := context.Background()
	old := &persistence.ExecutionRating{
		ExecutionID: "exec-rating-old", RaterID: "op-a", Verdict: persistence.VerdictUp,
		CreatedAt: time.Now().UTC().Add(-500 * 24 * time.Hour),
	}
	fresh := &persistence.ExecutionRating{
		ExecutionID: "exec-rating-fresh", RaterID: "op-a", Verdict: persistence.VerdictUp,
	}
	if err := repo.Upsert(ctx, old); err != nil {
		t.Fatalf("Upsert old: %v", err)
	}
	if err := repo.Upsert(ctx, fresh); err != nil {
		t.Fatalf("Upsert fresh: %v", err)
	}

	cutoff := time.Now().UTC().Add(-400 * 24 * time.Hour)
	n, err := repo.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("DeleteOlderThan: %v", err)
	}
	if n < 1 {
		t.Errorf("pruned %d rows, want at least the one past the horizon", n)
	}
	if _, err := repo.Get(ctx, "exec-rating-old", "op-a"); err == nil {
		t.Error("a rating past the horizon survived the prune")
	}
	if _, err := repo.Get(ctx, "exec-rating-fresh", "op-a"); err != nil {
		t.Errorf("a rating INSIDE the horizon was pruned: %v", err)
	}
}
