package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// MemoryQuarantineRepository persists chunks the ingest gates refused.
// SQLite table name is memory_quarantine (postgres uses
// project_memory_quarantine — same shape, shorter name).
type MemoryQuarantineRepository struct {
	db DBTX
}

func NewMemoryQuarantineRepository(db DBTX) *MemoryQuarantineRepository {
	return &MemoryQuarantineRepository{db: db}
}

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
		INSERT INTO memory_quarantine (
			id, project_id, source_artifact_id, producer_role,
			ingest_execution_id, content, content_hash,
			proposed_class, failed_gate, failure_detail, quarantined_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.ProjectID, item.SourceArtifactID, item.ProducerRole,
		item.IngestExecutionID, item.Content, item.ContentHash,
		item.ProposedClass, item.FailedGate, item.FailureDetail, sqliteTime(item.QuarantinedAt),
	)
	return err
}

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
		FROM memory_quarantine
		WHERE project_id = ?
		  AND released_at IS NULL
		  AND dropped_at IS NULL
		ORDER BY quarantined_at DESC
		LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.MemoryQuarantineItem
	for rows.Next() {
		item, scanErr := scanSqliteQuarantineItem(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *MemoryQuarantineRepository) Get(ctx context.Context, id string) (*persistence.MemoryQuarantineItem, error) {
	if id == "" {
		return nil, fmt.Errorf("Get: id required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, source_artifact_id, producer_role,
		       ingest_execution_id, content, content_hash,
		       proposed_class, failed_gate, failure_detail,
		       quarantined_at, released_at, released_chunk_id, dropped_at
		FROM memory_quarantine WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	return scanSqliteQuarantineItem(rows)
}

func (r *MemoryQuarantineRepository) MarkReleased(ctx context.Context, id, releasedChunkID string) error {
	if id == "" {
		return fmt.Errorf("MarkReleased: id required")
	}
	var chunkID sql.NullString
	if releasedChunkID != "" {
		chunkID = sql.NullString{String: releasedChunkID, Valid: true}
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE memory_quarantine
		SET released_at = ?, released_chunk_id = ?
		WHERE id = ? AND released_at IS NULL AND dropped_at IS NULL`,
		sqliteTime(time.Now().UTC()), chunkID, id)
	return err
}

func (r *MemoryQuarantineRepository) MarkDropped(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("MarkDropped: id required")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE memory_quarantine
		SET dropped_at = ?
		WHERE id = ? AND released_at IS NULL AND dropped_at IS NULL`,
		sqliteTime(time.Now().UTC()), id)
	return err
}

func (r *MemoryQuarantineRepository) CountByGate(ctx context.Context, projectID string) (map[string]int, error) {
	if projectID == "" {
		return nil, fmt.Errorf("CountByGate: projectID required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT failed_gate, COUNT(*)
		FROM memory_quarantine
		WHERE project_id = ?
		  AND released_at IS NULL
		  AND dropped_at IS NULL
		GROUP BY failed_gate
		ORDER BY COUNT(*) DESC`, projectID)
	if err != nil {
		return nil, err
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

func scanSqliteQuarantineItem(rows *sql.Rows) (*persistence.MemoryQuarantineItem, error) {
	item := &persistence.MemoryQuarantineItem{}
	var (
		producerRole, proposedClass, failureDetail, releasedChunk sql.NullString
		ingestExecID                                              sql.NullString
		quarantinedAt                                             sqlTime
		releasedAt, droppedAt                                     sqlNullTime
	)
	if err := rows.Scan(
		&item.ID, &item.ProjectID, &item.SourceArtifactID, &producerRole,
		&ingestExecID, &item.Content, &item.ContentHash,
		&proposedClass, &item.FailedGate, &failureDetail,
		&quarantinedAt, &releasedAt, &releasedChunk, &droppedAt,
	); err != nil {
		return nil, err
	}
	if producerRole.Valid {
		item.ProducerRole = &producerRole.String
	}
	if ingestExecID.Valid {
		item.IngestExecutionID = &ingestExecID.String
	}
	if proposedClass.Valid {
		item.ProposedClass = &proposedClass.String
	}
	if failureDetail.Valid {
		item.FailureDetail = &failureDetail.String
	}
	item.QuarantinedAt = quarantinedAt.Time
	if releasedAt.Valid {
		t := releasedAt.Time
		item.ReleasedAt = &t
	}
	if releasedChunk.Valid {
		item.ReleasedChunkID = &releasedChunk.String
	}
	if droppedAt.Valid {
		t := droppedAt.Time
		item.DroppedAt = &t
	}
	return item, nil
}

// ErrQuarantineNotFound — sentinel for missing-by-ID lookups.
var ErrQuarantineNotFound = errors.New("quarantine row not found")
