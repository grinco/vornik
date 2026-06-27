package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// errLeadHandoff is the sentinel runLeadPlanning returns when the
// lead emitted a non-continue outcome (checkpoint, external_wait,
// closure_request) and the executor handled the side-effects:
// task_message written, task transitioned to AWAITING_INPUT /
// AWAITING_EXTERNAL / left COMPLETED for closure_request.
//
// Callers (executePlanStep, runExecution) treat this as
// "execution finished cleanly without spawning children" — no
// failure attribution, no retry, no further plan-step execution.
var errLeadHandoff = errors.New("lead handed off to operator; execution complete")

// IsLeadHandoff reports whether err signals a successful lead
// hand-off rather than a real failure. workflow.go uses this to
// skip the COMPLETED status flip — the task is already in
// AWAITING_INPUT / AWAITING_EXTERNAL by the time we check.
func IsLeadHandoff(err error) bool {
	return errors.Is(err, errLeadHandoff)
}

// handleLeadHandoff writes the task_message corresponding to the
// lead's outcome and (for checkpoint / external_wait) transitions
// the task. closure_request leaves the task in COMPLETED so the
// operator can review + close.
//
// The scratchpad update + phase transitions are best-effort: we
// log and continue rather than fail the hand-off. The lead can
// re-emit them on the next execution if they didn't land.
func (e *Executor) handleLeadHandoff(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	leadStepID string,
	outcome *LeadOutcome,
) error {
	if outcome == nil {
		return fmt.Errorf("nil outcome")
	}
	switch outcome.Outcome {
	case LeadOutcomeCheckpoint:
		return e.handleCheckpointOutcome(ctx, task, execution, leadStepID, outcome)
	case LeadOutcomeExternalWait:
		return e.handleExternalWaitOutcome(ctx, task, execution, leadStepID, outcome)
	case LeadOutcomeClosureRequest:
		return e.handleClosureRequestOutcome(ctx, task, execution, leadStepID, outcome)
	default:
		return fmt.Errorf("unhandled outcome %q in handoff", outcome.Outcome)
	}
}

// handleCheckpointOutcome writes the checkpoint task_message and
// flips the task RUNNING → AWAITING_INPUT.
func (e *Executor) handleCheckpointOutcome(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	leadStepID string,
	outcome *LeadOutcome,
) error {
	meta, err := SerializeCheckpointMetadata(outcome.Checkpoint)
	if err != nil {
		return fmt.Errorf("serialize checkpoint: %w", err)
	}
	body := outcome.Message
	if body == "" {
		body = outcome.Checkpoint.Question
	}
	if body == "" {
		body = outcome.Checkpoint.TaskForHuman
	}
	if body == "" {
		body = outcome.Checkpoint.Draft
	}

	msg := &persistence.TaskMessage{
		TaskID:      task.ID,
		ExecutionID: &execution.ID,
		AuthorKind:  persistence.TaskMessageAuthorLead,
		MessageKind: persistence.TaskMessageKindCheckpoint,
		Content:     body,
		Metadata:    meta,
		CreatedAt:   time.Now().UTC(),
	}
	if err := e.taskMessageRepo.Insert(ctx, msg); err != nil {
		return fmt.Errorf("insert checkpoint: %w", err)
	}

	// Side effect: scratchpad + phase markers (best-effort).
	e.applyScratchpadUpdate(ctx, task.ID, execution.ID, outcome)
	e.applyPhaseTransitions(ctx, task.ID, execution.ID, outcome.PhaseTransitions)

	// Atomic RUNNING → AWAITING_INPUT.
	opts := persistence.TransitionOpts{ClearLease: true}
	if outcome.Plan != nil && outcome.Plan.Phase != "" {
		p := outcome.Plan.Phase
		opts.CurrentPhase = &p
	}
	ok, err := e.persistTaskRepo.TransitionConditional(ctx, task.ID,
		[]persistence.TaskStatus{persistence.TaskStatusRunning, persistence.TaskStatusLeased},
		persistence.TaskStatusAwaitingInput,
		opts,
	)
	if err != nil {
		return fmt.Errorf("transition to AWAITING_INPUT: %w", err)
	}
	if !ok {
		// Task drifted (cancel + checkpoint race); the message is
		// written, the operator can still see it, but the status
		// transition didn't apply. Log loud and proceed.
		e.logger.Warn().
			Str("task_id", task.ID).
			Str("execution_id", execution.ID).
			Msg("checkpoint emitted but task drifted out of RUNNING — transition no-op")
	} else {
		// Mirror the DB write into the in-memory struct so callers
		// downstream of this method see the same status the
		// scheduler / UI / notifier do.
		task.Status = persistence.TaskStatusAwaitingInput
		// Push a steering prompt to the originating chat/DM so the operator
		// who scheduled this task knows it's waiting on them.
		e.notifySteering(ctx, task, string(persistence.TaskStatusAwaitingInput))
		// A2A callers can't get a chat/DM push (A2A isn't a conversation
		// channel) — emit a live "paused" so the SSE bridge surfaces
		// "input-required" to a streaming caller.
		e.emitPaused(ctx, execution.ID, "awaiting_input")
	}
	e.logger.Info().
		Str("task_id", task.ID).
		Str("execution_id", execution.ID).
		Str("checkpoint_id", msg.ID).
		Str("kind", string(outcome.Checkpoint.Kind)).
		Msg("lead emitted checkpoint; task transitioned to AWAITING_INPUT")
	return nil
}

