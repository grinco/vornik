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
    cron_expr, recurrence_until, kind, last_task_id, last_delivered_task_id`

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
	kind := rem.Kind
	if kind == "" {
		kind = persistence.ReminderKindText
	}

	_, err := r.db.ExecContext(ctx, `
INSERT INTO dispatcher_reminders (
    id, operator_id, channel, channel_ref, project_id,
    fire_at, content, status, created_at, created_via,
    cron_expr, recurrence_until, kind, last_task_id, last_delivered_task_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
`,
		rem.ID, rem.OperatorID, rem.Channel, rem.ChannelRef, projectID,
		rem.FireAt.UTC(), rem.Content, string(rem.Status), rem.CreatedAt.UTC(), rem.CreatedVia,
		cronExpr, recurrenceUntil, string(kind),
		emptyToNullString(rem.LastTaskID), emptyToNullString(rem.LastDeliveredTaskID),
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

// UpdateFields mutates a PENDING row's fire_at + content + cron_expr +
// project_id. Refuses non-pending rows so the dispatcher tool can tell
// the operator "your reminder is already firing". Empty-string fields
// (content / cron_expr / project_id) preserve the existing column via
// the SQL COALESCE — so a fire_at-only edit leaves the rest intact.
func (r *ReminderRepository) UpdateFields(ctx context.Context, id string, upd persistence.ReminderFieldUpdate) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE dispatcher_reminders
SET fire_at = $2,
    content = COALESCE(NULLIF($3, ''), content),
    cron_expr = COALESCE(NULLIF($4, ''), cron_expr),
    project_id = COALESCE(NULLIF($5, ''), project_id)
WHERE id = $1 AND status = 'pending'
`, id, upd.FireAt.UTC(), upd.Content, upd.CronExpr, upd.ProjectID)
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

// CountTaskByOperator returns the number of NON-terminal task-kind
// reminders for one operator (pending, firing, awaiting_task, paused).
// Backs the per-operator task cap (design §6). Uses
// idx_dispatcher_reminders_operator_kind_status.
func (r *ReminderRepository) CountTaskByOperator(ctx context.Context, operatorID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM dispatcher_reminders
WHERE operator_id = $1 AND kind = 'task'
  AND status IN ('pending','firing','awaiting_task','paused')
`, operatorID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("reminder_repository: count_task: %w", err)
	}
	return n, nil
}

// Pause mutes a pending reminder. Refuses non-pending rows so an
// in-flight (firing/awaiting_task) reminder can't be paused mid-run —
// ErrNotFound surfaces that to the operator. Design §5.4.
func (r *ReminderRepository) Pause(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE dispatcher_reminders SET status = 'paused'
WHERE id = $1 AND status = 'pending'`, id)
	if err != nil {
		return fmt.Errorf("reminder_repository: pause: %w", err)
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

// Resume re-arms a paused reminder with a fresh fire_at. Refuses
// non-paused rows — ErrNotFound.
func (r *ReminderRepository) Resume(ctx context.Context, id string, nextFireAt time.Time) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE dispatcher_reminders SET status = 'pending', fire_at = $2
WHERE id = $1 AND status = 'paused'`, id, nextFireAt.UTC())
	if err != nil {
		return fmt.Errorf("reminder_repository: resume: %w", err)
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

// MarkTaskSpawned transitions a firing task-kind row to
// awaiting_task, stamps last_task_id, and — when nextFireAt is
// non-nil (recurring) — arms fire_at to the next slot in the same
// statement. Guarded on status='firing' so a crash between task
// creation and this call leaves the row firing (recoverable by the
// stuck-firing sweep) rather than silently corrupting state.
func (r *ReminderRepository) MarkTaskSpawned(ctx context.Context, id, taskID string, nextFireAt *time.Time) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE dispatcher_reminders
SET status = 'awaiting_task',
    last_task_id = $2,
    fire_at = COALESCE($3, fire_at)
WHERE id = $1 AND status = 'firing'
`, id, taskID, nullableTime(nextFireAt))
	if err != nil {
		return fmt.Errorf("reminder_repository: mark_task_spawned: %w", err)
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

// ClaimDelivery atomically claims delivery of a completed task's
// outcome. The conditional UPDATE...WHERE is the at-most-once
// serialization point: only the row whose last_task_id matches AND
// whose last_delivered_task_id hasn't already been stamped with this
// taskID transitions awaiting_task -> firing. A duplicate/HA-racing
// completion callback for the same task finds zero matching rows
// (RETURNING yields no rows) and gets (nil,false,nil) — never an
// error, since "lost the race" is an expected outcome, not a fault.
func (r *ReminderRepository) ClaimDelivery(ctx context.Context, taskID string) (*persistence.Reminder, bool, error) {
	// fired_at is refreshed to NOW() here (not just at the initial
	// firing/spawn) so a long-running task delivery gets a fresh
	// FiringGrace window. Without this, a delivery that outlives
	// FiringGrace (routine for a slow digest task) re-enters
	// 'firing' with a stale fired_at, and a concurrent
	// sweepStuckFiring tick can falsely reclaim the row mid-delivery
	// (spurious Reschedule/MarkErrored + false firing_reclaimed_total).
	row := r.db.QueryRowContext(ctx, `
UPDATE dispatcher_reminders
SET status = 'firing', fired_at = NOW()
WHERE last_task_id = $1
  AND status = 'awaiting_task'
  AND (last_delivered_task_id IS DISTINCT FROM $1)
RETURNING `+reminderColumns, taskID)
	rem, err := scanReminder(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil // nothing to claim (dup / already delivered)
		}
		return nil, false, fmt.Errorf("reminder_repository: claim_delivery: %w", err)
	}
	return rem, true, nil
}

