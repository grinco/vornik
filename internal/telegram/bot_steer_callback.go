package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/steering"
)

// handleSteerCallback owns the `steer:*` callback namespace — the button
// taps on a recovery-steering / approval prompt (rendered by the steering
// notifier). Actions:
//   - c       payload "<taskID>:<optionIndex>" — a decision-checkpoint choice
//   - approve payload "<taskID>" — approve an AWAITING_APPROVAL task
//   - reject   payload "<taskID>" — reject (cancel) an AWAITING_APPROVAL task
//
// Every path (a) authorizes the operator against the task's project, (b)
// records/applies the choice reusing the same resume machinery as a text
// reply, and (c) EDITS the original message to a "✓ recorded" line with the
// buttons stripped, so the prompt visibly stops looking un-acted-upon.
func (b *Bot) handleSteerCallback(ctx context.Context, chatID, userID int64, callbackID string, msgID int64, action, payload string) error {
	if b.taskRepo == nil {
		return b.answerCallbackQuery(ctx, callbackID, "Task control isn't available.", false)
	}

	// payload for "c" is "<taskID>:<index>"; task ids contain no colon, so
	// split on the LAST colon. approve/reject carry the bare task id.
	taskID := payload
	optIdx := -1
	if action == "c" {
		i := strings.LastIndex(payload, ":")
		if i < 0 {
			return b.answerCallbackQuery(ctx, callbackID, "This button is malformed.", false)
		}
		taskID = payload[:i]
		n, err := strconv.Atoi(payload[i+1:])
		if err != nil {
			return b.answerCallbackQuery(ctx, callbackID, "This button is malformed.", false)
		}
		optIdx = n
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return b.answerCallbackQuery(ctx, callbackID, "This button is malformed.", false)
	}

	task, err := b.taskRepo.Get(ctx, taskID)
	if err != nil || task == nil {
		return b.answerCallbackQuery(ctx, callbackID, "That task no longer exists.", false)
	}
	// Same project-scope gate as routeReplyToTask — a stale/forged button must
	// not act on a task the operator isn't cleared for.
	if task.ProjectID != "" && !b.UserCanAccessProject(userID, task.ProjectID) {
		return b.answerCallbackQuery(ctx, callbackID, "You are not authorized for this task's project.", true)
	}

	switch action {
	case "c":
		return b.steerChoice(ctx, chatID, userID, callbackID, msgID, task, optIdx)
	case "approve":
		return b.steerApprove(ctx, chatID, callbackID, msgID, task)
	case "reject":
		return b.steerReject(ctx, chatID, callbackID, msgID, task)
	default:
		return b.answerCallbackQuery(ctx, callbackID, "Unknown steering action.", false)
	}
}

