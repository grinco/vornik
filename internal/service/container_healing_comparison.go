package service

// The healing comparison window's adapter — config-assistant design §6.3.4
// step 1, plan §10 (WP9).
//
// The api package wants a proposal; the engine returns a *configassist.Result
// carrying a proposal, a refusal or neither. Adapting HERE keeps the api
// package free of configassist (it already avoids importing it for the
// operator entrypoints) and keeps the engine free of the api package's
// interface shape.

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/configassist"
	"vornik.io/vornik/internal/persistence"
)

type healingComparisonAdapter struct{ engine *configassist.Engine }

// ProposeForHealing runs the system entrypoint and unwraps the outcome.
//
// A REFUSAL IS AN ERROR HERE, deliberately. The comparison window counts
// agreements between two paths that both produced something; a refusal is
// neither agreement nor disagreement, and returning it as a nil proposal with
// a nil error would let the caller treat "the assistant declined" as "the
// assistant agreed to nothing" — which is how a window closes on a criterion
// nobody actually met.
func (a *healingComparisonAdapter) ProposeForHealing(ctx context.Context, projectID, workflowID, reason string, evidenceExecutionIDs []string) (*persistence.ControlPlaneProposal, error) {
	if a == nil || a.engine == nil {
		return nil, fmt.Errorf("configassist: engine not wired")
	}
	res, err := a.engine.ProposeForHealing(ctx, configassist.HealingRequest{
		ProjectID:            projectID,
		WorkflowID:           workflowID,
		EvidenceExecutionIDs: evidenceExecutionIDs,
		Reason:               reason,
		// SHADOW: the comparison window wants the assistant's opinion, not a
		// row in the operator's inbox. This adapter used to file a real,
		// actionable proposal before comparing — one the bridge could then
		// reject and leave behind, approvable through the ordinary workflow
		// with none of the healing-trial provenance a system repair carries
		// (audit 2026-09-15 CA-15).
		Shadow: true,
	})
	if err != nil {
		return nil, err
	}
	switch {
	case res == nil:
		return nil, fmt.Errorf("configassist: healing produced no result")
	case res.Refusal != nil:
		return nil, fmt.Errorf("configassist: healing refused: %s: %s", res.Refusal.Code, res.Refusal.Message)
	case res.Proposal == nil:
		return nil, fmt.Errorf("configassist: healing filed nothing and refused nothing")
	}
	return res.Proposal, nil
}