// FinalizeDelivery completes a claimed delivery: stamps
// last_delivered_task_id and moves the row off firing — fired when
// terminal (one-shot, or a recurring row past its RecurrenceUntil
// bound), pending when not (recurring; fire_at was already armed at
// MarkTaskSpawned time). Guarded on status='firing' so a duplicate
// finalize call (e.g. a retried webhook) surfaces as ErrNotFound
// rather than silently double-stamping fired_at.
func (r *ReminderRepository) FinalizeDelivery(ctx context.Context, id, taskID string, terminal bool) error {
	newStatus := "pending"
	if terminal {
		newStatus = "fired"
	}
	res, err := r.db.ExecContext(ctx, `
UPDATE dispatcher_reminders
SET status = $2, last_delivered_task_id = $3, fired_at = NOW()
WHERE id = $1 AND status = 'firing'
`, id, newStatus, taskID)
	if err != nil {
		return fmt.Errorf("reminder_repository: finalize_delivery: %w", err)
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

// ReclaimStuckFiring returns 'firing' rows whose fired_at < olderThan —
// the crash-recovery sweep's read side (design §9). FOR UPDATE SKIP
// LOCKED mirrors LeaseDue's HA-safety precedent so concurrent sweepers
// get disjoint batches, but this query does NOT mutate the rows; the
// runner decides per row whether to re-arm or mark errored (a plain
// SELECT keeps this method's contract identical for both the task-kind
// and text-kind stuck cases, rather than baking one outcome in here).
func (r *ReminderRepository) ReclaimStuckFiring(ctx context.Context, olderThan time.Time, limit int) ([]*persistence.Reminder, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `
SELECT ` + reminderColumns + `
FROM dispatcher_reminders
WHERE status = 'firing' AND fired_at < $1
ORDER BY fired_at ASC
LIMIT $2
FOR UPDATE SKIP LOCKED
`
	rows, err := r.db.QueryContext(ctx, q, olderThan.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("reminder_repository: reclaim_stuck_firing: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.Reminder
	for rows.Next() {
		rem, err := scanReminder(rows)
		if err != nil {
			return nil, fmt.Errorf("reminder_repository: reclaim_stuck_firing scan: %w", err)
		}
		out = append(out, rem)
	}
	return out, rows.Err()
}

// scanReminder reads one row from either *sql.Row or *sql.Rows.
// Mirrors the column order in reminderColumns.
func scanReminder(scanner interface {
	Scan(dest ...interface{}) error
}) (*persistence.Reminder, error) {
	var (
		rem                 persistence.Reminder
		projectID           sql.NullString
		firedAt             sql.NullTime
		cancelledAt         sql.NullTime
		lastError           sql.NullString
		status              string
		cronExpr            sql.NullString
		recurrenceUntil     sql.NullTime
		kind                string
		lastTaskID          sql.NullString
		lastDeliveredTaskID sql.NullString
	)
	if err := scanner.Scan(
		&rem.ID, &rem.OperatorID, &rem.Channel, &rem.ChannelRef, &projectID,
		&rem.FireAt, &rem.Content, &status, &rem.CreatedAt, &firedAt, &cancelledAt,
		&rem.CreatedVia, &rem.ErrorCount, &lastError,
		&cronExpr, &recurrenceUntil,
		&kind, &lastTaskID, &lastDeliveredTaskID,
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
	if kind == "" {
		kind = string(persistence.ReminderKindText)
	}
	rem.Kind = persistence.ReminderKind(kind)
	if lastTaskID.Valid {
		rem.LastTaskID = lastTaskID.String
	}
	if lastDeliveredTaskID.Valid {
		rem.LastDeliveredTaskID = lastDeliveredTaskID.String
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
