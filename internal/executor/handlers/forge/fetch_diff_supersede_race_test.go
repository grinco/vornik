package forge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// pushDuringFetchState is a review-state double whose row CHANGES while the
// forge round-trip is in flight — the interleaving the audit found nothing
// exercised, because "the fakes are synchronous".
//
// Every push in pushes lands during one CompareDiff/FetchDiff call, in order:
// the provider calls onFetch, which advances the row's pending head exactly as
// a real supersede would while this task still holds the claim.
type pushDuringFetchState struct {
	recordingReviewState
	baseline string
	pushes   []string
	closes   []string // reviewingHeadSHA of every APPLIED transition
	refusals int
}

func (s *pushDuringFetchState) Get(context.Context, string, string, int) (*persistence.ForgePRReviewState, error) {
	return &persistence.ForgePRReviewState{
		LastReviewedHeadSHA: s.baseline,
		PendingHeadSHA:      s.pendingHead,
	}, nil
}

func (s *pushDuringFetchState) BeginClosing(_ context.Context, _, _ string, _ int, reviewingHeadSHA, expectedPending string) (persistence.ClosingOutcome, error) {
	if s.pendingHead != expectedPending {
		s.refusals++
		return persistence.ClosingOutcome{PendingHeadSHA: s.pendingHead}, nil
	}
	s.closes = append(s.closes, reviewingHeadSHA)
	return persistence.ClosingOutcome{Applied: true}, nil
}

// onFetch is handed to the provider so a push lands mid-round-trip.
func (s *pushDuringFetchState) onFetch() {
	if len(s.pushes) == 0 {
		return
	}
	s.pendingHead, s.pushes = s.pushes[0], s.pushes[1:]
}