// handleExternalWaitOutcome writes the external_wait note and
// flips the task to AWAITING_EXTERNAL with expected_by stamped.
func (e *Executor) handleExternalWaitOutcome(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	leadStepID string,
	outcome *LeadOutcome,
) error {
	meta, _ := json.Marshal(map[string]any{
		"kind":        "external_wait",
		"expected_by": outcome.ExternalWait.ExpectedBy,
		"event_match": json.RawMessage(outcome.ExternalWait.EventMatch),
		"reason":      outcome.ExternalWait.Reason,
	})
	body := outcome.Message
	if body == "" {
		body = "waiting on external event"
	}
	if outcome.ExternalWait.Reason != "" {
		body = outcome.ExternalWait.Reason
	}
	msg := &persistence.TaskMessage{
		TaskID:      task.ID,
		ExecutionID: &execution.ID,
		AuthorKind:  persistence.TaskMessageAuthorLead,
		MessageKind: persistence.TaskMessageKindNote,
		Content:     body,
		Metadata:    meta,
		CreatedAt:   time.Now().UTC(),
	}
	if err := e.taskMessageRepo.Insert(ctx, msg); err != nil {
		return fmt.Errorf("insert external_wait note: %w", err)
	}

	e.applyScratchpadUpdate(ctx, task.ID, execution.ID, outcome)
	e.applyPhaseTransitions(ctx, task.ID, execution.ID, outcome.PhaseTransitions)

	opts := persistence.TransitionOpts{
		ClearLease: true,
		ExpectedBy: outcome.ExternalWait.ExpectedBy,
	}
	if outcome.Plan != nil && outcome.Plan.Phase != "" {
		p := outcome.Plan.Phase
		opts.CurrentPhase = &p
	}
	ok, err := e.persistTaskRepo.TransitionConditional(ctx, task.ID,
		[]persistence.TaskStatus{persistence.TaskStatusRunning, persistence.TaskStatusLeased},
		persistence.TaskStatusAwaitingExternal,
		opts,
	)
	if err != nil {
		return fmt.Errorf("transition to AWAITING_EXTERNAL: %w", err)
	}
	if !ok {
		e.logger.Warn().
			Str("task_id", task.ID).
			Msg("external_wait emitted but task drifted out of RUNNING")
	}
	e.logger.Info().
		Str("task_id", task.ID).
		Time("expected_by", *outcome.ExternalWait.ExpectedBy).
		Msg("lead emitted external_wait; task transitioned to AWAITING_EXTERNAL")
	return nil
}

// handleClosureRequestOutcome writes the closure_request message
// AND transitions the task RUNNING → COMPLETED so the lease is
// released and the scheduler doesn't retry the lead. The operator
// drives the final CLOSED transition via the API / UI / Telegram;
// COMPLETED is the resting state where the closure_request sits
// awaiting that confirmation.
func (e *Executor) handleClosureRequestOutcome(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	leadStepID string,
	outcome *LeadOutcome,
) error {
	meta, _ := json.Marshal(outcome.ClosureRequest)
	body := outcome.ClosureRequest.Summary
	if outcome.Message != "" {
		body = outcome.Message + "\n\n" + body
	}
	msg := &persistence.TaskMessage{
		TaskID:      task.ID,
		ExecutionID: &execution.ID,
		AuthorKind:  persistence.TaskMessageAuthorLead,
		MessageKind: persistence.TaskMessageKindClosureRequest,
		Content:     body,
		Metadata:    meta,
		CreatedAt:   time.Now().UTC(),
	}
	if err := e.taskMessageRepo.Insert(ctx, msg); err != nil {
		return fmt.Errorf("insert closure_request: %w", err)
	}
	e.applyScratchpadUpdate(ctx, task.ID, execution.ID, outcome)
	e.applyPhaseTransitions(ctx, task.ID, execution.ID, outcome.PhaseTransitions)

	// Atomic RUNNING → COMPLETED. Without this, the lease would
	// expire and the scheduler would re-lease the task, the lead
	// would re-run and emit closure_request again — until
	// max_attempts. The hand-off finalizer's notify path also
	// relies on the task being COMPLETED for the right
	// "task_completed" message vs. "still running" wording.
	ok, err := e.persistTaskRepo.TransitionConditional(ctx, task.ID,
		[]persistence.TaskStatus{persistence.TaskStatusRunning, persistence.TaskStatusLeased},
		persistence.TaskStatusCompleted,
		persistence.TransitionOpts{ClearLease: true},
	)
	if err != nil {
		return fmt.Errorf("transition to COMPLETED for closure_request: %w", err)
	}
	if !ok {
		e.logger.Warn().
			Str("task_id", task.ID).
			Msg("closure_request emitted but task drifted out of RUNNING")
	}
	e.logger.Info().
		Str("task_id", task.ID).
		Msg("lead emitted closure_request; task transitioned to COMPLETED awaiting operator close")
	// Parent unblock (2026-05-26): closure_request leaves the task in
	// COMPLETED — a terminal status for the scheduler — but the
	// hand-off path here NEVER reached handleSuccess, so
	// checkParentUnblock didn't fire. If this task is a child of a
	// parent in WAITING_FOR_CHILDREN, the parent sits forever.
	// Observed on T-a8e1 / T-0833 (2026-05-26): child closed via
	// closure_request, parent never woken. Mirror the call from
	// handleSuccess so the parent sweep + cross-project-call resolve
	// fire on this path too. Refresh the task once so the in-memory
	// Status is COMPLETED (TransitionConditional updated the DB but
	// not the local pointer).
	task.Status = persistence.TaskStatusCompleted
	e.resolveCrossProjectCallForTask(ctx, task, true)
	e.checkParentUnblock(ctx, task)
	return nil
}

