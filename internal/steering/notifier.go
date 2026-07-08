// Package steering delivers "your task needs you" notifications: when a task
// an operator created from a chat/DM enters a steering state
// (AWAITING_INPUT — it asked a question; AWAITING_APPROVAL — it's parked for
// approval), it pushes a plain message back to the channel the operator used,
// so they don't have to be watching the UI inbox to find out.
//
// Channel-agnostic + durable: the originating channel + session are resolved
// from the task's ChatTurnID via the chat_audit_log row (survives a daemon
// restart, unlike the in-memory per-channel followup maps). Telegram, Slack,
// and email are supported; web-chat is request-scoped (no daemon-initiated
// outbound) and A2A is not a conversation channel — both no-op here. See
// https://docs.vornik.io
package steering

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// dedupWindow suppresses a duplicate notification for the same (task, state)
// within this window — a guard against a transition hook firing twice for one
// transition. It is far shorter than the gap between two legitimate
// checkpoints (which includes the operator's reply time), so it never
// swallows a genuine second steering moment.
const dedupWindow = 30 * time.Second

// ChatAuditLookup is the narrow read the notifier needs: resolve a task's
// ChatTurnID to its originating chat row.
type ChatAuditLookup interface {
	GetByID(ctx context.Context, id string) (*persistence.ChatAuditEntry, error)
}

// ChannelResolver returns the conversation.Channel registered under a name
// ("telegram"/"slack"/"email"), or nil when that channel isn't wired.
type ChannelResolver interface {
	ResolveChannel(name string) conversation.Channel
}

// CheckpointReader fetches a task's open decision checkpoint so the notifier
// can turn its options into tap-to-answer buttons. Optional — nil disables
// button rendering (the prompt stays plain text). The returned message's
// Metadata carries the options as `{"options":[{"id","label"}]}`.
type CheckpointReader interface {
	GetOpenCheckpoint(ctx context.Context, taskID string) (*persistence.TaskMessage, error)
}

// Notifier implements the steering-notification send. Safe for concurrent
// use. A nil Notifier or one with enabled=false makes every call a no-op.
type Notifier struct {
	audit       ChatAuditLookup
	resolver    ChannelResolver
	tasks       TaskGetter       // optional; walks ParentTaskID to find a chat origin
	checkpoints CheckpointReader // optional; enables decision-option buttons
	baseURL     string           // external base URL for UI deep links; may be empty
	enabled     bool
	logger      zerolog.Logger

	mu   sync.Mutex
	sent map[string]time.Time // (taskID|state) -> last send, for dedup
}

// New builds a Notifier. enabled=false (or a nil audit/resolver) yields a
// no-op notifier. tasks is optional: when non-nil the notifier walks a task's
// ParentTaskID ancestry to find the originating chat. checkpoints is optional:
// when non-nil the notifier renders a decision checkpoint's options (and
// approval prompts) as inline buttons the operator taps, instead of a
// "reply with text" prompt; nil keeps the plain-text prompt.
func New(audit ChatAuditLookup, resolver ChannelResolver, tasks TaskGetter, checkpoints CheckpointReader, baseURL string, enabled bool, logger zerolog.Logger) *Notifier {
	return &Notifier{
		audit:       audit,
		resolver:    resolver,
		tasks:       tasks,
		checkpoints: checkpoints,
		baseURL:     strings.TrimRight(baseURL, "/"),
		enabled:     enabled,
		logger:      logger,
		sent:        map[string]time.Time{},
	}
}

// NotifySteeringRequired pushes a steering prompt for a task that just entered
// `state` (persistence.TaskStatusAwaitingInput / TaskStatusAwaitingApproval).
// Best-effort and non-fatal: every failure path logs and returns, never
// blocking the caller's state transition.
func (n *Notifier) NotifySteeringRequired(ctx context.Context, task *persistence.Task, state string) {
	if n == nil || !n.enabled || n.audit == nil || n.resolver == nil {
		return
	}
	if task == nil {
		return
	}
	// Resolve the originating chat turn — the task's own, or the nearest
	// chat-originated ancestor's (a chat-scheduled task's children carry only
	// ParentTaskID, not ChatTurnID). Empty ⇒ not chat-originated anywhere in
	// the lineage (API / autonomy / A2A) — no DM to notify.
	turnID := chatOriginTurnID(ctx, task, n.tasks)
	if turnID == "" {
		return
	}
	if n.recentlySent(task.ID, state) {
		return
	}

	row, err := n.audit.GetByID(ctx, turnID)
	if err != nil || row == nil {
		n.logger.Debug().Err(err).Str("task_id", task.ID).Str("chat_turn_id", turnID).
			Msg("steering: could not resolve originating chat turn; skipping")
		return
	}

	channelName, sessionID := decodeChatID(row.ChatID)
	if channelName == "" || sessionID == "" {
		return
	}
	ch := n.resolver.ResolveChannel(channelName)
	if ch == nil {
		// web-chat / a2a / an un-wired channel — nothing to send to.
		n.logger.Debug().Str("task_id", task.ID).Str("channel", channelName).
			Msg("steering: originating channel has no outbound; skipping")
		return
	}

	msg := conversation.ChannelMessage{
		SessionID: sessionID,
		Text:      n.composeText(task, state),
		Buttons:   n.buildSteeringButtons(ctx, task, state),
	}
	// Email's Send needs an addressable recipient + subject (it can't always
	// recover them from an in-memory session after a restart); supply them
	// from the durable audit row so email works cross-restart like the others.
	if channelName == "email" {
		if to := emailAddrFromUserID(row.UserID); to != "" {
			msg.ChannelSpecific = map[string]string{
				"to":      to,
				"subject": "vornik: a task needs your attention",
			}
		}
	}

	if _, err := ch.Send(ctx, msg); err != nil {
		n.logger.Warn().Err(err).Str("task_id", task.ID).Str("channel", channelName).
			Msg("steering: outbound send failed")
		return
	}
	n.markSent(task.ID, state)
	n.logger.Info().Str("task_id", task.ID).Str("channel", channelName).Str("state", state).
		Msg("steering: notified originating operator")
}

