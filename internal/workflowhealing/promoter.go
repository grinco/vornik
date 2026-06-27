package workflowhealing

// Promotion + rejection — Self-Healing Workflow Genome v1 (LLD
// § Promotion). Promotion is the ONLY path that mutates production, and
// it is ALWAYS a manual operator action (LLD non-negotiable #1: nothing
// auto-promotes). The Promoter:
//
//  1. loads the candidate and refuses anything not in trial_passed —
//     a candidate that has not cleared a trial CANNOT be promoted, and
//     a terminal (promoted/rejected) candidate is rejected;
//  2. APPROVES the linked memetic WorkflowProposal (pending → approved)
//     so the existing apply path's approved-only guard is satisfied —
//     we do NOT write a second apply path;
//  3. REUSES the memetic Applier (write WORKFLOW.md → validate → commit
//     → hot-reload → proposal applied) — the same code the operator
//     review UI calls;
//  4. flips the candidate to promoted (stamps promoted_by/promoted_at).
//
// The trigger that spawned the candidate is already terminal at
// generated_candidate (the shipped trigger CHECK constraint has no
// 'promoted' state); the candidate row carries the post-promotion
// lifecycle, so the trigger needs no further transition.
//
// Reject is the operator's "no" — it flips the candidate to rejected
// without touching the proposal or production.
//
// This unit performs NO autonomous work: every method is invoked
// directly by the operator promote/reject endpoint. There is no
// background loop.

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

var (
	// ErrCandidateNotPromotable is returned when promotion is requested
	// on a candidate that has not cleared a trial (status !=
	// trial_passed). Promotion requires a passing trial — this is the
	// gate that enforces "nothing promotes without trial_passed".
	ErrCandidateNotPromotable = errors.New("workflowhealing: candidate is not promotable (requires trial_passed)")
	// ErrNoProposalLinked is returned when a candidate has no linked
	// WorkflowProposal — there is nothing to apply.
	ErrNoProposalLinked = errors.New("workflowhealing: candidate has no linked proposal")
	// ErrApplierNotWired is returned when the Promoter was constructed
	// without a memetic applier — promotion is impossible.
	ErrApplierNotWired = errors.New("workflowhealing: memetic applier not wired")
)

// ProposalApplier is the narrow seam over the memetic apply path the
// Promoter reuses. Production wires this to *memetic.Applier.Apply; the
// signature is identical so the service layer passes the Applier
// directly. The Promoter NEVER reimplements write/validate/commit/reload
// — it only calls through here.
type ProposalApplier interface {
	// Apply runs the approved-proposal apply turn: write WORKFLOW.md to
	// both trees, validate, git-commit, hot-reload, stamp the proposal
	// applied. Returns the updated proposal row. Errors propagate
	// (memetic.ErrProposalNotApproved, persistence.ErrNotFound, etc.).
	Apply(ctx context.Context, proposalID, decidedBy string) (*persistence.WorkflowProposal, error)
}

// Metrics is the nil-safe metrics seam for promotion/rejection. The
// blackbox.Metrics type satisfies it via RecordPromotion (a thin
// adapter in the service layer); a nil Metrics is a no-op.
type Metrics interface {
	RecordPromotion()
}

// Promoter owns the operator promote/reject actions. All deps are
// supplied at construction; candidates + proposals + applier are
// required for Promote, candidates alone for Reject. Metrics is
// optional (nil-safe).
type Promoter struct {
	candidates persistence.WorkflowHealingCandidateRepository
	proposals  persistence.WorkflowProposalRepository
	applier    ProposalApplier
	metrics    Metrics
	log        zerolog.Logger
}

// NewPromoter wires the promoter. candidates is mandatory; proposals +
// applier are required only for Promote (Reject works without them).
// metrics may be nil.
func NewPromoter(
	candidates persistence.WorkflowHealingCandidateRepository,
	proposals persistence.WorkflowProposalRepository,
	applier ProposalApplier,
	metrics Metrics,
	log zerolog.Logger,
) *Promoter {
	return &Promoter{
		candidates: candidates,
		proposals:  proposals,
		applier:    applier,
		metrics:    metrics,
		log:        log,
	}
}

