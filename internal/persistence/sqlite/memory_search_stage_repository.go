package sqlite

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// MemorySearchStageRepository persists per-search stage rows (P3
// confidence-based retrieval routing) into memory_search_stages. SQLite
// stores the parameters blob as TEXT (JSON); the shape matches the
// Postgres JSONB column.
type MemorySearchStageRepository struct {
	db DBTX
}

// NewMemorySearchStageRepository constructs the repo over the shared DBTX.
func NewMemorySearchStageRepository(db DBTX) *MemorySearchStageRepository {
	return &MemorySearchStageRepository{db: db}
}

// RecordStage inserts one stage row. ID generated when empty; CreatedAt
// defaults to NOW() when zero.
func (r *MemorySearchStageRepository) RecordStage(ctx context.Context, stage *persistence.MemorySearchStage) error {
	if stage == nil {
		return fmt.Errorf("MemorySearchStageRepository.RecordStage: stage is nil")
	}
	if stage.ID == "" {
		stage.ID = persistence.GenerateID("mss")
	}
	if stage.CreatedAt.IsZero() {
		stage.CreatedAt = time.Now().UTC()
	}
	params := stage.Parameters
	if params == nil {
		params = []byte("{}")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO memory_search_stages (
			id, project_id, trace_id, stage, parameters, created_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		stage.ID, stage.ProjectID, stage.TraceID, stage.Stage, string(params), sqliteTime(stage.CreatedAt),
	)
	return err
}
