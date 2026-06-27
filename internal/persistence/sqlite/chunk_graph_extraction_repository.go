package sqlite

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// ChunkGraphExtractionRepository drives the KG worker's chunk
// selection. SQLite uses integer (0/1) for the boolean flag.
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
		WHERE needs_graph_extraction = 1
		ORDER BY created_at ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]persistence.ChunkForExtraction, 0, limit)
	for rows.Next() {
		var c persistence.ChunkForExtraction
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Content); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *ChunkGraphExtractionRepository) MarkExtracted(ctx context.Context, chunkID string) error {
	if chunkID == "" {
		return fmt.Errorf("MarkExtracted: chunk_id required")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE project_memory_chunks SET needs_graph_extraction = 0 WHERE id = ?`, chunkID)
	return err
}

func (r *ChunkGraphExtractionRepository) PendingCount(ctx context.Context) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_memory_chunks WHERE needs_graph_extraction = 1`,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Stats reads pending/done chunk counts + KG totals. SQLite has no
// FILTER clause; we issue separate COUNT queries.
func (r *ChunkGraphExtractionRepository) Stats(ctx context.Context) (*persistence.KGStats, error) {
	out := &persistence.KGStats{EntitiesByType: map[string]int{}}
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_memory_chunks WHERE needs_graph_extraction = 1`,
	).Scan(&out.ChunksPending); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_memory_chunks WHERE needs_graph_extraction = 0`,
	).Scan(&out.ChunksDone); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_entities`).Scan(&out.Entities); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_edges`).Scan(&out.Edges); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entity_mentions`).Scan(&out.Mentions); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT type, COUNT(*) FROM knowledge_entities GROUP BY type ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}
		out.EntitiesByType[t] = n
	}
	return out, rows.Err()
}

// ReflagChunksMissingEdges — see persistence.ChunkGraphExtractionRepository.
// The SQLite path is used by single-process development deployments and
// the integration test harness; the production multi-instance deploy
// runs on postgres where source_chunks is a native TEXT[]. We mirror
// the postgres semantics using SQLite's json_each over the TEXT-stored
// source_chunks payload so the same operator gesture works on both.
//
// Schema note: the migration that created knowledge_edges in SQLite
// stores source_chunks as a JSON TEXT array (no native array type),
// so the EXISTS check has to walk it via json_each.
func (r *ChunkGraphExtractionRepository) ReflagChunksMissingEdges(ctx context.Context, projectID string, countOnly bool) (int, error) {
	if projectID == "" {
		return 0, fmt.Errorf("ReflagChunksMissingEdges: project_id required")
	}
	const candidateSQL = `
		SELECT c.id
		FROM project_memory_chunks c
		WHERE c.project_id = ?
		  AND c.needs_graph_extraction = 0
		  AND NOT EXISTS (
		    SELECT 1
		    FROM knowledge_edges e, json_each(e.source_chunks) j
		    WHERE e.project_id = ?
		      AND e.lifecycle_state = 'published'
		      AND j.value = c.id
		  )`
	if countOnly {
		var n int
		err := r.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM ( `+candidateSQL+` )`,
			projectID, projectID).Scan(&n)
		if err != nil {
			return 0, err
		}
		return n, nil
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE project_memory_chunks
		SET    needs_graph_extraction = 1
		WHERE  id IN ( `+candidateSQL+` )`,
		projectID, projectID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
