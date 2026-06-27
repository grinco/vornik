package dispatcher

// set_reminder dispatcher tool. Lets the LLM record a future
// outbound message that the reminders heartbeat
// (internal/reminders) will deliver at the requested time.
//
// v1 (Phase A) is Telegram-only on the channel side: any
// non-Telegram session (chatID == 0) is refused with a clear
// "no channel of record" message. Phase B will resolve the
// active channel from the session and wire webchat / email.
//
// See https://docs.vornik.io

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/persistence"
)

// reminderMaxPendingPerOperator caps how many pending reminders
// one operator can stack. Prevents a hallucinating LLM from
// scheduling 10,000 reminders in a loop. Operator-tunable via
// VORNIK_REMINDERS_MAX_PENDING_PER_OPERATOR (read at boot in the
// service container; passed in here as a configured field on a
// future iteration — v1 uses the constant).
const reminderMaxPendingPerOperator = 50

// reminderMaxFutureWindow caps how far ahead a reminder can be
// scheduled. Defends against the LLM emitting "remind me in 50
// years" — unlikely but cheap to block.
const reminderMaxFutureWindow = 365 * 24 * time.Hour

// setReminderArgs is the parsed shape of the LLM's tool args.
// At most one of FireAtRFC3339 / FireInSeconds may be set; both
// missing is an error. We chose two narrow modes rather than a
// natural-language parser to keep v1 deterministic — the LLM
// can either supply an explicit timestamp (when it has the
// timezone right) or a duration-from-now (when it doesn't).
type setReminderArgs struct {
	FireAtRFC3339 string `json:"fire_at"`
	FireInSeconds int64  `json:"fire_in_seconds"`
	Content       string `json:"content"`
	Channel       string `json:"channel"` // optional v1 override; ignored unless == "telegram"
}

func (te *ToolExecutor) setReminder(ctx context.Context, argsJSON string, chatID int64, activeProject string) ToolResult {
	if te.reminderRepo == nil {
		return ToolResult{Content: "Reminders are not configured on this daemon. Ask the operator to enable the reminders subsystem."}
	}
	if chatID == 0 {
		// Phase A: only Telegram is wired. Webchat / email land
		// in Phase B alongside per-channel resolution.
		return ToolResult{Content: "set_reminder is only available on Telegram in v1; the current session has no Telegram chat of record."}
	}

	var args setReminderArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: "set_reminder: invalid arguments: " + err.Error()}
	}
	args.Content = strings.TrimSpace(args.Content)
	if args.Content == "" {
		return ToolResult{Content: "set_reminder: `content` is required (the body the reminder will deliver)."}
	}
	if len(args.Content) > 2000 {
		return ToolResult{Content: "set_reminder: `content` must be ≤ 2000 characters."}
	}

	fireAt, err := resolveReminderFireAt(args, time.Now())
	if err != nil {
		return ToolResult{Content: "set_reminder: " + err.Error()}
	}
	if fireAt.Before(time.Now().Add(-1 * time.Minute)) {
		return ToolResult{Content: "set_reminder: fire time is in the past."}
	}
	if fireAt.After(time.Now().Add(reminderMaxFutureWindow)) {
		return ToolResult{Content: fmt.Sprintf("set_reminder: fire time is more than %s in the future (cap).", reminderMaxFutureWindow)}
	}

	operatorID := "telegram:" + strconv.FormatInt(chatID, 10)

	n, err := te.reminderRepo.CountPendingByOperator(ctx, operatorID)
	if err != nil {
		return ToolResult{Content: "set_reminder: failed to check pending cap: " + err.Error()}
	}
	if n >= reminderMaxPendingPerOperator {
		return ToolResult{
			Content: fmt.Sprintf("set_reminder: you already have %d pending reminders (cap=%d). Cancel one with `vornikctl reminders cancel <id>` or wait for some to fire.", n, reminderMaxPendingPerOperator),
		}
	}

	rem := &persistence.Reminder{
		OperatorID: operatorID,
		Channel:    "telegram",
		ChannelRef: strconv.FormatInt(chatID, 10),
		ProjectID:  activeProject,
		FireAt:     fireAt.UTC(),
		Content:    args.Content,
		CreatedVia: "chat",
	}
	if err := te.reminderRepo.Insert(ctx, rem); err != nil {
		return ToolResult{Content: "set_reminder: insert failed: " + err.Error()}
	}
	if te.reminderKicker != nil {
		// Kick the heartbeat in case the reminder is due
		// immediately ("in 30s"). The default 30s poll would
		// otherwise leave the operator wondering whether the
		// reminder actually landed.
		te.reminderKicker.Kick()
	}
	// Audit the set so operators have a full set→fire→cancel
	// trail in /ui/admin/audit. Reuses the same channel + ref
	// captured on the row so the audit row alone answers
	// "who set what reminder for when, on which channel".
	if te.adminAuditRepo != nil {
		afterJSON, _ := json.Marshal(map[string]any{
			"reminder_id": rem.ID,
			"channel":     rem.Channel,
			"channel_ref": rem.ChannelRef,
			"project_id":  rem.ProjectID,
			"fire_at":     fireAt.UTC().Format(time.RFC3339),
			"content_len": len(rem.Content),
			"created_via": rem.CreatedVia,
		})
		_ = te.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: operatorID,
			Source:    "dispatcher",
			Action:    "reminder.set",
			Target:    rem.ID,
			After:     string(afterJSON),
		})
	}
	return ToolResult{
		Content:    fmt.Sprintf("Reminder %s set for %s. The bot will message you here when it fires.", rem.ID, fireAt.Format(time.RFC1123)),
		Provenance: outputguard.ProvenanceFirstParty,
	}
}

// resolveReminderFireAt converts the LLM's args into an absolute
// timestamp. Accepts either an RFC3339 string or a positive
// integer second offset. Returns an error string suitable for
// echoing back to the LLM.
func resolveReminderFireAt(args setReminderArgs, now time.Time) (time.Time, error) {
	if args.FireAtRFC3339 != "" {
		t, err := time.Parse(time.RFC3339, args.FireAtRFC3339)
		if err != nil {
			return time.Time{}, fmt.Errorf("fire_at must be RFC3339 (e.g. \"2026-05-24T09:00:00+02:00\"); got %q", args.FireAtRFC3339)
		}
		return t, nil
	}
	if args.FireInSeconds > 0 {
		return now.Add(time.Duration(args.FireInSeconds) * time.Second), nil
	}
	return time.Time{}, fmt.Errorf("supply either `fire_at` (RFC3339) or `fire_in_seconds` (positive integer)")
}
