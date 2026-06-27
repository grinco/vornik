// Package postgres — extracted_documents persistence.
//
// See https://docs.vornik.io §5 for the
// schema motivation and §11 (Phase 0) for the scoping.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExtractedDocumentRepository persists extractor output rows.
// Idempotent on (source_artifact_id, extractor_name, extractor_version).
type ExtractedDocumentRepository struct {
	db DBTX
}

// NewExtractedDocumentRepository creates the repository over a DB
// handle. db may be a *sql.DB or a transaction — both satisfy DBTX.
func NewExtractedDocumentRepository(db DBTX) *ExtractedDocumentRepository {
	return &ExtractedDocumentRepository{db: db}
}

// Upsert writes a row keyed on (source artifact, extractor name,
// extractor version). On conflict the existing row is updated in
// place — the ID is preserved so memory-chunk provenance pointers
// stay valid across re-extractions.
func (r *ExtractedDocumentRepository) Upsert(ctx context.Context, doc *persistence.ExtractedDocument) error {
	if doc == nil {
		return fmt.Errorf("extracted document is nil")
	}
	if doc.ExtractedAt.IsZero() {
		doc.ExtractedAt = time.Now().UTC()
	}
	if doc.Status == "" {
		doc.Status = persistence.ExtractedDocumentStatusOK
	}
	metadata := doc.MetadataBlob
	if len(metadata) == 0 {
		metadata = []byte("{}")
	}
	outline := doc.OutlineBlob
	if len(outline) == 0 {
		outline = []byte("[]")
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO extracted_documents (
			id, project_id, source_artifact_id, extractor_name, extractor_version,
			mime_type, storage_path, metadata_blob, outline_blob,
			section_count, total_text_bytes, extraction_duration_ms,
			status, extracted_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8::jsonb, $9::jsonb,
			$10, $11, $12,
			$13, $14
		)
		ON CONFLICT (source_artifact_id, extractor_name, extractor_version)
		DO UPDATE SET
			project_id             = EXCLUDED.project_id,
			mime_type              = EXCLUDED.mime_type,
			storage_path           = EXCLUDED.storage_path,
			metadata_blob          = EXCLUDED.metadata_blob,
			outline_blob           = EXCLUDED.outline_blob,
			section_count          = EXCLUDED.section_count,
			total_text_bytes       = EXCLUDED.total_text_bytes,
			extraction_duration_ms = EXCLUDED.extraction_duration_ms,
			status                 = EXCLUDED.status,
			extracted_at           = EXCLUDED.extracted_at
	`,
		doc.ID, doc.ProjectID, doc.SourceArtifactID, doc.ExtractorName, doc.ExtractorVersion,
		doc.MimeType, doc.StoragePath, string(metadata), string(outline),
		doc.SectionCount, doc.TotalTextBytes, doc.ExtractionDurationMS,
		doc.Status, doc.ExtractedAt,
	)
	if err != nil {
		return mapDBError(err)
	}
	return nil
}

// Get retrieves an extracted document by ID. Returns (nil, nil) when
// the row is missing — callers that need to distinguish "absent" from
// "error" rely on the nil-doc-no-error convention used elsewhere in
// the repository layer.
func (r *ExtractedDocumentRepository) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, source_artifact_id, extractor_name, extractor_version,
		       mime_type, storage_path, metadata_blob, outline_blob,
		       section_count, total_text_bytes, extraction_duration_ms,
		       status, extracted_at
		FROM extracted_documents
		WHERE id = $1
	`, id)
	return scanExtractedDocument(row)
}

// GetByArtifact returns the most-recently extracted document for the
// source artifact, preferring the highest extractor version. The
// ORDER BY is lexical on the version string — Phase 0 keeps semver
// parsing out of the SQL; callers wanting strict semver ordering
// should bump a numeric column in a future migration.
func (r *ExtractedDocumentRepository) GetByArtifact(ctx context.Context, sourceArtifactID string) (*persistence.ExtractedDocument, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, source_artifact_id, extractor_name, extractor_version,
		       mime_type, storage_path, metadata_blob, outline_blob,
		       section_count, total_text_bytes, extraction_duration_ms,
		       status, extracted_at
		FROM extracted_documents
		WHERE source_artifact_id = $1
		ORDER BY extractor_version DESC, extracted_at DESC
		LIMIT 1
	`, sourceArtifactID)
	return scanExtractedDocument(row)
}

// ListByProject returns rows for a project, newest first. limit <= 0
// means "no cap" — the operator UI uses 200 in practice.
func (r *ExtractedDocumentRepository) ListByProject(ctx context.Context, projectID string, limit int) ([]*persistence.ExtractedDocument, error) {
	query := `
		SELECT id, project_id, source_artifact_id, extractor_name, extractor_version,
		       mime_type, storage_path, metadata_blob, outline_blob,
		       section_count, total_text_bytes, extraction_duration_ms,
		       status, extracted_at
		FROM extracted_documents
		WHERE project_id = $1
		ORDER BY extracted_at DESC
	`
	args := []any{projectID}
	if limit > 0 {
		query += " LIMIT $2"
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*persistence.ExtractedDocument, 0, 16)
	for rows.Next() {
		doc, err := scanExtractedDocument(rows)
		if err != nil {
			return nil, err
		}
		if doc != nil {
			out = append(out, doc)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, mapDBError(err)
	}
	return out, nil
}

// Delete removes a row by ID. The on-disk sections directory is GC'd
// separately by the retention sweep.
func (r *ExtractedDocumentRepository) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("id is empty")
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM extracted_documents WHERE id = $1`, id)
	if err != nil {
		return mapDBError(err)
	}
	return nil
}

// scannable matches both *sql.Row and *sql.Rows so the same scanner
// serves Get/GetByArtifact (single row) and ListByProject (cursor).
type scannable interface {
	Scan(dest ...any) error
}

func scanExtractedDocument(row scannable) (*persistence.ExtractedDocument, error) {
	doc := &persistence.ExtractedDocument{}
	var metadata, outline []byte
	err := row.Scan(
		&doc.ID, &doc.ProjectID, &doc.SourceArtifactID, &doc.ExtractorName, &doc.ExtractorVersion,
		&doc.MimeType, &doc.StoragePath, &metadata, &outline,
		&doc.SectionCount, &doc.TotalTextBytes, &doc.ExtractionDurationMS,
		&doc.Status, &doc.ExtractedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, mapDBError(err)
	}
	doc.MetadataBlob = metadata
	doc.OutlineBlob = outline
	return doc, nil
}
