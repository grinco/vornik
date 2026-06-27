package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
	"vornik.io/vornik/internal/persistence"
)

// randRead is the entropy source for newLeaseID. Indirected as a
// package-level var (not a direct call to crypto/rand.Read) so
// tests can swap it for a failing fixture and exercise the
// OS-RNG-unavailable branch, which is otherwise unreachable.
var randRead = rand.Read

// newLeaseID returns a cryptographically random lease identifier. The ID
// is used to gate ReleaseLease and RenewLease, so it must be unguessable;
// the previous md5(random()) SQL recipe was collision-safe at current
// scale but relied on a broken hash and PostgreSQL's non-crypto random().
// 16 bytes of crypto/rand give 128 bits of entropy, hex-encoded.
func newLeaseID() (string, error) {
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		// crypto/rand.Read only fails if the OS RNG is unavailable.
		return "", fmt.Errorf("crypto/rand unavailable: %w", err)
	}
	return "lease-" + hex.EncodeToString(b[:]), nil
}

// TaskRepository provides PostgreSQL-backed task persistence.
type TaskRepository struct {
	db DBTX
}

// NewTaskRepository creates a new PostgreSQL-backed task repository.
func NewTaskRepository(db DBTX) *TaskRepository {
	return &TaskRepository{db: db}
}

// Ping checks if the database is reachable.
func (r *TaskRepository) Ping(ctx context.Context) error {
	var val int
	return r.db.QueryRowContext(ctx, "SELECT 1").Scan(&val)
}

// Create inserts a new task into the database.
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

	// Phase 23 columns (brief_amended_at, current_phase, expected_by,
	// closed_at, closed_by, message_count, open_checkpoint_id) are
	// not listed here — they all have DB defaults (NULL or 0) and
	// only the conversational lifecycle handlers write them, never
	// task creation.
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO tasks (
			id, project_id, workflow_id, idempotency_key, parent_task_id, creation_source,
			delegation_mode, status, priority, payload, dependencies,
			lease_id, leased_at, leased_by, lease_expires_at,
			attempt, max_attempts, last_error, last_error_class, created_at, updated_at,
			chat_turn_id
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11,
			$12, $13, $14, $15,
			$16, $17, $18, $19, $20, $21,
			$22
		)`,
		task.ID, task.ProjectID, task.WorkflowID, task.IdempotencyKey, task.ParentTaskID, task.CreationSource,
		task.DelegationMode, task.Status, task.Priority, task.Payload, pq.Array(task.Dependencies),
		task.LeaseID, task.LeasedAt, task.LeasedBy, task.LeaseExpiresAt,
		task.Attempt, task.MaxAttempts, task.LastError, task.LastErrorClass, task.CreatedAt, task.UpdatedAt,
		task.ChatTurnID,
	)
	if err != nil {
		return mapDBError(err)
	}

	return nil
}

// Get retrieves a task by ID.
func (r *TaskRepository) Get(ctx context.Context, id string) (*persistence.Task, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, workflow_id, idempotency_key, parent_task_id, creation_source,
		       delegation_mode, status, priority, payload, dependencies,
		       lease_id, leased_at, leased_by, lease_expires_at,
		       attempt, max_attempts, last_error, last_error_class, created_at, updated_at,
		       brief_amended_at, current_phase, expected_by, closed_at, closed_by, message_count, open_checkpoint_id, chat_turn_id
		FROM tasks
		WHERE id = $1
	`, id)

	return scanTask(row)
}

// GetByIdempotencyKey retrieves a task by project-scoped idempotency key.
func (r *TaskRepository) GetByIdempotencyKey(ctx context.Context, projectID, idempotencyKey string) (*persistence.Task, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, workflow_id, idempotency_key, parent_task_id, creation_source,
		       delegation_mode, status, priority, payload, dependencies,
		       lease_id, leased_at, leased_by, lease_expires_at,
		       attempt, max_attempts, last_error, last_error_class, created_at, updated_at,
		       brief_amended_at, current_phase, expected_by, closed_at, closed_by, message_count, open_checkpoint_id, chat_turn_id
		FROM tasks
		WHERE project_id = $1 AND idempotency_key = $2
	`, projectID, idempotencyKey)

	return scanTask(row)
}

// Update modifies an existing task.
func (r *TaskRepository) Update(ctx context.Context, task *persistence.Task) error {
	if task == nil {
		return fmt.Errorf("task is nil")
	}
	task.UpdatedAt = time.Now().UTC()

	_, err := r.db.ExecContext(ctx, `
		UPDATE tasks
		SET project_id = $2,
		    workflow_id = $3,
		    idempotency_key = $4,
		    parent_task_id = $5,
		    creation_source = $6,
		    delegation_mode = $7,
		    status = $8,
		    priority = $9,
		    payload = $10,
		    dependencies = $11,
		    lease_id = $12,
		    leased_at = $13,
		    leased_by = $14,
		    lease_expires_at = $15,
		    attempt = $16,
		    max_attempts = $17,
		    last_error = $18,
		    last_error_class = $19,
		    updated_at = $20
		WHERE id = $1
	`,
		task.ID, task.ProjectID, task.WorkflowID, task.IdempotencyKey, task.ParentTaskID, task.CreationSource,
		task.DelegationMode, task.Status, task.Priority, task.Payload, pq.Array(task.Dependencies),
		task.LeaseID, task.LeasedAt, task.LeasedBy, task.LeaseExpiresAt,
		task.Attempt, task.MaxAttempts, task.LastError, task.LastErrorClass, task.UpdatedAt,
	)
	if err != nil {
		return mapDBError(err)
	}
	return nil
}

// Delete removes a task by ID.
func (r *TaskRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1`, id)
	return mapDBError(err)
}