// steerChoice records a decision-checkpoint option tap as a checkpoint answer
// and resumes the task — mirroring routeReplyToTask's answer branch — then
// edits the prompt to "✓ Recorded: <label>".
func (b *Bot) steerChoice(ctx context.Context, chatID, userID int64, callbackID string, msgID int64, task *persistence.Task, optIdx int) error {
	if b.taskMessageRepo == nil || task.OpenCheckpointID == nil {
		return b.answerCallbackQuery(ctx, callbackID, "This decision was already handled.", false)
	}
	checkpointID := *task.OpenCheckpointID
	cp, err := b.taskMessageRepo.GetOpenCheckpoint(ctx, task.ID)
	if err != nil || cp == nil || len(cp.Metadata) == 0 {
		return b.answerCallbackQuery(ctx, callbackID, "This decision was already handled.", false)
	}
	var meta struct {
		Options []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"options"`
	}
	if err := json.Unmarshal(cp.Metadata, &meta); err != nil || optIdx < 0 || optIdx >= len(meta.Options) {
		return b.answerCallbackQuery(ctx, callbackID, "This button is from an older prompt.", false)
	}
	// Map THIS channel's 0-based callback index to the option id here, on the
	// adapter, and hand the primitive an id. The numbering conventions (0-based
	// wire, 1-based rendered text) never meet in shared code, so there is no
	// off-by-one to get wrong in steering.Answerer.
	opt := meta.Options[optIdx]

	res, err := steering.NewAnswerer(b.taskMessageRepo, b.taskRepo, b.rescheduler).
		Answer(ctx, steering.AnswerRequest{
			TaskID:       task.ID,
			CheckpointID: checkpointID,
			OptionID:     opt.ID,
			AuthorID:     fmt.Sprintf("tg:%d", userID),
			Source:       "telegram_button",
		})
	switch {
	case errors.Is(err, steering.ErrNoOpenCheckpoint),
		errors.Is(err, steering.ErrUnknownOption):
		return b.answerCallbackQuery(ctx, callbackID, "This decision was already handled.", false)
	case errors.Is(err, steering.ErrCheckpointNotChatAnswerable):
		// Not reachable from a steering button today (buildSteeringButtons only
		// renders plain decisions), but the primitive is the authority on which
		// kinds may be answered outside the UI — do not paper over a refusal.
		return b.answerCallbackQuery(ctx, callbackID, "This decision has to be made in the web UI.", true)
	case err != nil:
		return b.answerCallbackQuery(ctx, callbackID, "Could not record your choice: "+err.Error(), true)
	}
	if res.AlreadyHandled {
		b.markSteerRecorded(ctx, chatID, msgID, "✓ Recorded: "+res.RecordedLabel)
		return b.answerCallbackQuery(ctx, callbackID, "This decision was already handled.", false)
	}
	b.markSteerRecorded(ctx, chatID, msgID, "✓ Recorded: "+res.RecordedLabel)
	return b.answerCallbackQuery(ctx, callbackID, "Recorded: "+res.RecordedLabel, false)
}

// steerApprove resumes an AWAITING_APPROVAL task.
func (b *Bot) steerApprove(ctx context.Context, chatID int64, callbackID string, msgID int64, task *persistence.Task) error {
	ok, err := b.taskRepo.TransitionConditional(ctx, task.ID,
		[]persistence.TaskStatus{persistence.TaskStatusAwaitingApproval},
		persistence.TaskStatusQueued,
		persistence.TransitionOpts{ClearLease: true})
	if err != nil {
		return b.answerCallbackQuery(ctx, callbackID, "Could not approve: "+err.Error(), true)
	}
	if !ok {
		return b.answerCallbackQuery(ctx, callbackID, "This task was already handled or its state changed.", false)
	}
	if b.rescheduler != nil {
		b.rescheduler.Wake()
	}
	b.markSteerRecorded(ctx, chatID, msgID, "✅ Approved — task queued.")
	return b.answerCallbackQuery(ctx, callbackID, "Approved.", false)
}

// steerReject cancels an AWAITING_APPROVAL task (and cascades to its children
// when the canceller is wired).
func (b *Bot) steerReject(ctx context.Context, chatID int64, callbackID string, msgID int64, task *persistence.Task) error {
	ok, err := b.taskRepo.TransitionConditional(ctx, task.ID,
		[]persistence.TaskStatus{persistence.TaskStatusAwaitingApproval, persistence.TaskStatusAwaitingInput},
		persistence.TaskStatusCancelled,
		persistence.TransitionOpts{ClearLease: true})
	if err != nil {
		return b.answerCallbackQuery(ctx, callbackID, "Could not reject: "+err.Error(), true)
	}
	if !ok {
		return b.answerCallbackQuery(ctx, callbackID, "This task was already handled or its state changed.", false)
	}
	// Downward cascade so the cancelled parent doesn't strand children.
	if b.childCanceller != nil {
		b.childCanceller.CancelChildren(ctx, task.ID)
	}
	if b.rescheduler != nil {
		b.rescheduler.Wake()
	}
	b.markSteerRecorded(ctx, chatID, msgID, "✗ Rejected — task cancelled.")
	return b.answerCallbackQuery(ctx, callbackID, "Rejected.", false)
}

// markSteerRecorded edits the original prompt to a terminal "recorded" line
// with the buttons stripped, so the operator sees the action landed. Best-
// effort: a nil/zero message id or an edit failure just skips the edit (the
// callback toast still confirms).
func (b *Bot) markSteerRecorded(ctx context.Context, chatID, msgID int64, text string) {
	if msgID == 0 {
		return
	}
	empty := &InlineKeyboardMarkup{InlineKeyboard: [][]inlineKeyboardButton{}}
	if err := b.editMessageTextAndMarkup(ctx, chatID, msgID, text, empty); err != nil {
		b.logger.Warn().Err(err).Int64("chat_id", chatID).Int64("message_id", msgID).
			Msg("steer: failed to edit prompt to recorded state")
	}
}
