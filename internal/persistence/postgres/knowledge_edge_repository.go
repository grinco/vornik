package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/persistence"
)

// KnowledgeEdgeRepository is the Postgres backing for the
// knowledge_edges table. UpsertEdge implements the merge-on-conflict
// semantics from the LLD §3.2: identical (project, from, predicate,
// to) tuples collapse to one row whose source_chunks union grows.
type KnowledgeEdgeRepository struct {
	db DBTX
}

func NewKnowledgeEdgeRepository(db DBTX) *KnowledgeEdgeRepository {
	return &KnowledgeEdgeRepository{db: db}
}

func (r *KnowledgeEdgeRepository) UpsertEdge(ctx context.Context, e *persistence.KnowledgeEdge) error {
	if e == nil {
		return fmt.Errorf("KnowledgeEdgeRepository.UpsertEdge: nil edge")
	}
	if e.ProjectID == "" || e.FromEntity == "" || e.ToEntity == "" || e.Predicate == "" {
		return fmt.Errorf("UpsertEdge: project_id, from, to, predicate all required")
	}
	// No self-loops (also enforced by the knowledge_edges_no_self_loop DB
	// CHECK; this gives a clear error before the round trip).
	if e.FromEntity == e.ToEntity {
		return fmt.Errorf("UpsertEdge: self-loop edge (from == to == %q) not allowed", e.FromEntity)
	}
	if e.ID == "" {
		e.ID = persistence.GenerateID("kedge")
	}
	if e.LifecycleState == "" {
		e.LifecycleState = "published"
	}
	if e.Confidence == 0 {
		e.Confidence = 1.0
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if len(e.SourceChunks) == 0 {
		return fmt.Errorf("UpsertEdge: source_chunks required (≥1)")
	}

	// On conflict: append the new chunk(s) to source_chunks (no
	// duplicates), merge properties, take max(confidence,
	// faithfulness). lifecycle_state stays sticky on the existing
	// row — caller flips via UpdateLifecycle.
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO knowledge_edges (
			id, project_id, from_entity, to_entity, predicate,
			properties, source_chunks, extracted_by, confidence, faithfulness,
			lifecycle_state, epoch_id, created_at
		) VALUES (
			$1, $2, $3, $4, $5,
			COALESCE($6::jsonb, '{}'::jsonb), $7, $8, $9, $10,
			$11, $12, $13
		)
		ON CONFLICT (project_id, from_entity, predicate, to_entity) DO UPDATE
		SET source_chunks = (
		      SELECT array_agg(DISTINCT s)
		      FROM unnest(knowledge_edges.source_chunks || EXCLUDED.source_chunks) AS s
		    ),
		    properties = COALESCE(knowledge_edges.properties, '{}'::jsonb) || COALESCE(EXCLUDED.properties, '{}'::jsonb),
		    confidence = GREATEST(knowledge_edges.confidence, EXCLUDED.confidence),
		    faithfulness = GREATEST(
		        COALESCE(knowledge_edges.faithfulness, 0),
		        COALESCE(EXCLUDED.faithfulness, 0)
		    )`,
		e.ID, e.ProjectID, e.FromEntity, e.ToEntity, e.Predicate,
		jsonOrNull(e.Properties), pq.Array(e.SourceChunks), strOrNull(e.ExtractedBy), e.Confidence, e.Faithfulness,
		e.LifecycleState, e.EpochID, e.CreatedAt,
	)
	return mapDBError(err)
}

func (r *KnowledgeEdgeRepository) Get(ctx context.Context, id string) (*persistence.KnowledgeEdge, error) {
	if id == "" {
		return nil, fmt.Errorf("KnowledgeEdgeRepository.Get: id required")
	}
	row := r.db.QueryRowContext(ctx, knowledgeEdgeSelect+`WHERE id = $1`, id)
	return scanKnowledgeEdge(row)
}

func (r *KnowledgeEdgeRepository) List(ctx context.Context, filter persistence.KnowledgeEdgeFilter) ([]*persistence.KnowledgeEdge, error) {
	if filter.ProjectID == "" {
		return nil, fmt.Errorf("KnowledgeEdgeRepository.List: project_id required")
	}
	limit := filter.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	lifecycle := filter.Lifecycle
	if len(lifecycle) == 0 {
		lifecycle = []string{"published"}
	}

	q := knowledgeEdgeSelect + `WHERE project_id = $1 AND lifecycle_state = ANY($2)`
	args := []any{filter.ProjectID, pq.Array(lifecycle)}
	pos := 3
	if filter.FromEntity != "" {
		q += fmt.Sprintf(` AND from_entity = $%d`, pos)
		args = append(args, filter.FromEntity)
		pos++
	}
	if filter.ToEntity != "" {
		q += fmt.Sprintf(` AND to_entity = $%d`, pos)
		args = append(args, filter.ToEntity)
		pos++
	}
	if filter.Predicate != "" {
		q += fmt.Sprintf(` AND predicate = $%d`, pos)
		args = append(args, filter.Predicate)
		pos++
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT $%d`, pos)
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*persistence.KnowledgeEdge, 0)
	for rows.Next() {
		e, err := scanKnowledgeEdge(rows)
		if err != nil {
			return nil, err
		}
		if e != nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

func (r *KnowledgeEdgeRepository) EdgesForEntity(ctx context.Context, entityID string, limit int) ([]*persistence.KnowledgeEdge, error) {
	if entityID == "" {
		return nil, fmt.Errorf("EdgesForEntity: entityID required")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := knowledgeEdgeSelect + `
		WHERE (from_entity = $1 OR to_entity = $1)
		  AND lifecycle_state = 'published'
		ORDER BY created_at DESC
		LIMIT $2`
	rows, err := r.db.QueryContext(ctx, q, entityID, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*persistence.KnowledgeEdge, 0, limit)
	for rows.Next() {
		e, err := scanKnowledgeEdge(rows)
		if err != nil {
			return nil, err
		}
		if e != nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

func (r *KnowledgeEdgeRepository) UpdateLifecycle(ctx context.Context, id, newState string) error {
	if id == "" || newState == "" {
		return fmt.Errorf("UpdateLifecycle: id + newState required")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE knowledge_edges SET lifecycle_state = $2 WHERE id = $1`,
		id, newState)
	return mapDBError(err)
}

func (r *KnowledgeEdgeRepository) DropChunkFromSources(ctx context.Context, chunkID string) (int, error) {
	if chunkID == "" {
		return 0, fmt.Errorf("DropChunkFromSources: chunk_id required")
	}
	// Remove chunkID from every source_chunks array. Then for any
	// edge whose source_chunks went empty, flip lifecycle to
	// quarantined (no remaining evidence supports the edge).
	res, err := r.db.ExecContext(ctx, `
		WITH updated AS (
		    UPDATE knowledge_edges
		    SET source_chunks = array_remove(source_chunks, $1)
		    WHERE $1 = ANY(source_chunks)
		    RETURNING id, source_chunks
		)
		UPDATE knowledge_edges e
		SET lifecycle_state = 'quarantined'
		FROM updated u
		WHERE e.id = u.id AND cardinality(u.source_chunks) = 0`,
		chunkID)
	if err != nil {
		return 0, mapDBError(err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---------- helpers ----------

const knowledgeEdgeSelect = `
	SELECT id, project_id, from_entity, to_entity, predicate,
	       properties, source_chunks, extracted_by, confidence, faithfulness,
	       lifecycle_state, epoch_id, created_at
	FROM knowledge_edges `

func scanKnowledgeEdge(scanner interface {
	Scan(dest ...any) error
}) (*persistence.KnowledgeEdge, error) {
	var (
		e            persistence.KnowledgeEdge
		properties   sql.NullString
		sourceChunks pq.StringArray
		extractedBy  sql.NullString
		faithfulness sql.NullFloat64
		epochID      sql.NullString
	)
	err := scanner.Scan(
		&e.ID, &e.ProjectID, &e.FromEntity, &e.ToEntity, &e.Predicate,
		&properties, &sourceChunks, &extractedBy, &e.Confidence, &faithfulness,
		&e.LifecycleState, &epochID, &e.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, mapDBError(err)
	}
	if properties.Valid {
		e.Properties = []byte(properties.String)
	}
	e.SourceChunks = []string(sourceChunks)
	if extractedBy.Valid {
		e.ExtractedBy = extractedBy.String
	}
	if faithfulness.Valid {
		f := float32(faithfulness.Float64)
		e.Faithfulness = &f
	}
	if epochID.Valid {
		e.EpochID = &epochID.String
	}
	return &e, nil
}
