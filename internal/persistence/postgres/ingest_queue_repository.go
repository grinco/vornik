package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// IngestQueueRepository is the Postgres backing for project_ingest_queue
// (added in migration v23). The repo's surface is defined in the
// persistence.IngestQueueRepository interface; see that file for the
// contract reasoning.
type IngestQueueRepository struct {
	db DBTX
}

// NewIngestQueueRepository constructs the repo over the shared DBTX
// abstraction so the same constructor works against the instrumented
// and uninstrumented DB wrappers.
func NewIngestQueueRepository(db DBTX) *IngestQueueRepository {
	return &IngestQueueRepository{db: db}
}

// Enqueue inserts one queue row. ID generated when empty.
func (r *IngestQueueRepository) Enqueue(ctx context.Context, item *persistence.IngestQueueItem) error {
	if item == nil {
		return fmt.Errorf("IngestQueueRepository.Enqueue: item is nil")
	}
	if item.ProjectID == "" || item.SourceArtifactID == "" || item.ProducerRole == "" {
		return fmt.Errorf("IngestQueueRepository.Enqueue: project_id, source_artifact_id, producer_role are required")
	}
	if item.ID == "" {
		item.ID = persistence.GenerateID("ingq")
	}
	if item.State == "" {
		item.State = "queued"
	}
	if item.EnqueuedAt.IsZero() {
		item.EnqueuedAt = time.Now().UTC()
	}
	// Priority: 0 is a valid value (highest priority) so we don't
	// substitute a default. Producers that want default priority
	// pass 50 explicitly.
	// ON CONFLICT against the uq_ingest_queue_active partial unique index
	// (migration 97): if an active (queued|processing) row already exists
	// for this (project, source_artifact), the duplicate enqueue is a
	// race-safe no-op rather than a second row that re-ingests the same
	// content. The same artifact can still be re-ingested once its prior
	// run reaches a terminal (done|failed) state.
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO project_ingest_queue (
			id, project_id, source_artifact_id, producer_role,
			ingest_execution_id, priority, proposed_class, proposed_confidence,
			state, attempts, enqueued_at, repo_scope
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (project_id, source_artifact_id)
			WHERE state IN ('queued','processing')
			DO NOTHING
	`,
		item.ID, item.ProjectID, item.SourceArtifactID, item.ProducerRole,
		item.IngestExecutionID, item.Priority, item.ProposedClass, item.ProposedConfidence,
		item.State, item.Attempts, item.EnqueuedAt, item.RepoScope,
	)
	return mapDBError(err)
}

// ClaimBatch atomically transitions up to `limit` rows from queued
// → processing for the given project. FOR UPDATE SKIP LOCKED makes
// it safe under multiple concurrent worker goroutines: each call
// observes a disjoint set.
//
// Order: priority ASC (0=highest), then enqueued_at ASC. Matches the
// drain-path index from migration v23.
func (r *IngestQueueRepository) ClaimBatch(ctx context.Context, projectID string, limit int) ([]*persistence.IngestQueueItem, error) {
	if projectID == "" {
		return nil, fmt.Errorf("IngestQueueRepository.ClaimBatch: projectID is required")
	}
	if limit <= 0 {
		limit = 16
	}
	// CTE pattern: select-with-lock the candidate IDs, then update
	// in one statement. RETURNING gives us the fully-updated rows in
	// the right order. Single round-trip.
	rows, err := r.db.QueryContext(ctx, `
		WITH claimed AS (
			SELECT id
			FROM project_ingest_queue
			WHERE project_id = $1 AND state = 'queued'
			ORDER BY priority ASC, enqueued_at ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE project_ingest_queue q
		SET state = 'processing',
		    started_at = NOW(),
		    attempts = attempts + 1
		FROM claimed
		WHERE q.id = claimed.id
		RETURNING q.id, q.project_id, q.source_artifact_id, q.producer_role,
		          q.ingest_execution_id, q.priority, q.proposed_class,
		          q.proposed_confidence, q.state, q.attempts, q.enqueued_at,
		          q.started_at, q.finished_at, q.last_error, q.repo_scope
	`, projectID, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.IngestQueueItem
	for rows.Next() {
		item, scanErr := scanIngestQueueItem(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// RETURNING preserves the order from claimed (priority ASC,
	// enqueued_at ASC) under the planner's join ordering, but the
	// SQL spec doesn't guarantee it. Sort client-side so callers
	// that care about order get it deterministically.
	sortByPriorityThenAge(out)
	return out, nil
}

// MarkDone transitions one item from processing → done. Idempotent
// when the row is already done (no-op).
func (r *IngestQueueRepository) MarkDone(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("IngestQueueRepository.MarkDone: id is required")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE project_ingest_queue
		SET state = 'done',
		    finished_at = NOW(),
		    last_error = NULL
		WHERE id = $1 AND state IN ('processing','done')
	`, id)
	return mapDBError(err)
}

// MarkFailed transitions one item from processing → queued (retry)
// or processing → failed (terminal). Returns true when the item
// went terminal so the caller can emit a final-failure metric.
//
// maxAttempts of 0 or negative is normalised to 3 to match the
// scheduler's default attempt budget.
func (r *IngestQueueRepository) MarkFailed(ctx context.Context, id string, maxAttempts int, errorMsg string) (bool, error) {
	if id == "" {
		return false, fmt.Errorf("IngestQueueRepository.MarkFailed: id is required")
	}
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	// Single UPDATE: branch on attempts < maxAttempts inside the SQL
	// so the decision happens server-side and races (two workers
	// trying to fail the same row) settle deterministically.
	row := r.db.QueryRowContext(ctx, `
		UPDATE project_ingest_queue
		SET state       = CASE WHEN attempts < $2 THEN 'queued' ELSE 'failed' END,
		    finished_at = CASE WHEN attempts < $2 THEN finished_at ELSE NOW() END,
		    last_error  = $3
		WHERE id = $1 AND state = 'processing'
		RETURNING state
	`, id, maxAttempts, errorMsg)
	var state string
	if err := row.Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Row wasn't in 'processing' (already done, already
			// failed, or someone else handled it). Caller's view
			// of the pipeline catches this on the next iteration.
			return false, nil
		}
		return false, mapDBError(err)
	}
	return state == "failed", nil
}

