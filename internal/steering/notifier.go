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

// Notifier implements the steering-notification send. Safe for concurrent
// use. A nil Notifier or one with enabled=false makes every call a no-op.
type Notifier struct {
	audit    ChatAuditLookup
	resolver ChannelResolver
	baseURL  string // external base URL for UI deep links; may be empty
	enabled  bool
	logger   zerolog.Logger

	mu   sync.Mutex
	sent map[string]time.Time // (taskID|state) -> last send, for dedup
}

// New builds a Notifier. enabled=false (or a nil audit/resolver) yields a
// no-op notifier.
func New(audit ChatAuditLookup, resolver ChannelResolver, baseURL string, enabled bool, logger zerolog.Logger) *Notifier {
	return &Notifier{
		audit:    audit,
		resolver: resolver,
		baseURL:  strings.TrimRight(baseURL, "/"),
		enabled:  enabled,
		logger:   logger,
		sent:     map[string]time.Time{},
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
	if task == nil || task.ChatTurnID == nil || *task.ChatTurnID == "" {
		// Not chat-originated (API / autonomy / A2A) — no DM to notify.
		return
	}
	if n.recentlySent(task.ID, state) {
		return
	}

	row, err := n.audit.GetByID(ctx, *task.ChatTurnID)
	if err != nil || row == nil {
		n.logger.Debug().Err(err).Str("task_id", task.ID).Str("chat_turn_id", *task.ChatTurnID).
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
	if n.baseURL != "" && task.ProjectID != "" {
		fmt.Fprintf(b, "\nOpen it: %s/ui/projects/%s/tasks/%s", n.baseURL, task.ProjectID, task.ID)
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
