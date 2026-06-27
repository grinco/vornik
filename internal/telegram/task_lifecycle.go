package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Phase 28 of the conversational task lifecycle (LLD §5.2).
//
// Telegram becomes a per-task interactive surface. Notifications
// posted by NotifyTaskCompleted record their (chat_id, message_id)
// → task_id mapping. When the operator REPLIES to that
// notification, the bot routes the reply to task_messages — as
// an `answer` if the task has an open checkpoint, otherwise as
// a `directive` (course correction) or `message`.
//
// Also adds /inbox: a single command that lists every task in
// AWAITING_INPUT across projects.

// taskNotifKey identifies one outbound notification by chat +
// telegram message id.
type taskNotifKey struct {
	ChatID    int64
	MessageID int64
}

// taskNotifEntry is the per-key value: which task this
// notification is about + when it was sent. Old entries get
// pruned periodically so the map doesn't grow unbounded.
type taskNotifEntry struct {
	TaskID    string
	ProjectID string
	SentAt    time.Time
}

// taskNotifTracker is the in-memory message-id → task-id map.
// Survives the bot's lifetime (cleared on restart). 7-day TTL is
// long enough for slow vendor cycles, short enough that the map
// stays small.
type taskNotifTracker struct {
	mu      sync.RWMutex
	entries map[taskNotifKey]taskNotifEntry
}

func newTaskNotifTracker() *taskNotifTracker {
	return &taskNotifTracker{entries: make(map[taskNotifKey]taskNotifEntry)}
}

// remember records a notification's mapping. ttlPrune runs each
// remember to keep the map bounded.
func (t *taskNotifTracker) remember(chatID, msgID int64, taskID, projectID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[taskNotifKey{ChatID: chatID, MessageID: msgID}] = taskNotifEntry{
		TaskID:    taskID,
		ProjectID: projectID,
		SentAt:    time.Now().UTC(),
	}
	t.prune()
}

// lookup returns the task associated with the (chat, msg) pair
// or empty strings when none.
func (t *taskNotifTracker) lookup(chatID, msgID int64) (taskID, projectID string, ok bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, found := t.entries[taskNotifKey{ChatID: chatID, MessageID: msgID}]
	if !found {
		return "", "", false
	}
	return e.TaskID, e.ProjectID, true
}

// prune drops entries older than 7 days. Caller holds the write
// lock. O(n) but n stays small (one-per-notification, ~hundreds
// per week in production).
func (t *taskNotifTracker) prune() {
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	for k, e := range t.entries {
		if e.SentAt.Before(cutoff) {
			delete(t.entries, k)
		}
	}
}

