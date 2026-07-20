package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ReminderRepository is the SQLite stub for the scheduled-
// reminders ledger. v1 is Postgres-only — the LeaseDue query
// relies on FOR UPDATE SKIP LOCKED + TIMESTAMPTZ semantics that
// don't translate cleanly to SQLite. This stub satisfies the
// interface so the storage abstraction constructs a complete
// *Repositories on the SQLite branch (local dev / tests);
// every method returns ErrSQLiteRemindersUnsupported so the
// reminders heartbeat + set_reminder tool surface a clear
// error rather than silent breakage.
type ReminderRepository struct {
	_ *sql.DB
}

// NewReminderRepository returns the stub. Constructor shape
// matches the postgres impl so storage.Open can switch
// backends transparently.
func NewReminderRepository(db *sql.DB) *ReminderRepository {
	return &ReminderRepository{}
}

// ErrSQLiteRemindersUnsupported is returned by every method.
// Callers (heartbeat + set_reminder tool) nil-check the field
// today, so deployments wiring the SQLite branch get the
// "not configured" UX path. If a future caller forgets the
// nil-check, this error makes the SQLite-incompatibility
// explicit.
var ErrSQLiteRemindersUnsupported = errors.New("dispatcher_reminders: SQLite backend not supported in v1 (use Postgres)")

func (r *ReminderRepository) Insert(context.Context, *persistence.Reminder) error {
	return ErrSQLiteRemindersUnsupported
}

func (r *ReminderRepository) Get(context.Context, string) (*persistence.Reminder, error) {
	return nil, ErrSQLiteRemindersUnsupported
}

func (r *ReminderRepository) List(context.Context, persistence.ReminderListFilter) ([]*persistence.Reminder, error) {
	return nil, ErrSQLiteRemindersUnsupported
}

func (r *ReminderRepository) LeaseDue(context.Context, time.Time, int) ([]*persistence.Reminder, error) {
	return nil, ErrSQLiteRemindersUnsupported
}

func (r *ReminderRepository) MarkFired(context.Context, string) error {
	return ErrSQLiteRemindersUnsupported
}

func (r *ReminderRepository) Reschedule(context.Context, string, time.Time) error {
	return ErrSQLiteRemindersUnsupported
}

func (r *ReminderRepository) MarkErrored(context.Context, string, string) error {
	return ErrSQLiteRemindersUnsupported
}

func (r *ReminderRepository) Cancel(context.Context, string) error {
	return ErrSQLiteRemindersUnsupported
}

func (r *ReminderRepository) Delete(context.Context, string) error {
	return ErrSQLiteRemindersUnsupported
}

// UpdateFields is unsupported on the SQLite stub (Postgres-only feature).
func (r *ReminderRepository) UpdateFields(context.Context, string, persistence.ReminderFieldUpdate) error {
	return ErrSQLiteRemindersUnsupported
}

func (r *ReminderRepository) CountPendingByOperator(context.Context, string) (int, error) {
	return 0, ErrSQLiteRemindersUnsupported
}

// MarkTaskSpawned is unsupported on SQLite; task-kind reminders are
// Postgres-only in v1.
func (r *ReminderRepository) MarkTaskSpawned(context.Context, string, string, *time.Time) error {
	return ErrSQLiteRemindersUnsupported
}

// ClaimDelivery is unsupported on SQLite; task-kind reminders are
// Postgres-only in v1.
func (r *ReminderRepository) ClaimDelivery(context.Context, string) (*persistence.Reminder, bool, error) {
	return nil, false, ErrSQLiteRemindersUnsupported
}

// FinalizeDelivery is unsupported on SQLite; task-kind reminders are
// Postgres-only in v1.
func (r *ReminderRepository) FinalizeDelivery(context.Context, string, string, bool) error {
	return ErrSQLiteRemindersUnsupported
}

// CountTaskByOperator is unsupported on SQLite; task-kind reminders
// are Postgres-only in v1.
func (r *ReminderRepository) CountTaskByOperator(context.Context, string) (int, error) {
	return 0, ErrSQLiteRemindersUnsupported
}

// Pause is unsupported on SQLite; reminders are Postgres-only in v1.
func (r *ReminderRepository) Pause(context.Context, string) error {
	return ErrSQLiteRemindersUnsupported
}

// Resume is unsupported on SQLite; reminders are Postgres-only in v1.
func (r *ReminderRepository) Resume(context.Context, string, time.Time) error {
	return ErrSQLiteRemindersUnsupported
}

// ReclaimStuckFiring is unsupported on SQLite; reminders are
// Postgres-only in v1.
func (r *ReminderRepository) ReclaimStuckFiring(context.Context, time.Time, int) ([]*persistence.Reminder, error) {
	return nil, ErrSQLiteRemindersUnsupported
}
