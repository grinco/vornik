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
	"vornik.io/vornik/internal/reminders"
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
	// Cron, when non-empty, changes the recurrence cadence (5-field
	// POSIX). It wins over fire_at/fire_in_seconds — the next fire is
	// recomputed from the new expression, mirroring set_reminder.
	Cron string `json:"cron"`
	// Project, when non-empty, changes the project a task-kind reminder
	// runs in. It re-runs the same session ACL set_reminder enforces at
	// create time; rejected on text-kind reminders (they run nothing).
	Project   string `json:"project"`
	Rationale string `json:"rationale"`
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

func (te *ToolExecutor) updateReminderTool(ctx context.Context, argsJSON string, chatID int64, activeProject string, allowedProjects []string) ToolResult {
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
		return ToolResult{Content: fmt.Sprintf("update_reminder: reminder %q can't be modified in its current state (status=%s); only pending reminders are editable. If it's paused, resume it first; if a task-kind fire is in flight, wait for it to finish; otherwise create a new one.", args.ReminderID, row.Status)}
	}

	fireAt, cronExpr, errRes := resolveUpdateSchedule(args, row)
	if errRes != nil {
		return *errRes
	}
	newProject, errRes := te.resolveUpdateProject(args, row, activeProject, allowedProjects)
	if errRes != nil {
		return *errRes
	}

	content := strings.TrimSpace(args.Content)
	upd := persistence.ReminderFieldUpdate{FireAt: fireAt, Content: content, CronExpr: cronExpr, ProjectID: newProject}
	if err := te.reminderRepo.UpdateFields(ctx, args.ReminderID, upd); err != nil {
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
			// Record the resolved cron/project values (not just booleans)
			// so the audit trail can answer "moved to WHICH project?" for
			// the ACL-crossing project edit. Empty = the field was unchanged.
			"cron_expr":  cronExpr,
			"project_id": newProject,
			"rationale":  strings.TrimSpace(args.Rationale),
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

// resolveUpdateSchedule resolves the new fire_at + cron_expr for an
// update_reminder edit. A non-empty `cron` wins (validated, next fire
// recomputed) — mirroring set_reminder's cron-wins resolution so a
// recurring reminder's cadence can be changed in one call. Otherwise the
// fire_at is resolved in priority order: explicit fire_at (RFC3339, must
// be future) → fire_in_seconds offset → carry forward the row's existing
// fire_at. The past-fire guard applies ONLY to an explicit new fire_at;
// a carry-forward (no schedule field supplied — e.g. a content/project-
// only edit) is NOT re-validated, so a recurring row whose next fire has
// momentarily slipped past but isn't yet leased still accepts the edit.
// The returned cron_expr is "" when unchanged (COALESCE keeps the row's
// existing schedule). Returns a non-nil errResult on validation failure.
func resolveUpdateSchedule(args updateReminderArgs, row *persistence.Reminder) (time.Time, string, *ToolResult) {
	if c := strings.TrimSpace(args.Cron); c != "" {
		if err := reminders.ValidateCronExpr(c); err != nil {
			return time.Time{}, "", &ToolResult{Content: "update_reminder: invalid `cron` expression (need 5-field POSIX, e.g. \"0 9 * * *\"): " + err.Error()}
		}
		next, err := reminders.NextFireAt(c, time.Now())
		if err != nil {
			return time.Time{}, "", &ToolResult{Content: "update_reminder: cron produced no next fire time: " + err.Error()}
		}
		return next, c, nil
	}
	switch {
	case strings.TrimSpace(args.FireAtRFC3339) != "":
		t, perr := time.Parse(time.RFC3339, args.FireAtRFC3339)
		if perr != nil {
			return time.Time{}, "", &ToolResult{Content: "update_reminder: fire_at must be RFC3339: " + perr.Error()}
		}
		fireAt := t.UTC()
		if !fireAt.After(time.Now()) {
			return time.Time{}, "", &ToolResult{Content: "update_reminder: fire time is in the past."}
		}
		return fireAt, "", nil
	case args.FireInSeconds > 0:
		// A positive offset from now is future by construction.
		return time.Now().UTC().Add(time.Duration(args.FireInSeconds) * time.Second), "", nil
	default:
		// No schedule field supplied — carry the row's existing fire_at
		// forward WITHOUT re-validating it against now. A recurring row
		// whose next fire_at has momentarily slipped into the past but
		// isn't yet leased must still accept a content/project-only edit;
		// the heartbeat owns firing it. Re-checking here would reject the
		// edit with a misleading "fire time is in the past".
		return row.FireAt, "", nil
	}
}

// resolveUpdateProject resolves a `project` change on an update_reminder
// edit. Empty `project` returns "" (COALESCE keeps the existing project).
// A non-empty project is valid only on a task-kind reminder — a text
// reminder runs nothing, so a project is meaningless there and the edit
// is refused rather than silently stored. When present it MUST clear the
// same session ACL set_reminder enforces at create time
// (resolveProjectAllowed); without this re-auth, editing project would be
// an escalation path around that gate. Returns the resolved project (or
// "") and a non-nil errResult on rejection.
func (te *ToolExecutor) resolveUpdateProject(args updateReminderArgs, row *persistence.Reminder, activeProject string, allowedProjects []string) (string, *ToolResult) {
	p := strings.TrimSpace(args.Project)
	if p == "" {
		return "", nil
	}
	if !row.IsTaskKind() {
		return "", &ToolResult{Content: "update_reminder: `project` only applies to task-kind reminders (a text reminder runs nothing). Omit it, or cancel and recreate as a task-kind reminder."}
	}
	resolved, err := resolveProjectAllowed(p, activeProject, allowedProjects)
	if err != nil {
		return "", &ToolResult{Content: "update_reminder: " + err.Error()}
	}
	return resolved, nil
}

// pauseReminderArgs is the parsed shape of the pause tool's args.
type pauseReminderArgs struct {
	ReminderID string `json:"reminder_id"`
	Rationale  string `json:"rationale"`
}

// resumeReminderArgs is the parsed shape of the resume tool's args.
type resumeReminderArgs struct {
	ReminderID string `json:"reminder_id"`
	Rationale  string `json:"rationale"`
}

func (te *ToolExecutor) pauseReminderTool(ctx context.Context, argsJSON string, chatID int64) ToolResult {
	if te.reminderRepo == nil {
		return ToolResult{Content: "Reminders are not configured on this daemon."}
	}
	var args pauseReminderArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: "pause_reminder: invalid arguments: " + err.Error()}
	}
	if strings.TrimSpace(args.ReminderID) == "" {
		return ToolResult{Content: "pause_reminder: reminder_id is required."}
	}
	row, err := te.reminderRepo.Get(ctx, args.ReminderID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return ToolResult{Content: fmt.Sprintf("pause_reminder: reminder %q not found.", args.ReminderID)}
		}
		return ToolResult{Content: "pause_reminder: lookup failed: " + err.Error()}
	}
	if !reminderBelongsToCaller(row, chatID, ctx) {
		return ToolResult{Content: fmt.Sprintf("pause_reminder: reminder %q is not yours (belongs to a different operator).", args.ReminderID)}
	}
	if err := te.reminderRepo.Pause(ctx, args.ReminderID); err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return ToolResult{Content: fmt.Sprintf("pause_reminder: reminder %q can't be paused — it's either mid-run (a task is executing) or not in a pending state; it'll be pausable again once the current run finishes.", args.ReminderID)}
		}
		return ToolResult{Content: "pause_reminder: " + err.Error()}
	}
	if te.adminAuditRepo != nil {
		afterJSON, _ := json.Marshal(map[string]string{
			"reminder_id": args.ReminderID,
			"rationale":   strings.TrimSpace(args.Rationale),
		})
		_ = te.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: row.OperatorID,
			Source:    "dispatcher",
			Action:    "reminder.paused",
			Target:    args.ReminderID,
			After:     string(afterJSON),
		})
	}
	return ToolResult{Content: fmt.Sprintf("Reminder %s paused. Rationale: %s.", args.ReminderID, strings.TrimSpace(args.Rationale)), Provenance: outputguard.ProvenanceFirstParty}
}

