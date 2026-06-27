package workflowhealing

import (
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Recipe → proposal/candidate synthesis — the keystone that makes a
// deterministic recipe PROMOTABLE.
//
// recipes.go produces a RecipeResult (the candidate genome + metadata), but
// the promotion gate (promoter.go) hard-requires a WorkflowProposal:
// HealingCandidate.ProposalID must reference a real proposal, and the
// Promoter applies the change via the memetic Applier reading the proposal.
// Without a proposal a recipe candidate is un-promotable. These two
// constructors close that gap: ProposalFromRecipeResult synthesizes a
// pending proposal carrying the recipe's WORKFLOW.md (so it flows through the
// SAME operator approve → apply → commit → reload path as an architect
// proposal), and CandidateFromRecipeResult links a HealingCandidate to it —
// the sibling of CandidateFromArchitectProposal, but tagged with the recipe's
// real class/hash/risk/expected-effect instead of architect placeholders.

// RecipeProposalProvenance marks a WorkflowProposal as authored by a
// deterministic recipe rather than the LLM architect. It is stamped on the
// proposal's ArchitectModel field (there is no dedicated source column), so
// operators reading the proposals tree can tell a structural recipe apart
// from an architect proposal at a glance.
const RecipeProposalProvenance = "deterministic-recipe"

// recipeProposalKind maps a recipe's candidate class onto the closest
// structural WorkflowProposalKind, so the proposals tree, doctor, and any
// kind-gating policy classify recipe proposals consistently with
// architect ones.
func recipeProposalKind(class persistence.HealingCandidateClass) persistence.WorkflowProposalKind {
	switch class {
	case persistence.HealingCandidateRetryBudget:
		return persistence.WorkflowProposalKindChangeRetryPolicy
	case persistence.HealingCandidateVerifierInsertion:
		return persistence.WorkflowProposalKindAddStep
	default:
		return persistence.WorkflowProposalKindUnspecified
	}
}

// ProposalFromRecipeResult synthesizes a PENDING WorkflowProposal from a
// deterministic recipe result so a recipe candidate is promotable through the
// same apply path as an architect proposal. The returned proposal is NOT yet
// persisted — the caller inserts it via the WorkflowProposal repository, then
// links a candidate with CandidateFromRecipeResult.
//
// Pure except for the generated ID (mirrors the architect's
// persistence.GenerateID("wpr")). Returns nil for a nil result.
func ProposalFromRecipeResult(workflowID string, r *RecipeResult, now time.Time) *persistence.WorkflowProposal {
	if r == nil {
		return nil
	}
	return &persistence.WorkflowProposal{
		ID:             persistence.GenerateID("wpr"),
		WorkflowID:     workflowID,
		Status:         persistence.WorkflowProposalStatusPending,
		Kind:           recipeProposalKind(r.CandidateClass),
		ProposalYAML:   r.ProposalDiff,
		Motivation:     r.Motivation,
		EvidenceRunIDs: append([]string(nil), r.EvidenceExecutionIDs...),
		// Confidence here denotes the certainty of the *transformation* (a
		// deterministic structural edit), NOT a prediction that the repair
		// helps — the trial scorecard, not this field, gates promotion.
		Confidence:     1.0,
		ArchitectModel: RecipeProposalProvenance,
		CreatedAt:      now,
	}
}

// CandidateFromRecipeResult builds a HealingCandidate that links a trigger to
// a recipe-synthesized proposal. It mirrors CandidateFromArchitectProposal but
// carries the recipe's real class, genome hash, expected effect, and risk —
// not the architect-path placeholders. Returns nil if any argument is nil.
func CandidateFromRecipeResult(t *persistence.HealingTrigger, p *persistence.WorkflowProposal, r *RecipeResult) *persistence.HealingCandidate {
	if t == nil || p == nil || r == nil {
		return nil
	}
	return &persistence.HealingCandidate{
		TriggerID:           t.ID,
		ProjectID:           t.ProjectID,
		WorkflowID:          t.WorkflowID,
		ProposalID:          p.ID,
		CandidateGenomeHash: r.CandidateGenomeHash,
		CandidateClass:      r.CandidateClass,
		ProposalDiff:        r.ProposalDiff,
		Motivation:          r.Motivation,
		ExpectedEffect:      r.ExpectedEffect,
		RiskLevel:           r.RiskLevel,
		Status:              persistence.HealingCandidateDraft,
	}
}
