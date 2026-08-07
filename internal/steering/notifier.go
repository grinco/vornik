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

	"vornik.io/vornik/internal/chatorigin"
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
// ChatTurnID to its originating chat row. Alias of chatorigin.ChatAuditLookup
// — see internal/chatorigin's package doc for why this chain is shared with
// the narrator's chat push.
type ChatAuditLookup = chatorigin.ChatAuditLookup

// ChannelResolver returns the conversation.Channel registered under a name
// ("telegram"/"slack"/"email"), or nil when that channel isn't wired. Alias
// of chatorigin.ChannelResolver.
type ChannelResolver = chatorigin.ChannelResolver

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
	//
	// The dedup check sits BETWEEN turn-id resolution and the audit-row /
	// channel resolution (chatorigin.ResolveForTurn) so a duplicate-transition
	// fire never pays for the audit lookup — this is why NotifySteeringRequired
	// calls the two chatorigin halves separately instead of chatorigin.Resolve
	// in one shot (that combined helper is what the narrator's chat push uses,
	// since it has no equivalent dedup step to interleave).
	turnID := chatOriginTurnID(ctx, task, n.tasks)
	if turnID == "" {
		return
	}
	if n.recentlySent(task.ID, state) {
		return
	}

	// SHARED with internal/narrator's chat push via internal/chatorigin — a
	// future channel-resolution change (e.g. a Phase-5 channel migration)
	// updates this one call, not two duplicated chains (companion review
	// finding 5/8 on the narrated-execution design).
	res, ok := chatorigin.ResolveForTurn(ctx, turnID, n.audit, n.resolver)
	if !ok {
		n.logger.Debug().Str("task_id", task.ID).Str("chat_turn_id", turnID).
			Msg("steering: could not resolve originating channel; skipping")
		return
	}

	msg := conversation.ChannelMessage{
		SessionID: res.SessionID,
		Text:      n.composeText(ctx, task, state),
		Buttons:   n.buildSteeringButtons(ctx, task, state),
	}
	// Email's Send needs an addressable recipient + subject (it can't always
	// recover them from an in-memory session after a restart); supply them
	// from the durable audit row so email works cross-restart like the others.
	if res.ChannelName == "email" && res.AuditRow != nil {
		if to := chatorigin.EmailAddrFromUserID(res.AuditRow.UserID); to != "" {
			msg.ChannelSpecific = map[string]string{
				"to":      to,
				"subject": "vornik: a task needs your attention",
			}
		}
	}

	if _, err := res.Channel.Send(ctx, msg); err != nil {
		n.logger.Warn().Err(err).Str("task_id", task.ID).Str("channel", res.ChannelName).
			Msg("steering: outbound send failed")
		return
	}
	n.markSent(task.ID, state)
	n.logger.Info().Str("task_id", task.ID).Str("channel", res.ChannelName).Str("state", state).
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
// composeTextMaxBytes caps the whole rendered prompt. Sized against the
// SMALLEST channel limit, not Slack's: Telegram rejects over 4096 and email
// varies, so a cap chosen for Slack's ~40 000 would produce messages Telegram
// silently drops. 3500 leaves headroom for a channel adding its own decoration.
const composeTextMaxBytes = 3500

// TaskRef is the short handle the operator types back: the first 4 characters
// of the task id's RANDOM suffix — for task_20260806212011_77b90a7d0e1d0e47
// that is "77b9". Deliberately not the timestamp segment, which is
// near-identical across same-minute tasks and would collide constantly.
func TaskRef(taskID string) string {
	suffix := taskID
	if i := strings.LastIndex(taskID, "_"); i >= 0 && i+1 < len(taskID) {
		suffix = taskID[i+1:]
	}
	if len(suffix) > 4 {
		suffix = suffix[:4]
	}
	return suffix
}

// composeText builds the operator-facing prompt.
//
// The question and its options are rendered INTO THE TEXT rather than left to
// ChannelMessage.Buttons. Only Telegram reads Buttons, so for Slack and email a
// decision checkpoint used to arrive as a bare "needs your input" with no
// question and no options — structurally unanswerable
// (https://docs.vornik.io §v1.1).
//
// Assembly order matters: the actionable tail (options + reply instruction +
// UI link) is composed FIRST and the question gets whatever budget is left.
// Truncating the finished string from the end — the obvious implementation —
// would cut the reply instruction off a long question and reproduce the very
// bug this fixes.
func (n *Notifier) composeText(ctx context.Context, task *persistence.Task, state string) string {
	return n.composeTextWithHint(ctx, task, state, "")
}

