package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ReminderRepository implements persistence.ReminderRepository
// against PostgreSQL. Heartbeat-facing methods (LeaseDue) use
// FOR UPDATE SKIP LOCKED so the table is HA-safe out of the box
// — when 2026.8.0 lights up multi-instance deployments two
// daemons can run this query concurrently and each gets its own
// disjoint batch.
type ReminderRepository struct {
	db DBTX
}

// NewReminderRepository constructs a repo over db. Pass a *sql.DB
// so LeaseDue can begin its own transaction.
func NewReminderRepository(db DBTX) *ReminderRepository {
	return &ReminderRepository{db: db}
}

const reminderColumns = `id, operator_id, channel, channel_ref, project_id, fire_at, content,
    status, created_at, fired_at, cancelled_at, created_via, error_count, last_error,
    cron_expr, recurrence_until`

// Insert writes a new pending row. ID generated when empty.
func (r *ReminderRepository) Insert(ctx context.Context, rem *persistence.Reminder) error {
	if rem == nil {
		return fmt.Errorf("reminder_repository: nil reminder")
	}
	if rem.ID == "" {
		rem.ID = persistence.GenerateID("rem")
	}
	if rem.CreatedAt.IsZero() {
		rem.CreatedAt = time.Now().UTC()
	}
	if rem.CreatedVia == "" {
		rem.CreatedVia = "chat"
	}
	rem.Status = persistence.ReminderStatusPending
	projectID := emptyToNullString(rem.ProjectID)
	cronExpr := emptyToNullString(rem.CronExpr)
	recurrenceUntil := nullableTime(rem.RecurrenceUntil)

	_, err := r.db.ExecContext(ctx, `
INSERT INTO dispatcher_reminders (
    id, operator_id, channel, channel_ref, project_id,
    fire_at, content, status, created_at, created_via,
    cron_expr, recurrence_until
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
`,
		rem.ID, rem.OperatorID, rem.Channel, rem.ChannelRef, projectID,
		rem.FireAt.UTC(), rem.Content, string(rem.Status), rem.CreatedAt.UTC(), rem.CreatedVia,
		cronExpr, recurrenceUntil,
	)
	if err != nil {
		return fmt.Errorf("reminder_repository: insert: %w", err)
	}
	return nil
}

// Get returns one row by id. ErrNotFound when missing.
func (r *ReminderRepository) Get(ctx context.Context, id string) (*persistence.Reminder, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+reminderColumns+` FROM dispatcher_reminders WHERE id = $1`, id)
	rem, err := scanReminder(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("reminder_repository: get: %w", err)
	}
	return rem, nil
}

