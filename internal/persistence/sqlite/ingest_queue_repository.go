package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// IngestQueueRepository drives the memory-ingest worker.
//
// ClaimBatch uses BEGIN IMMEDIATE for atomicity instead of
// FOR UPDATE SKIP LOCKED (no SQLite analog). Concurrent workers
// serialize at the DB lock; correctness — no two workers see the
// same queued row — is preserved.
type IngestQueueRepository struct {
	db DBTX
}

func NewIngestQueueRepository(db DBTX) *IngestQueueRepository {
	return &IngestQueueRepository{db: db}
}

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
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO project_ingest_queue (
			id, project_id, source_artifact_id, producer_role,
			ingest_execution_id, priority, proposed_class, proposed_confidence,
			state, attempts, enqueued_at, repo_scope
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id, source_artifact_id)
			WHERE state IN ('queued','processing')
			DO NOTHING`,
		item.ID, item.ProjectID, item.SourceArtifactID, item.ProducerRole,
		item.IngestExecutionID, item.Priority, item.ProposedClass, item.ProposedConfidence,
		item.State, item.Attempts, sqliteTime(item.EnqueuedAt), item.RepoScope,
	)
	return err
}

func (r *IngestQueueRepository) ClaimBatch(ctx context.Context, projectID string, limit int) ([]*persistence.IngestQueueItem, error) {
	if projectID == "" {
		return nil, fmt.Errorf("IngestQueueRepository.ClaimBatch: projectID is required")
	}
	if limit <= 0 {
		limit = 16
	}
	db, ok := r.db.(*sql.DB)
	if !ok {
		return nil, fmt.Errorf("ClaimBatch: requires *sql.DB")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// SELECT candidates inside the IMMEDIATE tx, then UPDATE them
	// individually by PK. RETURNING * gives us the post-update
	// rows; in SQLite this requires SQLite 3.35+.
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM project_ingest_queue
		WHERE project_id = ? AND state = 'queued'
		ORDER BY priority ASC, enqueued_at ASC
		LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()

	if len(ids) == 0 {
		_ = tx.Commit()
		return nil, nil
	}

	now := sqliteTime(time.Now().UTC())
	out := make([]*persistence.IngestQueueItem, 0, len(ids))
	for _, id := range ids {
		_, err := tx.ExecContext(ctx, `
			UPDATE project_ingest_queue
			SET state = 'processing',
			    started_at = ?,
			    attempts = attempts + 1
			WHERE id = ?`, now, id)
		if err != nil {
			return nil, err
		}
		row := tx.QueryRowContext(ctx, `
			SELECT id, project_id, source_artifact_id, producer_role,
			       ingest_execution_id, priority, proposed_class,
			       proposed_confidence, state, attempts, enqueued_at,
			       started_at, finished_at, last_error, repo_scope
			FROM project_ingest_queue WHERE id = ?`, id)
		item, err := scanIngestQueueItem(row)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *IngestQueueRepository) MarkDone(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("IngestQueueRepository.MarkDone: id is required")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE project_ingest_queue
		SET state = 'done',
		    finished_at = ?,
		    last_error = NULL
		WHERE id = ? AND state IN ('processing','done')`,
		sqliteTime(time.Now().UTC()), id)
	return err
}

func (r *IngestQueueRepository) MarkFailed(ctx context.Context, id string, maxAttempts int, errorMsg string) (bool, error) {
	if id == "" {
		return false, fmt.Errorf("IngestQueueRepository.MarkFailed: id is required")
	}
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	// SQLite UPDATE … RETURNING (3.35+) so we get the post-update
	// state in one round-trip without losing race semantics.
	row := r.db.QueryRowContext(ctx, `
		UPDATE project_ingest_queue
		SET state       = CASE WHEN attempts < ? THEN 'queued' ELSE 'failed' END,
		    finished_at = CASE WHEN attempts < ? THEN finished_at ELSE ? END,
		    last_error  = ?
		WHERE id = ? AND state = 'processing'
		RETURNING state`,
		maxAttempts, maxAttempts, sqliteTime(time.Now().UTC()), errorMsg, id)
	var state string
	if err := row.Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return state == "failed", nil
}

func (r *IngestQueueRepository) ProjectsWithQueued(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 64
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT project_id
		FROM project_ingest_queue
		WHERE state = 'queued'
		ORDER BY project_id
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
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

func (r *IngestQueueRepository) QueueDepth(ctx context.Context, projectID string) (int, error) {
	if projectID == "" {
		return 0, fmt.Errorf("IngestQueueRepository.QueueDepth: projectID is required")
	}
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM project_ingest_queue
		WHERE project_id = ? AND state IN ('queued','processing')`,
		projectID).Scan(&n)
	return n, err
}

func (r *IngestQueueRepository) ResetStaleProcessing(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan < 0 {
		olderThan = 0
	}
	cutoff := time.Now().UTC().Add(-olderThan)
	res, err := r.db.ExecContext(ctx, `
		UPDATE project_ingest_queue
		SET state = 'queued', started_at = NULL
		WHERE state = 'processing'
		  AND started_at IS NOT NULL
		  AND started_at < ?`,
		sqliteTime(cutoff))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (r *IngestQueueRepository) CountStaleProcessing(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan < 0 {
		olderThan = 0
	}
	cutoff := time.Now().UTC().Add(-olderThan)
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM project_ingest_queue
		WHERE state = 'processing'
		  AND started_at IS NOT NULL
		  AND started_at < ?`,
		sqliteTime(cutoff)).Scan(&n)
	return n, err
}

func scanIngestQueueItem(scanner interface{ Scan(dest ...any) error }) (*persistence.IngestQueueItem, error) {
	item := &persistence.IngestQueueItem{}
	var (
		ingestExecID, proposedClass, lastError, repoScope sql.NullString
		enqueuedAt                                        sqlTime
		startedAt, finishedAt                             sqlNullTime
	)
	if err := scanner.Scan(
		&item.ID, &item.ProjectID, &item.SourceArtifactID, &item.ProducerRole,
		&ingestExecID, &item.Priority, &proposedClass,
		&item.ProposedConfidence, &item.State, &item.Attempts, &enqueuedAt,
		&startedAt, &finishedAt, &lastError, &repoScope,
	); err != nil {
		return nil, err
	}
	if ingestExecID.Valid {
		item.IngestExecutionID = &ingestExecID.String
	}
	if proposedClass.Valid {
		item.ProposedClass = &proposedClass.String
	}
	if lastError.Valid {
		item.LastError = &lastError.String
	}
	if repoScope.Valid {
		item.RepoScope = &repoScope.String
	}
	item.EnqueuedAt = enqueuedAt.Time
	if startedAt.Valid {
		t := startedAt.Time
		item.StartedAt = &t
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		item.FinishedAt = &t
	}
	return item, nil
}
