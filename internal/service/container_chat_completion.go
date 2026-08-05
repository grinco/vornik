package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chatorigin"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
)

// CUSTOMER REPORT 2026-07-30, remote installation: "vornik said on Slack it would
// schedule a job, then nothing happened; when asked the status of the job it didn't know
// anything about it."
//
// The promise was real — the task was created — but nothing ever reported back. Only
// telegram (its own followup map) and email (internal/email/followup.go) implement
// executor.CompletionNotifier. A task created from Slack registered nowhere, so no
// channel was told it finished, on any code path.
//
// Two properties the existing per-channel notifiers do NOT have, and which the report
// specifically calls for:
//
//   - DURABLE. Both existing maps live in memory, so a restart between "scheduled" and
//     "finished" loses the link silently. The operator said to assume a restart. This
//     resolves the destination from the DATABASE every time
//     (task.ChatTurnID → chat_audit_log → channel + session, via internal/chatorigin,
//     the same primitive the steering notifier already uses), so a restart changes
//     nothing.
//   - FIRES WITHOUT await_completion. The per-channel followups are registered only when
//     create_task asked for auto-resume. Someone who says "schedule this" and nothing
//     more gets no registration and therefore no word back — which is most of the time.
//
// It announces rather than resuming: a plain message costs no LLM turn, arrives
// immediately, and names the task and its UI page. Auto-resume, where a channel has it,
// stays that channel's business.
type chatCompletionNotifier struct {
	audit    chatorigin.ChatAuditLookup
	tasks    chatorigin.TaskGetter
	resolver chatorigin.ChannelResolver
	baseURL  string
	enabled  bool

	// channels bounds which resolved channel names this notifier speaks for.
	// Telegram and email announce completion through their own auto-resume path, so
	// firing here as well would report every task twice. Responsibility is therefore an
	// explicit allowlist, not "whatever resolves" — a channel that grows its own
	// notifier must be removed from here in the same change.
	channels map[string]bool

	logger zerolog.Logger

	// The executor calls NotifyTaskCompleted TWICE for a COMPLETED task (lead_handoff
	// plus the terminal transition — see internal/telegram/forum.go's note on the same
	// hazard), so dedup is required rather than defensive.
	mu   sync.Mutex
	sent map[string]time.Time
}

// completionDedupWindow bounds the dedup map's memory. Far longer than the gap between
// the executor's two calls, far shorter than any sane task lifetime.
const completionDedupWindow = 10 * time.Minute

func newChatCompletionNotifier(
	audit chatorigin.ChatAuditLookup,
	tasks chatorigin.TaskGetter,
	resolver chatorigin.ChannelResolver,
	baseURL string,
	enabled bool,
	channels map[string]bool,
	logger zerolog.Logger,
) *chatCompletionNotifier {
	return &chatCompletionNotifier{
		audit:    audit,
		tasks:    tasks,
		resolver: resolver,
		baseURL:  strings.TrimRight(baseURL, "/"),
		enabled:  enabled,
		channels: channels,
		logger:   logger,
		sent:     map[string]time.Time{},
	}
}

// NotifyTaskCompleted implements executor.CompletionNotifier.
//
// Best-effort throughout: every failure path returns quietly. This runs on the
// executor's terminal transition, and a task must reach COMPLETED even when nobody can
// be told about it.
func (n *chatCompletionNotifier) NotifyTaskCompleted(
	ctx context.Context,
	task *persistence.Task,
	success bool,
	message string,
) {
	if n == nil || !n.enabled || task == nil || task.ID == "" {
		return
	}
	if n.alreadySent(task.ID) {
		return
	}

	// DB-backed resolution, which is what makes this survive a restart.
	origin, ok := chatorigin.Resolve(ctx, task, n.tasks, n.audit, n.resolver)
	if !ok {
		// API / autonomy / A2A tasks have no chat origin. Not an error — there is
		// simply nobody to tell.
		return
	}
	if !n.channels[origin.ChannelName] {
		// A channel that announces completion itself. Staying quiet here is what keeps
		// a task from being reported twice.
		return
	}

	n.markSent(task.ID)
	text := n.composeText(origin.ChannelName, task, success, message)
	if _, err := origin.Channel.Send(ctx, conversation.ChannelMessage{
		Source:    origin.ChannelName,
		SessionID: origin.SessionID,
		Text:      text,
		// SpeakerID empty: this is the daemon speaking, not a person.
	}); err != nil {
		n.logger.Warn().
			Err(err).
			Str("task_id", task.ID).
			Str("channel", origin.ChannelName).
			Str("session_id", origin.SessionID).
			Msg("chat completion notice could not be delivered")
		return
	}
	n.logger.Info().
		Str("task_id", task.ID).
		Str("channel", origin.ChannelName).
		Str("session_id", origin.SessionID).
		Bool("success", success).
		Msg("chat completion notice delivered to the originating session")
}

