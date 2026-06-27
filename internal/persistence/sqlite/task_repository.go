package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// randRead is the entropy source for newLeaseID. Indirected as a
// package-level var so tests can swap it for a failing fixture to
// exercise the OS-RNG-unavailable branch.
var randRead = rand.Read

// newLeaseID returns a crypto-random lease identifier — 128 bits of
// entropy, hex-encoded. Matches the Postgres-side recipe so lease
// IDs round-trip cleanly between backends in mixed-backend tests.
func newLeaseID() (string, error) {
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		return "", fmt.Errorf("crypto/rand unavailable: %w", err)
	}
	return "lease-" + hex.EncodeToString(b[:]), nil
}

// TaskRepository is the SQLite-backed persistence.TaskRepository.
//
// Lease semantics: SQLite has no FOR UPDATE SKIP LOCKED, so
// LeaseTask wraps its candidate-pick + UPDATE in a BEGIN IMMEDIATE
// transaction. Concurrent callers serialize at the database lock
// (busy_timeout 5s from the DSN); each still sees the other's
// committed leases via the candidate query's `lease_id IS NULL`
// guard. The single-writer constraint means tests that assert
// timing semantics of parallel leasing (e.g. the postgres-side
// lease_concurrency_integration_test.go) won't apply here —
// correctness holds, contention shape doesn't.
//
// db is the connection handle. LeaseTask requires a *sql.DB (to
// call BeginTx); we keep the field typed as DBTX for parity with
// the other SQLite repos but assert *sql.DB on the lease path
// since transactions can't be started on a Tx.
type TaskRepository struct {
	db DBTX
}

// NewTaskRepository constructs a TaskRepository over db. db must
// be a *sql.DB for the lease path to work; passing a *sql.Tx
// surfaces a clear error on the first LeaseTask call rather than
// at construction time, so tests that wrap repos in transactions
// for isolation can still use the non-lease methods.
func NewTaskRepository(db DBTX) *TaskRepository {
	return &TaskRepository{db: db}
}

// Ping checks the connection.
func (r *TaskRepository) Ping(ctx context.Context) error {
	var val int
	return r.db.QueryRowContext(ctx, "SELECT 1").Scan(&val)
}

// taskSelectColumns is the full column list every Get/List/Lease
// path returns. Kept in one place so adding a column doesn't drift
// the column-order between the SELECT and the scanTask receiver.
const taskSelectColumns = `id, project_id, workflow_id, idempotency_key, parent_task_id, creation_source,
		delegation_mode, status, priority, payload, dependencies,
		lease_id, leased_at, leased_by, lease_expires_at,
		attempt, max_attempts, last_error, last_error_class, created_at, updated_at,
		brief_amended_at, current_phase, expected_by, closed_at, closed_by, message_count, open_checkpoint_id,
		chat_turn_id`

// Create inserts a new task row.
func (r *TaskRepository) Create(ctx context.Context, task *persistence.Task) error {
	if task == nil {
		return fmt.Errorf("task is nil")
	}
	now := time.Now().UTC()
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	if task.UpdatedAt.IsZero() {
		task.UpdatedAt = task.CreatedAt
	}
	if task.Attempt == 0 {
		task.Attempt = 1
	}
	if task.MaxAttempts == 0 {
		task.MaxAttempts = 3
	}
	if task.Status == "" {
		task.Status = persistence.TaskStatusQueued
	}
	if task.CreationSource == "" {
		task.CreationSource = persistence.TaskCreationSourceUser
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO tasks (
			id, project_id, workflow_id, idempotency_key, parent_task_id, creation_source,
			delegation_mode, status, priority, payload, dependencies,
			lease_id, leased_at, leased_by, lease_expires_at,
			attempt, max_attempts, last_error, last_error_class, created_at, updated_at,
			chat_turn_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ID, task.ProjectID, task.WorkflowID, task.IdempotencyKey, task.ParentTaskID, string(task.CreationSource),
		nullableDelegationMode(task.DelegationMode), string(task.Status), task.Priority,
		nullableBlob(task.Payload), sqliteStringArray(task.Dependencies),
		task.LeaseID, sqliteTimePtr(task.LeasedAt), task.LeasedBy, sqliteTimePtr(task.LeaseExpiresAt),
		task.Attempt, task.MaxAttempts, task.LastError, task.LastErrorClass,
		sqliteTime(task.CreatedAt), sqliteTime(task.UpdatedAt),
		task.ChatTurnID,
	)
	return err
}

func nullableDelegationMode(m *persistence.DelegationMode) interface{} {
	if m == nil {
		return nil
	}
	return string(*m)
}

// Get returns a task by ID; ErrNotFound when missing.
func (r *TaskRepository) Get(ctx context.Context, id string) (*persistence.Task, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+taskSelectColumns+` FROM tasks WHERE id = ?`, id)
	return scanSqliteTask(row)
}

