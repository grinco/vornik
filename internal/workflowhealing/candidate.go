package workflowhealing

import (
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
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
// left empty HERE and stamped by the caller via StampBaselineGenome,
// which needs the live workflow this constructor has no access to.
// (This comment used to claim the trial runner stamped it at trial
// time. It never did — see StampBaselineGenome.) Returns nil when
// either input is nil.
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

// StampBaselineGenome records what the workflow looked like when the repair
// was designed, which is what makes the promoter's currency gate meaningful
// (see ErrGenomeDrift). Nil-safe on both arguments.
//
// STAMPED AT CREATION, not at trial time. The distinction is the whole point:
// the baseline must be the file the proposal was WRITTEN AGAINST. Stamping it
// when the trial runs would record whatever happened to be live by then, which
// is precisely the drift the gate exists to catch — the check would then pass
// on a candidate that had already gone stale before anyone trialled it.
//
// (`persistHealingCandidate` carried a comment asserting the trial runner did
// this. It never did, and both candidates that reached trial_passed on the
// reference deployment had an empty baseline as a result — a documented
// behaviour nothing implements, which is worse than an absent one because it
// stops the next person looking.)
func StampBaselineGenome(cand *persistence.HealingCandidate, live *registry.Workflow) {
	if cand == nil || live == nil {
		return
	}
	cand.BaselineGenomeHash = GenomeHash(live)
}
