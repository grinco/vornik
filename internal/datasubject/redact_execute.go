package datasubject

import (
	"context"
	"errors"
	"fmt"
)

// Execution of a DispositionRedact action — the generative half of Art 17 erasure.
//
// A chunk naming two people cannot be half-deleted, so where the recorded ground
// leaves the controller discretion (§5.3) the chunk is REDACTED: the subject's data
// is rewritten out and everyone else's is preserved. That rewrite is an LLM call on
// a destructive path, which is why every step below is arranged so that the failure
// mode is "deferred to a human" rather than "reported as erased".
//
// see LLD § https://docs.vornik.io §5

// RedactionOutcome is what the store actually did with a proposed rewrite.
type RedactionOutcome string

const (
	// RedactionApplied means content, hash, embedding, queue and cache all moved
	// together in one transaction.
	RedactionApplied RedactionOutcome = "applied"
	// RedactionVersionChanged means the chunk's content_hash no longer matched the
	// value read at planning time, so the update matched zero rows and was
	// abandoned. The chunk changed under us; re-plan rather than overwrite.
	RedactionVersionChanged RedactionOutcome = "version_changed"
	// RedactionCollision means the rewritten text hashes to a chunk that already
	// exists in this project — two chunks differing only in the erased subject's
	// data. The survivor's id is returned so the caller can decide.
	RedactionCollision RedactionOutcome = "collision"
)

// ChunkRedactor is the store side of redaction.
//
// Deliberately narrow: the store owns hashing (so `memory.ContentHash` stays the one
// definition), the optimistic version guard, and the single transaction that moves
// content, hash, embedding, re-embed queue and embedding cache together. Nothing
// here re-implements those in the decision layer.
type ChunkRedactor interface {
	// LoadChunk returns the chunk's current text and content hash. The hash is the
	// version token the subsequent write is guarded on.
	LoadChunk(ctx context.Context, chunkID string) (content, contentHash string, err error)

	// RedactChunk replaces the chunk's content, guarded by expectedHash.
	//
	// One transaction, all of it or none: new content, recomputed hash,
	// `embedding = NULL`, a re-embed enqueued, and the PRE-redaction cache entry
	// evicted. Zero rows affected means the guard fired — report
	// RedactionVersionChanged and write nothing.
	//
	// On a hash collision, report RedactionCollision with the surviving chunk's id
	// and write nothing; the caller resolves it, because the resolution depends on
	// the plan and the store cannot see the plan.
	RedactChunk(ctx context.Context, chunkID, expectedHash, newContent string) (RedactionResult, error)
}

// RedactionResult is what the store did, with the field that matters for each
// outcome. A single overloaded return value would make the applied and collision
// paths read the same variable for different things.
type RedactionResult struct {
	Outcome RedactionOutcome
	// NewHash is what the chunk now hashes to. Set on RedactionApplied; recorded
	// against the request so the before/after pair is auditable.
	NewHash string
	// SurvivorID is the pre-existing chunk the rewrite collided with. Set on
	// RedactionCollision only.
	SurvivorID string
}

// ChunkRewriter produces the redacted text. The implementation is an LLM call;
// nothing in this package assumes it is correct — VerifyRedaction is what makes it
// safe to use on a destructive path.
type ChunkRewriter interface {
	// RewriteWithout returns content with every trace of the given identifiers
	// removed and all other subjects' information preserved.
	RewriteWithout(ctx context.Context, content string, identifiers []string) (string, error)
	// ModelVersion identifies what produced the rewrite, for the request record.
	// A generative decision about someone's personal data must be attributable.
	ModelVersion() string
}

// IdentifierSource supplies the subject's identifiers at EXECUTION time.
//
// Re-queried rather than carried from planning (§5 step 2): a plan-time snapshot can
// be stale, and verifying against a stale identifier set would report success while
// a newly-recorded identifier survives in the text.
type IdentifierSource interface {
	Identifiers(ctx context.Context, subjectID string) ([]string, error)
}