// GetByIdempotencyKey returns a task by project-scoped idempotency key.
func (r *TaskRepository) GetByIdempotencyKey(ctx context.Context, projectID, idempotencyKey string) (*persistence.Task, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+taskSelectColumns+` FROM tasks WHERE project_id = ? AND idempotency_key = ?`,
		projectID, idempotencyKey)
	return scanSqliteTask(row)
}

// Update rewrites the non-lifecycle fields of an existing task.
// Mirrors the postgres surface: lifecycle columns (brief_amended_at,
// current_phase, expected_by, closed_at, closed_by, message_count,
// open_checkpoint_id) are updated via TransitionConditional and not
// touched here.
func (r *TaskRepository) Update(ctx context.Context, task *persistence.Task) error {
	if task == nil {
		return fmt.Errorf("task is nil")
	}
	task.UpdatedAt = time.Now().UTC()
	_, err := r.db.ExecContext(ctx, `
		UPDATE tasks
		SET project_id = ?,
		    workflow_id = ?,
		    idempotency_key = ?,
		    parent_task_id = ?,
		    creation_source = ?,
		    delegation_mode = ?,
		    status = ?,
		    priority = ?,
		    payload = ?,
		    dependencies = ?,
		    lease_id = ?,
		    leased_at = ?,
		    leased_by = ?,
		    lease_expires_at = ?,
		    attempt = ?,
		    max_attempts = ?,
		    last_error = ?,
		    last_error_class = ?,
		    updated_at = ?
		WHERE id = ?`,
		task.ProjectID, task.WorkflowID, task.IdempotencyKey, task.ParentTaskID, string(task.CreationSource),
		nullableDelegationMode(task.DelegationMode), string(task.Status), task.Priority,
		nullableBlob(task.Payload), sqliteStringArray(task.Dependencies),
		task.LeaseID, sqliteTimePtr(task.LeasedAt), task.LeasedBy, sqliteTimePtr(task.LeaseExpiresAt),
		task.Attempt, task.MaxAttempts, task.LastError, task.LastErrorClass,
		sqliteTime(task.UpdatedAt),
		task.ID,
	)
	return err
}

// Delete removes a task by ID. No error on missing rows.
func (r *TaskRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id)
	return err
}

