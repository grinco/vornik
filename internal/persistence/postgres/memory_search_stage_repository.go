package postgres

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// MemorySearchStageRepository persists per-search stage rows (P3
// confidence-based retrieval routing) into memory_search_stages. The
// searcher writes one stage="trust_verdict" row per Routing-on search.
type MemorySearchStageRepository struct {
	db DBTX
}

// NewMemorySearchStageRepository constructs the repo over the shared DBTX.
func NewMemorySearchStageRepository(db DBTX) *MemorySearchStageRepository {
	return &MemorySearchStageRepository{db: db}
}

// RecordStage inserts one stage row. ID is generated when empty and
// CreatedAt defaults to NOW() when zero, mirroring the retrieval-audit repo
// so callers can pass partial structs.
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
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6)
	`,
		stage.ID, stage.ProjectID, stage.TraceID, stage.Stage, string(params), stage.CreatedAt,
	)
	return mapDBError(err)
}
