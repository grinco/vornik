package postgres

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// ChunkGraphExtractionRepository is the Postgres backing for the
// KG worker's chunk-selection queries. The migration v26 backfill
// flipped every existing chunk's needs_graph_extraction flag to
// TRUE; the partial index
// (idx_project_memory_chunks_needs_graph_extraction) keeps these
// queries cheap regardless of total chunk count.
type ChunkGraphExtractionRepository struct {
	db DBTX
}

func NewChunkGraphExtractionRepository(db DBTX) *ChunkGraphExtractionRepository {
	return &ChunkGraphExtractionRepository{db: db}
}

func (r *ChunkGraphExtractionRepository) FetchUnextracted(ctx context.Context, limit int) ([]persistence.ChunkForExtraction, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, content
		FROM project_memory_chunks
		WHERE needs_graph_extraction = TRUE
		ORDER BY created_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]persistence.ChunkForExtraction, 0, limit)
	for rows.Next() {
		var c persistence.ChunkForExtraction
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Content); err != nil {
			return nil, mapDBError(err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *ChunkGraphExtractionRepository) MarkExtracted(ctx context.Context, chunkID string) error {
	if chunkID == "" {
		return fmt.Errorf("MarkExtracted: chunk_id required")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE project_memory_chunks
		SET    needs_graph_extraction = FALSE
		WHERE  id = $1`, chunkID)
	return mapDBError(err)
}

func (r *ChunkGraphExtractionRepository) PendingCount(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT count(*) FROM project_memory_chunks
		WHERE needs_graph_extraction = TRUE`).Scan(&n)
	if err != nil {
		return 0, mapDBError(err)
	}
	return n, nil
}

// Stats reads pending/done chunk counts plus the resulting
// entity/edge/mention totals in one round-trip (well — five
// queries, but no per-row work). Each query hits an indexed
// path: chunks via the partial index, entities/edges/mentions
// via simple count(*) on small tables. Adequate for the
// /ui/memory landing page render.
func (r *ChunkGraphExtractionRepository) Stats(ctx context.Context) (*persistence.KGStats, error) {
	out := &persistence.KGStats{EntitiesByType: map[string]int{}}

	if err := r.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE needs_graph_extraction = TRUE),
		       count(*) FILTER (WHERE needs_graph_extraction = FALSE)
		FROM project_memory_chunks`).Scan(&out.ChunksPending, &out.ChunksDone); err != nil {
		return nil, mapDBError(err)
	}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM knowledge_entities`).Scan(&out.Entities); err != nil {
		return nil, mapDBError(err)
	}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM knowledge_edges`).Scan(&out.Edges); err != nil {
		return nil, mapDBError(err)
	}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM entity_mentions`).Scan(&out.Mentions); err != nil {
		return nil, mapDBError(err)
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT type, count(*) FROM knowledge_entities GROUP BY type ORDER BY count(*) DESC`)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, mapDBError(err)
		}
		out.EntitiesByType[t] = n
	}
	return out, rows.Err()
}

// ReflagChunksMissingEdges flips needs_graph_extraction = TRUE on
// every project chunk that produced ZERO published edges. The
// candidate set is computed via NOT EXISTS against
// knowledge_edges.source_chunks (a TEXT[] containing chunk IDs) —
// a single round-trip whose plan uses the GIN-friendly source_chunks
// index that knowledge_edges already carries.
//
// countOnly=true returns the candidate count without writing,
// powering the CLI's --dry-run preview.
//
// Both branches scope by project_id so a fix only re-flags the
// project the operator opted in. Lifecycle filter: only
// 'published' edges count — quarantined / superseded edges are
// ignored, matching the recall path's "active" view.
func (r *ChunkGraphExtractionRepository) ReflagChunksMissingEdges(ctx context.Context, projectID string, countOnly bool) (int, error) {
	if projectID == "" {
		return 0, fmt.Errorf("ReflagChunksMissingEdges: project_id required")
	}
	const candidateSQL = `
		SELECT c.id
		FROM project_memory_chunks c
		WHERE c.project_id = $1
		  AND c.needs_graph_extraction = FALSE
		  AND NOT EXISTS (
		    SELECT 1
		    FROM knowledge_edges e
		    WHERE e.project_id = $1
		      AND e.lifecycle_state = 'published'
		      AND c.id = ANY(e.source_chunks)
		  )`
	if countOnly {
		var n int
		err := r.db.QueryRowContext(ctx,
			`SELECT count(*) FROM ( `+candidateSQL+` ) candidates`, projectID).Scan(&n)
		if err != nil {
			return 0, mapDBError(err)
		}
		return n, nil
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE project_memory_chunks
		SET    needs_graph_extraction = TRUE
		WHERE  id IN ( `+candidateSQL+` )`, projectID)
	if err != nil {
		return 0, mapDBError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, mapDBError(err)
	}
	return int(n), nil
}
