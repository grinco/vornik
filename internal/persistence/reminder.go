package persistence

import (
	"context"
	"time"
)

// ReminderStatus enumerates the dispatcher_reminders.status values.
// See migration 55 for the schema-side CHECK constraint pinning the
// same set.
type ReminderStatus string

const (
	// ReminderStatusPending — not yet due, waiting on the clock.
	ReminderStatusPending ReminderStatus = "pending"
	// ReminderStatusFiring — leased by a heartbeat tick. The
	// intermediate row state between "claimed for delivery" and
	// "delivery confirmed by the channel". Lets ops debugging
	// distinguish "stuck in DB" from "stuck mid-send".
	ReminderStatusFiring ReminderStatus = "firing"
	// ReminderStatusFired — channel.Send returned without error.
	// Terminal.
	ReminderStatusFired ReminderStatus = "fired"
	// ReminderStatusCancelled — operator-cancelled before fire.
	// Terminal.
	ReminderStatusCancelled ReminderStatus = "cancelled"
	// ReminderStatusExpired — past fire_at but channel
	// unavailable and v1's no-retry policy gave up. Terminal.
	ReminderStatusExpired ReminderStatus = "expired"
	// ReminderStatusAwaitingTask — a task-kind fire created a task and
	// is waiting for it to complete. Non-terminal and NOT leasable
	// (LeaseDue filters status='pending'), which is what structurally
	// enforces the skip-if-still-running policy. See
	// https://docs.vornik.io §2.3.
	ReminderStatusAwaitingTask ReminderStatus = "awaiting_task"
	// ReminderStatusPaused — operator-muted; reachable only from
	// 'pending'. Not leasable. resume recomputes fire_at.
	ReminderStatusPaused ReminderStatus = "paused"
)

// IsTerminal reports whether the status is end-of-life. The
// heartbeat skips terminal rows; the cancel surface refuses them.
func (s ReminderStatus) IsTerminal() bool {
	switch s {
	case ReminderStatusFired, ReminderStatusCancelled, ReminderStatusExpired:
		return true
	default:
		return false
	}
}

// ReminderKind discriminates a static-text reminder from a task-kind
// reminder that spawns a task and delivers its outcome.
type ReminderKind string

const (
	// ReminderKindText delivers Content verbatim (the shipped behavior).
	ReminderKindText ReminderKind = "text"
	// ReminderKindTask runs a task from Content (the prompt) in
	// ProjectID and delivers its outcome.
	ReminderKindTask ReminderKind = "task"
)

// Reminder is one dispatcher_reminders row. Mirrors the schema
// shape one-for-one; the API/UI/CLI layers project subsets via
// their own DTOs.
type Reminder struct {
	ID          string
	OperatorID  string
	Channel     string
	ChannelRef  string
	ProjectID   string // optional
	FireAt      time.Time
	Content     string
	Status      ReminderStatus
	CreatedAt   time.Time
	FiredAt     *time.Time
	CancelledAt *time.Time
	CreatedVia  string // "chat" / "cli" / "ui" / "api"
	ErrorCount  int
	LastError   string
	// CronExpr is a 5-field POSIX cron expression. Non-empty means
	// the runner re-arms FireAt on every successful delivery
	// instead of marking the row terminal. One-shot rows leave
	// this empty. See migration 67.
	CronExpr string
	// RecurrenceUntil bounds a recurring reminder — once the next
	// computed fire_at exceeds this time the runner marks the row
	// 'fired' terminally. nil = unbounded.
	RecurrenceUntil *time.Time
	// Kind is "text" (default) or "task". See migration 130.
	Kind ReminderKind
	// LastTaskID is the most recent spawned task (task-kind only). Set
	// at spawn; OVERWRITTEN by the next fire, never cleared — points to
	// the in-flight task while one runs, else the last-completed task.
	LastTaskID string
	// LastDeliveredTaskID is the task whose outcome was already
	// delivered. The (LastTaskID != LastDeliveredTaskID) pair is the
	// at-most-once delivery guard.
	LastDeliveredTaskID string
}

// IsRecurring reports whether the reminder re-arms on fire. A
// non-empty CronExpr is the single source of truth — operators
// using one-shot semantics never populate the cron column.
func (r *Reminder) IsRecurring() bool {
	return r != nil && r.CronExpr != ""
}