// applyScratchpadUpdate merges the lead's scratchpad delta onto
// the task_scratchpad row. Best-effort — log on failure, don't
// abort the handoff.
func (e *Executor) applyScratchpadUpdate(ctx context.Context, taskID, executionID string, outcome *LeadOutcome) {
	if outcome == nil || outcome.ScratchpadUpdate == nil {
		return
	}
	if e.taskScratchpadRepo == nil {
		return
	}
	upd := outcome.ScratchpadUpdate

	// Read existing scratchpad so we can layer the delta. nil-safe:
	// when the row doesn't exist yet (first execution), treat as
	// empty defaults.
	existing, err := e.taskScratchpadRepo.Get(ctx, taskID)
	if err != nil {
		e.logger.Warn().Err(err).Str("task_id", taskID).Msg("scratchpad read failed; skipping update")
		return
	}
	row := existing
	if row == nil {
		row = &persistence.TaskScratchpad{TaskID: taskID}
	}
	if upd.Summary != "" {
		row.Summary = upd.Summary
	}
	if len(upd.Facts) > 0 {
		row.Facts = []byte(upd.Facts)
	}
	if len(upd.OpenQuestions) > 0 {
		if b, err := json.Marshal(upd.OpenQuestions); err == nil {
			row.OpenQuestions = b
		}
	}
	if upd.CurrentPhase != "" {
		p := upd.CurrentPhase
		row.CurrentPhase = &p
	}
	row.LastExecutionID = &executionID

	if err := e.taskScratchpadRepo.Upsert(ctx, row); err != nil {
		e.logger.Warn().Err(err).Str("task_id", taskID).Msg("scratchpad upsert failed")
	}
}

// handleLeadHandoffFinalization wraps the post-execution
// bookkeeping for a hand-off — sweep pending step outcomes,
// ingest OUTPUT artifacts, fire the completion notifier. The
// task's status was already set by the per-outcome handler
// (handleCheckpointOutcome → AWAITING_INPUT,
// handleExternalWaitOutcome → AWAITING_EXTERNAL,
// handleClosureRequestOutcome → COMPLETED) before this runs.
func (e *Executor) handleLeadHandoffFinalization(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	containerID string,
	result []byte,
) {
	e.sweepPendingOutcomes(ctx, execution.ID, "ok")
	e.ingestOutputArtifacts(ctx, task, execution)

	// Build a concise message from the result if we can.
	notifyMsg := "Task transitioned to AWAITING_INPUT (lead handed off to operator)"
	if len(result) > 0 {
		var r struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(result, &r); err == nil && r.Message != "" {
			notifyMsg = r.Message
		}
	}
	if e.notifier != nil {
		e.notifier.NotifyTaskCompleted(ctx, task, true, notifyMsg)
	}
	// Steering prompt to the originating chat/DM (default-on, not gated on the
	// completion-watcher opt-in the notifier above uses).
	e.notifySteering(ctx, task, string(persistence.TaskStatusAwaitingInput))
	// A2A SSE callers: surface "input-required" via the live stream.
	e.emitPaused(ctx, execution.ID, "awaiting_input")
}

// applyPhaseTransitions writes one phase_marker message per
// transition in the lead's outcome. Best-effort.
func (e *Executor) applyPhaseTransitions(ctx context.Context, taskID, executionID string, transitions []PhaseTransition) {
	if e.taskMessageRepo == nil {
		return
	}
	for _, pt := range transitions {
		meta, _ := json.Marshal(pt)
		msg := &persistence.TaskMessage{
			TaskID:      taskID,
			ExecutionID: &executionID,
			AuthorKind:  persistence.TaskMessageAuthorLead,
			MessageKind: persistence.TaskMessageKindPhaseMarker,
			Content:     pt.Phase + " " + pt.Status,
			Metadata:    meta,
			CreatedAt:   time.Now().UTC(),
		}
		if err := e.taskMessageRepo.Insert(ctx, msg); err != nil {
			e.logger.Warn().Err(err).Str("task_id", taskID).Msg("phase_marker insert failed")
		}
	}
}
