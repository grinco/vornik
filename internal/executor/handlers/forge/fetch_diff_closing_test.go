package forge

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// §5.2 of https://docs.vornik.io
//
// A review task is ABSORBING until it makes its comparison, and CLOSING from
// then on. Fetching the diff IS that comparison: after it, the running review
// can no longer see a newer head, so it must stop absorbing pushes. Releasing
// the claim here is what turns a later push into its own task instead of a
// supersede nobody will ever read.
//
// Without this the design's dropped-push case returns: a push landing after the
// fetch would supersede a SHA that is never fetched, and if the developer then
// stops pushing, the PR head is never reviewed while a posted review sits on it
// implying otherwise.

type recordingReviewState struct {
	setTaskCalls []string
	claim        string
	closedAt     string // the head recorded at the CLOSING transition
	pendingHead  string // the row's pending head, as the compare-and-set sees it
}

// Get returns the ABSENT form, which is this repository's miss contract
// ((nil, nil), see misscontract). The handler under test only ever calls
// SetTask; Get exists to satisfy the interface, and returning a fabricated row
// here would make the double disagree with every real backend.
func (r *recordingReviewState) Get(context.Context, string, string, int) (*persistence.ForgePRReviewState, error) {
	return nil, nil
}

func (r *recordingReviewState) ClaimOrSupersede(context.Context, string, string, int, string) (string, error) {
	return r.claim, nil
}

func (r *recordingReviewState) SetTask(_ context.Context, _, _ string, _ int, taskID string) error {
	r.setTaskCalls = append(r.setTaskCalls, taskID)
	r.claim = taskID
	return nil
}

// BeginClosing is the ABSORBING → CLOSING transition. Recorded as a SetTask("")
// so the existing assertions on claim release keep working unchanged.
//
// THE COMPARE-AND-SET IS MODELLED, not stubbed away. A double that applies
// unconditionally is LOOSER than every real backend, which means it certifies
// the pre-2026-09-03 behaviour — the whole reason the audit could find that hole
// with no test failing (internal/persistence/misscontract makes this argument
// in general).
func (r *recordingReviewState) BeginClosing(_ context.Context, _, _ string, _ int, reviewingHeadSHA, expectedPendingHeadSHA string) (persistence.ClosingOutcome, error) {
	if r.pendingHead != expectedPendingHeadSHA {
		return persistence.ClosingOutcome{PendingHeadSHA: r.pendingHead}, nil
	}
	r.setTaskCalls = append(r.setTaskCalls, "")
	r.claim = ""
	r.closedAt = reviewingHeadSHA
	return persistence.ClosingOutcome{Applied: true}, nil
}

func (r *recordingReviewState) MarkReviewed(context.Context, string, string, int, string, time.Time) error {
	return nil
}

func (r *recordingReviewState) SetPaused(context.Context, string, string, int, bool) error {
	return nil
}

// Fetching the diff must release the claim — the ABSORBING → CLOSING transition.
func TestFetchDiff_ReleasesTheReviewClaim(t *testing.T) {
	state := &recordingReviewState{claim: "task-1"}
	prov := &fakeProvider{diff: []byte("diff --git a/x b/x\n")}
	h := NewFetchDiffHandler(fakeResolver{p: prov})
	h.reviewState = state

	task := taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4})
	task.ID = "task-1"
	if _, err := h.Execute(context.Background(), executor.SystemStepInput{Task: task}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(state.setTaskCalls) != 1 || state.setTaskCalls[0] != "" {
		t.Fatalf("SetTask calls = %v, want exactly one release to \"\" — the review is still ABSORBING after its comparison", state.setTaskCalls)
	}
}

// A handler with no state repository must still fetch the diff. Failing the
// review because coalescing bookkeeping is unavailable would trade a duplicate
// review for no review at all.
func TestFetchDiff_NoReviewState_StillFetches(t *testing.T) {
	prov := &fakeProvider{diff: []byte("diff --git a/x b/x\n")}
	h := NewFetchDiffHandler(fakeResolver{p: prov})
	// reviewState deliberately nil.
	task := taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4})
	task.ID = "task-1"
	res, err := h.Execute(context.Background(), executor.SystemStepInput{Task: task})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Result) == 0 {
		t.Fatal("no result from a fetch with no review state wired")
	}
}

// A failed release must FAIL THE STEP rather than continue. Continuing would
// leave the task ABSORBING for the rest of its run, swallowing any push that
// arrives before it finishes; failing drives the task terminal, so the claim
// reads as dead and the next push enqueues. We lose the run and keep coverage.
func TestFetchDiff_ClaimReleaseFailure_FailsTheStep(t *testing.T) {
	prov := &fakeProvider{diff: []byte("diff --git a/x b/x\n")}
	h := NewFetchDiffHandler(fakeResolver{p: prov})
	h.reviewState = &failingReleaseState{}

	task := taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4})
	task.ID = "task-1"
	if _, err := h.Execute(context.Background(), executor.SystemStepInput{Task: task}); err == nil {
		t.Fatal("a failed claim release must fail the step; continuing leaves the task absorbing and drops pushes")
	}
}

type failingReleaseState struct{ recordingReviewState }

func (f *failingReleaseState) BeginClosing(context.Context, string, string, int, string, string) (persistence.ClosingOutcome, error) {
	return persistence.ClosingOutcome{}, errTestReleaseFailed
}

var errTestReleaseFailed = errors.New("release failed")

// The double must obey the same miss contract as the real repositories.
func TestRecordingReviewState_ObeysTheMissContract(t *testing.T) {
	r := &recordingReviewState{}
	repotest.AssertMiss(t, "ForgePRReviewStateRepository.Get", func() (*persistence.ForgePRReviewState, error) {
		return r.Get(context.Background(), "p", "o/r", 1)
	})
}