// IsTaskKind reports whether firing this reminder spawns a task rather
// than sending Content verbatim.
func (r *Reminder) IsTaskKind() bool { return r != nil && r.Kind == ReminderKindTask }

// ReminderListFilter drives ReminderRepository.List. Zero-value
// fields are "any"; PageSize defaults to 50 at the impl, capped
// at 500 so a buggy admin client can't drain the table.
type ReminderListFilter struct {
	OperatorID string
	ProjectID  string
	Status     ReminderStatus
	// FireBefore restricts to rows whose fire_at < this time.
	// Zero-value = unbounded. Drives the "upcoming reminders"
	// project-tile query.
	FireBefore time.Time
	// FireAfter mirrors FireBefore.
	FireAfter time.Time
	PageSize  int
}

// ReminderFieldUpdate carries the mutable fields of an update_reminder
// edit. It is applied only to a pending row (UpdateFields refuses others).
// FireAt is always written — the dispatcher tool resolves it up front
// (from an explicit timestamp, an offset, a cron-recomputed next-fire, or
// the row's current value). The three string fields follow "empty ==
// leave unchanged" semantics (COALESCE), matching the pre-existing content
// behavior; there is deliberately no way to CLEAR cron_expr or project_id
// through an edit (converting a recurring reminder back to one-shot, or
// unsetting a task-kind project, is a cancel-and-recreate operation).
type ReminderFieldUpdate struct {
	FireAt    time.Time
	Content   string // "" leaves the existing body unchanged
	CronExpr  string // "" leaves the existing schedule unchanged
	ProjectID string // "" leaves the existing project unchanged
}

