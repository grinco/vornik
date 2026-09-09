package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunForgeCIOutcomeSuite is the backend-agnostic contract for
// persistence.ForgeCIOutcomeRepository
// (LLD 2026-09-08-forge-ci-outcomes-design.md §4.1, §10).
//
// Three things here differ between the backends and are therefore worth
// asserting on both: the upsert (Postgres ON CONFLICT versus SQLite's INSERT OR
// REPLACE, where the obvious spelling silently resets columns it did not mean
// to), the jobs round-trip through a JSON column (the JSONB byte-exactness
// difference that reached CI once already), and ordering, which no round-trip
// test would catch because a single row is trivially ordered.
func RunForgeCIOutcomeSuite(t *testing.T, repo persistence.ForgeCIOutcomeRepository) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	const (
		proj  = "proj-ci"
		repo1 = "acme/infra"
		sha   = "sha-head-1"
	)

	t.Run("Upsert_then_ListByHeadSHA_round_trips_including_jobs", func(t *testing.T) {
		ciOutcomeRoundTrip(ctx, t, repo, proj, repo1, sha, now)
	})
	t.Run("A_redelivery_of_the_same_run_updates_rather_than_appends", func(t *testing.T) {
		ciOutcomeRedeliveryUpdates(ctx, t, repo, proj, repo1, sha, now)
	})
	t.Run("A_run_with_no_pull_request_is_recorded_with_number_zero", func(t *testing.T) {
		ciOutcomeNoPullRequest(ctx, t, repo, proj, repo1, sha, now)
	})
	t.Run("Several_runs_on_one_commit_come_back_newest_first", func(t *testing.T) {
		ciOutcomeNewestFirst(ctx, t, repo, proj, repo1, sha, now)
	})
	t.Run("A_commit_with_no_runs_is_empty_not_an_error", func(t *testing.T) {
		ciOutcomeEmptyIsNotAnError(ctx, t, repo, proj, repo1, sha, now)
	})
	t.Run("Another_projects_row_is_not_visible", func(t *testing.T) {
		ciOutcomeProjectScoped(ctx, t, repo, proj, repo1, sha, now)
	})
	t.Run("PruneBefore_removes_only_what_completed_earlier", func(t *testing.T) {
		ciOutcomePruneBefore(ctx, t, repo, proj, repo1, sha, now)
	})
	t.Run("A_redelivery_must_not_clear_commented_at", func(t *testing.T) {
		ciOutcomeCommentedAtSurvivesUpsert(ctx, t, repo, proj, repo1, sha, now)
	})
}

func ciOutcomeRoundTrip(ctx context.Context, t *testing.T, repo persistence.ForgeCIOutcomeRepository,
	proj, repo1, sha string, now time.Time) {
	t.Helper()
	in := &persistence.ForgeCIOutcome{
		ProjectID: proj, Repo: repo1, RunID: 1001,
		HeadSHA: sha, Number: 42,
		WorkflowName: "Terraform", WorkflowPath: ".github/workflows/terraform-plan.yml",
		RunAttempt: 1, Conclusion: "failure",
		StartedAt: now.Add(-10 * time.Minute), CompletedAt: now.Add(-time.Minute),
		Jobs: []persistence.ForgeCIJob{
			{Name: "terraform-plan", Status: "completed", Conclusion: "failure"},
			{Name: "lint", Status: "completed", Conclusion: "success"},
		},
		ArtifactExcerpt: "plan output", ArtifactBytes: 11, ArtifactTruncated: true,
		RecordedAt: now,
	}
	if err := repo.Upsert(ctx, in); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got := mustOneOutcome(ctx, t, repo, proj, repo1, sha)

	if got.RunID != in.RunID || got.Number != in.Number || got.Conclusion != in.Conclusion {
		t.Errorf("scalar round trip: got %+v", got)
	}
	if got.WorkflowPath != in.WorkflowPath {
		t.Errorf("WorkflowPath = %q, want %q — the path is what the workflow "+
			"filter matches on", got.WorkflowPath, in.WorkflowPath)
	}
	if len(got.Jobs) != 2 || got.Jobs[0].Name != "terraform-plan" ||
		got.Jobs[0].Conclusion != "failure" || got.Jobs[1].Name != "lint" {
		t.Errorf("jobs did not round-trip in order: %+v", got.Jobs)
	}
	if got.ArtifactExcerpt != "plan output" || !got.ArtifactTruncated {
		t.Errorf("artifact fields did not round-trip: %+v", got)
	}
}

