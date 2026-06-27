package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ArtifactRepository is the SQLite-backed
// persistence.ArtifactRepository.
type ArtifactRepository struct {
	db DBTX
}

// NewArtifactRepository constructs an ArtifactRepository over db.
func NewArtifactRepository(db DBTX) *ArtifactRepository {
	return &ArtifactRepository{db: db}
}

// Create inserts one artifact metadata row.
func (r *ArtifactRepository) Create(ctx context.Context, a *persistence.Artifact) error {
	if a == nil {
		return fmt.Errorf("artifact is nil")
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	if a.Origin == "" {
		a.Origin = persistence.ArtifactOriginUnknown
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO artifacts (
			id, project_id, execution_id, task_id, name, artifact_class,
			storage_path, size_bytes, content_hash_sha256, mime_type, created_at, origin
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ProjectID, a.ExecutionID, a.TaskID, a.Name, a.ArtifactClass,
		a.StoragePath, a.SizeBytes, a.ContentHashSHA256, a.MimeType, sqliteTime(a.CreatedAt),
		a.Origin,
	)
	return err
}

// UpdateTaskID late-binds an existing artifact to a task.
func (r *ArtifactRepository) UpdateTaskID(ctx context.Context, artifactID, taskID string) error {
	if artifactID == "" {
		return fmt.Errorf("artifact id is empty")
	}
	if taskID == "" {
		return fmt.Errorf("task id is empty")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE artifacts SET task_id = ? WHERE id = ?`,
		taskID, artifactID)
	return err
}

// Get returns one artifact by ID, ErrNotFound otherwise.
func (r *ArtifactRepository) Get(ctx context.Context, id string) (*persistence.Artifact, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, execution_id, task_id, name, artifact_class,
		       storage_path, size_bytes, content_hash_sha256, mime_type, created_at, origin
		FROM artifacts WHERE id = ?`, id)
	return scanSqliteArtifact(row)
}

// GetByHash returns one artifact by content hash, ErrNotFound otherwise.
func (r *ArtifactRepository) GetByHash(ctx context.Context, hash string) (*persistence.Artifact, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, execution_id, task_id, name, artifact_class,
		       storage_path, size_bytes, content_hash_sha256, mime_type, created_at, origin
		FROM artifacts WHERE content_hash_sha256 = ?`, hash)
	return scanSqliteArtifact(row)
}

// List returns rows matching filter, newest-first.
func (r *ArtifactRepository) List(ctx context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT id, project_id, execution_id, task_id, name, artifact_class,
		       storage_path, size_bytes, content_hash_sha256, mime_type, created_at, origin
		FROM artifacts WHERE 1=1`)
	args := make([]any, 0, 4)

	if filter.ProjectID != nil {
		b.WriteString(" AND project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.ExecutionID != nil {
		b.WriteString(" AND execution_id = ?")
		args = append(args, *filter.ExecutionID)
	}
	if filter.TaskID != nil {
		b.WriteString(" AND task_id = ?")
		args = append(args, *filter.TaskID)
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

	var out []*persistence.Artifact
	for rows.Next() {
		a, err := scanSqliteArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Delete removes one artifact by ID. No error on missing rows.
func (r *ArtifactRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM artifacts WHERE id = ?`, id)
	return err
}

// DeleteByExecutionID removes every artifact for one execution.
func (r *ArtifactRepository) DeleteByExecutionID(ctx context.Context, executionID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM artifacts WHERE execution_id = ?`, executionID)
	return err
}

func scanSqliteArtifact(scanner interface{ Scan(dest ...any) error }) (*persistence.Artifact, error) {
	var (
		a           persistence.Artifact
		executionID sql.NullString
		taskID      sql.NullString
		sizeBytes   sql.NullInt64
		contentHash sql.NullString
		mimeType    sql.NullString
		createdAt   sqlTime
	)
	err := scanner.Scan(
		&a.ID, &a.ProjectID, &executionID, &taskID, &a.Name, &a.ArtifactClass,
		&a.StoragePath, &sizeBytes, &contentHash, &mimeType, &createdAt,
		&a.Origin,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	if executionID.Valid {
		a.ExecutionID = &executionID.String
	}
	if taskID.Valid {
		a.TaskID = &taskID.String
	}
	if sizeBytes.Valid {
		a.SizeBytes = &sizeBytes.Int64
	}
	if contentHash.Valid {
		a.ContentHashSHA256 = &contentHash.String
	}
	if mimeType.Valid {
		a.MimeType = &mimeType.String
	}
	a.CreatedAt = createdAt.Time
	return &a, nil
}