func (te *ToolExecutor) resumeReminderTool(ctx context.Context, argsJSON string, chatID int64) ToolResult {
	if te.reminderRepo == nil {
		return ToolResult{Content: "Reminders are not configured on this daemon."}
	}
	var args resumeReminderArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: "resume_reminder: invalid arguments: " + err.Error()}
	}
	if strings.TrimSpace(args.ReminderID) == "" {
		return ToolResult{Content: "resume_reminder: reminder_id is required."}
	}
	row, err := te.reminderRepo.Get(ctx, args.ReminderID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return ToolResult{Content: fmt.Sprintf("resume_reminder: reminder %q not found.", args.ReminderID)}
		}
		return ToolResult{Content: "resume_reminder: lookup failed: " + err.Error()}
	}
	if !reminderBelongsToCaller(row, chatID, ctx) {
		return ToolResult{Content: fmt.Sprintf("resume_reminder: reminder %q is not yours (belongs to a different operator).", args.ReminderID)}
	}
	if row.CronExpr == "" {
		return ToolResult{Content: fmt.Sprintf("resume_reminder: reminder %q is one-shot and can't be resumed; create a new one.", args.ReminderID)}
	}
	next, err := reminders.NextFireAt(row.CronExpr, time.Now())
	if err != nil {
		return ToolResult{Content: "resume_reminder: " + err.Error()}
	}
	if err := te.reminderRepo.Resume(ctx, args.ReminderID, next); err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return ToolResult{Content: fmt.Sprintf("resume_reminder: reminder %q isn't paused (status may have changed).", args.ReminderID)}
		}
		return ToolResult{Content: "resume_reminder: " + err.Error()}
	}
	if te.adminAuditRepo != nil {
		afterJSON, _ := json.Marshal(map[string]string{
			"reminder_id":      args.ReminderID,
			"next_fire_at_utc": next.Format(time.RFC3339),
			"rationale":        strings.TrimSpace(args.Rationale),
		})
		_ = te.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: row.OperatorID,
			Source:    "dispatcher",
			Action:    "reminder.resumed",
			Target:    args.ReminderID,
			After:     string(afterJSON),
		})
	}
	return ToolResult{Content: fmt.Sprintf("Reminder %s resumed. Next fire: %s. Rationale: %s.",
		args.ReminderID, next.Format(time.RFC1123), strings.TrimSpace(args.Rationale)), Provenance: outputguard.ProvenanceFirstParty}
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