func ciOutcomeRedeliveryUpdates(ctx context.Context, t *testing.T, repo persistence.ForgeCIOutcomeRepository,
	proj, repo1, sha string, now time.Time) {
	t.Helper()
	in := &persistence.ForgeCIOutcome{
		ProjectID: proj, Repo: repo1, RunID: 1001,
		HeadSHA: sha, Number: 42, Conclusion: "success",
		WorkflowName: "Terraform", WorkflowPath: ".github/workflows/terraform-plan.yml",
		RunAttempt:  2,
		StartedAt:   now.Add(-9 * time.Minute),
		CompletedAt: now,
		RecordedAt:  now,
	}
	if err := repo.Upsert(ctx, in); err != nil {
		t.Fatalf("Upsert (redelivery): %v", err)
	}
	got := mustOneOutcome(ctx, t, repo, proj, repo1, sha)
	if got.Conclusion != "success" {
		t.Errorf("Conclusion = %q, want the updated value", got.Conclusion)
	}
	if got.RunAttempt != 2 {
		t.Errorf("RunAttempt = %d, want 2", got.RunAttempt)
	}
	// The re-run carried no artifact. It must CLEAR the previous one rather
	// than leave a stale excerpt attached to a different attempt — an
	// operator reading a green re-run must not be shown the failed
	// attempt's plan.
	if got.ArtifactExcerpt != "" || got.ArtifactTruncated {
		t.Errorf("the re-run's row still carries the previous attempt's artifact: %+v", got)
	}
}

func ciOutcomeNoPullRequest(ctx context.Context, t *testing.T, repo persistence.ForgeCIOutcomeRepository,
	proj, repo1, _ string, now time.Time) {
	t.Helper()
	const mainSHA = "sha-main-1"
	in := &persistence.ForgeCIOutcome{
		ProjectID: proj, Repo: repo1, RunID: 2002,
		HeadSHA: mainSHA, Number: 0, Conclusion: "success",
		WorkflowPath: ".github/workflows/deploy.yml",
		CompletedAt:  now, RecordedAt: now,
	}
	if err := repo.Upsert(ctx, in); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got := mustOneOutcome(ctx, t, repo, proj, repo1, mainSHA)
	if got.Number != 0 {
		t.Errorf("Number = %d, want 0", got.Number)
	}
	if got.HasPullRequest() {
		t.Error("HasPullRequest() true for a run with no pull request")
	}
}

func ciOutcomeNewestFirst(ctx context.Context, t *testing.T, repo persistence.ForgeCIOutcomeRepository,
	proj, repo1, _ string, now time.Time) {
	t.Helper()
	const multiSHA = "sha-multi"
	for i, at := range []time.Time{
		now.Add(-3 * time.Hour), now.Add(-time.Hour), now.Add(-2 * time.Hour),
	} {
		o := &persistence.ForgeCIOutcome{
			ProjectID: proj, Repo: repo1, RunID: int64(3000 + i),
			HeadSHA: multiSHA, Conclusion: "success",
			WorkflowPath: ".github/workflows/w.yml",
			CompletedAt:  at, RecordedAt: now,
		}
		if err := repo.Upsert(ctx, o); err != nil {
			t.Fatalf("Upsert %d: %v", i, err)
		}
	}
	got, err := repo.ListByHeadSHA(ctx, proj, repo1, multiSHA)
	if err != nil {
		t.Fatalf("ListByHeadSHA: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].CompletedAt.After(got[i-1].CompletedAt) {
			t.Errorf("rows are not newest-first: %v then %v",
				got[i-1].CompletedAt, got[i].CompletedAt)
		}
	}
}

func ciOutcomeEmptyIsNotAnError(ctx context.Context, t *testing.T, repo persistence.ForgeCIOutcomeRepository,
	proj, repo1, _ string, _ time.Time) {
	t.Helper()
	got, err := repo.ListByHeadSHA(ctx, proj, repo1, "sha-never-built")
	if err != nil {
		t.Fatalf("ListByHeadSHA: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d rows, want 0", len(got))
	}
}

func ciOutcomeProjectScoped(ctx context.Context, t *testing.T, repo persistence.ForgeCIOutcomeRepository,
	proj, repo1, sha string, now time.Time) {
	t.Helper()
	other := &persistence.ForgeCIOutcome{
		ProjectID: "proj-other", Repo: repo1, RunID: 4004,
		HeadSHA: sha, Conclusion: "success",
		WorkflowPath: ".github/workflows/w.yml",
		CompletedAt:  now, RecordedAt: now,
	}
	if err := repo.Upsert(ctx, other); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := repo.ListByHeadSHA(ctx, proj, repo1, sha)
	if err != nil {
		t.Fatalf("ListByHeadSHA: %v", err)
	}
	for _, o := range got {
		if o.ProjectID != proj {
			t.Fatalf("project %q leaked into project %q's read", o.ProjectID, proj)
		}
	}
}