// ReminderRepository persists dispatcher_reminders rows. The
// heartbeat poller calls LeaseDue at every tick; the dispatcher
// tool, CLI, and UI call Insert/List/Cancel/Get.
type ReminderRepository interface {
	// Insert stores a new pending reminder. ID is generated when
	// empty. Status forced to pending on insert (callers don't
	// pre-stamp terminal states).
	Insert(ctx context.Context, r *Reminder) error

	// Get returns one row by id. ErrNotFound when missing.
	Get(ctx context.Context, id string) (*Reminder, error)

	// List returns rows matching the filter, newest first by
	// fire_at. Drives the operator-facing surfaces.
	List(ctx context.Context, filter ReminderListFilter) ([]*Reminder, error)

	// LeaseDue atomically transitions pending rows whose fire_at
	// <= now to status='firing' and returns them. Uses
	// FOR UPDATE SKIP LOCKED so a multi-instance deployment
	// (2026.8.0) doesn't double-fire. limit caps the batch so a
	// backlog can't lock the table.
	LeaseDue(ctx context.Context, now time.Time, limit int) ([]*Reminder, error)

	// MarkFired completes a firing row: status=fired,
	// fired_at=NOW(). Returns ErrNotFound if the row doesn't
	// exist OR is no longer in 'firing' state — defensive against
	// a double-fire race.
	MarkFired(ctx context.Context, id string) error

	// Reschedule re-arms a recurring reminder after a successful
	// fire. Transitions status from 'firing' back to 'pending',
	// stamps the new fire_at, and bumps the fired_at audit
	// timestamp for the just-completed cycle. Returns
	// ErrNotFound when the row isn't in 'firing' state — same
	// defensive shape as MarkFired so a double-fire race surfaces
	// loudly rather than silently corrupting the schedule.
	Reschedule(ctx context.Context, id string, nextFireAt time.Time) error

	// MarkErrored stamps last_error + increments error_count.
	// Status stays at 'firing' so v1 can re-lease (when a retry
	// policy lands). For v1 the row is effectively stuck in
	// 'firing' until an operator cancels.
	MarkErrored(ctx context.Context, id, errorMessage string) error

	// UpdateFields mutates a pending row's fire_at + content +
	// cron_expr + project_id (see ReminderFieldUpdate). Refuses
	// non-pending rows (the heartbeat may be mid-fire) — zero
	// rows-affected surfaces as ErrNotFound so the dispatcher tool
	// can tell the operator the reminder already left the gate.
	// Each string field's empty value means "leave the column
	// unchanged" (COALESCE); there is deliberately no way to CLEAR a
	// column through this call (see ReminderFieldUpdate).
	UpdateFields(ctx context.Context, id string, upd ReminderFieldUpdate) error

	// Cancel transitions a non-terminal row to status=cancelled.
	// Idempotent — already-terminal rows return nil with no
	// state change.
	Cancel(ctx context.Context, id string) error

	// Delete physically removes the row. Distinct from Cancel
	// (which keeps the row for audit, just flips status). Use
	// when an operator wants to clean up stale rows that survived
	// a project deletion or a recurring-reminder gone awry — the
	// row's audit-history value is gone, but so is the visual
	// noise in `vornikctl reminders list`. ErrNotFound when the
	// id doesn't exist; idempotent within a session — a second
	// delete of the same id returns ErrNotFound.
	Delete(ctx context.Context, id string) error

	// CountPendingByOperator returns the number of pending
	// reminders for one operator. Drives the per-operator cap
	// the set_reminder tool enforces.
	CountPendingByOperator(ctx context.Context, operatorID string) (int, error)

	// MarkTaskSpawned transitions a firing task-kind row to
	// awaiting_task, stamps last_task_id, and — when nextFireAt is
	// non-nil (recurring) — arms fire_at to the next slot. Called
	// AFTER the task is created so a crash before this leaves the row
	// firing (recoverable, no lost fire). ErrNotFound if not firing.
	MarkTaskSpawned(ctx context.Context, id, taskID string, nextFireAt *time.Time) error

	// ClaimDelivery atomically claims delivery of a completed task's
	// outcome: awaiting_task -> firing for the row whose last_task_id
	// = taskID and last_delivered_task_id != taskID. Returns
	// (row,true,nil) to the single winner; (nil,false,nil) for a
	// duplicate/HA-racing callback. The at-most-once guard.
	ClaimDelivery(ctx context.Context, taskID string) (*Reminder, bool, error)

	// FinalizeDelivery completes a claimed delivery: stamps
	// last_delivered_task_id and moves the row off firing —
	// firing->fired when terminal (one-shot or past recurrence bound),
	// else firing->pending (recurring; fire_at already armed at spawn).
	// ErrNotFound if not firing.
	FinalizeDelivery(ctx context.Context, id, taskID string, terminal bool) error

	// CountTaskByOperator counts non-terminal task-kind reminders for
	// the per-operator task cap.
	CountTaskByOperator(ctx context.Context, operatorID string) (int, error)

	// Pause mutes a pending reminder: pending -> paused. Refuses any
	// non-pending row (firing/awaiting_task/etc.) so an in-flight
	// reminder can't be paused mid-run — ErrNotFound surfaces that
	// to the operator. Design §5.4.
	Pause(ctx context.Context, id string) error

	// Resume re-arms a paused reminder: paused -> pending, and stamps
	// fire_at = nextFireAt. Refuses non-paused rows — ErrNotFound.
	Resume(ctx context.Context, id string, nextFireAt time.Time) error

	// ReclaimStuckFiring returns rows stuck in 'firing' whose fired_at
	// (stamped when LeaseDue/ClaimDelivery flipped the row to firing) is
	// older than olderThan. Covers the three crash-recovery gaps design
	// §9 names: (a) a crash between LeaseDue and MarkTaskSpawned
	// (task-kind spawn interrupted), (b) a crash between Send/claim and
	// FinalizeDelivery (delivery interrupted), and (c) the pre-existing
	// text-kind case where a failed Send leaves the row firing forever
	// (MarkErrored deliberately doesn't change status — see its doc
	// comment). Uses FOR UPDATE SKIP LOCKED so concurrent sweepers
	// (future HA, per LeaseDue's precedent) claim disjoint batches, but
	// performs NO mutation itself — the caller (Runner.sweepStuckFiring)
	// decides per row whether to re-arm (recurring) or mark errored
	// (one-shot). Never returns 'awaiting_task' rows — that's the
	// long-running-task state, not stuck.
	ReclaimStuckFiring(ctx context.Context, olderThan time.Time, limit int) ([]*Reminder, error)
}
