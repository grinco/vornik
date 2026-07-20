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
	"os"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/reminders"
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

// defaultReminderMaxTaskPerOperator caps concurrent task-kind reminders
// per operator — a mis-fired cron costs an executor slot + model spend,
// so it's tighter than the general pending cap above. Overridable via
// VORNIK_REMINDERS_MAX_TASK_PER_OPERATOR (resolved per call by
// resolveMaxTaskPerOperator).
const defaultReminderMaxTaskPerOperator = 20

// maxTaskPerOperatorEnvVar overrides defaultReminderMaxTaskPerOperator.
// A non-positive or unparseable value falls back to the default rather
// than disabling the cap.
const maxTaskPerOperatorEnvVar = "VORNIK_REMINDERS_MAX_TASK_PER_OPERATOR"

// resolveMaxTaskPerOperator returns the per-operator task-kind reminder
// cap, honoring the env override when it parses to a positive integer.
func resolveMaxTaskPerOperator() int {
	if v := strings.TrimSpace(os.Getenv(maxTaskPerOperatorEnvVar)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultReminderMaxTaskPerOperator
}

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
	// Kind selects "text" (default — deliver Content verbatim) or
	// "task" (run Content as a task prompt in Project and deliver the
	// outcome). See https://docs.vornik.io
	Kind string `json:"kind"`
	// Cron is a 5-field POSIX cron expression. Non-empty means the
	// reminder is recurring (re-arms on every fire) rather than
	// one-shot. Required in practice for kind="task" (a scheduled
	// update is inherently recurring) but validated the same way for
	// either kind — a recurring text reminder is also supported.
	Cron string `json:"cron"`
	// Project is the project a task-kind reminder runs in. Optional —
	// defaults to the session's active project when omitted. Required
	// (from one source or the other) for kind="task".
	Project string `json:"project"`
}

func (te *ToolExecutor) setReminder(ctx context.Context, argsJSON string, chatID int64, activeProject string, allowedProjects []string) ToolResult {
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

	kind := persistence.ReminderKindText
	if strings.EqualFold(strings.TrimSpace(args.Kind), "task") {
		kind = persistence.ReminderKindTask
	}

	operatorID := "telegram:" + strconv.FormatInt(chatID, 10)

	fireAt, cronExpr, errResult := resolveReminderSchedule(args)
	if errResult != nil {
		return *errResult
	}

	project, errResult := te.resolveReminderProject(ctx, kind, args.Project, activeProject, allowedProjects, operatorID)
	if errResult != nil {
		return *errResult
	}

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
		ProjectID:  project,
		FireAt:     fireAt.UTC(),
		Content:    args.Content,
		CreatedVia: "chat",
		Kind:       kind,
		CronExpr:   cronExpr,
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
	te.auditReminderSet(ctx, rem, operatorID)

	if kind == persistence.ReminderKindTask {
		return ToolResult{
			Content:    fmt.Sprintf("Scheduled update %s set — runs %s in project %s. First run %s.", rem.ID, cronExpr, project, fireAt.Format(time.RFC1123)),
			Provenance: outputguard.ProvenanceFirstParty,
		}
	}
	return ToolResult{
		Content:    fmt.Sprintf("Reminder %s set for %s. The bot will message you here when it fires.", rem.ID, fireAt.Format(time.RFC1123)),
		Provenance: outputguard.ProvenanceFirstParty,
	}
}

// resolveReminderSchedule resolves the fire schedule for a
// set_reminder call: a non-empty cron always wins (recurring —
// task-kind updates are inherently recurring, but a recurring
// text reminder is allowed too). Otherwise falls back to the
// one-shot fire_at / fire_in_seconds modes, with the past/future
// window guards. Returns a non-nil errResult (ready to hand
// straight back to the caller) on any validation failure. Extracted
// out of setReminder to keep that function's size within lint
// bounds.
func resolveReminderSchedule(args setReminderArgs) (time.Time, string, *ToolResult) {
	if strings.TrimSpace(args.Cron) != "" {
		cronExpr := strings.TrimSpace(args.Cron)
		if err := reminders.ValidateCronExpr(cronExpr); err != nil {
			return time.Time{}, "", &ToolResult{Content: "set_reminder: invalid `cron` expression (need 5-field POSIX, e.g. \"0 7 * * *\"): " + err.Error()}
		}
		next, err := reminders.NextFireAt(cronExpr, time.Now())
		if err != nil {
			return time.Time{}, "", &ToolResult{Content: "set_reminder: cron produced no next fire time: " + err.Error()}
		}
		return next, cronExpr, nil
	}
	fireAt, err := resolveReminderFireAt(args, time.Now())
	if err != nil {
		return time.Time{}, "", &ToolResult{Content: "set_reminder: " + err.Error()}
	}
	if fireAt.Before(time.Now().Add(-1 * time.Minute)) {
		return time.Time{}, "", &ToolResult{Content: "set_reminder: fire time is in the past."}
	}
	if fireAt.After(time.Now().Add(reminderMaxFutureWindow)) {
		return time.Time{}, "", &ToolResult{Content: fmt.Sprintf("set_reminder: fire time is more than %s in the future (cap).", reminderMaxFutureWindow)}
	}
	return fireAt, "", nil
}