// List queries newest-first by fire_at with the given filters.
func (r *ReminderRepository) List(ctx context.Context, filter persistence.ReminderListFilter) ([]*persistence.Reminder, error) {
	if filter.PageSize <= 0 {
		filter.PageSize = 50
	}
	if filter.PageSize > 500 {
		filter.PageSize = 500
	}

	var (
		conditions []string
		args       []interface{}
	)
	add := func(cond string, val interface{}) {
		args = append(args, val)
		conditions = append(conditions, fmt.Sprintf(cond, len(args)))
	}
	if filter.OperatorID != "" {
		add("operator_id = $%d", filter.OperatorID)
	}
	if filter.ProjectID != "" {
		add("project_id = $%d", filter.ProjectID)
	}
	if filter.Status != "" {
		add("status = $%d", string(filter.Status))
	}
	if !filter.FireBefore.IsZero() {
		add("fire_at < $%d", filter.FireBefore.UTC())
	}
	if !filter.FireAfter.IsZero() {
		add("fire_at > $%d", filter.FireAfter.UTC())
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	args = append(args, filter.PageSize)
	q := `SELECT ` + reminderColumns + ` FROM dispatcher_reminders` + where +
		` ORDER BY fire_at ASC LIMIT $` + fmt.Sprint(len(args))

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("reminder_repository: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.Reminder
	for rows.Next() {
		rem, err := scanReminder(rows)
		if err != nil {
			return nil, fmt.Errorf("reminder_repository: list scan: %w", err)
		}
		out = append(out, rem)
	}
	return out, rows.Err()
}

// LeaseDue atomically claims pending rows whose fire_at <= now,
// transitions them to 'firing', and returns the claimed batch.
// FOR UPDATE SKIP LOCKED so concurrent pollers (HA) get disjoint
// batches.
func (r *ReminderRepository) LeaseDue(ctx context.Context, now time.Time, limit int) ([]*persistence.Reminder, error) {
	if limit <= 0 {
		limit = 100
	}
	// One CTE-driven UPDATE...RETURNING does the work in a single
	// round-trip and is its own implicit transaction — no Begin
	// needed on the caller's side.
	q := `
WITH due AS (
    SELECT id FROM dispatcher_reminders
    WHERE status = 'pending' AND fire_at <= $1
    ORDER BY fire_at ASC
    LIMIT $2
    FOR UPDATE SKIP LOCKED
)
UPDATE dispatcher_reminders
SET status = 'firing', fired_at = NOW()
WHERE id IN (SELECT id FROM due)
RETURNING ` + reminderColumns
	rows, err := r.db.QueryContext(ctx, q, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("reminder_repository: lease_due: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.Reminder
	for rows.Next() {
		rem, err := scanReminder(rows)
		if err != nil {
			return nil, fmt.Errorf("reminder_repository: lease_due scan: %w", err)
		}
		out = append(out, rem)
	}
	return out, rows.Err()
}

// MarkFired confirms successful delivery. Refuses to flip
// non-firing rows so a double-fire race surfaces as
// ErrNotFound rather than a silent success.
func (r *ReminderRepository) MarkFired(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE dispatcher_reminders
SET status = 'fired', fired_at = NOW()
WHERE id = $1 AND status = 'firing'
`, id)
	if err != nil {
		return fmt.Errorf("reminder_repository: mark_fired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// Reschedule re-arms a recurring reminder: status flips back
// to 'pending' with a fresh fire_at. The just-completed cycle's
// fired_at is preserved so operators can audit when the last
// fire happened. Refuses non-firing rows so a race against a
// concurrent Cancel surfaces as ErrNotFound.
func (r *ReminderRepository) Reschedule(ctx context.Context, id string, nextFireAt time.Time) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE dispatcher_reminders
SET status = 'pending', fire_at = $2
WHERE id = $1 AND status = 'firing'
`, id, nextFireAt.UTC())
	if err != nil {
		return fmt.Errorf("reminder_repository: reschedule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// MarkErrored stamps last_error + increments error_count. Status
// stays at 'firing' — v1's no-retry policy means the row sits
// there until an operator cancels.
func (r *ReminderRepository) MarkErrored(ctx context.Context, id, errorMessage string) error {
	_, err := r.db.ExecContext(ctx, `
UPDATE dispatcher_reminders
SET last_error = $2, error_count = error_count + 1
WHERE id = $1
`, id, errorMessage)
	if err != nil {
		return fmt.Errorf("reminder_repository: mark_errored: %w", err)
	}
	return nil
}

// UpdateFields mutates a PENDING row's fire_at + content.
// Refuses non-pending rows so the dispatcher tool can tell
// the operator "your reminder is already firing". content==""
// preserves the existing body via the SQL COALESCE.
func (r *ReminderRepository) UpdateFields(ctx context.Context, id string, fireAt time.Time, content string) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE dispatcher_reminders
SET fire_at = $2,
    content = COALESCE(NULLIF($3, ''), content)
WHERE id = $1 AND status = 'pending'
`, id, fireAt.UTC(), content)
	if err != nil {
		return fmt.Errorf("reminder_repository: update_fields: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Drivers that don't report affected-rows shouldn't
		// misclassify the update as a no-op; assume success
		// and let the caller's next Get find any drift.
		return nil
	}
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// Cancel transitions any non-terminal row to cancelled.
// Idempotent on already-terminal rows.
func (r *ReminderRepository) Cancel(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
UPDATE dispatcher_reminders
SET status = 'cancelled', cancelled_at = NOW()
WHERE id = $1 AND status NOT IN ('fired','cancelled','expired')
`, id)
	if err != nil {
		return fmt.Errorf("reminder_repository: cancel: %w", err)
	}
	return nil
}

// Delete physically removes a reminder row. Returns ErrNotFound
// when no row matches the id (the operator's manual cleanup tool
// should know the row was already gone). Distinct from Cancel
// which preserves the row for audit; Delete is for stale-row
// hygiene (project deleted under it, recurring rule gone awry,
// test row that lingered).
func (r *ReminderRepository) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM dispatcher_reminders WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("reminder_repository: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Driver doesn't report — treat as success (the most common
		// case where this branch fires is sqlite-in-tests, and
		// failing the delete on a non-broken driver-quirk would be
		// worse than the rare false-success on a row that the
		// caller is about to re-fetch and notice missing).
		return nil
	}
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// CountPendingByOperator returns the per-operator pending count.
// Backs the set_reminder cap enforcement.
func (r *ReminderRepository) CountPendingByOperator(ctx context.Context, operatorID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM dispatcher_reminders
WHERE operator_id = $1 AND status = 'pending'
`, operatorID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("reminder_repository: count_pending: %w", err)
	}
	return n, nil
}

// scanReminder reads one row from either *sql.Row or *sql.Rows.
// Mirrors the column order in reminderColumns.
func scanReminder(scanner interface {
	Scan(dest ...interface{}) error
}) (*persistence.Reminder, error) {
	var (
		rem             persistence.Reminder
		projectID       sql.NullString
		firedAt         sql.NullTime
		cancelledAt     sql.NullTime
		lastError       sql.NullString
		status          string
		cronExpr        sql.NullString
		recurrenceUntil sql.NullTime
	)
	if err := scanner.Scan(
		&rem.ID, &rem.OperatorID, &rem.Channel, &rem.ChannelRef, &projectID,
		&rem.FireAt, &rem.Content, &status, &rem.CreatedAt, &firedAt, &cancelledAt,
		&rem.CreatedVia, &rem.ErrorCount, &lastError,
		&cronExpr, &recurrenceUntil,
	); err != nil {
		return nil, err
	}
	rem.Status = persistence.ReminderStatus(status)
	if projectID.Valid {
		rem.ProjectID = projectID.String
	}
	if firedAt.Valid {
		t := firedAt.Time
		rem.FiredAt = &t
	}
	if cancelledAt.Valid {
		t := cancelledAt.Time
		rem.CancelledAt = &t
	}
	if lastError.Valid {
		rem.LastError = lastError.String
	}
	if cronExpr.Valid {
		rem.CronExpr = cronExpr.String
	}
	if recurrenceUntil.Valid {
		t := recurrenceUntil.Time
		rem.RecurrenceUntil = &t
	}
	return &rem, nil
}

// emptyToNullString returns sql.NullString{Valid: false} for an
// empty input, otherwise wraps the value. Distinct name from the
// existing nullableString helper (which takes *string) so the
// two coexist without import drama.
func emptyToNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{Valid: true, String: s}
}
