package dispatcher

// cancel_reminder + update_reminder dispatcher tools — peers
// of the existing set_reminder. Without these the LLM has no
// way to undo or modify a reminder it scheduled (operators had
// to drop to the CLI / UI). Both tools enforce per-operator
// scope: an operator can only modify reminders they own.
//
// Identity check uses the operator id stamped on the dispatcher
// Request (req.OperatorID -> ctx via WithOperatorID). When
// missing (synthetic turns) the tool refuses rather than risk
// a cross-operator modification.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/persistence"
)

// cancelReminderArgs is the parsed shape of the cancel tool's args.
type cancelReminderArgs struct {
	ReminderID string `json:"reminder_id"`
	Rationale  string `json:"rationale"`
}

// updateReminderArgs is the parsed shape of the update tool's
// args. At most one of FireAtRFC3339 / FireInSeconds may be
// set; both omitted means "keep the existing fire_at and only
// update content" (caller must supply Content in that case).
type updateReminderArgs struct {
	ReminderID    string `json:"reminder_id"`
	FireAtRFC3339 string `json:"fire_at"`
	FireInSeconds int64  `json:"fire_in_seconds"`
	Content       string `json:"content"`
	Rationale     string `json:"rationale"`
}

func (te *ToolExecutor) cancelReminderTool(ctx context.Context, argsJSON string, chatID int64) ToolResult {
	if te.reminderRepo == nil {
		return ToolResult{Content: "Reminders are not configured on this daemon."}
	}
	var args cancelReminderArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: "cancel_reminder: invalid arguments: " + err.Error()}
	}
	if strings.TrimSpace(args.ReminderID) == "" {
		return ToolResult{Content: "cancel_reminder: reminder_id is required."}
	}
	row, err := te.reminderRepo.Get(ctx, args.ReminderID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return ToolResult{Content: fmt.Sprintf("cancel_reminder: reminder %q not found.", args.ReminderID)}
		}
		return ToolResult{Content: "cancel_reminder: lookup failed: " + err.Error()}
	}
	if !reminderBelongsToCaller(row, chatID, ctx) {
		return ToolResult{Content: fmt.Sprintf("cancel_reminder: reminder %q is not yours (belongs to a different operator).", args.ReminderID)}
	}
	if err := te.reminderRepo.Cancel(ctx, args.ReminderID); err != nil {
		return ToolResult{Content: "cancel_reminder: " + err.Error()}
	}
	// Audit on the same channel as set/fire so the lifecycle
	// log is complete.
	if te.adminAuditRepo != nil {
		afterJSON, _ := json.Marshal(map[string]string{
			"reminder_id": args.ReminderID,
			"rationale":   strings.TrimSpace(args.Rationale),
		})
		_ = te.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: row.OperatorID,
			Source:    "dispatcher",
			Action:    "reminder.cancelled",
			Target:    args.ReminderID,
			After:     string(afterJSON),
		})
	}
	return ToolResult{Content: fmt.Sprintf("Reminder %s cancelled. Rationale: %s.", args.ReminderID, strings.TrimSpace(args.Rationale)), Provenance: outputguard.ProvenanceFirstParty}
}

func (te *ToolExecutor) updateReminderTool(ctx context.Context, argsJSON string, chatID int64) ToolResult {
	if te.reminderRepo == nil {
		return ToolResult{Content: "Reminders are not configured on this daemon."}
	}
	var args updateReminderArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: "update_reminder: invalid arguments: " + err.Error()}
	}
	if strings.TrimSpace(args.ReminderID) == "" {
		return ToolResult{Content: "update_reminder: reminder_id is required."}
	}
	row, err := te.reminderRepo.Get(ctx, args.ReminderID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return ToolResult{Content: fmt.Sprintf("update_reminder: reminder %q not found.", args.ReminderID)}
		}
		return ToolResult{Content: "update_reminder: lookup failed: " + err.Error()}
	}
	if !reminderBelongsToCaller(row, chatID, ctx) {
		return ToolResult{Content: fmt.Sprintf("update_reminder: reminder %q is not yours (belongs to a different operator).", args.ReminderID)}
	}
	if row.Status != persistence.ReminderStatusPending {
		return ToolResult{Content: fmt.Sprintf("update_reminder: reminder %q is no longer pending (status=%s); reminders that are firing / fired / cancelled / expired can't be modified. Create a new one.", args.ReminderID, row.Status)}
	}

	// Resolve the new fire_at. Three modes, in priority order:
	//   1. fire_at (RFC3339 absolute) — wins when set.
	//   2. fire_in_seconds — offset from now.
	//   3. neither — carry forward the existing fire_at.
	fireAt := row.FireAt
	switch {
	case strings.TrimSpace(args.FireAtRFC3339) != "":
		t, perr := time.Parse(time.RFC3339, args.FireAtRFC3339)
		if perr != nil {
			return ToolResult{Content: "update_reminder: fire_at must be RFC3339: " + perr.Error()}
		}
		fireAt = t.UTC()
	case args.FireInSeconds > 0:
		fireAt = time.Now().UTC().Add(time.Duration(args.FireInSeconds) * time.Second)
	}
	if !fireAt.After(time.Now()) {
		return ToolResult{Content: "update_reminder: fire time is in the past."}
	}

	content := strings.TrimSpace(args.Content)
	if err := te.reminderRepo.UpdateFields(ctx, args.ReminderID, fireAt, content); err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return ToolResult{Content: fmt.Sprintf("update_reminder: reminder %q is no longer pending; cannot modify.", args.ReminderID)}
		}
		return ToolResult{Content: "update_reminder: " + err.Error()}
	}
	if te.adminAuditRepo != nil {
		afterJSON, _ := json.Marshal(map[string]string{
			"reminder_id": args.ReminderID,
			"fire_at_utc": fireAt.Format(time.RFC3339),
			"content_set": fmt.Sprintf("%v", content != ""),
			"rationale":   strings.TrimSpace(args.Rationale),
		})
		_ = te.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: row.OperatorID,
			Source:    "dispatcher",
			Action:    "reminder.updated",
			Target:    args.ReminderID,
			After:     string(afterJSON),
		})
	}
	return ToolResult{Content: fmt.Sprintf("Reminder %s updated. New fire time: %s. Rationale: %s.",
		args.ReminderID, fireAt.Format(time.RFC1123), strings.TrimSpace(args.Rationale)), Provenance: outputguard.ProvenanceFirstParty}
}

// reminderBelongsToCaller checks the per-operator scope. Two
// signals are accepted:
//   - chatID matches the row's stored Telegram channel_ref
//     (set_reminder writes it as the int64 stringified).
//   - operator id from context matches row.OperatorID.
//
// Either is enough — Telegram-only deployments rely on chatID;
// multi-channel deployments rely on the ctx-stamped operator
// id. Both checks together gate against cross-operator
// modification.
func reminderBelongsToCaller(row *persistence.Reminder, chatID int64, ctx context.Context) bool {
	if row == nil {
		return false
	}
	if chatID != 0 && row.ChannelRef == fmt.Sprintf("%d", chatID) {
		return true
	}
	if opID, ok := operatorIDFromContext(ctx); ok && opID != "" {
		if row.OperatorID == opID {
			return true
		}
	}
	return false
}
