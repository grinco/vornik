package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

const forgeStateProject = "p-forge"
const forgeStateRepo = "acme/api"

// RunForgePRReviewStateSuite is the backend-agnostic conformance suite for the
// per-PR re-review state.
//
// Design: https://docs.vornik.io
//
// Both backends must agree, because the coalescing correctness argument is
// stated once in the design and has no "on sqlite, however" clause.
//
// Each case uses its OWN pull-request number so the suite has no order
// dependence: a case that only passes because an earlier one left the row in a
// particular shape is not testing the behaviour it claims to.
func RunForgePRReviewStateSuite(t *testing.T, repo persistence.ForgePRReviewStateRepository) {
	t.Helper()
	t.Run("MissingRowIsNotAnError", func(t *testing.T) { forgeMissingRow(t, repo) })
	t.Run("FirstClaimReportsNoPriorTask", func(t *testing.T) { forgeFirstClaim(t, repo) })
	t.Run("SupersedeReturnsHolderAndAdvancesSHA", func(t *testing.T) { forgeSupersede(t, repo) })
	t.Run("MarkReviewedAdvancesBaselineAndReleasesClaim", func(t *testing.T) { forgeMarkReviewed(t, repo) })
	t.Run("PauseRoundTrips", func(t *testing.T) { forgePause(t, repo) })
	t.Run("StateIsPerProjectNotPerRepo", func(t *testing.T) { forgePerProject(t, repo) })
	t.Run("BeginClosingReleasesTheClaimAndRecordsTheHead", func(t *testing.T) { forgeBeginClosing(t, repo) })
}

// A PR with no state is the ordinary first-delivery case, never an error.
func forgeMissingRow(t *testing.T, repo persistence.ForgePRReviewStateRepository) {
	got, err := repo.Get(context.Background(), forgeStateProject, "acme/never-seen", 999)
	if err != nil {
		t.Fatalf("Get on a missing row returned an error: %v", err)
	}
	if got != nil {
		t.Fatalf("Get on a missing row returned %+v, want nil", got)
	}
}

// Empty prior means "nobody was reviewing", so the caller enqueues. Anything
// else and the very first push for a PR is absorbed into a task that does not
// exist — the PR would never be reviewed.
func forgeFirstClaim(t *testing.T, repo persistence.ForgePRReviewStateRepository) {
	ctx := context.Background()
	const num = 101
	prior, err := repo.ClaimOrSupersede(ctx, forgeStateProject, forgeStateRepo, num, "sha-a")
	if err != nil {
		t.Fatalf("ClaimOrSupersede: %v", err)
	}
	if prior != "" {
		t.Fatalf("prior task = %q on a first claim, want empty", prior)
	}
	st, err := repo.Get(ctx, forgeStateProject, forgeStateRepo, num)
	if err != nil || st == nil {
		t.Fatalf("Get after claim: %v (state %+v)", err, st)
	}
	if st.PendingHeadSHA != "sha-a" {
		t.Errorf("PendingHeadSHA = %q, want sha-a", st.PendingHeadSHA)
	}
}

// The caller must learn WHICH task holds the PR, so it can ask that task
// whether it is still ABSORBING — and the supersede must advance the head
// without disturbing the claim.
func forgeSupersede(t *testing.T, repo persistence.ForgePRReviewStateRepository) {
	ctx := context.Background()
	const num = 102
	if _, err := repo.ClaimOrSupersede(ctx, forgeStateProject, forgeStateRepo, num, "sha-a"); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if err := repo.SetTask(ctx, forgeStateProject, forgeStateRepo, num, "task-1"); err != nil {
		t.Fatalf("SetTask: %v", err)
	}
	prior, err := repo.ClaimOrSupersede(ctx, forgeStateProject, forgeStateRepo, num, "sha-b")
	if err != nil {
		t.Fatalf("ClaimOrSupersede: %v", err)
	}
	if prior != "task-1" {
		t.Fatalf("prior task = %q, want task-1", prior)
	}
	st, err := repo.Get(ctx, forgeStateProject, forgeStateRepo, num)
	if err != nil || st == nil {
		t.Fatalf("Get: %v", err)
	}
	if st.PendingHeadSHA != "sha-b" {
		t.Errorf("PendingHeadSHA = %q, want sha-b — a supersede must advance the head", st.PendingHeadSHA)
	}
	if st.TaskID != "task-1" {
		t.Errorf("TaskID = %q after supersede, want task-1 undisturbed", st.TaskID)
	}
}

