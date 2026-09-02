package forge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Phase 4 of https://docs.vornik.io:
// incremental scope (§6).
//
// A re-review should address what CHANGED since the last review, not re-read the
// whole pull request and repeat findings the human has already seen. The
// baseline is last_reviewed_head_sha, and EVERY uncertainty resolves to a full
// review — losing the baseline must degrade to MORE review, never less.

type baselineState struct {
	recordingReviewState
	baseline string
	getErr   error
}

func (b *baselineState) Get(context.Context, string, string, int) (*persistence.ForgePRReviewState, error) {
	if b.getErr != nil {
		return nil, b.getErr
	}
	if b.baseline == "" {
		return nil, nil
	}
	return &persistence.ForgePRReviewState{LastReviewedHeadSHA: b.baseline}, nil
}

func incrementalTask(t *testing.T, headSHA string, full bool) executor.SystemStepInput {
	t.Helper()
	task := taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: headSHA, FullReview: full})
	task.ID = "task-1"
	return executor.SystemStepInput{Task: task}
}

// With a baseline recorded, the review asks for the RANGE, not the whole PR.
func TestFetchDiff_WithBaseline_ComparesTheRange(t *testing.T) {
	prov := &fakeProvider{compareDiff: []byte("only the new commits\n")}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(&baselineState{baseline: "sha-old"})

	if _, err := h.Execute(context.Background(), incrementalTask(t, "sha-new", false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotCompareBase != "sha-old" || prov.gotCompareHead != "sha-new" {
		t.Errorf("compared %q...%q, want sha-old...sha-new", prov.gotCompareBase, prov.gotCompareHead)
	}
}

// No baseline — a PR never reviewed before — must produce a FULL review.
func TestFetchDiff_NoBaseline_FetchesTheFullDiff(t *testing.T) {
	prov := &fakeProvider{diff: []byte("the whole PR\n")}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(&baselineState{})

	if _, err := h.Execute(context.Background(), incrementalTask(t, "sha-new", false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotCompareBase != "" {
		t.Errorf("compared %q...%q with no baseline; want the full diff", prov.gotCompareBase, prov.gotCompareHead)
	}
}

// FORCE-PUSH. The recorded baseline is no longer reachable from head, so the
// compare fails — and the review must fall back to the full diff rather than
// failing or reviewing nothing. This is the case that silently loses coverage if
// anyone "optimises" the fallback away.
func TestFetchDiff_UnreachableBaseline_FallsBackToFull(t *testing.T) {
	prov := &fakeProvider{
		compareErr: errors.New("404 not comparable"),
		diff:       []byte("the whole PR\n"),
	}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(&baselineState{baseline: "sha-rewritten"})

	res, err := h.Execute(context.Background(), incrementalTask(t, "sha-new", false))
	if err != nil {
		t.Fatalf("an unreachable baseline must fall back to a full review, not fail: %v", err)
	}
	if len(res.Result) == 0 {
		t.Fatal("no diff returned after the fallback")
	}
}

// An unreadable state row must also fall back to full, never to nothing.
func TestFetchDiff_StateReadError_FallsBackToFull(t *testing.T) {
	prov := &fakeProvider{diff: []byte("the whole PR\n")}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(&baselineState{getErr: errors.New("db down")})

	if _, err := h.Execute(context.Background(), incrementalTask(t, "sha-new", false)); err != nil {
		t.Fatalf("a state read error must degrade to a full review: %v", err)
	}
	if prov.gotCompareBase != "" {
		t.Error("attempted an incremental compare despite an unreadable baseline")
	}
}

// The explicit "full review" command ignores the baseline.
func TestFetchDiff_FullReviewRequested_IgnoresTheBaseline(t *testing.T) {
	prov := &fakeProvider{diff: []byte("the whole PR\n")}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(&baselineState{baseline: "sha-old"})

	if _, err := h.Execute(context.Background(), incrementalTask(t, "sha-new", true)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotCompareBase != "" {
		t.Errorf("compared %q...%q despite an explicit full-review request", prov.gotCompareBase, prov.gotCompareHead)
	}
}

// A baseline equal to head means nothing changed. Reviewing the whole PR again
// would repeat every finding; the step must say so rather than emit an empty
// diff the reviewer will hallucinate over.
func TestFetchDiff_BaselineEqualsHead_ReportsNothingToReview(t *testing.T) {
	prov := &fakeProvider{}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(&baselineState{baseline: "sha-same"})

	res, err := h.Execute(context.Background(), incrementalTask(t, "sha-same", false))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Result) == 0 {
		t.Fatal("no result for an already-reviewed head")
	}
	if prov.gotCompareBase != "" {
		t.Error("compared a range when head is already the reviewed baseline")
	}
}

var _ = time.Now

// The baseline advances only AFTER a review has actually posted. Advancing when
// a review merely started would let a crashed run mark commits as reviewed that
// no human ever saw — the silent coverage loss this feature exists to prevent.
type markRecorder struct {
	recordingReviewState
	marked []string
}

func (m *markRecorder) MarkReviewed(_ context.Context, _, _ string, _ int, sha string, _ time.Time) error {
	m.marked = append(m.marked, sha)
	return nil
}

func TestPostReview_AdvancesTheBaselineOnSuccess(t *testing.T) {
	rec := &markRecorder{}
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser()).WithReviewState(rec)

	task := taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-new"})
	task.ID = "task-1"
	if _, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       task,
		Step:       &registry.WorkflowStep{Handler: "forge.post_review"},
		PrevResult: json.RawMessage(`{"message":"looks good"}`),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rec.marked) != 1 || rec.marked[0] != "sha-new" {
		t.Fatalf("marked = %v, want [sha-new]", rec.marked)
	}
}

// A FAILED post must NOT advance the baseline, or the commits it never reviewed
// are permanently treated as reviewed.
func TestPostReview_FailedPost_DoesNotAdvanceTheBaseline(t *testing.T) {
	rec := &markRecorder{}
	prov := &fakeProvider{reviewErr: errors.New("502 from the forge")}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser()).WithReviewState(rec)

	task := taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-new"})
	task.ID = "task-1"
	if _, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       task,
		Step:       &registry.WorkflowStep{Handler: "forge.post_review"},
		PrevResult: json.RawMessage(`{"message":"looks good"}`),
	}); err == nil {
		t.Fatal("a failed post must surface as an error")
	}
	if len(rec.marked) != 0 {
		t.Fatalf("marked = %v after a FAILED post; those commits were never reviewed", rec.marked)
	}
}

