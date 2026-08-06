// Package chatorigin resolves the originating chat channel/session for a
// task that was scheduled from a conversational channel (Telegram, Slack,
// email, web-chat). It is the SHARED extraction of the resolve-and-send
// chain that used to live only in internal/steering/notifier.go: task →
// (own or nearest chat-originated ancestor's) ChatTurnID → chat_audit_log
// row → decoded (channel, session) → the wired conversation.Channel.
//
// Two callers depend on this chain staying in lockstep:
//   - internal/steering's Notifier (steering notifications: "your task
//     needs you").
//   - internal/narrator's chat push (task 2.3, milestone narration pushed
//     to the originating chat, https://docs.vornik.io
//     narrated-execution-design.md §5.7).
//
// Before this package existed, both packages duplicated the chain inline.
// Companion review (finding 5/8 on the narrated-execution design) flagged
// that a future channel-resolution change (e.g. a Phase-5 channel
// migration) would have to be applied in two places and could silently
// drift. Keeping the chain here means it updates once for both callers.
package chatorigin

import (
	"context"
	"strings"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// TaskGetter is the narrow read needed to walk a task's ParentTaskID
// ancestry looking for a chat origin. Satisfied by
// persistence.TaskRepository. Optional — a nil getter disables the
// ancestry walk (only the immediate task is inspected).
type TaskGetter interface {
	Get(ctx context.Context, id string) (*persistence.Task, error)
}

// ChatAuditLookup is the narrow read needed to resolve a ChatTurnID to its
// originating chat_audit_log row. Satisfied by persistence.ChatAuditRepository.
type ChatAuditLookup interface {
	GetByID(ctx context.Context, id string) (*persistence.ChatAuditEntry, error)
}

// ChannelResolver returns the conversation.Channel registered under a name
// ("telegram"/"slack"/"email"), or nil when that channel isn't wired (or
// isn't a daemon-initiated-outbound channel at all — web-chat, a2a).
type ChannelResolver interface {
	ResolveChannel(name string) conversation.Channel
}

// lineageWalkHardLimit bounds the ParentTaskID walk so a corrupt cycle
// can't spin forever. Mirrors executor.lineageWalkHardLimit.
const lineageWalkHardLimit = 256

// Reason explains WHY a resolution did not produce a sendable destination.
//
// Added 2026-08-05: the chain used to collapse every failure into a bare
// `false`, and its UI consumer rendered them all as "this task wasn't started
// from a chat channel". For a task that WAS chat-originated but whose
// chat_audit_log row had been lost, that message was false and sent the
// operator hunting for a missing feature instead of a missing row. The three
// failure modes have genuinely different meanings and different remedies, so
// callers get to tell them apart.
type Reason string

const (
	// ReasonNone is the zero value, carried by a successful resolution.
	ReasonNone Reason = ""

	// ReasonNotChatOriginated means neither the task nor any ancestor
	// carries a ChatTurnID: an API / autonomy / A2A task. Correct and
	// final — there is genuinely nothing to send to.
	ReasonNotChatOriginated Reason = "not_chat_originated"

	// ReasonOriginRecordMissing means a ChatTurnID WAS found (on the task
	// or an ancestor) but no chat_audit_log row exists for it. This is a
	// fault, not a property of the task: the row was never written or has
	// been pruned. The deliverable cannot be routed automatically, and
	// nothing backfills the row — surface it as a fault so the operator
	// can hand the file over another way.
	ReasonOriginRecordMissing Reason = "origin_record_missing"

	// ReasonOriginRecordMalformed means the audit row exists but its
	// ChatID could not be decoded into (channel, session).
	ReasonOriginRecordMalformed Reason = "origin_record_malformed"

	// ReasonChannelUnavailable means the origin decoded cleanly but that
	// channel isn't wired on this deployment — either genuinely absent
	// from config, or a channel that never accepts daemon-initiated
	// outbound (web-chat, a2a). Result.ChannelName still names it.
	ReasonChannelUnavailable Reason = "channel_unavailable"
)

// Result is the resolved originating-channel destination for a
// chat-originated task — enough for a caller to build a
// conversation.ChannelMessage and Send it.
type Result struct {
	// Channel is the resolved outbound channel. Never nil when ok=true.
	Channel conversation.Channel
	// ChannelName is the channel's registered name ("telegram", "slack",
	// "email", ...), e.g. for the email to/subject special-case.
	ChannelName string
	// SessionID is the channel-native session/thread id to reply into.
	SessionID string
	// AuditRow is the resolved chat_audit_log row backing this
	// resolution. Callers needing to reconstruct an address (email's
	// to/subject, recovered from AuditRow.UserID via
	// EmailAddrFromUserID) use this; most callers only need SessionID.
	AuditRow *persistence.ChatAuditEntry
	// Reason explains a failed resolution (ok=false). ReasonNone on
	// success. Callers that only branch on ok can ignore it; callers that
	// report to a human should not, because "no chat origin" and "chat
	// origin record lost" need different words.
	Reason Reason
}

// TurnID returns the ChatTurnID that a chat-push/notification for
// `task` should route to: the task's own ChatTurnID when set, otherwise the
// nearest ancestor's (walking ParentTaskID via getter). Returns "" when
// neither the task nor any ancestor is chat-originated (API / autonomy /
// A2A tasks never resolve here — never an error, just "nothing to push
// to").
//
// This is the fix for the 2026-07-08 report: a task scheduled from a
// Telegram chat carries a ChatTurnID, but the checkpoint / route /
// delegation children it spawns do NOT — they only carry ParentTaskID.
// Without the walk, a paused/completed descendant of a chat-scheduled task
// was mis-routed away from the originating chat.
func TurnID(ctx context.Context, task *persistence.Task, getter TaskGetter) string {
	if task == nil {
		return ""
	}
	if task.ChatTurnID != nil && *task.ChatTurnID != "" {
		return *task.ChatTurnID
	}
	if getter == nil {
		return ""
	}
	seen := map[string]bool{task.ID: true}
	parentID := task.ParentTaskID
	for i := 0; i < lineageWalkHardLimit; i++ {
		if parentID == nil || *parentID == "" || seen[*parentID] {
			return ""
		}
		seen[*parentID] = true
		parent, err := getter.Get(ctx, *parentID)
		if err != nil || parent == nil {
			// A missing ancestor (pruned/archived) terminates the chain —
			// best effort, same as the executor's lineage walkers.
			return ""
		}
		if parent.ChatTurnID != nil && *parent.ChatTurnID != "" {
			return *parent.ChatTurnID
		}
		parentID = parent.ParentTaskID
	}
	return ""
}

// ResolveForTurn resolves an already-known chat turn id to its originating
// channel: audit.GetByID → DecodeChatID → resolver.ResolveChannel. ok=false
// at any step (no row, an undecodable chat id, or an unresolved/un-wired
// channel) means "nothing to send to" — never an error to the caller. Which
// of those it was is in Result.Reason: a caller reporting to a human must not
// render a lost audit row as "this task never came from a chat".
func ResolveForTurn(ctx context.Context, turnID string, audit ChatAuditLookup, resolver ChannelResolver) (Result, bool) {
	if turnID == "" || audit == nil || resolver == nil {
		return Result{Reason: ReasonNotChatOriginated}, false
	}
	row, err := audit.GetByID(ctx, turnID)
	if err != nil || row == nil {
		// The turn id exists on the task but its row does not. NOT the same
		// as "never came from chat" — see ReasonOriginRecordMissing.
		return Result{Reason: ReasonOriginRecordMissing}, false
	}
	channelName, sessionID := DecodeChatID(row.ChatID)
	if channelName == "" || sessionID == "" {
		return Result{Reason: ReasonOriginRecordMalformed, AuditRow: row}, false
	}
	ch := resolver.ResolveChannel(channelName)
	if ch == nil {
		// web-chat / a2a / an un-wired channel — nothing to send to. Carry
		// the name anyway so a caller can say WHICH channel is unavailable.
		return Result{
			ChannelName: channelName,
			SessionID:   sessionID,
			AuditRow:    row,
			Reason:      ReasonChannelUnavailable,
		}, false
	}
	return Result{Channel: ch, ChannelName: channelName, SessionID: sessionID, AuditRow: row}, true
}

// Resolve is the full chain, task → originating channel: TurnID
// then ResolveForTurn. This is what internal/narrator's chat push calls
// directly; internal/steering's Notifier calls the two halves separately
// so it can dedup between them (see notifier.go's comment on ordering).
func Resolve(ctx context.Context, task *persistence.Task, tasks TaskGetter, audit ChatAuditLookup, resolver ChannelResolver) (Result, bool) {
	turnID := TurnID(ctx, task, tasks)
	if turnID == "" {
		return Result{Reason: ReasonNotChatOriginated}, false
	}
	return ResolveForTurn(ctx, turnID, audit, resolver)
}

// DecodeChatID reverses dispatcher.resolveChatID's encoding:
//   - a bare decimal string is a legacy Telegram chat_id → ("telegram", id)
//   - otherwise the form is "<channel>:<native-session-id>" → split on the
//     FIRST colon (Slack/email session ids may themselves contain colons).
func DecodeChatID(chatID string) (channel, session string) {
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

// EmailAddrFromUserID extracts the operator's address from the audit row's
// UserID, which the channel receiver formats as "<channel>:<speaker>" — for
// email the speaker IS the From address.
func EmailAddrFromUserID(userID string) string {
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
