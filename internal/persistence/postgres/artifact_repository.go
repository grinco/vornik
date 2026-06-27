package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ArtifactRepository provides PostgreSQL-backed artifact metadata persistence.
type ArtifactRepository struct {
	db DBTX
}

// NewArtifactRepository creates a new artifact repository.
func NewArtifactRepository(db DBTX) *ArtifactRepository {
	return &ArtifactRepository{db: db}
}

// Create inserts a new artifact record.
func (r *ArtifactRepository) Create(ctx context.Context, artifact *persistence.Artifact) error {
	if artifact == nil {
		return fmt.Errorf("artifact is nil")
	}
	if artifact.CreatedAt.IsZero() {
		artifact.CreatedAt = time.Now().UTC()
	}
	if artifact.Origin == "" {
		artifact.Origin = persistence.ArtifactOriginUnknown
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO artifacts (
			id, project_id, execution_id, task_id, name, artifact_class,
			storage_path, size_bytes, content_hash_sha256, mime_type, created_at, origin
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11, $12
		)
	`,
		artifact.ID, artifact.ProjectID, artifact.ExecutionID, artifact.TaskID, artifact.Name, artifact.ArtifactClass,
		artifact.StoragePath, artifact.SizeBytes, artifact.ContentHashSHA256, artifact.MimeType, artifact.CreatedAt,
		artifact.Origin,
	)
	if err != nil {
		return mapDBError(err)
	}
	return nil
}

// UpdateTaskID associates an existing artifact with a task. Used by the
// dispatcher to link a freshly snapshotted INPUT artifact to the task
// it was attached to once the task ID is known. A no-op when the
// supplied artifact ID doesn't exist — the dispatcher path is best-
// effort: the durable file in the artifact store is the load-bearing
// invariant, the task_id link is for UI/observability convenience.
func (r *ArtifactRepository) UpdateTaskID(ctx context.Context, artifactID, taskID string) error {
	if artifactID == "" {
		return fmt.Errorf("artifact id is empty")
	}
	if taskID == "" {
		return fmt.Errorf("task id is empty")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE artifacts SET task_id = $1 WHERE id = $2`,
		taskID, artifactID,
	)
	if err != nil {
		return mapDBError(err)
	}
	return nil
}

// Get retrieves an artifact by ID.
func (r *ArtifactRepository) Get(ctx context.Context, id string) (*persistence.Artifact, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, execution_id, task_id, name, artifact_class,
		       storage_path, size_bytes, content_hash_sha256, mime_type, created_at, origin
		FROM artifacts
		WHERE id = $1
	`, id)
	return scanArtifact(row)
}

// GetByHash retrieves an artifact by content hash.
func (r *ArtifactRepository) GetByHash(ctx context.Context, hash string) (*persistence.Artifact, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, execution_id, task_id, name, artifact_class,
		       storage_path, size_bytes, content_hash_sha256, mime_type, created_at, origin
		FROM artifacts
		WHERE content_hash_sha256 = $1
	`, hash)
	return scanArtifact(row)
}

// List retrieves artifacts matching the filter.
func (r *ArtifactRepository) List(ctx context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	query := `
		SELECT id, project_id, execution_id, task_id, name, artifact_class,
		       storage_path, size_bytes, content_hash_sha256, mime_type, created_at, origin
		FROM artifacts
		WHERE 1=1
	`
	args := make([]any, 0, 4)
	argPos := 1

	if filter.ProjectID != nil {
		query += fmt.Sprintf(" AND project_id = $%d", argPos)
		args = append(args, *filter.ProjectID)
		argPos++
	}
	if filter.ExecutionID != nil {
		query += fmt.Sprintf(" AND execution_id = $%d", argPos)
		args = append(args, *filter.ExecutionID)
		argPos++
	}
	if filter.TaskID != nil {
		query += fmt.Sprintf(" AND task_id = $%d", argPos)
		args = append(args, *filter.TaskID)
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

	var artifacts []*persistence.Artifact
	for rows.Next() {
		artifact, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

// Delete removes an artifact by ID.
func (r *ArtifactRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM artifacts WHERE id = $1`, id)
	return mapDBError(err)
}

// DeleteByExecutionID removes artifacts for an execution.
func (r *ArtifactRepository) DeleteByExecutionID(ctx context.Context, executionID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM artifacts WHERE execution_id = $1`, executionID)
	return mapDBError(err)
}

func scanArtifact(scanner interface {
	Scan(dest ...any) error
}) (*persistence.Artifact, error) {
	var (
		artifact    persistence.Artifact
		executionID sql.NullString
		taskID      sql.NullString
		sizeBytes   sql.NullInt64
		contentHash sql.NullString
		mimeType    sql.NullString
	)

	err := scanner.Scan(
		&artifact.ID, &artifact.ProjectID, &executionID, &taskID, &artifact.Name, &artifact.ArtifactClass,
		&artifact.StoragePath, &sizeBytes, &contentHash, &mimeType, &artifact.CreatedAt,
		&artifact.Origin,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, persistence.ErrNotFound
		}
		return nil, mapDBError(err)
	}

	if executionID.Valid {
		artifact.ExecutionID = &executionID.String
	}
	if taskID.Valid {
		artifact.TaskID = &taskID.String
	}
	if sizeBytes.Valid {
		artifact.SizeBytes = &sizeBytes.Int64
	}
	if contentHash.Valid {
		artifact.ContentHashSHA256 = &contentHash.String
	}
	if mimeType.Valid {
		artifact.MimeType = &mimeType.String
	}

	return &artifact, nil
}