// routeReplyToTask is called from HandleMessage when the operator
// replied to a notification we tracked. Decides whether the
// inbound text is an `answer` to an open checkpoint or a generic
// `message` / `directive`, writes the task_message, and
// (conditionally) re-queues the task.
//
// Returns (handled, err). handled=true means the reply was
// fully processed by this function and HandleMessage should not
// fall through to the dispatcher LLM. handled=false means the
// caller should try the normal dispatch path.
func (b *Bot) routeReplyToTask(ctx context.Context, msg *Message, taskID, projectID string) (bool, error) {
	if b.taskMessageRepo == nil || b.taskRepo == nil {
		return false, nil
	}

	task, err := b.taskRepo.Get(ctx, taskID)
	if err != nil {
		// Task vanished — fall through to dispatcher.
		b.logger.Warn().Err(err).Str("task_id", taskID).Msg("telegram: replied-to task no longer exists; falling through")
		return false, nil
	}

	// Per-project authorization. A reply (via reply-id or forum
	// topic) must not inject an operator directive/answer into a
	// task whose project the user isn't scoped for. Use the loaded
	// task.ProjectID as authoritative — on the forum path the
	// projectID argument is the thread-row UUID, not a project id.
	// Returning (true, nil) prevents fall-through to the dispatcher.
	if task.ProjectID != "" && !b.UserCanAccessProject(msg.UserID, task.ProjectID) {
		const deny = "You are not authorized for this task's project."
		if msg.MessageThreadID != 0 && b.forumEnabled() {
			if _, ferr := b.sendForumMessage(ctx, msg.MessageThreadID, deny); ferr != nil {
				_ = b.sendMessage(ctx, msg.ChatID, deny)
			}
		} else {
			_ = b.sendMessage(ctx, msg.ChatID, deny)
		}
		return true, nil
	}

	// Decide kind: open checkpoint → answer; otherwise directive
	// (operator replied because they have a course-correction).
	kind := persistence.TaskMessageKindDirective
	var checkpointID string
	if task.OpenCheckpointID != nil {
		kind = persistence.TaskMessageKindAnswer
		checkpointID = *task.OpenCheckpointID
	}

	authorID := msg.Username
	if authorID == "" {
		authorID = fmt.Sprintf("tg:%d", msg.UserID)
	}

	// Best-effort choice extraction: when the operator's text
	// matches one of the checkpoint's option labels (case-insensitive,
	// trimmed), record it as the structured choice in metadata.
	choice := ""
	if kind == persistence.TaskMessageKindAnswer && task.OpenCheckpointID != nil {
		if cp, gerr := b.taskMessageRepo.GetOpenCheckpoint(ctx, taskID); gerr == nil && cp != nil && len(cp.Metadata) > 0 {
			choice = matchChoiceFromText(cp.Metadata, msg.Text)
		}
	}
	meta := map[string]any{
		"source":              "telegram",
		"chat_id":             msg.ChatID,
		"reply_to_message_id": msg.ReplyToMessageID,
	}
	if msg.MessageThreadID != 0 {
		// Phase 29 — reply routed via Forum Topic, not reply-id.
		// Audit-log readers use this to tell the two paths apart.
		meta["source"] = "telegram_forum"
		meta["thread_id"] = msg.MessageThreadID
	}
	if choice != "" {
		meta["choice"] = choice
	}
	metaBytes, _ := json.Marshal(meta)

	tmsg := &persistence.TaskMessage{
		TaskID:      taskID,
		MessageKind: kind,
		AuthorKind:  persistence.TaskMessageAuthorOperator,
		AuthorID:    &authorID,
		Content:     msg.Text,
		Metadata:    metaBytes,
		CreatedAt:   time.Now().UTC(),
	}
	if checkpointID != "" {
		tmsg.ParentID = &checkpointID
	}
	if err := b.taskMessageRepo.Insert(ctx, tmsg); err != nil {
		_ = b.sendMessage(ctx, msg.ChatID, "⚠️ Could not record your reply: "+err.Error())
		return true, err
	}

	// Resolve checkpoint + transition if applicable.
	requeued := false
	if kind == persistence.TaskMessageKindAnswer && checkpointID != "" {
		_ = b.taskMessageRepo.MarkCheckpointResolved(ctx, taskID, checkpointID)
		ok, terr := b.taskRepo.TransitionConditional(ctx, taskID,
			[]persistence.TaskStatus{persistence.TaskStatusAwaitingInput},
			persistence.TaskStatusQueued,
			persistence.TransitionOpts{ClearLease: true})
		if terr == nil && ok {
			requeued = true
		}
	} else if kind == persistence.TaskMessageKindDirective {
		// Mirror the API/UI handler's re-queue policy (LLD §7.0).
		// Waiting states keep their attempt counter; terminal
		// states (FAILED / CANCELLED / COMPLETED) reset attempt=1
		// via RequeueTerminalTask so the corrected task gets a
		// fresh max_attempts budget and last_error is cleared.
		// CLOSED is excluded — archival is one-way.
		switch task.Status {
		case persistence.TaskStatusAwaitingInput,
			persistence.TaskStatusAwaitingExternal:
			ok, _ := b.taskRepo.TransitionConditional(ctx, taskID,
				[]persistence.TaskStatus{task.Status},
				persistence.TaskStatusQueued,
				persistence.TransitionOpts{ClearLease: true})
			requeued = ok
		case persistence.TaskStatusFailed,
			persistence.TaskStatusCancelled,
			persistence.TaskStatusCompleted:
			ok, _ := b.taskRepo.RequeueTerminalTask(ctx, taskID, 1, task.MaxAttempts)
			requeued = ok
		}
	}
	if requeued && b.rescheduler != nil {
		b.rescheduler.Wake()
	}

	// Acknowledge to the operator. When the reply came from a
	// forum topic, the ack goes back to the same topic via
	// sendForumMessage so the thread stays self-contained;
	// otherwise it lands in the main chat.
	ack := "✓ recorded as " + kind
	if requeued {
		ack += "; task re-queued"
	}
	if msg.MessageThreadID != 0 && b.forumEnabled() {
		if _, err := b.sendForumMessage(ctx, msg.MessageThreadID, ack); err != nil {
			b.logger.Warn().Err(err).
				Int64("chat_id", msg.ChatID).
				Int64("thread_id", msg.MessageThreadID).
				Msg("forum: failed to ack in topic; falling back to main chat")
			_ = b.sendMessage(ctx, msg.ChatID, ack)
		}
	} else {
		_ = b.sendMessage(ctx, msg.ChatID, ack)
	}
	return true, nil
}