// List returns tasks matching filter, newest-first.
func (r *TaskRepository) List(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
	var b strings.Builder
	b.WriteString(`SELECT ` + taskSelectColumns + ` FROM tasks WHERE 1=1`)
	args := make([]any, 0, 3)

	if filter.ProjectID != nil {
		b.WriteString(" AND project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.Status != nil {
		b.WriteString(" AND status = ?")
		args = append(args, string(*filter.Status))
	}
	if filter.UpdatedBefore != nil {
		b.WriteString(" AND updated_at < ?")
		args = append(args, *filter.UpdatedBefore)
	}
	b.WriteString(" ORDER BY created_at DESC")
	if filter.PageSize > 0 {
		b.WriteString(" LIMIT ?")
		args = append(args, filter.PageSize)
	}
	if filter.Offset > 0 {
		b.WriteString(" OFFSET ?")
		args = append(args, filter.Offset)
	}

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tasks []*persistence.Task
	for rows.Next() {
		t, err := scanSqliteTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// Count mirrors List's WHERE clauses without LIMIT/OFFSET.
func (r *TaskRepository) Count(ctx context.Context, filter persistence.TaskFilter) (int64, error) {
	var b strings.Builder
	b.WriteString(`SELECT COUNT(*) FROM tasks WHERE 1=1`)
	args := make([]any, 0, 2)
	if filter.ProjectID != nil {
		b.WriteString(" AND project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.Status != nil {
		b.WriteString(" AND status = ?")
		args = append(args, string(*filter.Status))
	}
	var total int64
	if err := r.db.QueryRowContext(ctx, b.String(), args...).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// UpdateStatus atomically updates task status.
func (r *TaskRepository) UpdateStatus(ctx context.Context, id string, status persistence.TaskStatus) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), sqliteTime(time.Now().UTC()), id)
	return err
}

// TransitionToCancelled is the atomic conditional CANCELLED transition.
func (r *TaskRepository) TransitionToCancelled(ctx context.Context, id string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET status = 'CANCELLED', updated_at = ?
		WHERE id = ?
		  AND status IN ('QUEUED','LEASED','RUNNING','PENDING')`,
		sqliteTime(time.Now().UTC()), id)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// TransitionConditional atomically updates a task's status only
// when it sits in one of `from`. Companion columns from opts are
// applied in the same UPDATE so the row never lands in an
// inconsistent state.
func (r *TaskRepository) TransitionConditional(
	ctx context.Context,
	id string,
	from []persistence.TaskStatus,
	to persistence.TaskStatus,
	opts persistence.TransitionOpts,
) (bool, error) {
	if id == "" || to == "" || len(from) == 0 {
		return false, fmt.Errorf("TransitionConditional: id, from, to required")
	}
	now := sqliteTime(time.Now().UTC())
	sets := []string{"status = ?", "updated_at = ?"}
	args := []any{string(to), now}

	if opts.SetClosedAtNow {
		sets = append(sets, "closed_at = ?")
		args = append(args, now)
	}
	if opts.ClosedBy != nil {
		sets = append(sets, "closed_by = ?")
		args = append(args, *opts.ClosedBy)
	}
	if opts.ExpectedBy != nil {
		sets = append(sets, "expected_by = ?")
		args = append(args, sqliteTime(*opts.ExpectedBy))
	}
	if opts.CurrentPhase != nil {
		sets = append(sets, "current_phase = ?")
		args = append(args, *opts.CurrentPhase)
	}
	if opts.BriefAmendedAt != nil {
		sets = append(sets, "brief_amended_at = ?")
		args = append(args, sqliteTime(*opts.BriefAmendedAt))
	}
	if opts.LastError != nil {
		sets = append(sets, "last_error = ?")
		args = append(args, *opts.LastError)
	}
	if opts.LastErrorClass != nil {
		sets = append(sets, "last_error_class = ?")
		args = append(args, *opts.LastErrorClass)
	}
	if opts.ClearLease {
		sets = append(sets,
			"lease_id = NULL",
			"leased_at = NULL",
			"leased_by = NULL",
			"lease_expires_at = NULL",
		)
	}
	if opts.Attempt > 0 {
		sets = append(sets, "attempt = ?")
		args = append(args, opts.Attempt)
	}
	if opts.MaxAttempts > 0 {
		sets = append(sets, "max_attempts = ?")
		args = append(args, opts.MaxAttempts)
	}

	// IN-list placeholders for the `from` filter.
	placeholders := make([]string, len(from))
	for i, s := range from {
		placeholders[i] = "?"
		args = append(args, string(s))
	}
	// id placeholder comes last in the WHERE clause.
	args = append(args, id)

	query := fmt.Sprintf(`
		UPDATE tasks SET %s
		WHERE status IN (%s) AND id = ?`,
		strings.Join(sets, ", "), strings.Join(placeholders, ","))

	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// RequeueTerminalTask atomically resets a terminal/PENDING task to
// QUEUED so the scheduler can pick it up again. Lease columns and
// last_error are cleared as part of the same UPDATE.
func (r *TaskRepository) RequeueTerminalTask(ctx context.Context, id string, attempt, maxAttempts int) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET
			status            = 'QUEUED',
			attempt           = ?,
			max_attempts      = ?,
			lease_id          = NULL,
			leased_at         = NULL,
			leased_by         = NULL,
			lease_expires_at  = NULL,
			last_error        = NULL,
			last_error_class  = NULL,
			updated_at        = ?
		WHERE id = ?
		  AND status IN ('FAILED','CANCELLED','COMPLETED','PENDING')`,
		attempt, maxAttempts, sqliteTime(time.Now().UTC()), id)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// LeaseTask atomically claims one queued task whose dependencies
// are satisfied and whose project isn't at its concurrency cap.
//
// SQLite semantics (vs Postgres `FOR UPDATE SKIP LOCKED`): the
// candidate-pick + UPDATE happen inside a BEGIN IMMEDIATE
// transaction, which acquires the database-level write lock. Two
// concurrent LeaseTask callers serialize: the second waits up to
// busy_timeout (5s from the DSN) for the first to commit, then
// runs against the post-commit state. Correctness equivalent —
// no two callers can pick the same row — just no parallel pickup.
func (r *TaskRepository) LeaseTask(ctx context.Context, opts persistence.LeaseOptions) (*persistence.Task, error) {
	// Lease IDs are generated in Go with crypto/rand (mirrors the
	// Postgres recipe). Failed RNG is fail-fast — we don't fall
	// back to a weaker source.
	leaseID, err := newLeaseID()
	if err != nil {
		return nil, err
	}

	// The lease path needs a transaction-capable handle. DBTX is
	// the storage-package union of *sql.DB and friends — only
	// *sql.DB exposes BeginTx. Cast and surface a clear error if
	// the caller wrapped us in a Tx.
	db, ok := r.db.(*sql.DB)
	if !ok {
		return nil, fmt.Errorf("sqlite: LeaseTask requires *sql.DB (got %T) — Tx-wrapped repos can read but not lease", r.db)
	}

	// BEGIN IMMEDIATE — SQLite's writer-lock acquisition mode.
	// LevelSerializable maps to it under modernc.org/sqlite. The
	// resulting transaction will block other writers up to
	// busy_timeout (5s) rather than racing them.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin lease tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op if Commit() succeeded

	// Per-project concurrency limits: build a comma-joined list
	// of project_ids whose active count (LEASED + RUNNING + QUEUED
	// with non-null lease_id) is at-or-above their cap.
	overCapProjects, err := computeOverCapProjects(ctx, tx, opts.ProjectConcurrencyLimits)
	if err != nil {
		return nil, fmt.Errorf("sqlite: compute over-cap projects: %w", err)
	}

	// Candidate query. Mirrors the Postgres shape minus
	// FOR UPDATE SKIP LOCKED.
	var b strings.Builder
	b.WriteString(`SELECT id FROM tasks WHERE status = 'QUEUED' AND lease_id IS NULL`)
	args := make([]any, 0, 8)

	if opts.ProjectID != "" {
		b.WriteString(" AND project_id = ?")
		args = append(args, opts.ProjectID)
	}
	if opts.PriorityFloor > 0 {
		b.WriteString(" AND priority >= ?")
		args = append(args, opts.PriorityFloor)
	}
	if len(overCapProjects) > 0 {
		placeholders := strings.Repeat("?,", len(overCapProjects))
		placeholders = placeholders[:len(placeholders)-1]
		b.WriteString(" AND project_id NOT IN (" + placeholders + ")")
		for _, pid := range overCapProjects {
			args = append(args, pid)
		}
	}
	// Archived-project hard-guard. Same NOT IN shape, separate
	// clause so a project can be over-cap AND archived without
	// either filter shadowing the other.
	if len(opts.ExcludedProjects) > 0 {
		placeholders := strings.Repeat("?,", len(opts.ExcludedProjects))
		placeholders = placeholders[:len(placeholders)-1]
		b.WriteString(" AND project_id NOT IN (" + placeholders + ")")
		for _, pid := range opts.ExcludedProjects {
			args = append(args, pid)
		}
	}
	// Dependency gating belongs in SQL, not in a bounded candidate
	// scan. The previous implementation fetched the first 64 queued
	// rows and filtered dependencies in Go, which starved a runnable
	// task sitting behind enough blocked siblings.
	b.WriteString(`
		AND NOT EXISTS (
			SELECT 1
			FROM json_each(tasks.dependencies) AS dep
			LEFT JOIN tasks d ON d.id = dep.value
			WHERE d.id IS NULL OR d.status <> 'COMPLETED'
		)`)
	// Order: project_priority (when configured) → task priority → created_at.
	// SQLite's CASE expression substitutes the priority CTE for
	// terseness — no need for a separate VALUES list in a CTE.
	projectPriorityOrder := ""
	if len(opts.ProjectPriorities) > 0 {
		def := opts.ProjectPriorityDefault
		if def == 0 {
			def = 50
		}
		var cb strings.Builder
		cb.WriteString("(CASE project_id ")
		for pid, prio := range opts.ProjectPriorities {
			cb.WriteString("WHEN ? THEN ? ")
			args = append(args, pid, prio)
		}
		fmt.Fprintf(&cb, "ELSE %d END) ASC, ", def)
		projectPriorityOrder = cb.String()
	}
	b.WriteString(" ORDER BY " + projectPriorityOrder + "priority ASC, created_at ASC")
	b.WriteString(" LIMIT 1")

	var chosenID string
	if err := tx.QueryRowContext(ctx, b.String(), args...).Scan(&chosenID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNoTasksAvailable
		}
		return nil, fmt.Errorf("sqlite: query candidate: %w", err)
	}

	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(opts.LeaseDurationSeconds) * time.Second)
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status = 'LEASED',
		    lease_id = ?,
		    leased_at = ?,
		    leased_by = ?,
		    lease_expires_at = ?,
		    updated_at = ?
		WHERE id = ?`,
		leaseID, sqliteTime(now), opts.LeaseHolder, sqliteTime(expiresAt), sqliteTime(now), chosenID,
	); err != nil {
		return nil, fmt.Errorf("sqlite: update leased task: %w", err)
	}

	// Re-SELECT the post-UPDATE row so the caller sees the lease
	// columns populated. SQLite 3.35+ supports UPDATE RETURNING
	// but using a plain SELECT keeps the path debuggable in
	// older SQLite builds; the row is already locked under tx.
	row := tx.QueryRowContext(ctx,
		`SELECT `+taskSelectColumns+` FROM tasks WHERE id = ?`, chosenID)
	task, err := scanSqliteTask(row)
	if err != nil {
		return nil, fmt.Errorf("sqlite: rescan leased task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit lease tx: %w", err)
	}
	return task, nil
}

// computeOverCapProjects returns project IDs whose active task
// count is at or above their configured concurrency limit. WAITING_
// FOR_CHILDREN excluded to match Postgres semantics — a parent
// yielding to its delegated child doesn't occupy a runtime slot.
func computeOverCapProjects(ctx context.Context, tx *sql.Tx, limits map[string]int) ([]string, error) {
	if len(limits) == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT project_id, COUNT(*) AS active
		FROM tasks
		WHERE status IN ('LEASED','RUNNING')
		   OR (status = 'QUEUED' AND lease_id IS NOT NULL)
		GROUP BY project_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var pid string
		var active int
		if err := rows.Scan(&pid, &active); err != nil {
			return nil, err
		}
		if cap, ok := limits[pid]; ok && active >= cap {
			out = append(out, pid)
		}
	}
	return out, rows.Err()
}

// RenewLease extends the current lease on a task. Refuses to touch
// rows whose status is no longer active.
func (r *TaskRepository) RenewLease(ctx context.Context, taskID, leaseID string, extendBySeconds int) error {
	expiresAt := time.Now().UTC().Add(time.Duration(extendBySeconds) * time.Second)
	res, err := r.db.ExecContext(ctx, `
		UPDATE tasks
		SET lease_expires_at = ?,
		    updated_at = ?
		WHERE id = ? AND lease_id = ?
		  AND status IN ('LEASED', 'RUNNING', 'WAITING_FOR_CHILDREN')`,
		sqliteTime(expiresAt), sqliteTime(time.Now().UTC()), taskID, leaseID)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return persistence.ErrLeaseNotFound
	}
	return nil
}

// ReleaseLease releases a task back to the queue or marks it
// terminal. Empty leaseID is rejected — use RequeueTerminalTask
// for terminal→QUEUED transitions.
func (r *TaskRepository) ReleaseLease(ctx context.Context, taskID, leaseID string, newStatus persistence.TaskStatus, opts persistence.ReleaseOptions) error {
	if leaseID == "" {
		return fmt.Errorf("ReleaseLease: leaseID required")
	}
	// COALESCE(NULLIF(?, 0), attempt) keeps the existing value
	// when the caller passes zero — same defaulting semantic as
	// the postgres impl.
	var attemptArg, maxAttemptsArg any = opts.Attempt, opts.MaxAttempts
	if opts.Attempt == 0 {
		attemptArg = nil
	}
	if opts.MaxAttempts == 0 {
		maxAttemptsArg = nil
	}
	var errArg, classArg any = opts.Error, opts.ErrorClass
	if opts.Error == "" {
		errArg = nil
	}
	if opts.ErrorClass == "" {
		classArg = nil
	}

	_, err := r.db.ExecContext(ctx, `
		UPDATE tasks
		SET status = ?,
		    lease_id = NULL,
		    leased_at = NULL,
		    leased_by = NULL,
		    lease_expires_at = NULL,
		    attempt = COALESCE(?, attempt),
		    max_attempts = COALESCE(?, max_attempts),
		    last_error = ?,
		    last_error_class = ?,
		    updated_at = ?
		WHERE id = ?
		  AND lease_id = ?`,
		string(newStatus), attemptArg, maxAttemptsArg, errArg, classArg,
		sqliteTime(time.Now().UTC()), taskID, leaseID)
	return err
}

// FindExpiredLeases returns tasks whose leases have expired.
func (r *TaskRepository) FindExpiredLeases(ctx context.Context, limit int) ([]*persistence.Task, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+taskSelectColumns+`
		FROM tasks
		WHERE status IN ('QUEUED', 'LEASED', 'RUNNING')
		  AND lease_expires_at IS NOT NULL
		  AND lease_expires_at < ?
		ORDER BY lease_expires_at ASC
		LIMIT ?`,
		sqliteTime(time.Now().UTC()), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var tasks []*persistence.Task
	for rows.Next() {
		t, err := scanSqliteTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// CountByStatus groups tasks by status for a project (empty
// project = all projects).
func (r *TaskRepository) CountByStatus(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT status, COUNT(*)
		FROM tasks
		WHERE (? = '' OR project_id = ?)
		GROUP BY status`, projectID, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[persistence.TaskStatus]int64)
	for rows.Next() {
		var status persistence.TaskStatus
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	return counts, rows.Err()
}

