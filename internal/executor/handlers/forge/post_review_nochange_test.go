package forge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// The no-change guard (LLD 2026-09-01-forge-rereview-triggers-design.md §16).
//
// THE INCIDENT: fetch_diff reported scope "no-change", nothing consumed it, the
// reviewer ran with a one-sentence diff, invented a review of a DIFFERENT pull
// request, and this handler posted it to GitHub as an APPROVAL. Every assertion
// below is on whether PostReview was CALLED, not on a returned value: the defect
// was a post that happened.

// stateReviewState is a review-state double that returns a row the guard reads.
type stateReviewState struct {
	recordingReviewState
	row     *persistence.ForgePRReviewState
	getErr  error
	marked  []string
	getCall int
}

func (s *stateReviewState) Get(context.Context, string, string, int) (*persistence.ForgePRReviewState, error) {
	s.getCall++
	return s.row, s.getErr
}

func (s *stateReviewState) MarkReviewed(_ context.Context, _, _ string, _ int, sha string, _ time.Time) error {
	s.marked = append(s.marked, sha)
	return nil
}

func nochangeInput(job forgeapi.ForgeJob) executor.SystemStepInput {
	task := taskWithJob(job)
	task.ID = "task-nochange"
	return executor.SystemStepInput{
		Task:       task,
		Step:       &registry.WorkflowStep{Handler: "forge.post_review"},
		PrevResult: json.RawMessage(`{"message":"looks good to me"}`),
	}
}

// THE REGRESSION. Head equals the baseline: the review covers commits already
// reviewed and posted about, so nothing may reach the forge.
func TestPostReview_RefusesWhenTheHeadWasAlreadyReviewed(t *testing.T) {
	st := &stateReviewState{row: &persistence.ForgePRReviewState{
		LastReviewedHeadSHA: "sha-a",
	}}
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser()).WithReviewState(st)

	res, err := h.Execute(context.Background(), nochangeInput(
		forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-a"}))
	if err != nil {
		t.Fatalf("a refusal is a success, not an error: %v", err)
	}
	if prov.gotNumber != 0 || prov.gotReview.Body != "" {
		t.Fatal("PostReview was CALLED for a head that was already reviewed — this is " +
			"the bot approving code it never read")
	}
	// The baseline must not move: nothing was posted, so nothing new was reviewed.
	if len(st.marked) != 0 {
		t.Errorf("marked = %v after refusing; the next real review would be scoped "+
			"against a review that never happened", st.marked)
	}
	var payload map[string]any
	if err := json.Unmarshal(res.Result, &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if posted, _ := payload["posted"].(bool); posted {
		t.Errorf("result says posted=true when nothing was posted: %v", payload)
	}
	if reason, _ := payload["reason"].(string); reason == "" {
		t.Error("a refusal must carry a reason — an operator asking why there is no " +
			"review on this PR has only the execution record to read")
	}
}

// A normal incremental review still posts. This is the case that must not
// regress: a guard that refuses everything is far worse than the bug.
func TestPostReview_PostsWhenTheHeadMoved(t *testing.T) {
	st := &stateReviewState{row: &persistence.ForgePRReviewState{
		LastReviewedHeadSHA: "sha-a",
	}}
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser()).WithReviewState(st)

	if _, err := h.Execute(context.Background(), nochangeInput(
		forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-b"})); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotNumber != 4 {
		t.Fatal("a review of a moved head must post")
	}
	if len(st.marked) != 1 || st.marked[0] != "sha-b" {
		t.Errorf("marked = %v, want [sha-b]", st.marked)
	}
}

// §16.5's table, row by row. Each of these would silently withhold a review
// someone asked for, which is a worse bug than the one being fixed.
func TestPostReview_MustNotRefuse(t *testing.T) {
	cases := []struct {
		name  string
		job   forgeapi.ForgeJob
		state *stateReviewState
	}{
		{
			// Asking is consent (§7). A human typing "full review" on an
			// unchanged head wants the answer again.
			name: "an explicit on-demand command",
			job:  forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-a", OnDemand: true},
			state: &stateReviewState{row: &persistence.ForgePRReviewState{
				LastReviewedHeadSHA: "sha-a",
			}},
		},
		{
			name: "an explicit full-review command",
			job:  forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-a", FullReview: true},
			state: &stateReviewState{row: &persistence.ForgePRReviewState{
				LastReviewedHeadSHA: "sha-a",
			}},
		},
		{
			// "" means we do not know, and every uncertainty in §6 resolves
			// toward MORE review.
			name: "an empty head",
			job:  forgeapi.ForgeJob{Repo: "o/r", Number: 4},
			state: &stateReviewState{row: &persistence.ForgePRReviewState{
				LastReviewedHeadSHA: "",
			}},
		},
		{
			name:  "a row that cannot be read",
			job:   forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-a"},
			state: &stateReviewState{getErr: errors.New("store down")},
		},
		{
			name:  "no row yet — never reviewed",
			job:   forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-a"},
			state: &stateReviewState{row: nil},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeProvider{}
			h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser()).WithReviewState(tc.state)
			if _, err := h.Execute(context.Background(), nochangeInput(tc.job)); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if prov.gotNumber != 4 {
				t.Error("REFUSED a review that had to be posted — this silently " +
					"withholds a review someone asked for")
			}
		})
	}
}