// List retrieves tasks matching the filter.
func (r *TaskRepository) List(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
	query := `
		SELECT id, project_id, workflow_id, idempotency_key, parent_task_id, creation_source,
		       delegation_mode, status, priority, payload, dependencies,
		       lease_id, leased_at, leased_by, lease_expires_at,
		       attempt, max_attempts, last_error, last_error_class, created_at, updated_at,
		       brief_amended_at, current_phase, expected_by, closed_at, closed_by, message_count, open_checkpoint_id, chat_turn_id
		FROM tasks
		WHERE 1=1
	`
	args := make([]any, 0, 3)
	argPos := 1

	if filter.ProjectID != nil {
		query += fmt.Sprintf(" AND project_id = $%d", argPos)
		args = append(args, *filter.ProjectID)
		argPos++
	}
	if filter.Status != nil {
		query += fmt.Sprintf(" AND status = $%d", argPos)
		args = append(args, *filter.Status)
		argPos++
	}
	if filter.UpdatedBefore != nil {
		query += fmt.Sprintf(" AND updated_at < $%d", argPos)
		args = append(args, *filter.UpdatedBefore)
		argPos++
	}

	query += " ORDER BY created_at DESC"
	if filter.PageSize > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argPos)
		args = append(args, filter.PageSize)
		argPos++
	}
	if filter.Offset > 0 {
		query += fmt.Sprintf(" OFFSET $%d", argPos)
		args = append(args, filter.Offset)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []*persistence.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// Count mirrors List's WHERE clauses without LIMIT/OFFSET so the
// API can return a real Total for paginated responses. A handler
// that returns len(page) as Total can't tell the client when to
// stop fetching — that was the audit's pagination finding.
func (r *TaskRepository) Count(ctx context.Context, filter persistence.TaskFilter) (int64, error) {
	query := `SELECT COUNT(*) FROM tasks WHERE 1=1`
	args := make([]any, 0, 2)
	argPos := 1

	if filter.ProjectID != nil {
		query += fmt.Sprintf(" AND project_id = $%d", argPos)
		args = append(args, *filter.ProjectID)
		argPos++
	}
	if filter.Status != nil {
		query += fmt.Sprintf(" AND status = $%d", argPos)
		args = append(args, *filter.Status)
	}

	var total int64
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return 0, mapDBError(err)
	}
	return total, nil
}

// UpdateStatus atomically updates task status.
func (r *TaskRepository) UpdateStatus(ctx context.Context, id string, status persistence.TaskStatus) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE tasks SET status = $2, updated_at = NOW() WHERE id = $1`, id, status)
	return mapDBError(err)
}

// TransitionToCancelled is the atomic conditional CANCELLED transition.
// Implemented as a single UPDATE so a task that races to COMPLETED
// between the handler's read and write cannot have its terminal state
// silently overwritten — the WHERE clause gates the write on the live
// row's status, not on a value the handler sampled earlier.
func (r *TaskRepository) TransitionToCancelled(ctx context.Context, id string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET status = 'CANCELLED', updated_at = NOW()
		WHERE id = $1
		  AND status IN ('QUEUED','LEASED','RUNNING','PENDING')
	`, id)
	if err != nil {
		return false, mapDBError(err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, mapDBError(err)
	}
	return rows > 0, nil
}