// A push absorbed while the review was ABSORBING must be COVERED by that
// review. The task carries the head SHA of the delivery that created it, but a
// supersede advances pending_head_sha — so fetching job.HeadSHA reviews an
// already-stale head and leaves the absorbed commits unreviewed with no task of
// their own. If no further push arrives they are never reviewed at all, which is
// precisely the dropped-push failure coalescing must not introduce.
type supersededState struct {
	recordingReviewState
	baseline string
	pending  string
}

func (s *supersededState) Get(context.Context, string, string, int) (*persistence.ForgePRReviewState, error) {
	return &persistence.ForgePRReviewState{
		LastReviewedHeadSHA: s.baseline,
		PendingHeadSHA:      s.pending,
	}, nil
}

func TestFetchDiff_ReviewsTheSupersededHeadNotItsCreatingOne(t *testing.T) {
	prov := &fakeProvider{compareDiff: []byte("through the newest push\n")}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).
		WithReviewState(&supersededState{baseline: "sha-old", pending: "sha-newest"})

	// The task was created by the push at sha-created; sha-newest landed after
	// and was absorbed into this review.
	if _, err := h.Execute(context.Background(), incrementalTask(t, "sha-created", false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotCompareHead != "sha-newest" {
		t.Errorf("compared up to %q, want sha-newest — the absorbed push was left unreviewed", prov.gotCompareHead)
	}
}

// `@vornik review` is documented as INCREMENTAL, but an issue_comment payload
// carries no head SHA — GitHub's issue object has no head. The review must
// therefore fall back to the head coalescing recorded for the PR, not to a full
// diff, or the documented command silently never does what it says.
func TestFetchDiff_CommandWithNoHeadSHA_UsesTheRecordedHead(t *testing.T) {
	prov := &fakeProvider{compareDiff: []byte("since the last review\n")}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).
		WithReviewState(&supersededState{baseline: "sha-old", pending: "sha-current"})

	// HeadSHA empty, exactly as a comment-command job arrives.
	if _, err := h.Execute(context.Background(), incrementalTask(t, "", false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotCompareHead != "sha-current" {
		t.Errorf("compared up to %q, want sha-current — an incremental command fell back to a full review", prov.gotCompareHead)
	}
}

// With neither a job head nor a recorded one, there is nothing to bound a range
// with, and a full review is the only honest answer.
func TestFetchDiff_NoHeadAnywhere_FallsBackToFull(t *testing.T) {
	prov := &fakeProvider{diff: []byte("the whole PR\n")}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).
		WithReviewState(&supersededState{baseline: "sha-old", pending: ""})

	if _, err := h.Execute(context.Background(), incrementalTask(t, "", false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotCompareBase != "" {
		t.Error("attempted an incremental compare with no head to bound it")
	}
}

// A review posted in answer to a comment must say WHAT it was answering.
//
// Without this the review is orphaned from its request: a reader sees
// "changes requested" with no idea which question produced it, and if the
// comment is later edited or deleted the context is gone for good. That is not
// hypothetical — it happened on the first PR this feature reviewed.
func TestPostReview_QuotesTheRequestingComment(t *testing.T) {
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())

	task := taskWithJob(forgeapi.ForgeJob{
		Repo: "o/r", Number: 4, HeadSHA: "sha-new",
		OnDemand: true, Command: "review",
		CommentAuthor: "vgrinco",
		CommentBody:   "@vornik review\n\nPlease check the optional-field handling.",
	})
	task.ID = "task-1"
	if _, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       task,
		Step:       &registry.WorkflowStep{Handler: "forge.post_review"},
		PrevResult: json.RawMessage(`{"message":"looks good"}`),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	body := prov.gotReview.Body
	if !strings.Contains(body, "vgrinco") {
		t.Errorf("review does not attribute the request:\n%s", body)
	}
	if !strings.Contains(body, "> Please check the optional-field handling.") {
		t.Errorf("review does not quote the request:\n%s", body)
	}
	if !strings.Contains(body, "looks good") {
		t.Error("the review's own prose was lost")
	}
}

// An event-driven review has no request to quote, and inventing a header for it
// would be noise on every single push.
func TestPostReview_EventDrivenReviewHasNoQuote(t *testing.T) {
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())

	task := taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-new"})
	task.ID = "task-1"
	if _, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       task,
		Step:       &registry.WorkflowStep{Handler: "forge.post_review"},
		PrevResult: json.RawMessage(`{"message":"looks good"}`),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(prov.gotReview.Body, "asked") {
		t.Errorf("a push-triggered review invented a request header:\n%s", prov.gotReview.Body)
	}
}

// A very long comment must not push the review itself out of view.
func TestPostReview_LongCommentIsTruncated(t *testing.T) {
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())

	task := taskWithJob(forgeapi.ForgeJob{
		Repo: "o/r", Number: 4, OnDemand: true, Command: "review",
		CommentAuthor: "vgrinco",
		CommentBody:   "@vornik review " + strings.Repeat("x", 4000),
	})
	task.ID = "task-1"
	if _, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       task,
		Step:       &registry.WorkflowStep{Handler: "forge.post_review"},
		PrevResult: json.RawMessage(`{"message":"ok"}`),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(prov.gotReview.Body) > 2500 {
		t.Errorf("quote was not truncated; body is %d chars", len(prov.gotReview.Body))
	}
	if !strings.Contains(prov.gotReview.Body, "ok") {
		t.Error("the review's own prose was crowded out by the quote")
	}
}
