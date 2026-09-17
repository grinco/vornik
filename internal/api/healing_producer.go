package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowhealing"
)

// THE HEALING CANDIDATE PRODUCER — config-assistant design §6.3.4, cut over
// to the config assistant on 2026-09-16 (§6.3.4b).
//
// A healing trigger's evidence goes to the assistant, whose single-workflow
// edit becomes the genome a healing CANDIDATE is trialled and promoted from.
// The deterministic recipe path still runs first; this handles what recipes
// cannot.
//
// What this file no longer contains: the read-only comparison window that ran
// the assistant alongside `memetic.Architect` and recorded whether their
// genomes agreed. Its pass criterion — five consecutive agreeing triggers —
// was never met and could not be, because the architect it compared against
// produced nothing to agree with (2026-09-16: confidence 0 against a 0.6
// threshold, no candidate since 2026-08-15). Comparing against an inert
// baseline measures nothing, so the window and the architect were removed
// together. §6.3.4b records the reasoning and the rollback.
//
// healingAssistant is the assistant as this comparison needs it. An interface
// so the api package does not import configassist for one call, and so the
// comparison is testable without an engine.
type healingAssistant interface {
	ProposeForHealing(ctx context.Context, projectID, workflowID, reason string, evidenceExecutionIDs []string) (*persistence.ControlPlaneProposal, error)
}

// healingReason renders what regressed, for the assistant's intent.
// The trigger already carries the metric and both sides of the comparison;
// re-deriving them here would be a second description of one measurement.
func healingReason(t *persistence.HealingTrigger) string {
	if t == nil {
		return ""
	}
	return "Metric " + t.MetricName + " regressed: baseline " +
		formatHealingValue(t.BaselineValue) + ", comparison " +
		formatHealingValue(t.ComparisonValue) + ", threshold " +
		formatHealingValue(t.ThresholdValue) + "."
}

// formatHealingValue renders a metric value without pretending to a precision
// the trigger does not carry.
func formatHealingValue(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// SetHealingAssistant wires the config assistant as the healing candidate
// producer, replacing the memetic architect (WP9 step 3, 2026-09-16).
//
// A SETTER, not a ServerOption: the assistant engine does not exist until
// after api.NewServer has been called — the engine needs the server's own
// config-assistant wiring — so an option would have been dead by
// construction.
func (s *Server) SetHealingAssistant(a healingAssistant) { s.healingAssistant = a }

// generateAssistantCandidate runs the config assistant for a healing trigger
// and synthesizes the WorkflowProposal the trial runner and promotion gate
// read (WP9 step 3, the 2026-09-16 cutover).
//
// NOT shadow mode: this is the authoritative producer now, so the proposal is
// filed. What it produces is still a class-C healing CANDIDATE — the
// assistant's own apply path refuses classes C, D and E on every entrypoint,
// so nothing here can auto-apply, and promotion remains a manual operator
// action the database CHECK-constrains.
func (s *Server) generateAssistantCandidate(ctx context.Context, t *persistence.HealingTrigger) (*persistence.WorkflowProposal, error) {
	if s.healingAssistant == nil || t == nil {
		return nil, errHealingProducerUnavailable
	}
	cp, err := s.healingAssistant.ProposeForHealing(ctx, t.ProjectID, t.WorkflowID,
		healingReason(t), t.EvidenceExecutionIDs)
	if err != nil {
		return nil, err
	}
	// The bridge's refusal rules are the safety property, and after the
	// cutover there is no architect to fall back to — so a refusal stays a
	// refusal rather than degrading into a partial genome (§6.3.4).
	genome, err := workflowhealing.GenomeFromAssistantProposal(cp, t.WorkflowID)
	if err != nil {
		return nil, err
	}
	proposal, err := workflowhealing.ProposalFromAssistantProposal(
		t.WorkflowID, cp, genome, t.EvidenceExecutionIDs, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if s.workflowProposals == nil {
		return nil, errHealingProducerUnavailable
	}
	if err := s.workflowProposals.Insert(ctx, proposal); err != nil {
		return nil, fmt.Errorf("persisting the healing proposal failed: %w", err)
	}
	return proposal, nil
}

// errHealingProducerUnavailable is the "nothing is wired" case, distinct from
// a producer that ran and declined.
var errHealingProducerUnavailable = errors.New("healing producer not available")

// mapHealingProducerError turns a producer failure into an HTTP answer that
// says which KIND of failure it was.
//
// The distinction matters more after the cutover than before it: a bridge
// refusal ("the assistant proposed something that is not a single workflow
// edit") is a different operator action from "the assistant declined" or "the
// producer is not wired", and before the cutover the first of those silently
// fell back to the architect.
func (s *Server) mapHealingProducerError(w http.ResponseWriter, workflowID string, err error) {
	switch {
	case errors.Is(err, errHealingProducerUnavailable):
		respondError(w, http.StatusServiceUnavailable, "HEALING_PRODUCER_DISABLED",
			"no healing producer is wired on this deployment")
	case errors.Is(err, workflowhealing.ErrNotASingleWorkflowEdit):
		respondError(w, http.StatusUnprocessableEntity, "PROPOSAL_NOT_A_SINGLE_WORKFLOW_EDIT",
			"the assistant's proposal was not exactly one edit to "+workflowID+
				"'s workflow file, so it cannot become a healing candidate: "+err.Error())
	default:
		s.logger.Warn().Err(err).Str("workflow_id", workflowID).
			Msg("healing: the assistant produced no usable candidate")
		respondError(w, http.StatusBadGateway, "HEALING_PRODUCER_FAILED",
			"the assistant produced no usable candidate for "+workflowID+": "+err.Error())
	}
}