// auditReminderSet writes a reminder.set admin-audit row for a
// successful Insert so operators have a full set→fire→cancel trail
// in /ui/admin/audit. Reuses the same channel + ref captured on the
// row so the audit row alone answers "who set what reminder for
// when, on which channel". No-op when adminAuditRepo isn't wired.
// Extracted out of setReminder to keep that function's size within
// lint bounds.
func (te *ToolExecutor) auditReminderSet(ctx context.Context, rem *persistence.Reminder, operatorID string) {
	if te.adminAuditRepo == nil {
		return
	}
	afterJSON, _ := json.Marshal(map[string]any{
		"reminder_id": rem.ID,
		"channel":     rem.Channel,
		"channel_ref": rem.ChannelRef,
		"project_id":  rem.ProjectID,
		"fire_at":     rem.FireAt.UTC().Format(time.RFC3339),
		"content_len": len(rem.Content),
		"created_via": rem.CreatedVia,
		"kind":        string(rem.Kind),
		"cron_expr":   rem.CronExpr,
	})
	_ = te.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: operatorID,
		Source:    "dispatcher",
		Action:    "reminder.set",
		Target:    rem.ID,
		After:     string(afterJSON),
	})
}

// resolveReminderProject resolves the project a set_reminder call
// operates in. A task-kind reminder is a live task-scheduling
// primitive (it runs a prompt in a project on a cron), so — unlike a
// text reminder — it must clear the same session ACL every sibling
// task tool enforces (create_task, list_tasks, ...); without this
// gate a session could schedule work in a project it has no access
// to. A text-kind reminder doesn't run anything, so it keeps the
// pre-existing, ungated active-project fallback. Extracted out of
// setReminder to keep that function's size within lint bounds.
func (te *ToolExecutor) resolveReminderProject(ctx context.Context, kind persistence.ReminderKind, explicitProject, activeProject string, allowedProjects []string, operatorID string) (string, *ToolResult) {
	project := strings.TrimSpace(explicitProject)
	if kind == persistence.ReminderKindTask {
		return te.resolveTaskReminderProject(ctx, project, activeProject, allowedProjects, operatorID)
	}
	if project == "" {
		project = activeProject
	}
	return project, nil
}

// resolveTaskReminderProject resolves and ACL-checks the project a
// task-kind set_reminder call will run in, and enforces the
// per-operator task cap. Returns a non-nil errResult (ready to hand
// straight back to the caller) when the request must be refused —
// no project could be determined, the resolved project is outside
// the session's allowedProjects, or the task cap is already hit.
// Extracted out of setReminder to keep that function's cognitive
// complexity within lint bounds; the friendly "need a project" case
// is checked first so a bare empty-project request gets a
// task-shaped message rather than resolveProjectAllowed's more
// generic wording.
func (te *ToolExecutor) resolveTaskReminderProject(ctx context.Context, project, activeProject string, allowedProjects []string, operatorID string) (string, *ToolResult) {
	if project == "" && activeProject == "" {
		return "", &ToolResult{Content: "set_reminder: a task-kind scheduled update needs a `project` to run in (none given and no active project). Tell me which project, e.g. \"in the news project\"."}
	}
	resolved, err := resolveProjectAllowed(project, activeProject, allowedProjects)
	if err != nil {
		return "", &ToolResult{Content: "set_reminder: " + err.Error()}
	}
	taskCount, err := te.reminderRepo.CountTaskByOperator(ctx, operatorID)
	if err != nil {
		return "", &ToolResult{Content: "set_reminder: failed to check task cap: " + err.Error()}
	}
	if maxTask := resolveMaxTaskPerOperator(); taskCount >= maxTask {
		return "", &ToolResult{
			Content: fmt.Sprintf("set_reminder: you already have %d scheduled task-updates (cap=%d). Cancel one first.", taskCount, maxTask),
		}
	}
	return resolved, nil
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
