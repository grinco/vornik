package workflowhealing

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// The bridge from a configuration-assistant proposal to a healing candidate —
// config-assistant design §6.3.4, plan §10 (WP9).
//
// WHY A BRIDGE IS NEEDED AT ALL, since the plan calls this step a "switch":
// the two producers return incompatible artifacts. `memetic.Architect` returns
// a WorkflowProposal whose ProposalYAML IS the genome; the assistant returns a
// ControlPlaneProposal carrying `{op, path, content}` file operations. Neither
// can be handed to the other's consumer.
//
// WHY IT IS HONEST RATHER THAN A COERCION: workflow genomes are FILES in the
// deployed config tree (`configs/workflows/<id>.md`), the apply engine writes
// `op.Content` verbatim (`atomicWrite([]byte(op.Content))`, no templating or
// normalisation), and the architect path hashes its genome under the same
// `<workflowID>.md` name. So a one-op assistant proposal against that path
// carries the SAME BYTES the trial runner would apply — not a rendering of
// them, not a diff.

// ErrNotASingleWorkflowEdit is returned when a proposal cannot become a healing
// candidate. One error for every shape, because the caller's response to all of
// them is identical: fall back to the architect path and record the refusal.
var ErrNotASingleWorkflowEdit = errors.New("workflowhealing: proposal is not a single edit to the named workflow")

// GenomeFromAssistantProposal extracts the new genome from an assistant
// proposal, or refuses.
//
// IT REFUSES RATHER THAN TRIMMING, and that is the rule worth mutating. A
// healing candidate is trialled by hashing ONE genome — `CandidateGenomeHash`
// is a single column on a single row, and there is no machinery to trial two.
// So a two-op proposal has no honest reduction: dropping the second op would
// trial something nobody proposed, and the difference would be invisible in
// the scorecard. The only truthful outcomes are "trial exactly what was
// proposed" or "refuse".
//
// The failure mode this protects against is NOT the bridge going inert. Inert
// is safe: the architect path stays wired and a refusal degrades to it. The
// danger is a high refusal rate inviting someone to accept the PRIMARY op and
// ignore the rest, which is exactly the trimming forbidden here (design
// §6.3.4, review-20260915-cc32 F2).
func GenomeFromAssistantProposal(p *persistence.ControlPlaneProposal, workflowID string) (string, error) {
	if p == nil || strings.TrimSpace(workflowID) == "" {
		return "", ErrNotASingleWorkflowEdit
	}
	ops, err := assistantOps(p)
	if err != nil {
		return "", err
	}
	if len(ops) != 1 {
		// Zero ops is refused too: a proposal that changes nothing would
		// otherwise become a candidate with an empty genome, and an empty
		// genome hashes to something the trial runner would cheerfully apply.
		return "", fmt.Errorf("%w: %d file operations, want exactly 1", ErrNotASingleWorkflowEdit, len(ops))
	}
	if !isWorkflowPath(ops[0].Path, workflowID) {
		return "", fmt.Errorf("%w: touches %q, not the workflow %q", ErrNotASingleWorkflowEdit, ops[0].Path, workflowID)
	}
	if strings.TrimSpace(ops[0].Content) == "" {
		return "", fmt.Errorf("%w: the operation has no content", ErrNotASingleWorkflowEdit)
	}
	// Returned VERBATIM. Trimming or normalising here would break the one
	// property that makes the bridge honest: these are the bytes the trial
	// runner applies, and the comparison window hashes them against the
	// architect path's genome for the same trigger.
	return ops[0].Content, nil
}

// assistantOps reads the proposal's file operations, tolerating the
// single-target back-compat shape the apply engine also accepts.
func assistantOps(p *persistence.ControlPlaneProposal) ([]assistantFileOp, error) {
	if raw := strings.TrimSpace(p.ApplyOps); raw != "" {
		var ops []assistantFileOp
		if err := json.Unmarshal([]byte(raw), &ops); err != nil {
			return nil, fmt.Errorf("%w: apply ops are not readable: %v", ErrNotASingleWorkflowEdit, err)
		}
		return ops, nil
	}
	// Phase-2a shape: one implicit replace of (ApplyTarget, ApplyContent).
	// Read the same way the apply engine reads it, so the bridge and the
	// applier cannot disagree about what a proposal contains.
	if strings.TrimSpace(p.ApplyTarget) == "" {
		return nil, fmt.Errorf("%w: review-only proposal, nothing to apply", ErrNotASingleWorkflowEdit)
	}
	return []assistantFileOp{{Op: "replace", Path: p.ApplyTarget, Content: p.ApplyContent}}, nil
}