// TransitionConditional atomically updates a task's status only
// when it sits in one of `from`. Companion columns from opts are
// applied in the same UPDATE so the row never lands in an
// inconsistent state — e.g. CLOSED with no closed_at, or
// AWAITING_EXTERNAL with no expected_by.
//
// Returns (true, nil) on transition; (false, nil) when status drifted
// concurrently or no row matched.
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
	// Build the SET clause dynamically: status + updated_at always;
	// opt columns only when the matching pointer is non-nil. Args
	// numbered as we go.
	sets := []string{"status = $1", "updated_at = NOW()"}
	args := []any{string(to)}
	pos := 2

	if opts.SetClosedAtNow {
		sets = append(sets, "closed_at = NOW()")
	}
	if opts.ClosedBy != nil {
		sets = append(sets, fmt.Sprintf("closed_by = $%d", pos))
		args = append(args, *opts.ClosedBy)
		pos++
	}
	if opts.ExpectedBy != nil {
		sets = append(sets, fmt.Sprintf("expected_by = $%d", pos))
		args = append(args, *opts.ExpectedBy)
		pos++
	}
	if opts.CurrentPhase != nil {
		sets = append(sets, fmt.Sprintf("current_phase = $%d", pos))
		args = append(args, *opts.CurrentPhase)
		pos++
	}
	if opts.BriefAmendedAt != nil {
		sets = append(sets, fmt.Sprintf("brief_amended_at = $%d", pos))
		args = append(args, *opts.BriefAmendedAt)
		pos++
	}
	if opts.LastError != nil {
		sets = append(sets, fmt.Sprintf("last_error = $%d", pos))
		args = append(args, *opts.LastError)
		pos++
	}
	if opts.LastErrorClass != nil {
		sets = append(sets, fmt.Sprintf("last_error_class = $%d", pos))
		args = append(args, *opts.LastErrorClass)
		pos++
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
		sets = append(sets, fmt.Sprintf("attempt = $%d", pos))
		args = append(args, opts.Attempt)
		pos++
	}
	if opts.MaxAttempts > 0 {
		sets = append(sets, fmt.Sprintf("max_attempts = $%d", pos))
		args = append(args, opts.MaxAttempts)
		pos++
	}

	// id placeholder + status ANY placeholder. status arg goes
	// through pq.Array because the lib/pq driver doesn't auto-
	// convert []string to a Postgres TEXT[] / enum[].
	idPlaceholder := pos
	args = append(args, id)
	pos++
	statusPlaceholder := pos
	statusStrings := make([]string, len(from))
	for i, s := range from {
		statusStrings[i] = string(s)
	}
	args = append(args, pq.Array(statusStrings))

	query := fmt.Sprintf(`
		UPDATE tasks SET %s
		WHERE id = $%d
		  AND status = ANY($%d)
	`, strings.Join(sets, ", "), idPlaceholder, statusPlaceholder)

	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, mapDBError(err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, mapDBError(err)
	}
	return rows > 0, nil
}

