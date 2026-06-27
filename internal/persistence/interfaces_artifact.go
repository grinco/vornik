// Package persistence — artifact + extracted-document interfaces.
//
// Artifact storage (binary inputs/outputs) plus the document-extraction cache (extracted_documents). Both surface the operator-uploaded content path that feeds memory ingest.
// Split from interfaces.go on 2026-05-28 to keep each domain in
// its own file. Same package; no API change — pure file-org.
package persistence

import (
	"context"
)

// ArtifactRepository defines the interface for artifact persistence operations.
type ArtifactRepository interface {
	// Create inserts a new artifact record into the database.
	Create(ctx context.Context, artifact *Artifact) error

	// Get retrieves an artifact by ID.
	Get(ctx context.Context, id string) (*Artifact, error)

	// GetByHash retrieves an artifact by its content hash (for deduplication).
	GetByHash(ctx context.Context, hash string) (*Artifact, error)

	// List retrieves artifacts based on filter criteria.
	List(ctx context.Context, filter ArtifactFilter) ([]*Artifact, error)

	// Delete removes an artifact record by ID.
	Delete(ctx context.Context, id string) error

	// DeleteByExecutionID removes all artifacts associated with an execution.
	DeleteByExecutionID(ctx context.Context, executionID string) error

	// UpdateTaskID associates an artifact with a task post-creation.
	// The dispatcher snapshots input_files into the artifact store
	// before persisting the task (the artifact storage path needs to
	// land in the task payload), then calls this to link the row.
	UpdateTaskID(ctx context.Context, artifactID, taskID string) error
}

// ExtractedDocumentRepository persists the cached output of
// document extractors. The repository is idempotent on
// (source_artifact_id, extractor_name, extractor_version): callers
// can Upsert and trust that re-running an extractor doesn't
// duplicate rows. See document-extraction-design.md §5.
type ExtractedDocumentRepository interface {
	// Upsert inserts a new extracted-document row, or updates the
	// existing row for the same (source artifact, extractor name,
	// extractor version) triple. The row's ID is preserved on update
	// so existing memory-chunk provenance pointers remain valid.
	Upsert(ctx context.Context, doc *ExtractedDocument) error

	// Get retrieves an extracted document by ID.
	Get(ctx context.Context, id string) (*ExtractedDocument, error)

	// GetByArtifact returns the most-recent extraction for the given
	// source artifact across all extractors. Returns nil when no
	// extraction has been recorded; callers should not treat that as
	// an error. When multiple extractor versions exist for the same
	// source, the highest-version row wins (semver-ish lexical sort).
	GetByArtifact(ctx context.Context, sourceArtifactID string) (*ExtractedDocument, error)

	// ListByProject returns extracted documents for a project,
	// ordered by extracted_at DESC. Used by the operator UI's
	// /ui/projects/{id}/documents page.
	ListByProject(ctx context.Context, projectID string, limit int) ([]*ExtractedDocument, error)

	// Delete removes an extracted-document row. The on-disk
	// sections directory is GC'd separately (see
	// internal/retention/).
	Delete(ctx context.Context, id string) error
}