type assistantFileOp struct {
	Op      string `json:"op"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

// isWorkflowPath reports whether a config-relative path is the named
// workflow's genome file.
//
// Matched on the base name under a workflows/ directory rather than on a
// single hardcoded string, because the config tree nests
// (`workflows/<id>.md`) and a future layout change should fail loudly here
// rather than silently accept a file that merely ends in the right name.
func isWorkflowPath(p, workflowID string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	clean := path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if path.Base(clean) != workflowID+".md" {
		return false
	}
	return path.Base(path.Dir(clean)) == "workflows"
}

// CandidateFromAssistantProposal is the sibling of
// CandidateFromArchitectProposal for an assistant-sourced repair.
//
// A SIBLING, not a widening of the existing builder. Two proposal shapes in one
// function is how the architect-sourced and recipe-sourced paths already
// learned to disagree — `RecipeProposalProvenance` exists because that
// distinction had to be recovered after the fact — and this is a third source
// with a third artifact.
//
// Returns nil when either input is nil, matching its sibling's contract.
func CandidateFromAssistantProposal(t *persistence.HealingTrigger, p *persistence.ControlPlaneProposal, genome string) *persistence.HealingCandidate {
	if t == nil || p == nil || strings.TrimSpace(genome) == "" {
		return nil
	}
	candidateHash := ""
	if h, err := GenomeHashFromMarkdown([]byte(genome), t.WorkflowID+".md"); err == nil {
		candidateHash = h
	}
	return &persistence.HealingCandidate{
		TriggerID:           t.ID,
		ProjectID:           t.ProjectID,
		WorkflowID:          t.WorkflowID,
		ProposalID:          p.ID,
		CandidateGenomeHash: candidateHash,
		// Recorded as architect-class because that is what the column means to
		// every consumer: "a model proposed a structural repair", as opposed
		// to a deterministic recipe. The PROVENANCE — which model surface
		// produced it — is the proposal id, which is a control-plane proposal
		// here and a workflow proposal for the architect. Inventing a fourth
		// candidate class would change a shared enum for a distinction the
		// proposal id already carries.
		CandidateClass: persistence.HealingCandidateAssistant,
		ProposalDiff:   genome,
		Motivation:     p.Rationale,
		ExpectedEffect: "Assistant-proposed structural repair; see motivation and trial scorecard.",
		RiskLevel:      persistence.HealingRiskMedium,
		Status:         persistence.HealingCandidateDraft,
	}
}

// AssistantProposalProvenance marks a WorkflowProposal the CONFIG ASSISTANT
// produced, distinguishing it from an architect row (a model id) and a recipe
// row (RecipeProposalProvenance).
//
// The field is called ArchitectModel for historical reasons — it predates
// there being more than one producer. It is not renamed here: a column rename
// during a producer cutover is two risky changes wearing one commit, and the
// name is wrong in a way that is documented rather than load-bearing.
const AssistantProposalProvenance = "config-assistant"

// ProposalFromAssistantProposal synthesizes the WorkflowProposal a healing
// candidate is built from, out of the assistant's ControlPlaneProposal.
//
// WP9 step 3 (2026-09-16): the assistant replaces the memetic architect as the
// healing producer. The genome has to arrive as a WorkflowProposal because
// that is what the trial runner, the promotion gate and the proposals UI
// read — exactly as ProposalFromRecipeResult already does for the
// deterministic recipe path. `workflow_proposals` is the GENOME table, not a
// memetic-owned one; memetic was a producer feeding it, and removing that
// producer does not remove the table.
//
// The bridge's refusal rules apply unchanged (§6.3.4): exactly one file
// operation, against the named workflow's file. Before the cutover a refusal
// fell back to the architect; after it, there is nothing to fall back to, so
// a refusal must stay a refusal. Synthesizing a partial or empty genome here
// would hand the trial runner something nobody proposed, which is the one
// outcome §6.3.4 refuses to trade for availability.
func ProposalFromAssistantProposal(
	workflowID string,
	cp *persistence.ControlPlaneProposal,
	genome string,
	evidenceRunIDs []string,
	now time.Time,
) (*persistence.WorkflowProposal, error) {
	if cp == nil {
		return nil, ErrNotASingleWorkflowEdit
	}
	// Re-derive the genome through the bridge rather than trusting the
	// caller's copy: this function is the last point before a row is written,
	// and the refusal rules are the reason the row is safe.
	verified, err := GenomeFromAssistantProposal(cp, workflowID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(genome) != "" && genome != verified {
		return nil, fmt.Errorf("%w: the supplied genome does not match the proposal's file content", ErrNotASingleWorkflowEdit)
	}
	motivation := strings.TrimSpace(cp.Rationale)
	if motivation == "" {
		motivation = strings.TrimSpace(cp.Title)
	}
	return &persistence.WorkflowProposal{
		ID:             persistence.GenerateID("wpr"),
		WorkflowID:     workflowID,
		Status:         persistence.WorkflowProposalStatusPending,
		ProposalYAML:   verified,
		Motivation:     motivation,
		EvidenceRunIDs: append([]string(nil), evidenceRunIDs...),
		// Confidence describes the TRANSFORMATION, not a prediction that the
		// repair helps — the trial scorecard gates promotion, not this field.
		// The assistant's own judge verdict already gated the proposal before
		// it reached here, so a second self-reported number would be a
		// confidence about a confidence. The architect's confidence-vs-
		// threshold gate is precisely what produced zero candidates for a
		// month (2026-09-16 smoke test: confidence 0 against threshold 0.6).
		Confidence:     1.0,
		ArchitectModel: AssistantProposalProvenance,
		CreatedAt:      now,
	}, nil
}

// CandidateFromWorkflowProposal builds a healing candidate from a filed
// WorkflowProposal, classing it by the proposal's own PROVENANCE.
//
// The class is what an operator reads before deciding whether to trust a
// candidate, so it must name the producer that actually made it. Deriving it
// from the row rather than from the call site is what stops the two drifting:
// the first cutover deploy filed an assistant genome labelled "architect"
// because the call site still named the old constructor (2026-09-16).
func CandidateFromWorkflowProposal(t *persistence.HealingTrigger, p *persistence.WorkflowProposal) *persistence.HealingCandidate {
	cand := CandidateFromArchitectProposal(t, p)
	if cand == nil || p == nil {
		return cand
	}
	switch p.ArchitectModel {
	case AssistantProposalProvenance:
		cand.CandidateClass = persistence.HealingCandidateAssistant
	case RecipeProposalProvenance:
		// A recipe row reaches here only if something bypassed the recipe
		// path's own constructor; classing it honestly beats inheriting
		// whatever the architect default was.
		cand.CandidateClass = persistence.HealingCandidateClass(RecipeProposalProvenance)
	}
	return cand
}