// Releasing the claim is what lets the NEXT push enqueue rather than supersede
// into a task that has already finished.
func forgeMarkReviewed(t *testing.T, repo persistence.ForgePRReviewStateRepository) {
	ctx := context.Background()
	const num = 103
	if _, err := repo.ClaimOrSupersede(ctx, forgeStateProject, forgeStateRepo, num, "sha-b"); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if err := repo.SetTask(ctx, forgeStateProject, forgeStateRepo, num, "task-1"); err != nil {
		t.Fatalf("SetTask: %v", err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	if err := repo.MarkReviewed(ctx, forgeStateProject, forgeStateRepo, num, "sha-b", at); err != nil {
		t.Fatalf("MarkReviewed: %v", err)
	}
	st, err := repo.Get(ctx, forgeStateProject, forgeStateRepo, num)
	if err != nil || st == nil {
		t.Fatalf("Get: %v", err)
	}
	if st.LastReviewedHeadSHA != "sha-b" {
		t.Errorf("LastReviewedHeadSHA = %q, want sha-b", st.LastReviewedHeadSHA)
	}
	if st.TaskID != "" {
		t.Errorf("TaskID = %q after MarkReviewed, want empty (claim released)", st.TaskID)
	}
	if st.LastReviewedAt == nil {
		t.Fatal("LastReviewedAt is nil after MarkReviewed")
	}
	if d := st.LastReviewedAt.Sub(at); d > time.Second || d < -time.Second {
		t.Errorf("LastReviewedAt = %v, want ~%v", st.LastReviewedAt, at)
	}
}

// Pausing must not disturb the review baseline: silencing a noisy PR should not
// also turn its next review into a full-diff one.
func forgePause(t *testing.T, repo persistence.ForgePRReviewStateRepository) {
	ctx := context.Background()
	const num = 104
	at := time.Now().UTC().Truncate(time.Second)
	if err := repo.MarkReviewed(ctx, forgeStateProject, forgeStateRepo, num, "sha-b", at); err != nil {
		t.Fatalf("seed baseline: %v", err)
	}
	if err := repo.SetPaused(ctx, forgeStateProject, forgeStateRepo, num, true); err != nil {
		t.Fatalf("SetPaused(true): %v", err)
	}
	st, err := repo.Get(ctx, forgeStateProject, forgeStateRepo, num)
	if err != nil || st == nil {
		t.Fatalf("Get: %v", err)
	}
	if !st.AutoReviewPaused {
		t.Error("AutoReviewPaused = false after SetPaused(true)")
	}
	if st.LastReviewedHeadSHA != "sha-b" {
		t.Errorf("LastReviewedHeadSHA = %q after pause, want sha-b untouched", st.LastReviewedHeadSHA)
	}
	if err := repo.SetPaused(ctx, forgeStateProject, forgeStateRepo, num, false); err != nil {
		t.Fatalf("SetPaused(false): %v", err)
	}
	st, err = repo.Get(ctx, forgeStateProject, forgeStateRepo, num)
	if err != nil || st == nil {
		t.Fatalf("Get after unpause: %v", err)
	}
	if st.AutoReviewPaused {
		t.Error("AutoReviewPaused = true after SetPaused(false)")
	}
}

// One installation can serve several projects. Two projects watching the SAME
// repo and PR must not share state, or pausing one silences the other.
func forgePerProject(t *testing.T, repo persistence.ForgePRReviewStateRepository) {
	ctx := context.Background()
	const num = 105
	if _, err := repo.ClaimOrSupersede(ctx, forgeStateProject, forgeStateRepo, num, "sha-mine"); err != nil {
		t.Fatalf("seed first project: %v", err)
	}
	if err := repo.SetTask(ctx, forgeStateProject, forgeStateRepo, num, "task-mine"); err != nil {
		t.Fatalf("SetTask(first project): %v", err)
	}
	if _, err := repo.ClaimOrSupersede(ctx, "p-forge-other", forgeStateRepo, num, "sha-theirs"); err != nil {
		t.Fatalf("ClaimOrSupersede(other project): %v", err)
	}
	if err := repo.SetTask(ctx, "p-forge-other", forgeStateRepo, num, "task-theirs"); err != nil {
		t.Fatalf("SetTask(other project): %v", err)
	}
	mine, err := repo.Get(ctx, forgeStateProject, forgeStateRepo, num)
	if err != nil || mine == nil {
		t.Fatalf("Get(first project): %v", err)
	}
	if mine.TaskID != "task-mine" {
		t.Errorf("TaskID = %q — a second project's claim overwrote the first; the key is not project-scoped", mine.TaskID)
	}
	if mine.PendingHeadSHA != "sha-mine" {
		t.Errorf("PendingHeadSHA = %q — a second project's head SHA leaked across projects", mine.PendingHeadSHA)
	}
}

// BeginClosing is the ABSORBING → CLOSING transition: it must release the claim
// AND record the head the review fetched, in one step. Releasing without
// recording would leave the baseline unable to advance to what was actually
// reviewed; recording without releasing would keep absorbing pushes into a
// review that has already read its diff.
func forgeBeginClosing(t *testing.T, repo persistence.ForgePRReviewStateRepository) {
	ctx := context.Background()
	const num = 106
	if _, err := repo.ClaimOrSupersede(ctx, forgeStateProject, forgeStateRepo, num, "sha-a"); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if err := repo.SetTask(ctx, forgeStateProject, forgeStateRepo, num, "task-1"); err != nil {
		t.Fatalf("SetTask: %v", err)
	}
	if err := repo.BeginClosing(ctx, forgeStateProject, forgeStateRepo, num, "sha-fetched"); err != nil {
		t.Fatalf("BeginClosing: %v", err)
	}
	st, err := repo.Get(ctx, forgeStateProject, forgeStateRepo, num)
	if err != nil || st == nil {
		t.Fatalf("Get: %v", err)
	}
	if st.TaskID != "" {
		t.Errorf("TaskID = %q after BeginClosing, want empty — the review is still absorbing pushes it cannot see", st.TaskID)
	}
	if st.ReviewingHeadSHA != "sha-fetched" {
		t.Errorf("ReviewingHeadSHA = %q, want sha-fetched", st.ReviewingHeadSHA)
	}

	// A push landing now supersedes pending WITHOUT disturbing what is being
	// reviewed — the two SHAs are deliberately separate fields.
	if _, err := repo.ClaimOrSupersede(ctx, forgeStateProject, forgeStateRepo, num, "sha-later"); err != nil {
		t.Fatalf("later push: %v", err)
	}
	st, _ = repo.Get(ctx, forgeStateProject, forgeStateRepo, num)
	if st.ReviewingHeadSHA != "sha-fetched" {
		t.Errorf("ReviewingHeadSHA = %q after a later push, want sha-fetched — pending must not overwrite what is under review", st.ReviewingHeadSHA)
	}
}