func ciOutcomePruneBefore(ctx context.Context, t *testing.T, repo persistence.ForgeCIOutcomeRepository,
	proj, repo1, _ string, now time.Time) {
	t.Helper()
	const pruneSHA = "sha-prune"
	old := &persistence.ForgeCIOutcome{
		ProjectID: proj, Repo: repo1, RunID: 5005, HeadSHA: pruneSHA,
		Conclusion: "success", WorkflowPath: ".github/workflows/w.yml",
		CompletedAt: now.Add(-72 * time.Hour), RecordedAt: now,
	}
	fresh := &persistence.ForgeCIOutcome{
		ProjectID: proj, Repo: repo1, RunID: 5006, HeadSHA: pruneSHA,
		Conclusion: "success", WorkflowPath: ".github/workflows/w.yml",
		CompletedAt: now, RecordedAt: now,
	}
	for _, o := range []*persistence.ForgeCIOutcome{old, fresh} {
		if err := repo.Upsert(ctx, o); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	n, err := repo.PruneBefore(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PruneBefore: %v", err)
	}
	if n < 1 {
		t.Errorf("PruneBefore removed %d rows, want at least the stale one", n)
	}
	got, err := repo.ListByHeadSHA(ctx, proj, repo1, pruneSHA)
	if err != nil {
		t.Fatalf("ListByHeadSHA: %v", err)
	}
	if len(got) != 1 || got[0].RunID != 5006 {
		t.Errorf("prune kept the wrong rows: %+v", got)
	}
}

func mustOneOutcome(ctx context.Context, t *testing.T, repo persistence.ForgeCIOutcomeRepository,
	projectID, repoName, sha string) *persistence.ForgeCIOutcome {
	t.Helper()
	got, err := repo.ListByHeadSHA(ctx, projectID, repoName, sha)
	if err != nil {
		t.Fatalf("ListByHeadSHA: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows for %s, want exactly 1", len(got), sha)
	}
	return got[0]
}

// ciOutcomeCommentedAtSurvivesUpsert is the assertion that separates the two
// backends (design §13.6).
//
// Every other column on this row records what CI REPORTED and is refreshed by
// each delivery. commented_at records what Forge DID about it, and the upsert
// must leave it alone — a redelivery that cleared it would post a second
// comment on the same run.
//
// Postgres gets this right by omitting the column from its SET list. SQLite
// would NOT with `INSERT OR REPLACE`, which replaces the whole row: this case
// fails against that spelling, which is why it is asserted rather than assumed.
func ciOutcomeCommentedAtSurvivesUpsert(ctx context.Context, t *testing.T, repo persistence.ForgeCIOutcomeRepository,
	proj, repo1, _ string, now time.Time) {
	t.Helper()
	const commentedSHA = "sha-commented"
	base := func() *persistence.ForgeCIOutcome {
		return &persistence.ForgeCIOutcome{
			ProjectID: proj, Repo: repo1, RunID: 6006, HeadSHA: commentedSHA,
			Conclusion: "failure", WorkflowPath: ".github/workflows/w.yml",
			CompletedAt: now, RecordedAt: now,
		}
	}
	if err := repo.Upsert(ctx, base()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// ESTABLISH THE PRECONDITION rather than assume it. The Postgres lane runs
	// against a DATABASE THAT PERSISTS between runs, and the upsert above
	// deliberately preserves commented_at — so a previous run's claim on this
	// fixture id survives and the first claim below fails, which is the suite
	// mis-reporting a clean tree as broken. (Third instance of this class in
	// this repository; the other two were the rating-rollup seeders.)
	if err := repo.ReleaseComment(ctx, proj, repo1, 6006); err != nil {
		t.Fatalf("ReleaseComment (precondition): %v", err)
	}

	claimed, err := repo.ClaimComment(ctx, proj, repo1, 6006, now)
	if err != nil || !claimed {
		t.Fatalf("ClaimComment on an unclaimed run: claimed=%v err=%v", claimed, err)
	}
	got := mustOneOutcome(ctx, t, repo, proj, repo1, commentedSHA)
	if got.CommentedAt.IsZero() {
		t.Fatal("ClaimComment did not record the timestamp")
	}

	// THE CLAIM IS EXCLUSIVE (regression, headmatch PR #52 2026-09-09: a
	// redelivery posted a second comment). A second claim must be refused by
	// the STORE — the caller's in-memory outcome cannot answer this, because
	// the one an ingress builds from a webhook always has a zero CommentedAt.
	claimed, err = repo.ClaimComment(ctx, proj, repo1, 6006, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ClaimComment (second): %v", err)
	}
	if claimed {
		t.Error("a second claim on the same run SUCCEEDED — the run would be commented on twice")
	}

	// Released, it can be claimed again: a post that failed must be retryable.
	if err := repo.ReleaseComment(ctx, proj, repo1, 6006); err != nil {
		t.Fatalf("ReleaseComment: %v", err)
	}
	if got := mustOneOutcome(ctx, t, repo, proj, repo1, commentedSHA); !got.CommentedAt.IsZero() {
		t.Error("ReleaseComment left commented_at set")
	}
	claimed, err = repo.ClaimComment(ctx, proj, repo1, 6006, now)
	if err != nil || !claimed {
		t.Fatalf("ClaimComment after release: claimed=%v err=%v", claimed, err)
	}

	// The redelivery. Everything CI reported may be refreshed; the comment
	// record may not.
	again := base()
	again.Conclusion = "failure"
	if err := repo.Upsert(ctx, again); err != nil {
		t.Fatalf("Upsert (redelivery): %v", err)
	}
	got = mustOneOutcome(ctx, t, repo, proj, repo1, commentedSHA)
	if got.CommentedAt.IsZero() {
		t.Error("a redelivery CLEARED commented_at — the same run would be " +
			"commented on twice")
	}
}
