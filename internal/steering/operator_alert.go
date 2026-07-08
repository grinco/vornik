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

// configured reports whether a channel is set. A per-project recipient
// resolver (ProjectRecipients) supplies the actual recipients; Session is
// only a fallback for projects that resolve to no one, so it is no longer
// required for the notifier to be usable.
func (c OperatorAlertConfig) configured() bool {
	return strings.TrimSpace(c.Channel) != ""
}

// ProjectRecipients resolves the operator session IDs to alert for a task's
// project on a channel — e.g. the Telegram chat IDs of allowed users who may
// access that project. Implemented in the service layer (backed by the
// Telegram allow-list); nil means "no per-project routing, use the fallback
// session only". Returning an empty slice means "nobody has access", which
// falls through to the configured fallback session.
type ProjectRecipients interface {
	RecipientsForProject(channel, projectID string) []string
}

// ownerlessSteeringSources are the CreationSources of tasks that have NO
// originating chat the chat/DM Notifier could reach: autonomy loop tasks and
// the routed / checkpoint sub-tasks they (or any adaptive task) spawn — a
// routed child never inherits its parent's ChatTurnID, so a checkpoint on it
// notifies nobody without this fallback. USER / COMPANION / A2A / DELEGATION
// are excluded: they carry a chat origin or their own push path.
var ownerlessSteeringSources = map[persistence.TaskCreationSource]bool{
	persistence.TaskCreationSourceAutonomous: true,
	persistence.TaskCreationSourceRoute:      true,
	persistence.TaskCreationSourceCheckpoint: true,
}

// OperatorAlertNotifier is the steering sink for ownerless autonomy tasks. It
// implements executor.SteeringNotifier and fans in alongside the chat/DM
// Notifier (which no-ops for these tasks). Safe for concurrent use; a nil
// notifier, enabled=false, or an unconfigured recipient makes every call a
// no-op.
type OperatorAlertNotifier struct {
	resolver   ChannelResolver
	recipients ProjectRecipients
	tasks      TaskGetter // optional; walks ParentTaskID to detect a chat origin
	baseURL    string
	cfg        OperatorAlertConfig
	enabled    bool
	logger     zerolog.Logger

	mu   sync.Mutex
	sent map[string]time.Time
}

// NewOperatorAlert builds an OperatorAlertNotifier. enabled=false, a nil
// resolver, or an unconfigured cfg (no channel) yields a no-op notifier.
// recipients may be nil (falls back to cfg.Session for every project). tasks
// is optional: when non-nil, a task whose ancestry carries a ChatTurnID is
// treated as chat-owned (the chat Notifier handles it) and skipped here, so a
// chat-scheduled task's children don't get a duplicate generic operator alert.
func NewOperatorAlert(resolver ChannelResolver, recipients ProjectRecipients, tasks TaskGetter, baseURL string, cfg OperatorAlertConfig, enabled bool, logger zerolog.Logger) *OperatorAlertNotifier {
	return &OperatorAlertNotifier{
		resolver:   resolver,
		recipients: recipients,
		tasks:      tasks,
		baseURL:    strings.TrimRight(baseURL, "/"),
		cfg:        cfg,
		enabled:    enabled,
		logger:     logger,
		sent:       map[string]time.Time{},
	}
}

// recipientSessions resolves the channel session IDs to alert for a task's
// project — per-project operators first, the configured fallback session
// otherwise.
func (n *OperatorAlertNotifier) recipientSessions(projectID string) []string {
	if n.recipients != nil {
		if s := n.recipients.RecipientsForProject(n.cfg.Channel, projectID); len(s) > 0 {
			return s
		}
	}
	if strings.TrimSpace(n.cfg.Session) != "" {
		return []string{n.cfg.Session}
	}
	return nil
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
	// This checks the whole ancestry, not just the immediate task: a
	// checkpoint/route child of a chat-scheduled task carries only
	// ParentTaskID, so without the walk it would wrongly get this generic
	// alert instead of routing back to the originating chat (2026-07-08 fix).
	if chatOriginTurnID(ctx, task, n.tasks) != "" {
		return
	}
	// Fire only for genuinely ownerless tasks — autonomy loop tasks AND the
	// routed / checkpoint sub-tasks they spawn that have NO chat ancestor.
	// USER / COMPANION / A2A / DELEGATION are excluded (chat origin or their
	// own push path).
	if !ownerlessSteeringSources[task.CreationSource] {
		return
	}

	ch := n.resolver.ResolveChannel(n.cfg.Channel)
	if ch == nil {
		n.logger.Debug().Str("task_id", task.ID).Str("channel", n.cfg.Channel).
			Msg("operator-alert: configured channel has no outbound; skipping")
		return
	}

	// Fan out to the operators with access to THIS task's project (wildcard +
	// project-scoped), so assistant tasks reach assistant operators, janka
	// tasks reach janka operators, etc. Falls back to the configured session.
	sessions := n.recipientSessions(task.ProjectID)
	if len(sessions) == 0 {
		n.logger.Debug().Str("task_id", task.ID).Str("project", task.ProjectID).
			Msg("operator-alert: no recipients for project; skipping")
		return
	}

	text := n.composeText(task, state)
	for _, session := range sessions {
		dedupKey := task.ID + "|" + session
		if n.recentlySent(dedupKey, state) {
			continue
		}
		msg := conversation.ChannelMessage{SessionID: session, Text: text}
		if n.cfg.Channel == "email" && n.cfg.Address != "" {
			msg.ChannelSpecific = map[string]string{
				"to":      n.cfg.Address,
				"subject": "vornik: an autonomous task needs your attention",
			}
		}
		if _, err := ch.Send(ctx, msg); err != nil {
			n.logger.Warn().Err(err).Str("task_id", task.ID).Str("channel", n.cfg.Channel).
				Str("session", session).Msg("operator-alert: outbound send failed")
			continue
		}
		n.markSent(dedupKey, state)
		n.logger.Info().Str("task_id", task.ID).Str("project", task.ProjectID).
			Str("channel", n.cfg.Channel).Str("session", session).Str("state", state).
			Msg("operator-alert: notified operator of ownerless task")
	}
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

func (n *OperatorAlertNotifier) recentlySent(recipientKey, state string) bool {
	key := recipientKey + "|" + state
	n.mu.Lock()
	defer n.mu.Unlock()
	last, ok := n.sent[key]
	if !ok {
		return false
	}
	return time.Since(last) < dedupWindow
}

func (n *OperatorAlertNotifier) markSent(recipientKey, state string) {
	key := recipientKey + "|" + state
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
	if n.baseURL != "" {
		// Canonical UI task-detail route is /ui/tasks/{id} — the nested
		// /ui/projects/{p}/tasks/{id} form is the API path shape and 404s in
		// the browser (operator-reported 2026-07-08).
		fmt.Fprintf(b, "\nOpen it: %s/ui/tasks/%s", n.baseURL, task.ID)
	}
	return b.String()
}