// RedactionProposal is what an operator reviews before a rewrite is committed.
type RedactionProposal struct {
	Table       LinkableTable `json:"table"`
	RowID       string        `json:"row_id"`
	Before      string        `json:"before"`
	After       string        `json:"after"`
	Model       string        `json:"model"`
	Identifiers []string      `json:"identifiers"`
}

// RedactedAction records a committed redaction for the request record and the
// subject-facing report.
type RedactedAction struct {
	Table      LinkableTable `json:"table"`
	RowID      string        `json:"row_id"`
	BeforeHash string        `json:"before_hash"`
	AfterHash  string        `json:"after_hash"`
	Model      string        `json:"model"`
	// Verified is always true for a committed redaction — the write is unreachable
	// otherwise. Recorded explicitly because it is the claim the report makes.
	Verified bool `json:"verified"`
	// ReviewBypassed marks a redaction applied under --apply, so a later audit can
	// find every record a generative model changed without a human reading it.
	ReviewBypassed bool `json:"review_bypassed"`
}

// CollisionDeletion records a chunk deleted because its redacted form duplicated an
// existing chunk. Reported separately from a redaction because what the subject is
// told differs: this record was removed in full, not rewritten.
type CollisionDeletion struct {
	Table      LinkableTable `json:"table"`
	RowID      string        `json:"row_id"`
	SurvivorID string        `json:"survivor_id"`
}

// ErrReviewRequired is the reason recorded when a proposal had no approver. It is a
// deferral, not a failure: nothing went wrong, a human simply has not looked yet.
var ErrReviewRequired = errors.New("datasubject: redaction awaits operator review")

// RedactDeps are the stores a redaction needs beyond the delete path. Left nil on
// an Executor that only deletes, in which case a planned redaction defers with the
// capability-missing reason rather than failing.
type RedactDeps struct {
	Redactor    ChunkRedactor
	Rewriter    ChunkRewriter
	Identifiers IdentifierSource
	// Approve is the operator review gate (§8). It is default-ON permanently: a
	// human reads what a generative model proposes to do to a record concerning a
	// third party before it happens. A nil Approve therefore DEFERS rather than
	// applying — fail closed, because the alternative is an unreviewed generative
	// write to someone else's data.
	Approve func(RedactionProposal) (bool, error)
	// ApplyWithoutReview is --apply. Audited, not silent: every action it commits
	// carries ReviewBypassed.
	ApplyWithoutReview bool
}

// collisionRetry marks a chunk deferred because the chunk it collided with is itself
// awaiting redaction in this same plan. Deleting into that survivor would keep a
// chunk that still contains the subject (§4.2 collision sub-case), so it waits for
// the second pass.
type collisionRetry struct {
	action     Action
	survivorID string
}

