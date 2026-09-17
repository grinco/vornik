package configassist

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// The SYSTEM entrypoint — config-assistant design §6.3.4, plan §10 (WP9).
//
// `EntrypointSystem` has carried the comment "the healing initiator (WP9),
// never a human" since the first release and had no caller until this file.
// This is that caller's surface: a healing trigger hands over a workflow and
// the executions that evidence its regression, and gets back a proposal whose
// single file operation is the repaired workflow.
//
// WHAT IT IS NOT, because an earlier draft of §6.3.4 got this loose and the
// correction is the load-bearing part: this is NOT an exception to the class
// ceiling. A workflow edit is class C, and `Engine.mayAutoApply` refuses C, D
// and E for every entrypoint including this one, whatever the project's opt-in
// says. So a healing proposal never auto-applies through the assistant's apply
// path at all. It becomes a healing CANDIDATE, and application is governed by
// the healing promotion gate — a replay-gated trial, a scorecard, and a manual
// operator promotion — exactly as the self-healing design already requires.
// The assistant proposes and may apply; healing promotes. Different gates, and
// conflating them is how a topology edit would reach a deployment untrialled.

// HealingRequest is one trigger's ask.
type HealingRequest struct {
	ProjectID  string
	WorkflowID string
	// EvidenceExecutionIDs are the runs that evidence the regression. They
	// reach the engine as grounding, the same evidence the operator surfaces
	// carry (§4.1), so the proposal cites measurements rather than asserting a
	// repair.
	EvidenceExecutionIDs []string
	// Reason is the trigger's own description of what regressed — the metric,
	// the baseline and the comparison. It becomes the intent.
	Reason string
	// Shadow runs the request for COMPARISON only: the proposal is built and
	// returned but never filed, never auto-applied, and marked so nothing can
	// mistake it for an actionable one.
	//
	// The comparison window (§6.3.4 step 1) used the ordinary path, which
	// files. A proposal the bridge then rejected stayed in the operator's
	// inbox — approvable through the normal workflow, with none of the
	// healing trial provenance a system repair is supposed to carry (audit
	// 2026-09-15 CA-15). Observing a second opinion must not deposit one.
	Shadow bool
}

// ProposeForHealing runs the assistant for a healing trigger.
//
// CALLER CONFINEMENT: this is the ONLY function that constructs a request with
// `EntrypointSystem`, and a source contract names its permitted caller BY
// SYMBOL rather than counting callers. Counting is not confining — an absence
// test passes for a new caller too, because that caller simply becomes "the
// one" (round 3's F5, restated in review-20260915-cc32 F3.1). A caller reaching
// this entrypoint without a trial behind it would file healing candidates
// nothing ever trialled: quieter than an unreviewed apply, and the same wrong.
func (e *Engine) ProposeForHealing(ctx context.Context, req HealingRequest) (*Result, error) {
	if e == nil {
		return nil, fmt.Errorf("configassist: engine not wired")
	}
	if strings.TrimSpace(req.WorkflowID) == "" {
		return nil, fmt.Errorf("configassist: healing request has no workflow")
	}
	return e.Propose(ctx, Request{
		ProjectID:  req.ProjectID,
		Intent:     healingIntent(req),
		Entrypoint: EntrypointSystem,
		Shadow:     req.Shadow,
		RequestID:  persistence.GenerateID("careq"),
		Actor: Actor{
			// The TRIGGER is the actor, not a person and not an agent. A
			// healing proposal that recorded a human would make an automatic
			// repair look like somebody's decision in the ledger.
			Kind:         "system",
			Principal:    "healing:" + req.WorkflowID,
			CredentialID: "healing:" + req.WorkflowID,
			SourceID:     "healing:" + req.WorkflowID,
		},
	})
}

// healingIntent turns the trigger into the one instruction the assistant acts
// on.
//
// It names the workflow file EXPLICITLY and asks for that file alone, because
// the bridge refuses a proposal touching anything else
// (workflowhealing.GenomeFromAssistantProposal). Asking loosely and then
// refusing what comes back would make the refusal path the common one, and a
// high refusal rate is what tempts someone to start trimming proposals to fit.
func healingIntent(req HealingRequest) string {
	var b strings.Builder
	b.WriteString("A regression was detected in the workflow ")
	b.WriteString(req.WorkflowID)
	b.WriteString(".\n\n")
	if r := strings.TrimSpace(req.Reason); r != "" {
		b.WriteString(r)
		b.WriteString("\n\n")
	}
	if len(req.EvidenceExecutionIDs) > 0 {
		b.WriteString("Evidence executions: ")
		b.WriteString(strings.Join(req.EvidenceExecutionIDs, ", "))
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Propose a structural repair by editing EXACTLY ONE file: workflows/%s.md. "+
		"Do not touch any other file — a proposal that edits a second file is refused and the "+
		"repair is lost. Ground the change in the evidence above; if the evidence does not "+
		"support a specific repair, say so rather than guessing.", req.WorkflowID)
	return b.String()
}