// composeTextWithHint renders the prompt with a CHANNEL-SUPPLIED reply
// instruction. The reply protocol is per-channel and, on Slack, per-DEPLOYMENT:
// several vornik instances share one workspace, each answering its own
// configured slash command (/vornik, /holy, …). Hardcoding "/vornik answer"
// here would tell a /holy operator to invoke a different instance — a separate
// daemon with a separate database, where the 4-hex ref matches nothing or, on a
// collision, answers an unrelated task. Telegram, which answers by tapping a
// button, passes "" and gets no reply line at all.
func (n *Notifier) composeTextWithHint(ctx context.Context, task *persistence.Task, state, replyHint string) string {
	var what string
	switch state {
	case string(persistence.TaskStatusAwaitingApproval):
		what = "is waiting for your approval before it runs"
	default: // AWAITING_INPUT
		what = "needs your input — it asked a question and paused"
	}
	head := fmt.Sprintf("🔔 Task %s (project %s) %s.", task.ID, task.ProjectID, what)

	question, options := n.checkpointBody(ctx, task, state)

	tail := &strings.Builder{}
	if len(options) > 0 {
		tail.WriteString("\n")
		for i, o := range options {
			fmt.Fprintf(tail, "\n  %d. %s", i+1, o)
		}
		if replyHint != "" {
			fmt.Fprintf(tail, "\n\nReply:  %s", replyHint)
		}
	} else if question != "" && replyHint != "" {
		fmt.Fprintf(tail, "\n\nReply:  %s", replyHint)
	}
	if n.baseURL != "" {
		// Canonical UI task-detail route is /ui/tasks/{id} — the nested
		// /ui/projects/{p}/tasks/{id} form is the API path shape and 404s in
		// the browser (operator-reported 2026-07-08).
		fmt.Fprintf(tail, "\nOpen:   %s/ui/tasks/%s", n.baseURL, task.ID)
	}

	// Whatever is left after the fixed parts belongs to the question.
	budget := composeTextMaxBytes - len(head) - tail.Len() - len("\n\n")
	if question != "" && budget > 0 {
		if len(question) > budget {
			cut := budget - len("…")
			if cut < 0 {
				cut = 0
			}
			question = strings.TrimSpace(question[:cut]) + "…"
		}
		return head + "\n\n" + question + tail.String()
	}
	return head + tail.String()
}

// checkpointBody reads the open checkpoint's question and option labels.
// Returns empty values when no checkpoint reader is wired or nothing is open —
// the caller then renders the generic nudge rather than an empty options block.
//
// AWAITING_APPROVAL has no checkpoint row; it renders the fixed approve/reject
// pair so approvals and decisions share one grammar for the operator to learn.
func (n *Notifier) checkpointBody(ctx context.Context, task *persistence.Task, state string) (string, []string) {
	if state == string(persistence.TaskStatusAwaitingApproval) {
		return "", []string{"approve", "reject"}
	}
	if n.checkpoints == nil {
		return "", nil
	}
	cp, err := n.checkpoints.GetOpenCheckpoint(ctx, task.ID)
	if err != nil || cp == nil {
		return "", nil
	}
	question := strings.TrimSpace(cp.Content)
	var meta struct {
		Question string `json:"question"`
		Options  []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"options"`
	}
	if len(cp.Metadata) > 0 {
		_ = json.Unmarshal(cp.Metadata, &meta)
	}
	if question == "" {
		question = strings.TrimSpace(meta.Question)
	}
	labels := make([]string, 0, len(meta.Options))
	for _, o := range meta.Options {
		// Same label→id fallback buildSteeringButtons applies, so the text and
		// the buttons can never disagree about what option 2 is.
		if o.Label != "" {
			labels = append(labels, o.Label)
			continue
		}
		labels = append(labels, o.ID)
	}
	return question, labels
}

// decodeChatID forwards to the shared chatorigin.DecodeChatID (kept as a
// package-local name so existing callers/tests in this package don't need
// the chatorigin-qualified name). See that function's doc comment for the
// encoding rules.
func decodeChatID(chatID string) (channel, session string) {
	return chatorigin.DecodeChatID(chatID)
}

// emailAddrFromUserID forwards to the shared chatorigin.EmailAddrFromUserID.
func emailAddrFromUserID(userID string) string {
	return chatorigin.EmailAddrFromUserID(userID)
}

// isAllDigits is retained locally (not exported by chatorigin) purely
// because steering_edgecases_test.go exercises it directly as a package-
// internal helper unit; DecodeChatID's own behaviour no longer calls this
// copy.
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
