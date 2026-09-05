package persistence

import (
	"context"
	"time"
)

// ForgePRReviewState is one row in `forge_pr_review_state` — everything the
// re-review machinery needs to remember about a single pull request between
// deliveries.
//
// Design: https://docs.vornik.io
// (§5.2 coalescing, §6 incremental scope).
//
// WHY THIS TABLE EXISTS AT ALL. Coalescing needs a durable per-PR claim, and no
// existing store can hold one: `tasks` has exactly one string-keyed lookup
// (`GetByIdempotencyKey`) and that field is already committed to per-delivery
// keying for redelivery safety; `TaskFilter` has no session or payload
// predicate, so "the in-flight review for this PR" is not an expressible query;
// and `channel_sessions.History` is the chat transcript, owned by the
// conversational path.
type ForgePRReviewState struct {
	// ProjectID scopes the row. Deliberately NOT the installation id: one
	// installation can serve several configured projects, and two projects
	// watching the same repo must not share review state, because the config
	// deciding workflow and allowlist is per project.
	ProjectID string

	// Repo is the "owner/name" full name; Number the pull request number.
	Repo   string
	Number int

	// TaskID names the review task this PR most recently enqueued, if any.
	//
	// A POINTER, NEVER A CLAIM. There is deliberately no "absorbing" boolean:
	// the ABSORBING state is DERIVED by loading this task and asking whether it
	// is still non-terminal and has not yet compared. A boolean could drift out
	// of step with the task and then absorb every later push into a corpse,
	// leaving the PR unreviewed until a daemon restart — the one genuinely
	// dropped-push failure this design has. A stale pointer instead degrades to
	// "load it, see terminal, enqueue", which is the safe direction.
	//
	// No foreign key: this codebase RETAINS terminal task rows for audit, so
	// ON DELETE CASCADE would essentially never fire and would be a second
	// cleanup mechanism to keep in step with the first.
	TaskID string

	// PendingHeadSHA is the newest head commit observed for this PR while a
	// review was ABSORBING. Coalescing bookkeeping only — it NEVER participates
	// in incremental scope computation, so a stale value cannot narrow a review.
	PendingHeadSHA string

	// ReviewingHeadSHA is the head the in-flight review actually FETCHED.
	//
	// Distinct from PendingHeadSHA on purpose. Pending keeps moving while a
	// review runs; this is frozen at the moment the review read the diff, and
	// it is what the baseline advances to when the review posts. Marking
	// pending instead would claim commits as reviewed that landed after the
	// fetch and were never looked at.
	ReviewingHeadSHA string

	// LastReviewedHeadSHA is the head commit of the most recent SUCCESSFULLY
	// POSTED review, and the sole authority for incremental scope: the range is
	// always LastReviewedHeadSHA..head. Empty means "never reviewed", which
	// resolves to a full review — losing this must degrade to more review, never
	// less. Written by phase 4; carried here from phase 2 so one migration
	// serves both.
	LastReviewedHeadSHA string

	// LastReviewedAt is when that review posted. Nil when never reviewed.
	LastReviewedAt *time.Time

	// AutoReviewPaused suppresses the automatic triggers for this PR only,
	// leaving explicit commands working. The per-PR escape hatch that avoids
	// touching project config.
	AutoReviewPaused bool

	UpdatedAt time.Time
}

// ForgePRReviewStateRepository persists per-PR re-review state.
//
// Every method is keyed on (projectID, repo, number) because that is the
// identity of a pull request in this system; nothing here is addressable by
// task id alone.
type ForgePRReviewStateRepository interface {
	// Get returns the row, or (nil, nil) when this PR has no state yet. A
	// missing row is the normal first-delivery case, NOT an error: callers
	// treat it as "never reviewed, nothing in flight", which is the
	// fail-toward-more-review direction.
	Get(ctx context.Context, projectID, repo string, number int) (*ForgePRReviewState, error)

	// ClaimOrSupersede is the coalescing primitive, and it is one atomic
	// statement on purpose.
	//
	// It records headSHA as the newest observed head for this PR, and reports
	// which task (if any) currently holds the claim. Callers compare that task
	// against the task store to decide ABSORBING vs CLOSING; this method does
	// not read `tasks` itself, keeping the derivation in one place.
	//
	// Returns the task id the row held BEFORE this call, so a caller can tell
	// "nobody was reviewing" (empty) from "task X was". Doing the read and the
	// write separately would let two deliveries both observe an empty claim.
	ClaimOrSupersede(ctx context.Context, projectID, repo string, number int, headSHA string) (priorTaskID string, err error)

	// SetTask points the row at the review task that now owns this PR. Called
	// immediately after the task is created, so the claim and the task come
	// into existence together.
	SetTask(ctx context.Context, projectID, repo string, number int, taskID string) error

	// BeginClosing performs the ABSORBING → CLOSING transition as ONE step: it
	// releases the claim and records the head the review actually fetched.
	//
	// One call rather than two because the pair must not be separable. A
	// trigger landing between a claim release and a SHA write would enqueue its
	// own task (correct) against a row that then gets stamped with a SHA nobody
	// agreed on.
	//
	// CONDITIONAL ON expectedPendingHeadSHA — the value of PendingHeadSHA the
	// caller read when it decided which head to fetch. Design §5.2 requires the
	// read and the transition to be "ONE atomic step on the per-PR row, so no
	// trigger can land between them", and no implementation can hold that
	// atomicity across the forge round-trip the caller makes in between. The
	// compare-and-set restores it: a push absorbed during the fetch advances
	// PendingHeadSHA, the transition then does not apply, and the caller learns
	// the newer head instead of committing to a stale one.
	//
	// Unconditional, this was the §5.2 "SHA_C is lost" failure reintroduced: the
	// claim is held for the whole fetch, so a push landing in that window is
	// ABSORBED and its delivery skipped as superseded — and nothing afterwards
	// reads the leftover PendingHeadSHA, so if the developer does not push
	// again that head is permanently unreviewed while a posted review implies
	// coverage.
	//
	// Returns Applied=false and the row's current PendingHeadSHA when the
	// transition did not apply. That is not an error: the caller's response is
	// to fetch again at the newer head, not to give up.
	BeginClosing(ctx context.Context, projectID, repo string, number int, reviewingHeadSHA, expectedPendingHeadSHA string) (ClosingOutcome, error)

	// MarkReviewed advances LastReviewedHeadSHA and clears the claim, and is
	// called ONLY after the review has actually posted. Advancing it when a
	// review merely starts would let a crashed review permanently mark commits
	// as reviewed — silently losing coverage, which is the failure this whole
	// design exists to prevent.
	MarkReviewed(ctx context.Context, projectID, repo string, number int, headSHA string, at time.Time) error

	// SetPaused sets or clears the per-PR automatic-review suppression.
	SetPaused(ctx context.Context, projectID, repo string, number int, paused bool) error
}

// ClosingOutcome is BeginClosing's answer: whether the ABSORBING → CLOSING
// transition applied, and if not, what the row now holds.
//
// A struct rather than a bare bool because the caller needs the newer head to
// act on the refusal, and rather than an error because a superseding push is
// the system working, not a fault.
type ClosingOutcome struct {
	// Applied reports that the claim was released and ReviewingHeadSHA written.
	Applied bool

	// PendingHeadSHA is the row's current pending head. Meaningful when
	// Applied is false: it is the head that superseded the one the caller
	// fetched, and the head the caller should fetch next.
	PendingHeadSHA string
}
