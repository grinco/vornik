package executor

import (
	"context"
	"encoding/json"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// resolveOperatorCheckpointAction inspects the task conversation for
// the operator's most recent answer to a recovery `decision` checkpoint
// and returns the structured action attached to the chosen option (LLD
// §9), or nil when there is none.
//
// Resolution:
//  1. Find the most recent answer message (author=operator,
//     kind=answer) carrying a `choice` id in its metadata.
//  2. Resolve the checkpoint it answered — its ParentID, falling back to
//     the most recent checkpoint message — and parse that checkpoint's
//     persisted options.
//  3. Return the chosen option's Action (already normalized at parse
//     time: an invalid/unknown action was demoted to nil prose, so a
//     non-nil result is always one of the four valid types).
//
// nil result => today's prose-hint behaviour (backward compatible). The
// messages are passed oldest→newest as the executor loads them; this
// walks backward to find the latest answer.
func resolveOperatorCheckpointAction(msgs []*persistence.TaskMessage) *CheckpointOptionAction {
	if len(msgs) == 0 {
		return nil
	}
	// Latest answer wins (operator may re-answer; the resume acts on the
	// most recent decision).
	var answer *persistence.TaskMessage
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m != nil && m.MessageKind == persistence.TaskMessageKindAnswer {
			answer = m
			break
		}
	}
	if answer == nil {
		return nil
	}
	choice := checkpointAnswerChoice(answer.Metadata)
	if choice == "" {
		return nil
	}

	// Locate the checkpoint the answer responded to: prefer the threaded
	// parent, else the most recent checkpoint message.
	var checkpoint *persistence.TaskMessage
	if answer.ParentID != nil && *answer.ParentID != "" {
		for _, m := range msgs {
			if m != nil && m.ID == *answer.ParentID {
				checkpoint = m
				break
			}
		}
	}
	if checkpoint == nil {
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			if m != nil && m.MessageKind == persistence.TaskMessageKindCheckpoint {
				checkpoint = m
				break
			}
		}
	}
	if checkpoint == nil || len(checkpoint.Metadata) == 0 {
		return nil
	}

	var cp CheckpointPayload
	if err := json.Unmarshal(checkpoint.Metadata, &cp); err != nil {
		return nil
	}
	for _, opt := range cp.Options {
		if opt.ID == choice {
			return opt.Action
		}
	}
	return nil
}

// checkpointAnswerChoice extracts the operator's chosen option id from
// an answer message's metadata ({"choice":"<id>"}). Returns "" when the
// metadata is absent/malformed or carries no choice.
func checkpointAnswerChoice(meta json.RawMessage) string {
	if len(meta) == 0 {
		return ""
	}
	var m struct {
		Choice string `json:"choice"`
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		return ""
	}
	return m.Choice
}

// applyRecoveryCheckpointAction applies the operator-approved structured
// action (LLD §9) BEFORE the recovery step's lead re-plans / the failed
// step is retried, via the existing seams:
//
//   - reroute_workflow → delegateSelectedWorkflow (bounded by the
//     project's AdaptiveCandidateWorkflows).
//   - model_fallback  → ApplyFallbackModelOverride (replay-free
//     operator_model_override).
//   - retry / skip    → no-op here (the prose-hint path is unchanged).
//
// FAIL-SAFE: every failure (no candidate list, workflow outside the
// allow-list, no fallback configured, persistence hiccup) is logged at
// warn and swallowed — the recovery then falls through to today's
// prose-hint behaviour instead of crashing. Guardrails are PRESERVED:
// nothing fires until the operator approved the option (this is only
// reached on resume after an answer), reroute stays allow-list-bounded,
// and model_fallback stays operator-gated.
//
// Returns true when an action was applied (so the caller can log it).
func (e *Executor) applyRecoveryCheckpointAction(
	ctx context.Context,
	task *persistence.Task,
	project *registry.Project,
	swarm *registry.Swarm,
	action *CheckpointOptionAction,
) bool {
	if action == nil || task == nil {
		return false
	}
	switch action.Type {
	case CheckpointActionRerouteWorkflow:
		if project == nil {
			e.logger.Warn().Str("task_id", task.ID).
				Msg("recovery action reroute_workflow: no project resolved — demoting to prose")
			return false
		}
		used, err := e.delegateSelectedWorkflow(ctx, task, project, action.Workflow)
		if err != nil {
			// delegateSelectedWorkflow already rejects workflows outside
			// AdaptiveCandidateWorkflows + the routing-loop guards; surface
			// that as the fail-safe demote rather than failing recovery.
			e.logger.Warn().Err(err).
				Str("task_id", task.ID).
				Str("requested_workflow", action.Workflow).
				Msg("recovery action reroute_workflow rejected — demoting to prose hint")
			return false
		}
		e.logger.Info().
			Str("task_id", task.ID).
			Str("workflow", used).
			Msg("recovery action reroute_workflow applied: delegated child task on operator-chosen candidate workflow")
		return true

	case CheckpointActionModelFallback:
		applied, err := ApplyFallbackModelOverride(ctx, staticSwarmResolver{sw: swarm}, e.persistTaskRepo, task)
		if err != nil {
			e.logger.Warn().Err(err).Str("task_id", task.ID).
				Msg("recovery action model_fallback failed to persist override — demoting to prose hint")
			return false
		}
		if !applied {
			e.logger.Warn().Str("task_id", task.ID).
				Msg("recovery action model_fallback: no role has a configured fallback — demoting to prose hint")
			return false
		}
		e.logger.Info().Str("task_id", task.ID).
			Msg("recovery action model_fallback applied: operator_model_override written for roles with a fallback")
		return true

	case CheckpointActionRetry, CheckpointActionSkip:
		// No new seam — the chosen option's label still flows back as the
		// prose hint, exactly as today.
		return false

	default:
		// Should be unreachable: the parser demotes unknown types to nil.
		return false
	}
}

// staticSwarmResolver adapts an already-resolved *registry.Swarm to the
// fallbackSwarmResolver interface ApplyFallbackModelOverride expects, so
// the recovery apply path reuses the same override core as the UI/API
// "retry on fallback" button without a second registry lookup. The
// project is irrelevant to FallbackModelOverrides, so it's returned nil.
type staticSwarmResolver struct {
	sw *registry.Swarm
}

func (s staticSwarmResolver) GetProjectWithSwarm(string) (*registry.Project, *registry.Swarm, error) {
	return nil, s.sw, nil
}