// buildSteeringButtons turns a steering event into tap-to-answer buttons:
//   - AWAITING_APPROVAL → ✅ Approve / ❌ Reject
//   - AWAITING_INPUT with an open decision checkpoint → one button per option
//
// Returns nil (plain-text prompt) for a free-text question or when no
// checkpoint reader is wired. The callback data is the wire format the
// Telegram `steer:` handler decodes (`steer:<action>:<payload>`), kept compact
// so it fits Telegram's 64-byte callback cap (task ids are ~47 chars). Only
// channels that support buttons render these; others ignore them.
func (n *Notifier) buildSteeringButtons(ctx context.Context, task *persistence.Task, state string) [][]conversation.MessageButton {
	if state == string(persistence.TaskStatusAwaitingApproval) {
		approve := "steer:approve:" + task.ID
		reject := "steer:reject:" + task.ID
		// Respect Telegram's 64-byte callback cap (task ids are usually ~47
		// chars, but don't assume) — an over-cap button would be silently
		// dropped by Telegram, so fall back to the text-reply prompt instead.
		if len(approve) > 64 || len(reject) > 64 {
			return nil
		}
		return [][]conversation.MessageButton{{
			{Label: "✅ Approve", CallbackData: approve},
			{Label: "❌ Reject", CallbackData: reject},
		}}
	}
	if state != string(persistence.TaskStatusAwaitingInput) || n.checkpoints == nil {
		return nil
	}
	cp, err := n.checkpoints.GetOpenCheckpoint(ctx, task.ID)
	if err != nil || cp == nil || len(cp.Metadata) == 0 {
		return nil
	}
	var meta struct {
		Options []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"options"`
	}
	if err := json.Unmarshal(cp.Metadata, &meta); err != nil || len(meta.Options) == 0 {
		return nil
	}
	rows := make([][]conversation.MessageButton, 0, len(meta.Options))
	for i, o := range meta.Options {
		label := o.Label
		if label == "" {
			label = o.ID
		}
		// Encode the option by INDEX (not id) so the callback stays within the
		// 64-byte cap regardless of option-id length; the tap handler resolves
		// index→option against the same stored checkpoint.
		data := fmt.Sprintf("steer:c:%s:%d", task.ID, i)
		if len(data) > 64 {
			continue // task id too long to encode — fall back to text reply
		}
		rows = append(rows, []conversation.MessageButton{{Label: label, CallbackData: data}})
	}
	if len(rows) == 0 {
		return nil
	}
	return rows
}

func (n *Notifier) recentlySent(taskID, state string) bool {
	key := taskID + "|" + state
	n.mu.Lock()
	defer n.mu.Unlock()
	last, ok := n.sent[key]
	if !ok {
		return false
	}
	return time.Since(last) < dedupWindow
}

func (n *Notifier) markSent(taskID, state string) {
	key := taskID + "|" + state
	n.mu.Lock()
	n.sent[key] = time.Now()
	// Opportunistic prune so the map can't grow unbounded over a long uptime.
	if len(n.sent) > 4096 {
		cutoff := time.Now().Add(-dedupWindow)
		for k, t := range n.sent {
			if t.Before(cutoff) {
				delete(n.sent, k)
			}
		}
	}
	n.mu.Unlock()
}

// composeText builds the operator-facing prompt: what the task needs + a UI
// deep link to act on it.
func (n *Notifier) composeText(task *persistence.Task, state string) string {
	var what string
	switch state {
	case string(persistence.TaskStatusAwaitingApproval):
		what = "is waiting for your approval before it runs"
	default: // AWAITING_INPUT
		what = "needs your input — it asked a question and paused"
	}
	b := &strings.Builder{}
	fmt.Fprintf(b, "🔔 Task %s (project %s) %s.", task.ID, task.ProjectID, what)
	if n.baseURL != "" {
		// Canonical UI task-detail route is /ui/tasks/{id} — the nested
		// /ui/projects/{p}/tasks/{id} form is the API path shape and 404s in
		// the browser (operator-reported 2026-07-08).
		fmt.Fprintf(b, "\nOpen it: %s/ui/tasks/%s", n.baseURL, task.ID)
	}
	return b.String()
}

// decodeChatID reverses dispatcher.resolveChatID's encoding:
//   - a bare decimal string is a legacy Telegram chat_id → ("telegram", id)
//   - otherwise the form is "<channel>:<native-session-id>" → split on the
//     FIRST colon (Slack/email session ids may themselves contain colons).
func decodeChatID(chatID string) (channel, session string) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return "", ""
	}
	if isAllDigits(chatID) {
		return "telegram", chatID
	}
	if i := strings.IndexByte(chatID, ':'); i > 0 && i < len(chatID)-1 {
		return chatID[:i], chatID[i+1:]
	}
	return "", ""
}

// emailAddrFromUserID extracts the operator's address from the audit row's
// UserID, which the channel receiver formats as "<channel>:<speaker>" — for
// email the speaker IS the From address.
func emailAddrFromUserID(userID string) string {
	userID = strings.TrimSpace(userID)
	if i := strings.IndexByte(userID, ':'); i >= 0 && i < len(userID)-1 {
		return userID[i+1:]
	}
	return userID
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