// composeText renders the notice.
//
// Status comes from the `success` bool rather than task.Status: the executor's in-memory
// *Task is stale when this fires (UpdateStatus writes the row, not the struct) — the
// 2026-05-21 incident recorded in telegram/bot.go's triggerFollowup.
func (n *chatCompletionNotifier) composeText(channelName string, task *persistence.Task, success bool, message string) string {
	var sb strings.Builder
	// Formatted for reading: an emoji carries the outcome at a glance in a busy
	// channel, and the identifiers are code-spanned so they can be copied. The TTS
	// path strips all of it — see internal/voice.ForSpeech.
	if success {
		fmt.Fprintf(&sb, ":white_check_mark: *Finished* the job you asked for — task `%s`", task.ID)
	} else {
		fmt.Fprintf(&sb, ":x: *That job did not finish* — task `%s` (status %s)", task.ID, task.Status)
	}
	if task.ProjectID != "" {
		fmt.Fprintf(&sb, " in project `%s`", task.ProjectID)
	}
	sb.WriteString(".\n")

	if !success && task.LastError != nil && strings.TrimSpace(*task.LastError) != "" {
		fmt.Fprintf(&sb, "*Error:* %s\n", truncateForChat(*task.LastError, 800))
	}
	if strings.TrimSpace(message) != "" {
		fmt.Fprintf(&sb, "*Last status:* %s\n", truncateForChat(message, 500))
	}
	// The link is the point: it is how someone reads the result without asking again.
	// Rendered as a hyperlink on words rather than pasted raw — a task URL is long and
	// a channel full of them is unreadable (operator, 2026-07-30).
	if n.baseURL != "" && task.ProjectID != "" {
		url := fmt.Sprintf("%s/ui/projects/%s/tasks/%s", n.baseURL, task.ProjectID, task.ID)
		fmt.Fprintf(&sb, "%s\n", conversation.Link(channelName, url, "open the task"))
	}
	sb.WriteString("\n_Ask me about it and I can summarise the result._")
	return sb.String()
}

func truncateForChat(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

func (n *chatCompletionNotifier) alreadySent(taskID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, seen := n.sent[taskID]
	return seen
}

func (n *chatCompletionNotifier) markSent(taskID string) {
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	for id, at := range n.sent {
		if now.Sub(at) > completionDedupWindow {
			delete(n.sent, id)
		}
	}
	n.sent[taskID] = now
}

// Compile-time guard: this satisfies the executor's contract.
var _ executor.CompletionNotifier = (*chatCompletionNotifier)(nil)

// chatCompletionSink builds the durable "your job finished" notifier. Returns nil when
// the chat-audit repo isn't wired, since resolution is entirely DB-backed and there is
// nothing to resolve without it — same precondition as steeringNotifier.
//
// Gated by SteeringNotificationsEnabled: it is the same class of daemon-initiated
// outbound to a person's chat session, and an operator who turned those off did not mean
// "except for completions".
func (c *Container) chatCompletionSink() executor.CompletionNotifier {
	if c == nil || c.repos == nil || c.repos.ChatAudit == nil {
		return nil
	}
	baseURL := ""
	enabled := false
	if c.Config != nil {
		baseURL = c.Config.PublicOrigin()
		enabled = c.Config.SteeringNotificationsEnabled
	}
	if !enabled {
		return nil
	}
	return newChatCompletionNotifier(
		c.repos.ChatAudit,
		c.steeringTaskGetter(),
		&containerChannelResolver{c: c},
		baseURL,
		true,
		// Slack and email. Telegram still announces completion through its own
		// auto-resume followup; adding it here would report every task twice.
		// When it migrates, remove its notifier in the SAME change rather than
		// listing it here as well.
		//
		// Email moved here 2026-08-05. Its own notifier was in-memory and gated
		// on await_completion, so a task scheduled without that flag — or one
		// whose daemon restarted mid-flight — reported nothing at all: the same
		// customer-visible break already fixed for Slack. Worse, its resume was
		// composed by feeding a synthetic turn back through the dispatcher,
		// which needs an LLM call. The 2026-07-30 incident is the proof: the
		// resume fired and died with "LLM call failed: context deadline
		// exceeded", so the message announcing the breakage was itself broken by
		// the breakage. This path sends a plain composed notice with no LLM turn
		// and resolves the origin from the DB, which is what makes it survive
		// both a restart and a degraded model.
		map[string]bool{"slack": true, "email": true},
		c.Logger.With().Str("component", "chat-completion").Logger(),
	)
}