// ProjectsWithQueued returns up to `limit` distinct project IDs
// that currently have at least one queued row. Worker uses this to
// scan only projects with work pending.
func (r *IngestQueueRepository) ProjectsWithQueued(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 64
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT project_id
		FROM project_ingest_queue
		WHERE state = 'queued'
		ORDER BY project_id
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// QueueDepth returns the count of queued + processing rows for one
// project. Powers the depth gauge + alerting.
func (r *IngestQueueRepository) QueueDepth(ctx context.Context, projectID string) (int, error) {
	if projectID == "" {
		return 0, fmt.Errorf("IngestQueueRepository.QueueDepth: projectID is required")
	}
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM project_ingest_queue
		WHERE project_id = $1 AND state IN ('queued','processing')
	`, projectID).Scan(&n)
	if err != nil {
		return 0, mapDBError(err)
	}
	return n, nil
}

// ResetStaleProcessing transitions any row whose state is 'processing'
// and whose started_at is older than `olderThan` back to 'queued'.
// The previous incarnation of the daemon claimed those rows but never
// finished — without this sweep they stay 'processing' forever and
// the worker never touches them again (ClaimBatch only picks 'queued').
//
// The attempts counter is preserved so a row that's been retried twice
// before the crash still hits MaxAttempts on its final retry; the per-
// row started_at is cleared so future depth metrics treat the row as
// freshly queued. Operator-visible via the count returned + the
// "reset stale processing rows" log line.
func (r *IngestQueueRepository) ResetStaleProcessing(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan < 0 {
		olderThan = 0
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE project_ingest_queue
		SET state = 'queued',
		    started_at = NULL
		WHERE state = 'processing'
		  AND started_at IS NOT NULL
		  AND started_at < NOW() - ($1::bigint || ' seconds')::interval
	`, int64(olderThan.Seconds()))
	if err != nil {
		return 0, mapDBError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// CountStaleProcessing counts rows currently stuck in 'processing'
// with started_at older than `olderThan`. Powers the
// memory_ingest_queue_stale_processing gauge: under healthy operation
// it should always be zero (single-item processing completes in well
// under a minute); a non-zero reading flags either a wedged worker or
// the post-crash window before the startup sweep ran.
func (r *IngestQueueRepository) CountStaleProcessing(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan < 0 {
		olderThan = 0
	}
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM project_ingest_queue
		WHERE state = 'processing'
		  AND started_at IS NOT NULL
		  AND started_at < NOW() - ($1::bigint || ' seconds')::interval
	`, int64(olderThan.Seconds())).Scan(&n)
	if err != nil {
		return 0, mapDBError(err)
	}
	return n, nil
}

// scanIngestQueueItem pulls one row into the model. The column order
// here matches every SELECT in this file — keep them in sync if you
// add a column.
func scanIngestQueueItem(rows *sql.Rows) (*persistence.IngestQueueItem, error) {
	item := &persistence.IngestQueueItem{}
	if err := rows.Scan(
		&item.ID, &item.ProjectID, &item.SourceArtifactID, &item.ProducerRole,
		&item.IngestExecutionID, &item.Priority, &item.ProposedClass,
		&item.ProposedConfidence, &item.State, &item.Attempts, &item.EnqueuedAt,
		&item.StartedAt, &item.FinishedAt, &item.LastError, &item.RepoScope,
	); err != nil {
		return nil, err
	}
	return item, nil
}

// sortByPriorityThenAge applies the same ordering the SQL produced.
// Belt-and-braces — RETURNING's ordering with FROM-joined CTEs is
// planner-dependent in PostgreSQL.
func sortByPriorityThenAge(items []*persistence.IngestQueueItem) {
	if len(items) < 2 {
		return
	}
	// Insertion sort: batches are tiny (default 16, soft cap 64),
	// not worth bringing in sort.Slice overhead.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0; j-- {
			a, b := items[j-1], items[j]
			if a.Priority < b.Priority {
				break
			}
			if a.Priority == b.Priority && !a.EnqueuedAt.After(b.EnqueuedAt) {
				break
			}
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}