// RequeueTerminalTask is the atomic counterpart for retry: only
// requeues if the task's current status is one of the three terminal
// states OR PENDING. PENDING was added 2026-05-16 after the
// task_20260516015931_2c9658cb6a103380 incident: a paused-execution
// resume that never arrived left the task stuck in PENDING and the
// scheduler's pickup query (WHERE status = 'QUEUED') skipped it
// forever, leaving the operator-clicked Retry as the only path back
// to QUEUED. Replaces the prior code path that called ReleaseLease
// with an empty leaseID — a lease primitive being misused for a
// state-machine transition.
func (r *TaskRepository) RequeueTerminalTask(ctx context.Context, id string, attempt, maxAttempts int) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET
			status            = 'QUEUED',
			attempt           = $2,
			max_attempts      = $3,
			lease_id          = NULL,
			leased_at         = NULL,
			leased_by         = NULL,
			lease_expires_at  = NULL,
			last_error        = NULL,
			last_error_class  = NULL,
			updated_at        = NOW()
		WHERE id = $1
		  AND status IN ('FAILED','CANCELLED','COMPLETED','PENDING')
	`, id, attempt, maxAttempts)
	if err != nil {
		return false, mapDBError(err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, mapDBError(err)
	}
	return rows > 0, nil
}

// LeaseTask atomically claims a queued task.
// When ProjectConcurrencyLimits is set, projects that already have the
// maximum number of active tasks are excluded from consideration.
//
// Query shape (N = number of projects with a concurrency limit):
//
//	WITH active_counts AS (
//	  -- single aggregate over every active task, grouped by project.
//	  -- WAITING_FOR_CHILDREN deliberately excluded: a parent that
//	  -- yielded to a delegated child is not occupying a runtime
//	  -- slot, and counting it deadlocks strict-adaptive routing on
//	  -- projects with maxConcurrentTasks=1 (parent blocks its own
//	  -- child from being leased).
//	  SELECT project_id, count(*) active FROM tasks
//	  WHERE status IN ('LEASED','RUNNING')
//	     OR (status='QUEUED' AND lease_id IS NOT NULL)
//	  GROUP BY project_id),
//	limits(project_id, cap) AS (VALUES (?,?),(?,?),…),
//	over_cap AS (SELECT l.project_id FROM limits l
//	             JOIN active_counts a USING(project_id) WHERE a.active >= l.cap),
//	candidate AS (SELECT id FROM tasks
//	              WHERE status='QUEUED' AND lease_id IS NULL
//	                AND project_id NOT IN (SELECT project_id FROM over_cap)
//	              ORDER BY priority, created_at
//	              FOR UPDATE SKIP LOCKED LIMIT 1)
//	UPDATE tasks ... WHERE id = (SELECT id FROM candidate) RETURNING ...;
//
// Replaces the prior shape that attached one correlated
// `(SELECT count(*) FROM tasks t2 WHERE t2.project_id = $i) >= $j`
// subquery per project. With 100 projects that was 100 correlated
// subscans per LeaseTask call, dominating lease latency under load.
// The new shape does one aggregate pass plus a small join regardless
// of N.
func (r *TaskRepository) LeaseTask(ctx context.Context, opts persistence.LeaseOptions) (*persistence.Task, error) {
	const maxConcurrencyProjects = 100

	args := []any{}
	argPos := 1

	// Build the limits/over_cap CTEs when per-project limits are set.
	// When empty, skip them entirely — the common single-project
	// chat path doesn't care about per-project throttling.
	var preludeCTEs string
	var overCapFilter string
	if len(opts.ProjectConcurrencyLimits) > 0 {
		limitRows := make([]string, 0, len(opts.ProjectConcurrencyLimits))
		n := 0
		for pid, limit := range opts.ProjectConcurrencyLimits {
			if n >= maxConcurrencyProjects {
				break
			}
			limitRows = append(limitRows, fmt.Sprintf("($%d::text, $%d::int)", argPos, argPos+1))
			args = append(args, pid, limit)
			argPos += 2
			n++
		}
		preludeCTEs = `
			active_counts AS (
				SELECT project_id, count(*)::int AS active
				FROM tasks
				WHERE status IN ('LEASED', 'RUNNING')
				   OR (status = 'QUEUED' AND lease_id IS NOT NULL)
				GROUP BY project_id
			),
			limits(project_id, cap) AS (VALUES ` + strings.Join(limitRows, ", ") + `),
			over_cap AS (
				SELECT l.project_id
				FROM limits l
				JOIN active_counts a ON a.project_id = l.project_id
				WHERE a.active >= l.cap
			),`
		overCapFilter = ` AND tasks.project_id NOT IN (SELECT project_id FROM over_cap)`
	}

	// Project priority CTE (LLD §4.2): primary sort key for the
	// candidate ORDER BY. Higher-priority projects (lower numeric
	// values) drain before lower-priority ones get a turn —
	// strict priority across projects, FIFO within. Projects absent
	// from ProjectPriorities fall back to ProjectPriorityDefault
	// (which itself defaults to 50, matching the project schema's
	// documented value).
	//
	// Skip the CTE entirely when no priorities are configured —
	// the candidate ORDER BY then degrades to (Task.Priority,
	// CreatedAt), the pre-2026.5.4 behaviour.
	var projectPriorityJoin, projectPrioritySortKey string
	if len(opts.ProjectPriorities) > 0 {
		priorityDefault := opts.ProjectPriorityDefault
		if priorityDefault == 0 {
			priorityDefault = 50
		}
		priorityRows := make([]string, 0, len(opts.ProjectPriorities))
		n := 0
		for pid, prio := range opts.ProjectPriorities {
			if n >= maxConcurrencyProjects {
				break
			}
			priorityRows = append(priorityRows, fmt.Sprintf("($%d::text, $%d::int)", argPos, argPos+1))
			args = append(args, pid, prio)
			argPos += 2
			n++
		}
		preludeCTEs += `
			project_priorities(project_id, project_priority) AS (VALUES ` + strings.Join(priorityRows, ", ") + `),`
		projectPriorityJoin = ` LEFT JOIN project_priorities pp ON pp.project_id = tasks.project_id`
		projectPrioritySortKey = fmt.Sprintf(`COALESCE(pp.project_priority, %d) ASC, `, priorityDefault)
	}

	// Assemble the candidate CTE with optional per-call filters.
	// Every unqualified column reference in this clause must be
	// table-qualified (`tasks.column`) because the candidate query
	// LEFT JOINs project_priorities when priorities are configured,
	// and any column shared between the two tables (project_id) is
	// otherwise ambiguous to Postgres. status / lease_id / priority /
	// created_at / dependencies are unique to tasks today, but the
	// blanket qualification keeps the next JOIN addition from
	// surfacing the same `column reference is ambiguous` parse
	// error in production (commit 4854b36 missed this and produced
	// exactly that error against every lease attempt for ~hours
	// after deploy).
	candidateWhere := " AND tasks.status = 'QUEUED' AND tasks.lease_id IS NULL"
	if opts.ProjectID != "" {
		candidateWhere = fmt.Sprintf(" AND tasks.project_id = $%d", argPos) + candidateWhere
		args = append(args, opts.ProjectID)
		argPos++
	}
	if opts.PriorityFloor > 0 {
		candidateWhere += fmt.Sprintf(" AND tasks.priority >= $%d", argPos)
		args = append(args, opts.PriorityFloor)
		argPos++
	}
	// Archived-project hard-guard: skip every queued task whose
	// project_id appears in the exclusion list. Same code path the
	// scheduler hits each lease attempt, so an operator's archive
	// click stops dispatch within one tick — no waiting for the
	// sweeper. Empty list skips the clause entirely.
	if len(opts.ExcludedProjects) > 0 {
		placeholders := make([]string, 0, len(opts.ExcludedProjects))
		for _, pid := range opts.ExcludedProjects {
			placeholders = append(placeholders, fmt.Sprintf("$%d", argPos))
			args = append(args, pid)
			argPos++
		}
		candidateWhere += " AND tasks.project_id NOT IN (" + strings.Join(placeholders, ", ") + ")"
	}
	// Dependency gating (LLD §4.1): a task with a populated
	// dependencies[] array becomes eligible only when EVERY listed
	// dependency is in COMPLETED. A pending, failed, or cancelled
	// dependency disqualifies. Empty dependencies[] passes
	// trivially — the NOT EXISTS clause never fires.
	//
	// Implementation walks the dependencies array via unnest() and
	// joins back to tasks; the NOT EXISTS form is "no row exists
	// where the dependency is non-COMPLETED", which is true iff all
	// dependencies are COMPLETED OR the array is empty.
	candidateWhere += `
				AND NOT EXISTS (
					SELECT 1 FROM unnest(tasks.dependencies) AS dep_id
					LEFT JOIN tasks d ON d.id = dep_id
					WHERE d.id IS NULL OR d.status <> 'COMPLETED'
				)`

	// Lease IDs are generated in Go with crypto/rand instead of SQL's
	// md5(random()) so they are collision- and prediction-resistant, and
	// so the postgres package has no lingering MD5 dependency.
	leaseID, err := newLeaseID()
	if err != nil {
		return nil, err
	}
	leaseIDPos := argPos
	leaseHolderPos := argPos + 1
	leaseDurPos := argPos + 2
	args = append(args, leaseID, opts.LeaseHolder, opts.LeaseDurationSeconds)

	// preludeCTEs (if non-empty) defines active_counts, limits, and
	// over_cap BEFORE candidate references over_cap. Keeping that
	// order explicit avoids relying on Postgres's visibility rules
	// for later CTEs.
	// Trim the trailing comma from preludeCTEs so the CTE list is
	// well-formed when no `candidate AS` separator follows directly.
	// The inline query below appends `candidate AS` after preludeCTEs;
	// each prelude entry already ends with a comma, which is correct.
	query := `
		WITH ` + preludeCTEs + `
		candidate AS (
			SELECT tasks.id
			FROM tasks` + projectPriorityJoin + `
			WHERE 1=1` + candidateWhere + overCapFilter + `
			-- Ordering (LLD §4.2):
			--   1. Project priority (DESC across projects: lower numeric
			--      value = higher priority, so ASC selects the highest-
			--      priority project's queue first). Projects absent from
			--      project_priorities fall back to ProjectPriorityDefault
			--      via COALESCE so a missing config row doesn't promote
			--      a project to priority 0 by default.
			--   2. Task.Priority within a single project (same convention).
			--   3. CreatedAt ASC — FIFO by submission. Tiebreaker on
			--      created_at (not updated_at) gives FIFO within a
			--      project at the same task priority. updated_at bumps
			--      on retries, status flips, and lease renewals, which
			--      would let a recently-retried task jump ahead of an
			--      older queued sibling — wrong for chained-task
			--      workflows where task N+1 expects task N's artifacts.
			ORDER BY ` + projectPrioritySortKey + `tasks.priority ASC, tasks.created_at ASC
			FOR UPDATE OF tasks SKIP LOCKED
			LIMIT 1
		)
		UPDATE tasks
		SET status = 'LEASED',
		    lease_id = $` + fmt.Sprint(leaseIDPos) + `,
		    leased_at = NOW(),
		    leased_by = $` + fmt.Sprint(leaseHolderPos) + `,
		    lease_expires_at = NOW() + ($` + fmt.Sprint(leaseDurPos) + ` || ' seconds')::interval,
		    updated_at = NOW()
		WHERE id = (SELECT id FROM candidate)
		RETURNING id, project_id, workflow_id, idempotency_key, parent_task_id, creation_source,
		          delegation_mode, status, priority, payload, dependencies,
		          lease_id, leased_at, leased_by, lease_expires_at,
		          attempt, max_attempts, last_error, last_error_class, created_at, updated_at,
		       brief_amended_at, current_phase, expected_by, closed_at, closed_by, message_count, open_checkpoint_id, chat_turn_id
	`

	row := r.db.QueryRowContext(ctx, query, args...)
	task, err := scanTask(row)
	if err != nil {
		// scanTask already routes sql.ErrNoRows through mapDBError,
		// which maps it to persistence.ErrNotFound. Translate that
		// back into the lease-specific sentinel — the LeaseTask
		// contract is ErrNoTasksAvailable when nothing is leasable.
		// Without this remap callers see "not found" and have to
		// special-case BOTH errors (scheduler.go:467 currently does;
		// the integration test at lease_dependencies_integration_test.go
		// did not, hence the regression).
		if err == sql.ErrNoRows || errors.Is(err, persistence.ErrNotFound) {
			return nil, persistence.ErrNoTasksAvailable
		}
		return nil, mapDBError(err)
	}
	return task, nil
}

// renewLeaseSQL is the canonical lease-renewal statement. The
// status guard is LOAD-BEARING: any code path that flips a leased
// task's status to anything outside the active set will silently
// orphan the lease, and the next RenewLease call will return
// ErrLeaseNotFound. See https://docs.vornik.io §4.6
// for the full contract; the unit test
// `TestRenewLeaseSQL_HasLoadBearingGuards` locks the guard in
// place against future regressions.
const renewLeaseSQL = `
		UPDATE tasks
		SET lease_expires_at = NOW() + ($3 || ' seconds')::interval,
		    updated_at = NOW()
		WHERE id = $1 AND lease_id = $2
		  AND status IN ('LEASED', 'RUNNING', 'WAITING_FOR_CHILDREN')
	`

// RenewLease extends the current lease on a task. Refuses to touch rows
// whose status is no longer active (LEASED / RUNNING / WAITING_FOR_CHILDREN)
// — otherwise a pathologically-timed renewal that races with ReleaseLease
// could resurrect the lease on a task that's already been returned to the
// queue or marked terminal.
func (r *TaskRepository) RenewLease(ctx context.Context, taskID, leaseID string, extendBySeconds int) error {
	res, err := r.db.ExecContext(ctx, renewLeaseSQL, taskID, leaseID, extendBySeconds)
	if err != nil {
		return mapDBError(err)
	}
	// 0 rows updated means the lease no longer matches (externally
	// cancelled, status flipped terminal, leaseID rotated). Pre-fix
	// this returned nil, so the scheduler's dispatch watch loop
	// treated a stale lease as successfully renewed and never
	// escalated — executor kept running while the DB row showed
	// CANCELLED. Match the mock's behaviour: surface ErrLeaseNotFound
	// so dispatchViaExecutor can escalate via its renewal-failure
	// counter.
	rows, err := res.RowsAffected()
	if err != nil {
		// RowsAffected itself failed — propagate, but don't fabricate
		// an ErrLeaseNotFound; let the caller see the real DB error.
		return mapDBError(err)
	}
	if rows == 0 {
		return persistence.ErrLeaseNotFound
	}
	return nil
}

// ReleaseLease releases a task back to the queue or marks it terminal.
// Both last_error (freeform detail) and last_error_class (typed tag)
// are set in the same UPDATE so operators never see them diverge.
// A successful release clears both — new executions shouldn't carry
// stale error context into their retry.
//
// Empty leaseID is rejected: the prior wildcard predicate
// `($2 = ” OR lease_id = $2)` let any caller mutate any task's
// state regardless of who held the lease. Use RequeueTerminalTask
// for terminal→QUEUED transitions and UpdateStatus for non-lease
// status changes.
func (r *TaskRepository) ReleaseLease(ctx context.Context, taskID, leaseID string, newStatus persistence.TaskStatus, opts persistence.ReleaseOptions) error {
	if leaseID == "" {
		return fmt.Errorf("ReleaseLease: leaseID required (use RequeueTerminalTask for terminal-to-QUEUED transitions)")
	}
	query := `
		UPDATE tasks
		SET status = $3,
		    lease_id = NULL,
		    leased_at = NULL,
		    leased_by = NULL,
		    lease_expires_at = NULL,
		    attempt = COALESCE(NULLIF($4, 0), attempt),
		    max_attempts = COALESCE(NULLIF($5, 0), max_attempts),
		    last_error = NULLIF($6, ''),
		    last_error_class = NULLIF($7, ''),
		    updated_at = NOW()
		WHERE id = $1
		  AND lease_id = $2
	`
	_, err := r.db.ExecContext(ctx, query, taskID, leaseID, newStatus, opts.Attempt, opts.MaxAttempts, opts.Error, opts.ErrorClass)
	return mapDBError(err)
}

// FindExpiredLeases finds tasks whose leases expired.
func (r *TaskRepository) FindExpiredLeases(ctx context.Context, limit int) ([]*persistence.Task, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, workflow_id, idempotency_key, parent_task_id, creation_source,
		       delegation_mode, status, priority, payload, dependencies,
		       lease_id, leased_at, leased_by, lease_expires_at,
		       attempt, max_attempts, last_error, last_error_class, created_at, updated_at,
		       brief_amended_at, current_phase, expected_by, closed_at, closed_by, message_count, open_checkpoint_id, chat_turn_id
		FROM tasks
		WHERE status IN ('QUEUED', 'LEASED', 'RUNNING')
		  AND lease_expires_at IS NOT NULL
		  AND lease_expires_at < NOW()
		ORDER BY lease_expires_at ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []*persistence.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// CountByStatus counts tasks grouped by status.
func (r *TaskRepository) CountByStatus(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT status, COUNT(*)
		FROM tasks
		WHERE ($1 = '' OR project_id = $1)
		GROUP BY status
	`, projectID)
	if err != nil {
		return nil, mapDBError(err)
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
// updated_at is at or after `since`, optionally filtered to one of
// `errorClasses`. Powers the per-project circuit breaker (handleFailure
// path in the executor) — a high count within a short window pauses
// autonomy on the project so a stuck loop can't burn through budget.
//
// Empty errorClasses means count any failure class; non-empty means
// match exactly. The query uses (project_id, status, updated_at)
// access pattern; the existing tasks_status_updated_at index covers
// it cleanly.
func (r *TaskRepository) CountRecentFailures(ctx context.Context, projectID string, errorClasses []string, since time.Time) (int, error) {
	if projectID == "" {
		return 0, fmt.Errorf("CountRecentFailures: projectID is required")
	}
	query := `
		SELECT COUNT(*)
		FROM tasks
		WHERE project_id = $1
		  AND status = 'FAILED'
		  AND updated_at >= $2`
	args := []any{projectID, since}
	if len(errorClasses) > 0 {
		// Build a placeholder list ($3, $4, ...) for the IN clause.
		placeholders := make([]string, len(errorClasses))
		for i, c := range errorClasses {
			placeholders[i] = fmt.Sprintf("$%d", len(args)+1)
			args = append(args, c)
			_ = i // silence linter
		}
		query += " AND last_error_class IN (" + strings.Join(placeholders, ",") + ")"
	}
	var count int
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, mapDBError(err)
	}
	return count, nil
}

