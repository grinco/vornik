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
}

// IsRecurring reports whether the reminder re-arms on fire. A
// non-empty CronExpr is the single source of truth — operators
// using one-shot semantics never populate the cron column.
func (r *Reminder) IsRecurring() bool {
	return r != nil && r.CronExpr != ""
}

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

	// UpdateFields mutates a pending row's fire_at + content.
	// Refuses non-pending rows (the heartbeat may be mid-fire)
	// — zero rows-affected surfaces as ErrNotFound so the
	// dispatcher tool can tell the operator the reminder
	// already left the gate. content="" leaves content
	// unchanged via COALESCE; passing the empty string is
	// otherwise indistinguishable from "delete the body".
	UpdateFields(ctx context.Context, id string, fireAt time.Time, content string) error

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
}
