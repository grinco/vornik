// SQLite-backed extracted_documents repository. Mirrors the
// Postgres implementation (postgres/extracted_document_repository.go)
// with adjustments for SQLite's syntax (? placeholders, TEXT
// columns for JSONB / TIMESTAMPTZ). Used by tests + minimal-build
// deployments; the daemon's production backend is Postgres.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExtractedDocumentRepository is the SQLite-backed implementation.
type ExtractedDocumentRepository struct {
	db DBTX
}

// NewExtractedDocumentRepository constructs the repository over db.
func NewExtractedDocumentRepository(db DBTX) *ExtractedDocumentRepository {
	return &ExtractedDocumentRepository{db: db}
}

// Upsert writes a row keyed on
// (source_artifact_id, extractor_name, extractor_version) and
// preserves the ID on conflict so memory-chunk provenance pointers
// survive re-extraction. Schema lives in
// sqlite/schema.go's "extracted_documents" block.
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
	metadata := string(doc.MetadataBlob)
	if metadata == "" {
		metadata = "{}"
	}
	outline := string(doc.OutlineBlob)
	if outline == "" {
		outline = "[]"
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO extracted_documents (
			id, project_id, source_artifact_id, extractor_name, extractor_version,
			mime_type, storage_path, metadata_blob, outline_blob,
			section_count, total_text_bytes, extraction_duration_ms,
			status, extracted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source_artifact_id, extractor_name, extractor_version)
		DO UPDATE SET
			project_id             = excluded.project_id,
			mime_type              = excluded.mime_type,
			storage_path           = excluded.storage_path,
			metadata_blob          = excluded.metadata_blob,
			outline_blob           = excluded.outline_blob,
			section_count          = excluded.section_count,
			total_text_bytes       = excluded.total_text_bytes,
			extraction_duration_ms = excluded.extraction_duration_ms,
			status                 = excluded.status,
			extracted_at           = excluded.extracted_at
	`,
		doc.ID, doc.ProjectID, doc.SourceArtifactID, doc.ExtractorName, doc.ExtractorVersion,
		doc.MimeType, doc.StoragePath, metadata, outline,
		doc.SectionCount, doc.TotalTextBytes, doc.ExtractionDurationMS,
		doc.Status, sqliteTime(doc.ExtractedAt),
	)
	return err
}

// Get retrieves an extracted document by ID. Returns (nil, nil) on
// missing-row (matches the postgres repo's nil-doc convention).
func (r *ExtractedDocumentRepository) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, source_artifact_id, extractor_name, extractor_version,
		       mime_type, storage_path, metadata_blob, outline_blob,
		       section_count, total_text_bytes, extraction_duration_ms,
		       status, extracted_at
		FROM extracted_documents
		WHERE id = ?
	`, id)
	return scanExtractedDocumentSQLite(row)
}

// GetByArtifact returns the highest-extractor-version row for the
// source artifact. Lexical sort on extractor_version — same caveat
// as the postgres repo; semver-strict ordering needs a numeric col.
func (r *ExtractedDocumentRepository) GetByArtifact(ctx context.Context, sourceArtifactID string) (*persistence.ExtractedDocument, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, source_artifact_id, extractor_name, extractor_version,
		       mime_type, storage_path, metadata_blob, outline_blob,
		       section_count, total_text_bytes, extraction_duration_ms,
		       status, extracted_at
		FROM extracted_documents
		WHERE source_artifact_id = ?
		ORDER BY extractor_version DESC, extracted_at DESC
		LIMIT 1
	`, sourceArtifactID)
	return scanExtractedDocumentSQLite(row)
}

// ListByProject returns rows newest-first. limit <= 0 disables the cap.
func (r *ExtractedDocumentRepository) ListByProject(ctx context.Context, projectID string, limit int) ([]*persistence.ExtractedDocument, error) {
	query := `
		SELECT id, project_id, source_artifact_id, extractor_name, extractor_version,
		       mime_type, storage_path, metadata_blob, outline_blob,
		       section_count, total_text_bytes, extraction_duration_ms,
		       status, extracted_at
		FROM extracted_documents
		WHERE project_id = ?
		ORDER BY extracted_at DESC
	`
	args := []any{projectID}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*persistence.ExtractedDocument, 0, 16)
	for rows.Next() {
		doc, err := scanExtractedDocumentSQLite(rows)
		if err != nil {
			return nil, err
		}
		if doc != nil {
			out = append(out, doc)
		}
	}
	return out, rows.Err()
}

// Delete removes a row by ID.
func (r *ExtractedDocumentRepository) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("id is empty")
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM extracted_documents WHERE id = ?`, id)
	return err
}

type sqliteScannable interface {
	Scan(dest ...any) error
}

func scanExtractedDocumentSQLite(row sqliteScannable) (*persistence.ExtractedDocument, error) {
	doc := &persistence.ExtractedDocument{}
	var metadata, outline string
	var extractedAt string
	err := row.Scan(
		&doc.ID, &doc.ProjectID, &doc.SourceArtifactID, &doc.ExtractorName, &doc.ExtractorVersion,
		&doc.MimeType, &doc.StoragePath, &metadata, &outline,
		&doc.SectionCount, &doc.TotalTextBytes, &doc.ExtractionDurationMS,
		&doc.Status, &extractedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	doc.MetadataBlob = []byte(metadata)
	doc.OutlineBlob = []byte(outline)
	if t, err := parseSqliteTime(extractedAt); err == nil {
		doc.ExtractedAt = t
	}
	return doc, nil
}