// Promote applies the candidate's linked proposal to production and
// marks the candidate promoted. It is invoked ONLY by the operator
// promote endpoint (manual action, LLD non-negotiable #1).
//
// Preconditions enforced here:
//   - candidate exists (else ErrCandidateNotFound);
//   - candidate status == trial_passed (else ErrCandidateNotPromotable
//     for non-terminal, ErrCandidateTerminal for promoted/rejected);
//   - candidate has a linked proposal (else ErrNoProposalLinked);
//   - applier is wired (else ErrApplierNotWired).
//
// Sequence (proposal-state-machine aware):
//  1. If the proposal is still pending, approve it (Decide → approved).
//     The architect leaves proposals pending; promotion is the operator
//     approving + applying in one action.
//  2. Apply via the memetic path.
//  3. Flip the candidate to promoted, stamping promotedBy.
//
// promotedBy is the operator identity (admin email / key id) for
// provenance — it lands on both the proposal's decided_by/applied note
// and the candidate's promoted_by column.
func (p *Promoter) Promote(ctx context.Context, candidateID, promotedBy string) (*persistence.HealingCandidate, error) {
	if p.candidates == nil {
		return nil, fmt.Errorf("workflowhealing.Promote: candidates repo not wired")
	}

	cand, err := p.candidates.Get(ctx, candidateID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrCandidateNotFound, candidateID)
		}
		return nil, fmt.Errorf("workflowhealing.Promote: load candidate: %w", err)
	}

	// Terminal candidates can't be re-promoted.
	if cand.Status.IsTerminal() {
		return nil, fmt.Errorf("%w: %s (status=%s)", ErrCandidateTerminal, candidateID, cand.Status)
	}
	// Only a trial_passed candidate is promotable — this is the hard
	// gate that prevents promoting an untried or failed candidate.
	if cand.Status != persistence.HealingCandidateTrialPassed {
		return nil, fmt.Errorf("%w: %s (status=%s)", ErrCandidateNotPromotable, candidateID, cand.Status)
	}
	if cand.ProposalID == "" {
		return nil, fmt.Errorf("%w: %s", ErrNoProposalLinked, candidateID)
	}
	if p.applier == nil || p.proposals == nil {
		return nil, ErrApplierNotWired
	}

	// 1. Ensure the linked proposal is approved. The architect leaves
	// it pending; promotion is the operator approving it. If it is
	// already approved (operator approved via the review UI first),
	// skip Decide. Anything else (applied/rejected/rolled_back) is a
	// state we cannot promote from.
	prop, err := p.proposals.Get(ctx, cand.ProposalID)
	if err != nil {
		return nil, fmt.Errorf("workflowhealing.Promote: load proposal %s: %w", cand.ProposalID, err)
	}
	switch prop.Status {
	case persistence.WorkflowProposalStatusPending:
		notes := fmt.Sprintf("promoted via self-healing candidate %s", candidateID)
		if derr := p.proposals.Decide(ctx, cand.ProposalID, persistence.WorkflowProposalStatusApproved, promotedBy, notes); derr != nil {
			return nil, fmt.Errorf("workflowhealing.Promote: approve proposal %s: %w", cand.ProposalID, derr)
		}
	case persistence.WorkflowProposalStatusApproved:
		// Already approved by the operator; proceed straight to apply.
	default:
		return nil, fmt.Errorf("workflowhealing.Promote: proposal %s is in status %q; cannot promote", cand.ProposalID, prop.Status)
	}

	// 2. Reuse the memetic apply path. This is the ONLY production
	// mutation, and it runs the existing write→validate→commit→reload.
	if _, aerr := p.applier.Apply(ctx, cand.ProposalID, promotedBy); aerr != nil {
		return nil, fmt.Errorf("workflowhealing.Promote: apply proposal %s: %w", cand.ProposalID, aerr)
	}

	// 3. Flip the candidate to promoted. A failure here leaves the
	// proposal applied but the candidate not stamped — surface the
	// error so the operator can reconcile; we do NOT roll back the
	// apply (the workflow change is already live and correct).
	if perr := p.candidates.Promote(ctx, candidateID, promotedBy); perr != nil {
		return nil, fmt.Errorf("workflowhealing.Promote: mark candidate promoted (proposal %s already applied): %w", cand.ProposalID, perr)
	}

	if p.metrics != nil {
		p.metrics.RecordPromotion()
	}

	p.log.Info().
		Str("candidate_id", candidateID).
		Str("proposal_id", cand.ProposalID).
		Str("workflow_id", cand.WorkflowID).
		Str("trigger_id", cand.TriggerID).
		Str("promoted_by", promotedBy).
		Msg("workflowhealing: candidate promoted (memetic apply path)")

	// Return the freshly-stamped candidate row.
	updated, err := p.candidates.Get(ctx, candidateID)
	if err != nil {
		// Best-effort read-back; the promotion succeeded regardless.
		return cand, nil
	}
	return updated, nil
}

// Reject is the operator's "no" — it flips a non-terminal candidate to
// rejected without touching the proposal or production. Invoked only by
// the operator reject endpoint.
//
//   - candidate missing → ErrCandidateNotFound;
//   - candidate already terminal → ErrCandidateTerminal.
func (p *Promoter) Reject(ctx context.Context, candidateID string) (*persistence.HealingCandidate, error) {
	if p.candidates == nil {
		return nil, fmt.Errorf("workflowhealing.Reject: candidates repo not wired")
	}

	cand, err := p.candidates.Get(ctx, candidateID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrCandidateNotFound, candidateID)
		}
		return nil, fmt.Errorf("workflowhealing.Reject: load candidate: %w", err)
	}
	if cand.Status.IsTerminal() {
		return nil, fmt.Errorf("%w: %s (status=%s)", ErrCandidateTerminal, candidateID, cand.Status)
	}

	if err := p.candidates.Reject(ctx, candidateID); err != nil {
		return nil, fmt.Errorf("workflowhealing.Reject: mark candidate rejected: %w", err)
	}

	p.log.Info().
		Str("candidate_id", candidateID).
		Str("workflow_id", cand.WorkflowID).
		Str("trigger_id", cand.TriggerID).
		Msg("workflowhealing: candidate rejected by operator")

	updated, err := p.candidates.Get(ctx, candidateID)
	if err != nil {
		return cand, nil
	}
	return updated, nil
}