// With no review state wired at all the handler behaves exactly as it did
// before the incremental machinery existed.
func TestPostReview_NoReviewStateStillPosts(t *testing.T) {
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
	if _, err := h.Execute(context.Background(), nochangeInput(
		forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-a"})); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotNumber != 4 {
		t.Error("a deployment without review state must post exactly as before")
	}
}

// ReviewingHeadSHA is what the review actually FETCHED and is what MarkReviewed
// would record, so it is what the guard compares. Here the job's head still
// equals the baseline but the review actually covered a newer commit: it must
// post.
func TestPostReview_ReviewingHeadWinsOverTheJobHead(t *testing.T) {
	st := &stateReviewState{row: &persistence.ForgePRReviewState{
		LastReviewedHeadSHA: "sha-a",
		ReviewingHeadSHA:    "sha-b",
	}}
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser()).WithReviewState(st)

	if _, err := h.Execute(context.Background(), nochangeInput(
		forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-a"})); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotNumber != 4 {
		t.Fatal("the review FETCHED sha-b, which is unreviewed — it must post")
	}
	if len(st.marked) != 1 || st.marked[0] != "sha-b" {
		t.Errorf("marked = %v, want [sha-b] — the guard and the mark must agree "+
			"on which head this review covered", st.marked)
	}
}

// The inverse: the job claims a new head but the review actually fetched the
// one already reviewed. The guard must follow what was FETCHED and refuse.
func TestPostReview_RefusesOnTheFetchedHeadNotTheClaimedOne(t *testing.T) {
	st := &stateReviewState{row: &persistence.ForgePRReviewState{
		LastReviewedHeadSHA: "sha-a",
		ReviewingHeadSHA:    "sha-a",
	}}
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser()).WithReviewState(st)

	if _, err := h.Execute(context.Background(), nochangeInput(
		forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-b"})); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotNumber != 0 {
		t.Fatal("the review FETCHED sha-a, which was already reviewed — the job's " +
			"claimed head is not what it read")
	}
}

// §16.5's force-push case: the branch has moved to sha-b, but this review
// covers sha-a, which was already reviewed and posted about. Still refused —
// a later force-push does not make reviewed code unread.
func TestPostReview_RefusesAfterAForcePushOnAnAlreadyReviewedHead(t *testing.T) {
	st := &stateReviewState{row: &persistence.ForgePRReviewState{
		LastReviewedHeadSHA: "sha-a",
		ReviewingHeadSHA:    "sha-a",
		PendingHeadSHA:      "sha-b", // the branch moved on
	}}
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser()).WithReviewState(st)

	if _, err := h.Execute(context.Background(), nochangeInput(
		forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-a"})); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotNumber != 0 {
		t.Fatal("sha-a was already reviewed; a force-push elsewhere does not make it unread")
	}
}

// THE ORDERING §16.4 RESTS ON. If MarkReviewed ever ran before PostReview, the
// guard would compare this review's own head against itself and refuse EVERY
// review — an outage far worse than the bug. Nothing in the compiler prevents
// that reordering, so it is pinned here.
func TestPostReview_MarksOnlyAfterPosting(t *testing.T) {
	order := []string{}
	st := &orderRecordingState{onMark: func() { order = append(order, "mark") }}
	prov := &orderRecordingProvider{onPost: func() { order = append(order, "post") }}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser()).WithReviewState(st)

	if _, err := h.Execute(context.Background(), nochangeInput(
		forgeapi.ForgeJob{Repo: "o/r", Number: 4, HeadSHA: "sha-b"})); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(order) != 2 || order[0] != "post" || order[1] != "mark" {
		t.Fatalf("order = %v, want [post mark]. The guard reads LastReviewedHeadSHA "+
			"BEFORE the post; if the mark moves earlier it would hold this review's "+
			"own head and every review would be refused", order)
	}
}

type orderRecordingState struct {
	recordingReviewState
	onMark func()
}

func (s *orderRecordingState) Get(context.Context, string, string, int) (*persistence.ForgePRReviewState, error) {
	return &persistence.ForgePRReviewState{LastReviewedHeadSHA: "sha-a"}, nil
}

func (s *orderRecordingState) MarkReviewed(_ context.Context, _, _ string, _ int, _ string, _ time.Time) error {
	s.onMark()
	return nil
}

type orderRecordingProvider struct {
	fakeProvider
	onPost func()
}

func (p *orderRecordingProvider) PostReview(ctx context.Context, repo string, number int, r forgeapi.ReviewSpec) error {
	p.onPost()
	return p.fakeProvider.PostReview(ctx, repo, number, r)
}
