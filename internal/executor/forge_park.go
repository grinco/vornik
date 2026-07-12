package executor

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// handleSystemStepBlocked parks a task whose deterministic system step returned
// a PublishBlockedSignal — it cannot proceed without operator action (e.g.
// forge.open_change_request could not push the branch because the GitHub App
// lacks the `workflows` permission). It reuses the lead-handoff checkpoint seam
// (the same path the model-health circuit breaker parks through): write an
// action_required checkpoint task_message, flip the task RUNNING → AWAITING_INPUT,
// and notify the operator. The caller returns errLeadHandoff so the execution
// finishes cleanly (no failure, no retry) and the hand-off finalizer fires. The
// item stays consumed ([x]) so autonomy does not loop; the operator either
// clears the cause and resumes (answer → QUEUED) or applies the attached patch
// and closes.
func (e *Executor) handleSystemStepBlocked(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	stepID string,
	sig *PublishBlockedSignal,
) error {
	if sig == nil {
		return fmt.Errorf("handleSystemStepBlocked: nil signal")
	}
	body := sig.Reason
	if body == "" {
		body = "This task cannot proceed without operator action."
	}
	if sig.Remediation != "" {
		body += "\n\nTo unblock: " + sig.Remediation
	}
	if sig.ArtifactName != "" {
		body += fmt.Sprintf("\n\nA patch you can submit by hand is attached: %s", sig.ArtifactName)
		if sig.ArtifactID != "" {
			body += fmt.Sprintf(" (artifact %s)", sig.ArtifactID)
		}
		body += ". Apply it on a clean branch with `git am <file>` (format-patch) or `git apply <file>` (diff), then open the change manually."
	}
	outcome := &LeadOutcome{
		Outcome: LeadOutcomeCheckpoint,
		Message: body,
		Checkpoint: &CheckpointPayload{
			Kind:         CheckpointKindActionRequired,
			Question:     sig.Reason,
			TaskForHuman: sig.Remediation,
		},
	}
	return e.handleLeadHandoff(ctx, task, execution, stepID, outcome)
}