// routeForumReplyIfApplicable handles inbound messages posted
// inside a Telegram Forum Topic. When forum routing is enabled
// and the (chat_id, message_thread_id) pair maps to a known task,
// the reply is routed straight to task_messages via
// routeReplyToTask — bypassing both the in-memory notifTracker
// and the dispatcher LLM.
//
// Returns (handled, err). handled=true means HandleMessage
// should not fall through to subsequent routing. handled=false
// means: not a forum message, or thread not mapped to a task —
// the existing notifTracker / dispatcher paths take over.
func (b *Bot) routeForumReplyIfApplicable(ctx context.Context, msg *Message) (bool, error) {
	if !b.forumEnabled() || msg == nil || msg.MessageThreadID == 0 {
		return false, nil
	}
	// Slash-command escape hatch: an operator inside a topic who
	// types /help wants help, not to record "/help" as a directive
	// on the task. Mirrors the notifTracker path's slash-prefix
	// guard.
	if strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
		return false, nil
	}
	thread, err := b.threadRepo.GetByThread(ctx, msg.ChatID, msg.MessageThreadID)
	if err != nil {
		// ErrNotFound → unknown topic, fall through to dispatcher.
		// Any other error: log + fall through so the operator
		// still gets *some* response.
		if !errors.Is(err, persistence.ErrNotFound) {
			b.logger.Warn().Err(err).
				Int64("chat_id", msg.ChatID).
				Int64("thread_id", msg.MessageThreadID).
				Msg("forum: thread lookup failed; falling through")
		}
		return false, nil
	}
	return b.routeReplyToTask(ctx, msg, thread.TaskID, thread.ID)
}

// matchChoiceFromText scans a checkpoint's options for one whose
// id or label matches the operator's text (case-insensitive,
// substring-or-prefix). Returns the option id, or empty string
// when no clear match.
func matchChoiceFromText(meta []byte, text string) string {
	var raw struct {
		Options []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"options"`
	}
	if err := json.Unmarshal(meta, &raw); err != nil || len(raw.Options) == 0 {
		return ""
	}
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return ""
	}
	for _, o := range raw.Options {
		if strings.EqualFold(o.ID, lower) || strings.EqualFold(o.Label, lower) {
			return o.ID
		}
	}
	// Looser: option id appears as a word in the message.
	for _, o := range raw.Options {
		if strings.Contains(lower, strings.ToLower(o.ID)) {
			return o.ID
		}
	}
	return ""
}

// renderInbox lists the operator's awaiting-input tasks across
// projects. Powers the /inbox command.
//
// Filter: status = AWAITING_INPUT only (deliberately leaves
// AWAITING_EXTERNAL out — operator action isn't blocking those,
// time/event is). Limit 25 for chat readability.
func (b *Bot) renderInbox(ctx context.Context, userID int64) (string, error) {
	if b.taskRepo == nil {
		return "Inbox unavailable: task repository not configured.", nil
	}
	awaiting := persistence.TaskStatusAwaitingInput
	// Per-user project scoping. AllowedProjectsForUser returns nil for an
	// unrestricted user (no allowlist / wildcard) — no filtering then. A
	// non-nil set (possibly empty = deny-all) restricts results so a
	// scoped operator never sees other projects' awaiting-input tasks.
	// TaskFilter narrows only by status, so when a scope applies we widen
	// PageSize and post-filter, then trim back to 25 for chat readability.
	allowed := b.AllowedProjectsForUser(userID)
	pageSize := 25
	if allowed != nil {
		pageSize = 200
	}
	tasks, err := b.taskRepo.List(ctx, persistence.TaskFilter{
		Status:   &awaiting,
		PageSize: pageSize,
	})
	if err != nil {
		return "", fmt.Errorf("list awaiting tasks: %w", err)
	}
	if allowed != nil {
		permitted := make(map[string]bool, len(allowed))
		for _, p := range allowed {
			permitted[p] = true
		}
		filtered := tasks[:0]
		for _, t := range tasks {
			if permitted[t.ProjectID] {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
		if len(tasks) > 25 {
			tasks = tasks[:25]
		}
	}
	if len(tasks) == 0 {
		return "📭 Inbox is empty — no tasks awaiting your input.", nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "📬 %d task(s) awaiting your input:\n\n", len(tasks))
	for _, t := range tasks {
		title := taskTitleFromPayload(t.Payload, 80)
		if title == "" {
			title = t.ID
		}
		phase := ""
		if t.CurrentPhase != nil && *t.CurrentPhase != "" {
			phase = " — " + *t.CurrentPhase
		}
		fmt.Fprintf(&sb, "• %s%s\n  /task %s\n\n", title, phase, t.ID)
	}
	return sb.String(), nil
}

// taskTitleFromPayload extracts payload.context.prompt and trims it
// to a human-readable, maxLen-bounded title. Returns empty string
// when the payload has no prompt — caller substitutes the task ID
// (or another fallback) in that case. Used by /inbox and by the
// forum-topic naming path.
func taskTitleFromPayload(payload []byte, maxLen int) string {
	if len(payload) == 0 {
		return ""
	}
	var pl struct {
		Context struct {
			Prompt string `json:"prompt"`
		} `json:"context"`
	}
	if err := json.Unmarshal(payload, &pl); err != nil {
		return ""
	}
	p := strings.TrimSpace(pl.Context.Prompt)
	if p == "" {
		return ""
	}
	if maxLen > 3 && len(p) > maxLen {
		p = p[:maxLen-3] + "..."
	}
	return p
}
