package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// MemoryQuarantineRepository is the Postgres backing for
// project_memory_quarantine (added in migration v23). DMARC-style
// quarantine pattern — chunks the gates refused are operator-
// inspectable and operator-releasable, never silently dropped.
type MemoryQuarantineRepository struct {
	db DBTX
}

// NewMemoryQuarantineRepository constructs the repo over the shared
// DBTX abstraction.
func NewMemoryQuarantineRepository(db DBTX) *MemoryQuarantineRepository {
	return &MemoryQuarantineRepository{db: db}
}

// Insert stores one quarantine row. ID generated when empty.
// QuarantinedAt defaults to NOW() server-side.
func (r *MemoryQuarantineRepository) Insert(ctx context.Context, item *persistence.MemoryQuarantineItem) error {
	if item == nil {
		return fmt.Errorf("MemoryQuarantineRepository.Insert: nil item")
	}
	if item.ProjectID == "" || item.SourceArtifactID == "" || item.FailedGate == "" {
		return fmt.Errorf("MemoryQuarantineRepository.Insert: project_id, source_artifact_id, failed_gate required")
	}
	if item.ID == "" {
		item.ID = persistence.GenerateID("qrtn")
	}
	if item.QuarantinedAt.IsZero() {
		item.QuarantinedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO project_memory_quarantine (
			id, project_id, source_artifact_id, producer_role,
			ingest_execution_id, content, content_hash,
			proposed_class, failed_gate, failure_detail, quarantined_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`,
		item.ID, item.ProjectID, item.SourceArtifactID, item.ProducerRole,
		item.IngestExecutionID, item.Content, item.ContentHash,
		item.ProposedClass, item.FailedGate, item.FailureDetail, item.QuarantinedAt,
	)
	return mapDBError(err)
}

// ListPending returns up to `limit` un-released, un-dropped rows for
// a project, newest first. Powers the operator dashboard +
// `vornikctl memory quarantine list`.
func (r *MemoryQuarantineRepository) ListPending(ctx context.Context, projectID string, limit int) ([]*persistence.MemoryQuarantineItem, error) {
	if projectID == "" {
		return nil, fmt.Errorf("ListPending: projectID required")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, source_artifact_id, producer_role,
		       ingest_execution_id, content, content_hash,
		       proposed_class, failed_gate, failure_detail,
		       quarantined_at, released_at, released_chunk_id, dropped_at
		FROM project_memory_quarantine
		WHERE project_id = $1
		  AND released_at IS NULL
		  AND dropped_at IS NULL
		ORDER BY quarantined_at DESC
		LIMIT $2
	`, projectID, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.MemoryQuarantineItem
	for rows.Next() {
		item, scanErr := scanQuarantineItem(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// Get returns one quarantine row by ID.
func (r *MemoryQuarantineRepository) Get(ctx context.Context, id string) (*persistence.MemoryQuarantineItem, error) {
	if id == "" {
		return nil, fmt.Errorf("Get: id required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, source_artifact_id, producer_role,
		       ingest_execution_id, content, content_hash,
		       proposed_class, failed_gate, failure_detail,
		       quarantined_at, released_at, released_chunk_id, dropped_at
		FROM project_memory_quarantine
		WHERE id = $1
	`, id)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	return scanQuarantineItem(rows)
}

// MarkReleased records that the operator released the row and
// links to the chunk that landed in memory.
func (r *MemoryQuarantineRepository) MarkReleased(ctx context.Context, id, releasedChunkID string) error {
	if id == "" {
		return fmt.Errorf("MarkReleased: id required")
	}
	var chunkID sql.NullString
	if releasedChunkID != "" {
		chunkID = sql.NullString{String: releasedChunkID, Valid: true}
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE project_memory_quarantine
		SET released_at = NOW(), released_chunk_id = $2
		WHERE id = $1 AND released_at IS NULL AND dropped_at IS NULL
	`, id, chunkID)
	return mapDBError(err)
}

// MarkDropped records that the operator dropped the row.
func (r *MemoryQuarantineRepository) MarkDropped(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("MarkDropped: id required")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE project_memory_quarantine
		SET dropped_at = NOW()
		WHERE id = $1 AND released_at IS NULL AND dropped_at IS NULL
	`, id)
	return mapDBError(err)
}

// CountByGate returns counts of pending quarantine rows grouped by
// failed_gate for one project. Powers the dashboard's
// "what gate is rejecting most?" widget.
func (r *MemoryQuarantineRepository) CountByGate(ctx context.Context, projectID string) (map[string]int, error) {
	if projectID == "" {
		return nil, fmt.Errorf("CountByGate: projectID required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT failed_gate, COUNT(*)
		FROM project_memory_quarantine
		WHERE project_id = $1
		  AND released_at IS NULL
		  AND dropped_at IS NULL
		GROUP BY failed_gate
		ORDER BY COUNT(*) DESC
	`, projectID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]int)
	for rows.Next() {
		var gate string
		var count int
		if err := rows.Scan(&gate, &count); err != nil {
			return nil, err
		}
		out[gate] = count
	}
	return out, rows.Err()
}

// scanQuarantineItem pulls one row into the model.
func scanQuarantineItem(rows *sql.Rows) (*persistence.MemoryQuarantineItem, error) {
	item := &persistence.MemoryQuarantineItem{}
	if err := rows.Scan(
		&item.ID, &item.ProjectID, &item.SourceArtifactID, &item.ProducerRole,
		&item.IngestExecutionID, &item.Content, &item.ContentHash,
		&item.ProposedClass, &item.FailedGate, &item.FailureDetail,
		&item.QuarantinedAt, &item.ReleasedAt, &item.ReleasedChunkID, &item.DroppedAt,
	); err != nil {
		return nil, err
	}
	return item, nil
}

// ErrQuarantineNotFound — sentinel for missing-by-ID lookups.
var ErrQuarantineNotFound = errors.New("quarantine row not found")