// GetChildren returns child tasks for a parent.
func (r *TaskRepository) GetChildren(ctx context.Context, parentTaskID string) ([]*persistence.Task, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, workflow_id, idempotency_key, parent_task_id, creation_source,
		       delegation_mode, status, priority, payload, dependencies,
		       lease_id, leased_at, leased_by, lease_expires_at,
		       attempt, max_attempts, last_error, last_error_class, created_at, updated_at,
		       brief_amended_at, current_phase, expected_by, closed_at, closed_by, message_count, open_checkpoint_id, chat_turn_id
		FROM tasks
		WHERE parent_task_id = $1
		ORDER BY created_at ASC
	`, parentTaskID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []*persistence.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// CountChildrenForParents returns the direct-child count keyed by parent
// task ID. Parents with zero children are absent from the result map.
// Powers the UI list's "Subtasks (N)" pill in one round trip rather than
// N+1 GetChildren calls.
func (r *TaskRepository) CountChildrenForParents(ctx context.Context, parentTaskIDs []string) (map[string]int, error) {
	out := make(map[string]int)
	if len(parentTaskIDs) == 0 {
		return out, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT parent_task_id, COUNT(*)::int
		FROM tasks
		WHERE parent_task_id = ANY($1)
		GROUP BY parent_task_id
	`, pq.Array(parentTaskIDs))
	if err != nil {
		return nil, mapDBError(err)
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

// GetDependencies returns tasks that must complete before the given task.
func (r *TaskRepository) GetDependencies(ctx context.Context, taskID string) ([]*persistence.Task, error) {
	// Must select every column scanTask reads (29) — the eight
	// trailing columns were missing, so any call errored with a
	// scan-arity mismatch ("expected 29 destination arguments, not
	// 21"). Mirrors GetDependents (bug sweep 2026-06-04).
	rows, err := r.db.QueryContext(ctx, `
		SELECT dep.id, dep.project_id, dep.workflow_id, dep.idempotency_key, dep.parent_task_id, dep.creation_source,
		       dep.delegation_mode, dep.status, dep.priority, dep.payload, dep.dependencies,
		       dep.lease_id, dep.leased_at, dep.leased_by, dep.lease_expires_at,
		       dep.attempt, dep.max_attempts, dep.last_error, dep.last_error_class, dep.created_at, dep.updated_at,
		       dep.brief_amended_at, dep.current_phase, dep.expected_by, dep.closed_at, dep.closed_by, dep.message_count, dep.open_checkpoint_id, dep.chat_turn_id
		FROM tasks task
		JOIN tasks dep ON dep.id = ANY(task.dependencies)
		WHERE task.id = $1
		ORDER BY dep.created_at ASC
	`, taskID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []*persistence.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// GetDependents returns tasks waiting on the given task.
func (r *TaskRepository) GetDependents(ctx context.Context, taskID string) ([]*persistence.Task, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, workflow_id, idempotency_key, parent_task_id, creation_source,
		       delegation_mode, status, priority, payload, dependencies,
		       lease_id, leased_at, leased_by, lease_expires_at,
		       attempt, max_attempts, last_error, last_error_class, created_at, updated_at,
		       brief_amended_at, current_phase, expected_by, closed_at, closed_by, message_count, open_checkpoint_id, chat_turn_id
		FROM tasks
		WHERE $1 = ANY(dependencies)
		ORDER BY created_at ASC
	`, taskID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []*persistence.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func scanTask(scanner interface {
	Scan(dest ...any) error
}) (*persistence.Task, error) {
	var (
		task           persistence.Task
		workflowID     sql.NullString
		idempotencyKey sql.NullString
		parentTaskID   sql.NullString
		delegationMode sql.NullString
		leaseID        sql.NullString
		leasedAt       sql.NullTime
		leasedBy       sql.NullString
		leaseExpiresAt sql.NullTime
		lastError      sql.NullString
		lastErrorClass sql.NullString
		dependencies   pq.StringArray
		// Phase 23 — conversational task lifecycle columns. All
		// nullable / default-zero so existing rows scan cleanly.
		briefAmendedAt   sql.NullTime
		currentPhase     sql.NullString
		expectedBy       sql.NullTime
		closedAt         sql.NullTime
		closedBy         sql.NullString
		messageCount     sql.NullInt64
		openCheckpointID sql.NullString
		// Migration v46.
		chatTurnID sql.NullString
	)

	err := scanner.Scan(
		&task.ID, &task.ProjectID, &workflowID, &idempotencyKey, &parentTaskID, &task.CreationSource,
		&delegationMode, &task.Status, &task.Priority, &task.Payload, &dependencies,
		&leaseID, &leasedAt, &leasedBy, &leaseExpiresAt,
		&task.Attempt, &task.MaxAttempts, &lastError, &lastErrorClass, &task.CreatedAt, &task.UpdatedAt,
		&briefAmendedAt, &currentPhase, &expectedBy, &closedAt, &closedBy, &messageCount, &openCheckpointID,
		&chatTurnID,
	)
	if err != nil {
		return nil, mapDBError(err)
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
	if leaseID.Valid {
		task.LeaseID = &leaseID.String
	}
	if leasedAt.Valid {
		task.LeasedAt = &leasedAt.Time
	}
	if leasedBy.Valid {
		task.LeasedBy = &leasedBy.String
	}
	if leaseExpiresAt.Valid {
		task.LeaseExpiresAt = &leaseExpiresAt.Time
	}
	if lastError.Valid {
		task.LastError = &lastError.String
	}
	if lastErrorClass.Valid {
		task.LastErrorClass = &lastErrorClass.String
	}
	task.Dependencies = []string(dependencies)

	// Phase 23 columns.
	if briefAmendedAt.Valid {
		task.BriefAmendedAt = &briefAmendedAt.Time
	}
	if currentPhase.Valid {
		task.CurrentPhase = &currentPhase.String
	}
	if expectedBy.Valid {
		task.ExpectedBy = &expectedBy.Time
	}
	if closedAt.Valid {
		task.ClosedAt = &closedAt.Time
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

func mapDBError(err error) error {
	if err == nil {
		return nil
	}
	if err == sql.ErrNoRows {
		return persistence.ErrNotFound
	}
	if pgErr, ok := err.(*pq.Error); ok {
		switch pgErr.Code {
		case "23505":
			return persistence.ErrDuplicateKey
		case "23503":
			return persistence.ErrNotFound
		}
	}
	return err
}
