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

// OperatorAlertConfig is the fallback recipient for steering on tasks that
// have NO originating chat — the autonomy loop's own tasks. The shipped
// chat/DM Notifier resolves a recipient from the task's ChatTurnID, so an
// autonomy-created task that parks at AWAITING_APPROVAL would otherwise notify
// nobody and stall silently. This config names a single operator channel +
// session to alert instead. An empty Channel/Session disables the fallback
// (the backwards-compatible default — nothing fires unless an operator opts
// in by configuring a recipient).
type OperatorAlertConfig struct {
	// Channel is the conversation channel name to alert ("telegram" /
	// "slack" / "email").
	Channel string `yaml:"channel" json:"channel"`
	// Session is the native session id within that channel (a Telegram
	// chat_id, a Slack composite session, an email Message-ID/thread key).
	Session string `yaml:"session" json:"session"`
	// Address is the email recipient, required only when Channel == "email"
	// (the channel's Send needs an addressable To it can't recover from a
	// synthetic session). Ignored for other channels.
	Address string `yaml:"address" json:"address"`
}

// configured reports whether a usable recipient is set.
func (c OperatorAlertConfig) configured() bool {
	return strings.TrimSpace(c.Channel) != "" && strings.TrimSpace(c.Session) != ""
}

// OperatorAlertNotifier is the steering sink for ownerless autonomy tasks. It
// implements executor.SteeringNotifier and fans in alongside the chat/DM
// Notifier (which no-ops for these tasks). Safe for concurrent use; a nil
// notifier, enabled=false, or an unconfigured recipient makes every call a
// no-op.
type OperatorAlertNotifier struct {
	resolver ChannelResolver
	baseURL  string
	cfg      OperatorAlertConfig
	enabled  bool
	logger   zerolog.Logger

	mu   sync.Mutex
	sent map[string]time.Time
}

// NewOperatorAlert builds an OperatorAlertNotifier. enabled=false, a nil
// resolver, or an unconfigured cfg yields a no-op notifier.
func NewOperatorAlert(resolver ChannelResolver, baseURL string, cfg OperatorAlertConfig, enabled bool, logger zerolog.Logger) *OperatorAlertNotifier {
	return &OperatorAlertNotifier{
		resolver: resolver,
		baseURL:  strings.TrimRight(baseURL, "/"),
		cfg:      cfg,
		enabled:  enabled,
		logger:   logger,
		sent:     map[string]time.Time{},
	}
}

// NotifySteeringRequired alerts the configured operator recipient when an
// ownerless autonomy task enters a steering state. It deliberately fires ONLY
// for tasks the chat/DM Notifier can't reach: autonomy-created (no originating
// chat). Chat-originated tasks are skipped so the operator isn't notified
// twice for the same task. Best-effort and non-fatal.
func (n *OperatorAlertNotifier) NotifySteeringRequired(ctx context.Context, task *persistence.Task, state string) {
	if n == nil || !n.enabled || n.resolver == nil || !n.cfg.configured() {
		return
	}
	if task == nil {
		return
	}
	// Chat-originated tasks are the chat Notifier's job; don't double-notify.
	if task.ChatTurnID != nil && *task.ChatTurnID != "" {
		return
	}
	// Scope to autonomy-created tasks: these are the genuinely ownerless ones
	// that would otherwise stall unseen. Other ownerless sources (A2A) have
	// their own push path; user/API tasks are the operator's own doing.
	if task.CreationSource != persistence.TaskCreationSourceAutonomous {
		return
	}
	if n.recentlySent(task.ID, state) {
		return
	}

	ch := n.resolver.ResolveChannel(n.cfg.Channel)
	if ch == nil {
		n.logger.Debug().Str("task_id", task.ID).Str("channel", n.cfg.Channel).
			Msg("operator-alert: configured channel has no outbound; skipping")
		return
	}

	msg := conversation.ChannelMessage{
		SessionID: n.cfg.Session,
		Text:      n.composeText(task, state),
	}
	if n.cfg.Channel == "email" && n.cfg.Address != "" {
		msg.ChannelSpecific = map[string]string{
			"to":      n.cfg.Address,
			"subject": "vornik: an autonomous task needs your attention",
		}
	}

	if _, err := ch.Send(ctx, msg); err != nil {
		n.logger.Warn().Err(err).Str("task_id", task.ID).Str("channel", n.cfg.Channel).
			Msg("operator-alert: outbound send failed")
		return
	}
	n.markSent(task.ID, state)
	n.logger.Info().Str("task_id", task.ID).Str("channel", n.cfg.Channel).Str("state", state).
		Msg("operator-alert: notified operator of ownerless autonomy task")
}

// NotifyOperator pushes a free-form operator alert (e.g. a cluster-monitor
// endpoint-down notification) to the same configured recipient as the steering
// fallback. Best-effort and non-fatal; a nil/disabled/unconfigured notifier is
// a no-op. Independent of the task-steering dedup. The cluster monitor relies
// on its own edge-triggering so this isn't called per-tick.
func (n *OperatorAlertNotifier) NotifyOperator(ctx context.Context, subject, body string) {
	if n == nil || !n.enabled || n.resolver == nil || !n.cfg.configured() {
		return
	}
	ch := n.resolver.ResolveChannel(n.cfg.Channel)
	if ch == nil {
		n.logger.Debug().Str("channel", n.cfg.Channel).Msg("operator-alert: configured channel has no outbound; skipping")
		return
	}
	text := subject
	if body != "" {
		text = subject + "\n" + body
	}
	msg := conversation.ChannelMessage{SessionID: n.cfg.Session, Text: text}
	if n.cfg.Channel == "email" && n.cfg.Address != "" {
		msg.ChannelSpecific = map[string]string{"to": n.cfg.Address, "subject": subject}
	}
	if _, err := ch.Send(ctx, msg); err != nil {
		n.logger.Warn().Err(err).Str("channel", n.cfg.Channel).Msg("operator-alert: outbound send failed")
	}
}

func (n *OperatorAlertNotifier) recentlySent(taskID, state string) bool {
	key := taskID + "|" + state
	n.mu.Lock()
	defer n.mu.Unlock()
	last, ok := n.sent[key]
	if !ok {
		return false
	}
	return time.Since(last) < dedupWindow
}

func (n *OperatorAlertNotifier) markSent(taskID, state string) {
	key := taskID + "|" + state
	n.mu.Lock()
	n.sent[key] = time.Now()
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

// composeText builds the operator-facing alert: which autonomy task needs
// attention + a UI deep link to act on it.
func (n *OperatorAlertNotifier) composeText(task *persistence.Task, state string) string {
	var what string
	switch state {
	case string(persistence.TaskStatusAwaitingApproval):
		what = "is waiting for your approval before it runs"
	default: // AWAITING_INPUT
		what = "needs your input — it asked a question and paused"
	}
	b := &strings.Builder{}
	fmt.Fprintf(b, "🔔 Autonomous task %s (project %s) %s. No chat originated it, so this is your operator alert.", task.ID, task.ProjectID, what)
	if n.baseURL != "" && task.ProjectID != "" {
		fmt.Fprintf(b, "\nOpen it: %s/ui/projects/%s/tasks/%s", n.baseURL, task.ProjectID, task.ID)
	}
	return b.String()
}