// CountRecentFailures counts FAILED tasks in a project whose
// updated_at is at or after `since`, optionally filtered by error
// class.
func (r *TaskRepository) CountRecentFailures(ctx context.Context, projectID string, errorClasses []string, since time.Time) (int, error) {
	if projectID == "" {
		return 0, fmt.Errorf("CountRecentFailures: projectID is required")
	}
	var b strings.Builder
	b.WriteString(`SELECT COUNT(*) FROM tasks WHERE project_id = ? AND status = 'FAILED' AND updated_at >= ?`)
	args := []any{projectID, sqliteTime(since)}

	if len(errorClasses) > 0 {
		placeholders := make([]string, len(errorClasses))
		for i, c := range errorClasses {
			placeholders[i] = "?"
			args = append(args, c)
		}
		b.WriteString(" AND last_error_class IN (" + strings.Join(placeholders, ",") + ")")
	}
	var count int
	if err := r.db.QueryRowContext(ctx, b.String(), args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// GetChildren returns direct children of a parent task.
func (r *TaskRepository) GetChildren(ctx context.Context, parentTaskID string) ([]*persistence.Task, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+taskSelectColumns+` FROM tasks WHERE parent_task_id = ? ORDER BY created_at ASC`,
		parentTaskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tasks []*persistence.Task
	for rows.Next() {
		t, err := scanSqliteTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// CountChildrenForParents returns a map keyed by parent task ID
// of direct-child counts. Parents with zero children are absent.
func (r *TaskRepository) CountChildrenForParents(ctx context.Context, parentTaskIDs []string) (map[string]int, error) {
	out := make(map[string]int)
	if len(parentTaskIDs) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(parentTaskIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(parentTaskIDs))
	for i, id := range parentTaskIDs {
		args[i] = id
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT parent_task_id, COUNT(*) FROM tasks
		 WHERE parent_task_id IN (`+placeholders+`)
		 GROUP BY parent_task_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var pid string
		var n int
		if err := rows.Scan(&pid, &n); err != nil {
			return nil, err
		}
		out[pid] = n
	}
	return out, rows.Err()
}

// GetDependencies returns tasks listed in the dependencies of taskID.
// Postgres uses unnest+JOIN; SQLite fetches the parent's dependencies
// JSON, decodes it, then runs a single IN-list query for the deps.
// Two round-trips instead of one, but the second is keyed on PK so
// it's fast.
func (r *TaskRepository) GetDependencies(ctx context.Context, taskID string) ([]*persistence.Task, error) {
	var deps sqliteStringArray
	err := r.db.QueryRowContext(ctx,
		`SELECT dependencies FROM tasks WHERE id = ?`, taskID).Scan(&deps)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	if len(deps) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(deps))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(deps))
	for i, d := range deps {
		args[i] = d
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+taskSelectColumns+` FROM tasks WHERE id IN (`+placeholders+`)
		 ORDER BY created_at ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tasks []*persistence.Task
	for rows.Next() {
		t, err := scanSqliteTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// GetDependents returns tasks whose dependencies list includes taskID.
// SQLite stores dependencies as JSON text, so json_each gives exact
// membership instead of fuzzy LIKE matching over the encoded blob.
func (r *TaskRepository) GetDependents(ctx context.Context, taskID string) ([]*persistence.Task, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+taskSelectColumns+` FROM tasks
		 WHERE EXISTS (
		   SELECT 1 FROM json_each(tasks.dependencies) AS dep
		   WHERE dep.value = ?
		 )
		 ORDER BY created_at ASC`,
		taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tasks []*persistence.Task
	for rows.Next() {
		t, err := scanSqliteTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// scanSqliteTask reads one task row from a scanner.
func scanSqliteTask(scanner interface{ Scan(dest ...any) error }) (*persistence.Task, error) {
	var (
		task             persistence.Task
		workflowID       sql.NullString
		idempotencyKey   sql.NullString
		parentTaskID     sql.NullString
		delegationMode   sql.NullString
		payload          sql.NullString
		dependencies     sqliteStringArray
		leaseID          sql.NullString
		leasedAt         sqlNullTime
		leasedBy         sql.NullString
		leaseExpiresAt   sqlNullTime
		lastError        sql.NullString
		lastErrorClass   sql.NullString
		createdAt        sqlTime
		updatedAt        sqlTime
		briefAmendedAt   sqlNullTime
		currentPhase     sql.NullString
		expectedBy       sqlNullTime
		closedAt         sqlNullTime
		closedBy         sql.NullString
		messageCount     sql.NullInt64
		openCheckpointID sql.NullString
		chatTurnID       sql.NullString
	)
	err := scanner.Scan(
		&task.ID, &task.ProjectID, &workflowID, &idempotencyKey, &parentTaskID, &task.CreationSource,
		&delegationMode, &task.Status, &task.Priority, &payload, &dependencies,
		&leaseID, &leasedAt, &leasedBy, &leaseExpiresAt,
		&task.Attempt, &task.MaxAttempts, &lastError, &lastErrorClass, &createdAt, &updatedAt,
		&briefAmendedAt, &currentPhase, &expectedBy, &closedAt, &closedBy, &messageCount, &openCheckpointID,
		&chatTurnID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	if workflowID.Valid {
		task.WorkflowID = &workflowID.String
	}
	if idempotencyKey.Valid {
		task.IdempotencyKey = &idempotencyKey.String
	}
	if parentTaskID.Valid {
		task.ParentTaskID = &parentTaskID.String
	}
	if delegationMode.Valid {
		mode := persistence.DelegationMode(delegationMode.String)
		task.DelegationMode = &mode
	}
	if payload.Valid {
		task.Payload = []byte(payload.String)
	}
	task.Dependencies = []string(dependencies)
	if leaseID.Valid {
		task.LeaseID = &leaseID.String
	}
	if leasedAt.Valid {
		t := leasedAt.Time
		task.LeasedAt = &t
	}
	if leasedBy.Valid {
		task.LeasedBy = &leasedBy.String
	}
	if leaseExpiresAt.Valid {
		t := leaseExpiresAt.Time
		task.LeaseExpiresAt = &t
	}
	if lastError.Valid {
		task.LastError = &lastError.String
	}
	if lastErrorClass.Valid {
		task.LastErrorClass = &lastErrorClass.String
	}
	task.CreatedAt = createdAt.Time
	task.UpdatedAt = updatedAt.Time
	if briefAmendedAt.Valid {
		t := briefAmendedAt.Time
		task.BriefAmendedAt = &t
	}
	if currentPhase.Valid {
		task.CurrentPhase = &currentPhase.String
	}
	if expectedBy.Valid {
		t := expectedBy.Time
		task.ExpectedBy = &t
	}
	if closedAt.Valid {
		t := closedAt.Time
		task.ClosedAt = &t
	}
	if closedBy.Valid {
		task.ClosedBy = &closedBy.String
	}
	if messageCount.Valid {
		task.MessageCount = int(messageCount.Int64)
	}
	if openCheckpointID.Valid {
		task.OpenCheckpointID = &openCheckpointID.String
	}
	if chatTurnID.Valid {
		task.ChatTurnID = &chatTurnID.String
	}
	return &task, nil
}
