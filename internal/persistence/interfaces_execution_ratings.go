package persistence

import (
	"context"
	"time"
)

// Human verdicts on what an execution produced
// (LLD 2026-09-04-execution-ratings-design).
//
// A rating is an EDITABLE SIDECAR: editable because an operator's first
// reaction to a digest is not their considered one, and a sidecar — its own
// table, no foreign key — because a rating living on the execution would change
// the history a later step derives from. The execution record is what the
// system reasons over; the rating is what a human thinks of it, and those must
// not be the same row (design §2).
//
// Three things it is NOT, all load-bearing (design §2):
//   - NOT an input to behaviour. Nothing in the executor, the instinct layer or
//     the skill store reads a rating. It informs an OPERATOR deciding whether to
//     approve or retire something; it never gates a run.
//   - NOT telemetry. It has no handoff of its own.
//   - NOT a score. Up or down plus an optional one-line reason — no scale,
//     because the difference between three and four stars is undefinable, and no
//     average, because an average of these is the vanity metric the design
//     spends §5 avoiding.

// Rating verdicts. The set is closed here, at the handler, and by the table's
// own CHECK constraint, so a future writer that skips the handler still cannot
// introduce a third value.
const (
	VerdictUp   = "up"
	VerdictDown = "down"
)

// ExecutionRatingReasonMax bounds the optional reason. Over the cap is REFUSED
// rather than truncated: a rating that says half of what its author wrote is
// worse than one that made them shorten it (design §3).
const ExecutionRatingReasonMax = 500

// ExecutionRating is one human verdict on one execution, by one rater.
type ExecutionRating struct {
	// ExecutionID is the run being judged. Keyed to the EXECUTION, not the
	// task, because that is how execution_injected_skills and
	// instinct_applications are keyed — a task-keyed rating would need a join
	// to reach either, and a task with three executions could not say which one
	// was bad.
	ExecutionID string
	// RaterID is the resolved operator identity. Never empty: an anonymous
	// rating cannot be edited by its author, attributed in a rollup, or told
	// apart from a second rater's.
	RaterID string
	// Verdict is VerdictUp or VerdictDown.
	Verdict string
	// Reason is an optional one-line justification, "" when not given. Never
	// required — asking for a reason before accepting a down-vote is how you
	// stop getting down-votes.
	Reason string
	// CreatedAt is when this rater FIRST judged the run. Preserved across
	// edits, so "when was this first judged" survives a change of mind.
	CreatedAt time.Time
	// UpdatedAt moves on every re-rating.
	UpdatedAt time.Time
}

// ExecutionRatingRepository is the backend-agnostic contract for execution
// ratings. Implemented by internal/persistence/{postgres,sqlite} and verified
// by repotest.RunExecutionRatingSuite.
type ExecutionRatingRepository interface {
	// Upsert records or replaces one rater's verdict on one execution.
	// Re-rating replaces the verdict and reason and bumps UpdatedAt while
	// PRESERVING CreatedAt; a different rater is a different row.
	Upsert(ctx context.Context, rating *ExecutionRating) error

	// Get returns one rater's rating for one execution.
	//
	// An absent rating is ErrNotFound, per the codebase-wide miss convention
	// rather than the (nil, nil) shape "unrated is ordinary" would suggest.
	// ForgePRReviewStateRepository.Get is the one registered exception, and it
	// earned that by having callers who read absence as a POSITIVE state
	// ("never reviewed, nothing in flight"). Nothing here does: the handler
	// turns absence into a 404, which is the honest HTTP answer to "what is my
	// rating on this run", and keeping the exception set at one is worth more
	// than the ergonomics of a nil check.
	Get(ctx context.Context, executionID, raterID string) (*ExecutionRating, error)

	// ListByExecution returns every rater's rating for one execution, oldest
	// first. Empty when none.
	ListByExecution(ctx context.Context, executionID string) ([]*ExecutionRating, error)

	// Delete removes one rater's rating. Deleting one that is not there is not
	// an error — the caller's intent is "my rating should not exist", and it
	// does not.
	Delete(ctx context.Context, executionID, raterID string) error

	// DeleteOlderThan prunes ratings whose CreatedAt precedes cutoff and
	// returns how many went. Ratings have their OWN horizon rather than the
	// execution's: a rating is small, it is the scarcest signal in the system,
	// and phase 2 compares windows (design §6).
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}