// TestFetchDiff_PushDuringTheFetch_IsNotLost — regression for the 2026-09-03
// four-week audit's P1.
//
// Design §5.2 states the correctness invariant: read pending_head_sha and
// transition ABSORBING → CLOSING as ONE atomic step, "so no trigger can land
// between them". The implementation was not atomic — it read the pending head,
// made a real network round-trip to the forge, and only then committed to the
// PRE-fetch head unconditionally.
//
// For the whole duration of that fetch the claim is still held, so a push
// landing in the window is ABSORBED: ClaimOrSupersede returns the live claim,
// the coordinator skips the delivery as "superseded", and only pending_head_sha
// advances. The task then released the claim on the stale head and
// MarkReviewedHead advanced the baseline to it. Nothing reconciled the leftover
// newer pending head — the only non-storage consumer of that column is the read
// at the top of this handler. If the developer did not push again, that head was
// permanently unreviewed while a posted review implied coverage.
func TestFetchDiff_PushDuringTheFetch_IsNotLost(t *testing.T) {
	st := &pushDuringFetchState{baseline: "sha-old"}
	st.pendingHead = "sha-a"
	// One push lands while the first CompareDiff is in flight.
	st.pushes = []string{"sha-c"}

	prov := &fakeProvider{compareDiff: []byte("the range\n"), onFetch: st.onFetch}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(st)

	if _, err := h.Execute(context.Background(), incrementalTask(t, "sha-a", false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if st.refusals != 1 {
		t.Errorf("refusals = %d, want 1 — the transition committed to a head a push had already moved past", st.refusals)
	}
	if prov.gotCompareHead != "sha-c" {
		t.Errorf("the review covered up to %q, want sha-c — the absorbed push is unreviewed and no task exists for it",
			prov.gotCompareHead)
	}
	if len(st.closes) != 1 || st.closes[0] != "sha-c" {
		t.Errorf("closed at %v, want [sha-c] — the baseline would advance past a head nobody read", st.closes)
	}
}

// The step result must name the head that was ACTUALLY reviewed after a
// re-fetch, because post_review writes the baseline from it. Reporting the
// pre-fetch head would advance coverage past commits nobody looked at, which is
// the failure the re-fetch exists to prevent, moved one step later.
func TestFetchDiff_AfterARefetch_ReportsTheHeadItActuallyReviewed(t *testing.T) {
	st := &pushDuringFetchState{baseline: "sha-old"}
	st.pendingHead = "sha-a"
	st.pushes = []string{"sha-c"}

	prov := &fakeProvider{compareDiff: []byte("the range\n"), onFetch: st.onFetch}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(st)

	res, err := h.Execute(context.Background(), incrementalTask(t, "sha-a", false))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Result, &out); err != nil {
		t.Fatalf("unmarshal step result: %v", err)
	}
	if got, _ := out["head_sha"].(string); got != "sha-c" {
		t.Errorf("head_sha = %q, want sha-c", got)
	}
}

// It converges as soon as one fetch completes quietly — a burst does not become
// one re-fetch per push forever.
func TestFetchDiff_ConvergesAfterTheBurstStops(t *testing.T) {
	st := &pushDuringFetchState{baseline: "sha-old"}
	st.pendingHead = "sha-a"
	st.pushes = []string{"sha-b", "sha-c"} // two pushes, then quiet

	prov := &fakeProvider{compareDiff: []byte("the range\n"), onFetch: st.onFetch}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(st)

	if _, err := h.Execute(context.Background(), incrementalTask(t, "sha-a", false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if st.refusals != 2 {
		t.Errorf("refusals = %d, want 2", st.refusals)
	}
	if prov.gotCompareHead != "sha-c" {
		t.Errorf("the review covered up to %q, want sha-c", prov.gotCompareHead)
	}
}

// A pusher that never lets a fetch complete quietly must not hold the step open.
// Failing is the safe direction: the task goes terminal, the claim dies with it,
// and the next trigger enqueues its own review — loud, where a narrowed review
// would be silent.
func TestFetchDiff_NeverConverges_FailsTheStep(t *testing.T) {
	st := &pushDuringFetchState{baseline: "sha-old"}
	st.pendingHead = "sha-a"
	st.pushes = []string{"sha-b", "sha-c", "sha-d", "sha-e", "sha-f"}

	prov := &fakeProvider{compareDiff: []byte("the range\n"), onFetch: st.onFetch}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(st)

	_, err := h.Execute(context.Background(), incrementalTask(t, "sha-a", false))
	if err == nil {
		t.Fatal("a review that never stopped being superseded returned success; " +
			"it would post against a head it did not read")
	}
	if !strings.Contains(err.Error(), "superseded") {
		t.Errorf("error = %q, want it to name the supersede so the failure is diagnosable", err)
	}
	if len(st.closes) != 0 {
		t.Errorf("closed at %v despite never converging — the claim was released on an unreviewed head", st.closes)
	}
	if st.refusals != maxFetchAttempts {
		t.Errorf("refusals = %d, want %d — the loop is not bounded where it says it is", st.refusals, maxFetchAttempts)
	}
}

// A quiet PR must not pay for any of this: with nothing superseding, exactly one
// fetch and one transition.
func TestFetchDiff_NoPush_ClosesOnTheFirstAttempt(t *testing.T) {
	st := &pushDuringFetchState{baseline: "sha-old"}
	st.pendingHead = "sha-a"

	prov := &fakeProvider{compareDiff: []byte("the range\n"), onFetch: st.onFetch}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(st)

	if _, err := h.Execute(context.Background(), incrementalTask(t, "sha-a", false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if st.refusals != 0 {
		t.Errorf("refusals = %d on a quiet PR, want 0", st.refusals)
	}
	if len(st.closes) != 1 || st.closes[0] != "sha-a" {
		t.Errorf("closes = %v, want [sha-a]", st.closes)
	}
}

// A full review absorbs pushes exactly like an incremental one, so its
// transition needs the same comparison point. Skipping the compare-and-set for
// `full review` would leave the hole open on the one command a human explicitly
// asked for: the first fetch may have raced the push, and only the re-fetch is
// guaranteed to have run after it landed.
//
// The head it RECORDS stays the job's own, which is the deliberate asymmetry
// with the incremental path. An incremental review names its upper bound in the
// request and can claim it; a full fetch returns whatever the forge had at some
// instant during the call, which we cannot name — so it claims only the head it
// knows. Under-claiming costs a repeated finding; over-claiming loses a commit.
func TestFetchDiff_FullReview_ComparesThePendingHeadButClaimsOnlyItsOwn(t *testing.T) {
	st := &pushDuringFetchState{baseline: "sha-old"}
	st.pendingHead = "sha-a"
	st.pushes = []string{"sha-c"}

	prov := &fakeProvider{diff: []byte("the whole PR\n"), onFetch: st.onFetch}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(st)

	if _, err := h.Execute(context.Background(), incrementalTask(t, "sha-a", true)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if st.refusals != 1 {
		t.Errorf("refusals = %d on a full review, want 1 — the fetch that raced the push was accepted as final", st.refusals)
	}
	if len(st.closes) != 1 || st.closes[0] != "sha-a" {
		t.Errorf("closes = %v, want [sha-a] — a full fetch cannot name the head it returned, so it must not claim a newer one",
			st.closes)
	}
}

func (s *pushDuringFetchState) MarkReviewed(context.Context, string, string, int, string, time.Time) error {
	return nil
}

// unreadableState's row EXISTS and holds a pending head; the review just cannot
// read it. That is the shape of a transient store outage, not of a fresh PR.
type unreadableState struct {
	recordingReviewState
	closes   []string
	refusals int
	gets     int
}

func (s *unreadableState) Get(context.Context, string, string, int) (*persistence.ForgePRReviewState, error) {
	s.gets++
	return nil, errors.New("db down")
}

func (s *unreadableState) BeginClosing(_ context.Context, _, _ string, _ int, head, expected string) (persistence.ClosingOutcome, error) {
	if s.pendingHead != expected {
		s.refusals++
		return persistence.ClosingOutcome{PendingHeadSHA: s.pendingHead}, nil
	}
	s.closes = append(s.closes, head)
	return persistence.ClosingOutcome{Applied: true}, nil
}

func (s *unreadableState) MarkReviewed(context.Context, string, string, int, string, time.Time) error {
	return nil
}

// AN UNREADABLE STATE ROW MUST NOT FAIL THE REVIEW — caught by the companion
// architectural review of the compare-and-set, as a regression the fix itself
// introduced.
//
// diffForJob resolves a failed Get to a full review and reports no observation.
// With the observation and "no pending head" both spelled as the empty string,
// the compare-and-set had nothing true to compare, refused every attempt, and
// the review died because the BOOKKEEPING was unavailable — the exact inversion
// of this handler's stated asymmetry (a duplicate review beats no review) and of
// §6's "every uncertainty resolves to full".
//
// A refusal always names the row's current pending head, so one refusal yields a
// comparison point that did not come through Get. It still compares: a push
// landing after that refusal refuses again rather than being stranded.
func TestFetchDiff_UnreadableStateRow_StillReviews(t *testing.T) {
	st := &unreadableState{}
	st.pendingHead = "sha-a"

	prov := &fakeProvider{diff: []byte("the whole PR\n")}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(st)

	if _, err := h.Execute(context.Background(), incrementalTask(t, "sha-a", false)); err != nil {
		t.Fatalf("an unreadable state row failed the review outright: %v", err)
	}
	if st.refusals != 1 {
		t.Errorf("refusals = %d, want 1 — it should converge on the head the first refusal named", st.refusals)
	}
	if len(st.closes) != 1 || st.closes[0] != "sha-a" {
		t.Errorf("closes = %v, want [sha-a] — the claim was never released, so the task keeps absorbing", st.closes)
	}
}

// A PR with genuinely NO state row is a real observation, not an unknown one, so
// it closes on the first attempt. Conflating the two is what caused the bug
// above; this pins the other side of the distinction.
func TestFetchDiff_NoStateRowYet_ClosesOnTheFirstAttempt(t *testing.T) {
	st := &recordingReviewState{} // Get returns (nil, nil): the miss contract
	prov := &fakeProvider{diff: []byte("the whole PR\n")}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithReviewState(st)

	if _, err := h.Execute(context.Background(), incrementalTask(t, "sha-a", false)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if st.closedAt != "sha-a" {
		t.Errorf("closedAt = %q, want sha-a", st.closedAt)
	}
}
