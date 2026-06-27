package workflowhealing

import (
	"vornik.io/vornik/internal/persistence"
)

// CandidateFromArchitectProposal builds the workflow_healing_candidates
// row that LINKS a healing trigger to the WorkflowProposal the memetic
// architect just produced for it. Shared by BOTH generate-candidate
// surfaces — the admin API handler and the /ui/admin/blackbox trigger
// page — so the two paths cannot drift. (Regression context
// 2026-06-06: the UI path stamped the trigger but never persisted a
// candidate row, so /ui/admin/blackbox/candidates stayed empty for
// every UI-generated candidate.)
//
// proposal_diff/motivation are denormalised copies; the proposal
// remains the apply-path source of truth. The baseline genome hash is
// left empty — the trial runner stamps it against the live genome at
// trial time. Returns nil when either input is nil.
func CandidateFromArchitectProposal(t *persistence.HealingTrigger, p *persistence.WorkflowProposal) *persistence.HealingCandidate {
	if t == nil || p == nil {
		return nil
	}
	candidateHash := ""
	if h, err := GenomeHashFromMarkdown([]byte(p.ProposalYAML), p.WorkflowID+".md"); err == nil {
		candidateHash = h
	}
	return &persistence.HealingCandidate{
		TriggerID:           t.ID,
		ProjectID:           t.ProjectID,
		WorkflowID:          t.WorkflowID,
		ProposalID:          p.ID,
		CandidateGenomeHash: candidateHash,
		// Architect-sourced (the deterministic recipes are a separate
		// generation path).
		CandidateClass: persistence.HealingCandidateArchitect,
		ProposalDiff:   p.ProposalYAML,
		Motivation:     p.Motivation,
		ExpectedEffect: "Architect-proposed structural repair; see motivation and trial scorecard.",
		RiskLevel:      persistence.HealingRiskMedium,
		Status:         persistence.HealingCandidateDraft,
	}
}
