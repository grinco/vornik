package api

import (
	"context"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// THE HEALING CANDIDATE PRODUCER, as one callable unit.
//
// Why this exists as a method rather than staying inline in the HTTP handler:
// the UI had its OWN copy of this orchestration (load trigger → check open →
// call producer → stamp trigger → persist candidate). When the 2026-09-16
// cutover repointed the producer from the memetic architect to the config
// assistant, it repointed the API copy and not the UI one — so the UI's
// Generate candidate button lost its producer entirely and the page started
// telling operators to call a deleted endpoint.
//
// Two implementations of one decision is how that happens, and the fix is not
// to repoint the second copy but to delete it. The HTTP handler and the UI
// handler are now two PRESENTATIONS of this single producer: they differ in
// how they render an outcome, never in what the outcome is.
//
// THE OPERATOR TYPES NOTHING. Everything the assistant needs is derived from
// the trigger row: the project, the workflow, the evidence execution set, and
// the intent (healingReason renders the metric, both sides of the comparison
// and the threshold). A generate-candidate action is one button, by
// construction — there is no prompt field and there must not be one, because
// a hand-typed intent would not be reproducible from the evidence.

var (
	// ErrHealingTriggerRepoUnavailable — no trigger ledger on this deployment.
	ErrHealingTriggerRepoUnavailable = errors.New("api: workflow-healing trigger repository not wired")
	// ErrHealingTriggerNotFound — no such trigger.
	ErrHealingTriggerNotFound = errors.New("api: no such healing trigger")
	// ErrHealingTriggerNotOpen — only an open trigger can generate. A
	// resolved one already has its candidate (or was dismissed).
	ErrHealingTriggerNotOpen = errors.New("api: only open triggers can generate candidates")
)

// TriggerStampError reports the one partial-failure worth its own type: the
// proposal was created and persisted, but stamping the trigger with its id
// failed. The operator must not lose the producer's work, so the proposal id
// travels with the error and every surface points at it.
type TriggerStampError struct {
	ProposalID string
	Err        error
}

func (e *TriggerStampError) Error() string {
	return fmt.Sprintf("proposal %s created but trigger stamp failed: %v", e.ProposalID, e.Err)
}

func (e *TriggerStampError) Unwrap() error { return e.Err }

// HealingCandidateOutcome is what the producer decided.
type HealingCandidateOutcome struct {
	// Proposal is the filed WorkflowProposal. Never nil on a nil error.
	Proposal *persistence.WorkflowProposal
	// ByRecipe is true when a deterministic structural recipe handled it and
	// no model was called at all — worth surfacing, because it is the cheap,
	// predictable path and an operator reading "candidate generated" should
	// know which produced it.
	ByRecipe bool
}

// GenerateHealingCandidateForTrigger runs the full producer for one trigger
// and returns what it decided. It is the ONLY implementation; both the admin
// API handler and the UI generate-candidate button call it.
//
// Order is deliberate and unchanged from the cutover: a deterministic
// structural recipe is tried FIRST, because it is cheaper and more predictable
// than any model, and the assistant handles only what recipes cannot.
//
// Persistence is layered by how much it would cost to lose: the proposal and
// the trigger stamp are durable before the candidate row is attempted, and a
// candidate-insert failure is logged rather than rolled back — losing the
// denormalised row is recoverable, losing the producer's output is not.
func (s *Server) GenerateHealingCandidateForTrigger(ctx context.Context, triggerID string) (*HealingCandidateOutcome, error) {
	if s.healingTriggerRepo == nil {
		return nil, ErrHealingTriggerRepoUnavailable
	}
	t, err := s.healingTriggerRepo.Get(ctx, triggerID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrHealingTriggerNotFound, triggerID)
		}
		return nil, fmt.Errorf("api: trigger lookup failed: %w", err)
	}
	if t.Status != persistence.HealingTriggerStatusOpen {
		return nil, fmt.Errorf("%w: %s is %s", ErrHealingTriggerNotOpen, triggerID, t.Status)
	}

	// 1. Deterministic recipe, before paying for the assistant.
	if out, handled, rerr := s.recipeCandidate(ctx, t); handled {
		return out, rerr
	}

	// 2. The config assistant (WP9 step 3, the 2026-09-16 cutover).
	if s.healingAssistant == nil {
		return nil, errHealingProducerUnavailable
	}
	proposal, err := s.generateAssistantCandidate(ctx, t)
	if err != nil {
		return nil, err
	}
	if serr := s.healingTriggerRepo.MarkGenerated(ctx, triggerID, proposal.ID); serr != nil {
		return nil, &TriggerStampError{ProposalID: proposal.ID, Err: serr}
	}
	s.persistHealingCandidate(ctx, t, proposal)
	return &HealingCandidateOutcome{Proposal: proposal}, nil
}

// recipeCandidate runs the deterministic structural path. handled is false when
// no recipe applies OR when the recipe's proposal could not be persisted — in
// both cases the caller falls through to the assistant, because an
// un-persistable recipe must not cost the operator their repair.
func (s *Server) recipeCandidate(ctx context.Context, t *persistence.HealingTrigger) (*HealingCandidateOutcome, bool, error) {
	proposal, cand, ok := s.tryRecipeCandidate(ctx, t)
	if !ok {
		return nil, false, nil
	}
	if err := s.workflowProposals.Insert(ctx, proposal); err != nil {
		s.logger.Warn().Err(err).Str("trigger_id", t.ID).
			Msg("healing recipe proposal insert failed; falling back to the assistant")
		return nil, false, nil
	}
	if err := s.healingTriggerRepo.MarkGenerated(ctx, t.ID, proposal.ID); err != nil {
		return nil, true, &TriggerStampError{ProposalID: proposal.ID, Err: err}
	}
	// The recipe path fingerprints its own baseline (CandidateFromRecipeResult
	// stamps BaselineGenomeHash from the workflow it transformed), so the
	// promoter's currency gate can check it without a second lookup here.
	if s.healingCandidateRepo != nil {
		if err := s.healingCandidateRepo.Insert(ctx, cand); err != nil {
			s.logger.Warn().Err(err).Str("trigger_id", t.ID).Str("proposal_id", proposal.ID).
				Msg("healing recipe candidate persist failed; proposal + trigger stamp are durable")
		}
	}
	s.logger.Info().Str("trigger_id", t.ID).Str("proposal_id", proposal.ID).
		Str("candidate_class", string(cand.CandidateClass)).
		Msg("healing candidate generated by deterministic recipe (no model called)")
	return &HealingCandidateOutcome{Proposal: proposal, ByRecipe: true}, true, nil
}