// applyRedact performs one redaction and records the outcome.
//
// Returns a non-nil *collisionRetry when the action should be retried after the rest
// of the plan, which is the only case the caller has to sequence.
func (e *Executor) applyRedact(ctx context.Context, a Action, ids []string,
	plan *ErasurePlan, res *ErasureResult) *collisionRetry {
	d := e.Redact
	before, beforeHash, err := d.Redactor.LoadChunk(ctx, a.RowID)
	if err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("load chunk for redaction: %v", err)})
		return nil
	}

	after, err := d.Rewriter.RewriteWithout(ctx, before, ids)
	if err != nil {
		// A model failure is a deferral, not a data error: nothing was written and
		// the chunk is exactly as it was.
		e.deferRedaction(a, res, fmt.Sprintf("the rewrite could not be produced (%v); "+
			"this record has NOT been changed", err))
		return nil
	}

	// THE FLOOR. A rewrite that still contains the subject is discarded — no write,
	// chunk stays deferred. This is what makes a generative step acceptable here.
	if err := VerifyRedaction(ids, after); err != nil {
		var verr *RedactionVerificationError
		reason := "the proposed rewrite did not verify clean, so it was discarded and " +
			"this record has NOT been changed"
		if errors.As(err, &verr) {
			reason = fmt.Sprintf("%s (%d identifier(s) still present)", reason, len(verr.Surviving))
		}
		e.deferRedaction(a, res, reason)
		return nil
	}

	if !e.reviewApproved(a, before, after, ids, res) {
		return nil
	}

	out, err := d.Redactor.RedactChunk(ctx, a.RowID, beforeHash, after)
	if err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("commit redaction: %v", err)})
		return nil
	}

	switch out.Outcome {
	case RedactionApplied:
		res.Redacted = append(res.Redacted, RedactedAction{
			Table: a.Table, RowID: a.RowID,
			BeforeHash:     beforeHash,
			AfterHash:      out.NewHash,
			Model:          d.Rewriter.ModelVersion(),
			Verified:       true,
			ReviewBypassed: d.ApplyWithoutReview,
		})
	case RedactionVersionChanged:
		// The version guard fired. All four post-conditions matter here: no partial
		// write happened (the store guarantees that), the chunk stays deferred, the
		// request stays un-actioned via Complete(), and the reason is recorded.
		e.deferRedaction(a, res, "this record changed while the erasure was being "+
			"prepared, so it was left untouched rather than overwritten; it will be "+
			"re-planned on the next run")
	case RedactionCollision:
		if plan.pendingRedaction(out.SurvivorID) {
			return &collisionRetry{action: a, survivorID: out.SurvivorID}
		}
		e.resolveCollision(ctx, a, out.SurvivorID, res)
	default:
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("unknown redaction outcome %q", out.Outcome)})
	}
	return nil
}

// reviewApproved applies the §8 gate. Returns false when the redaction must not
// proceed, having already recorded the deferral.
func (e *Executor) reviewApproved(a Action, before, after string, ids []string,
	res *ErasureResult) bool {
	d := e.Redact
	if d.ApplyWithoutReview {
		return true
	}
	if d.Approve == nil {
		e.deferRedaction(a, res, ErrReviewRequired.Error())
		return false
	}
	ok, err := d.Approve(RedactionProposal{
		Table: a.Table, RowID: a.RowID, Before: before, After: after,
		Model: d.Rewriter.ModelVersion(), Identifiers: ids,
	})
	if err != nil {
		e.deferRedaction(a, res, fmt.Sprintf("operator review could not be completed (%v)", err))
		return false
	}
	if !ok {
		e.deferRedaction(a, res, "an operator declined the proposed rewrite; this record "+
			"has NOT been changed and needs manual handling")
		return false
	}
	return true
}

// resolveCollision deletes the chunk whose redacted form duplicates an existing one.
//
// The survivor already carries the other subjects' data and does not contain this
// subject, so keeping one copy is both correct and what was wanted — two identical
// chunks were never intended. Recorded as a DELETION, distinctly from a redaction,
// because that is what the subject is told.
func (e *Executor) resolveCollision(ctx context.Context, a Action, survivorID string, res *ErasureResult) {
	if err := e.Rows.DeleteRow(ctx, a.Table, a.RowID); err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("delete chunk that collided with %s: %v", survivorID, err)})
		return
	}
	res.CollisionDeleted = append(res.CollisionDeleted, CollisionDeletion{
		Table: a.Table, RowID: a.RowID, SurvivorID: survivorID,
	})
}

func (e *Executor) deferRedaction(a Action, res *ErasureResult, reason string) {
	res.Deferred = append(res.Deferred, DeferredAction{
		Table: a.Table, RowID: a.RowID, Disposition: a.Disposition, Reason: reason,
	})
}

// pendingRedaction reports whether the given row id is itself awaiting redaction in
// this plan. Deleting a chunk into a survivor that still contains the subject would
// preserve exactly what the request asked to remove.
func (p *ErasurePlan) pendingRedaction(rowID string) bool {
	if p == nil || rowID == "" {
		return false
	}
	for _, a := range p.Actions {
		if a.RowID == rowID && a.Disposition == DispositionRedact {
			return true
		}
	}
	return false
}
