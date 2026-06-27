package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/persistence"
)

// KnowledgeEntityRepository is the Postgres backing for the
// knowledge_entities table introduced in migration v26 (Phase 43+
// of the knowledge-graph memory roadmap; see
// https://docs.vornik.io).
type KnowledgeEntityRepository struct {
	db DBTX
}

// NewKnowledgeEntityRepository constructs the repo over a DBTX.
func NewKnowledgeEntityRepository(db DBTX) *KnowledgeEntityRepository {
	return &KnowledgeEntityRepository{db: db}
}

func (r *KnowledgeEntityRepository) Insert(ctx context.Context, e *persistence.KnowledgeEntity) error {
	if e == nil {
		return fmt.Errorf("KnowledgeEntityRepository.Insert: nil entity")
	}
	if e.ProjectID == "" || e.Type == "" || e.CanonicalName == "" {
		return fmt.Errorf("KnowledgeEntityRepository.Insert: project_id, type, canonical_name required")
	}
	if e.ID == "" {
		// Deterministic ID from the identity triple → idempotent extraction
		// (same entity always maps to the same ID; the UNIQUE(project_id,
		// type, canonical_name) constraint stays the authoritative dedup key).
		e.ID = persistence.DeterministicEntityID(e.ProjectID, e.Type, e.CanonicalName)
	}
	if e.LifecycleState == "" {
		e.LifecycleState = "published"
	}
	if e.ValidationStatus == "" {
		e.ValidationStatus = "unverified"
	}
	if e.Confidence == 0 {
		e.Confidence = 1.0
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = e.CreatedAt
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO knowledge_entities (
			id, project_id, type, canonical_name,
			aliases, description, properties, embedding,
			extracted_by, resolved_by, confidence,
			lifecycle_state, validation_status, epoch_id, expires_at, supersedes_id,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, $4,
			COALESCE($5::jsonb, '[]'::jsonb), $6, COALESCE($7::jsonb, '{}'::jsonb), $8,
			$9, $10, $11,
			$12, $13, $14, $15, $16,
			$17, $18
		)`,
		e.ID, e.ProjectID, e.Type, e.CanonicalName,
		jsonOrNull(e.Aliases), e.Description, jsonOrNull(e.Properties), encodeVector(e.Embedding),
		strOrNull(e.ExtractedBy), strOrNull(e.ResolvedBy), e.Confidence,
		e.LifecycleState, e.ValidationStatus, e.EpochID, e.ExpiresAt, e.SupersedesID,
		e.CreatedAt, e.UpdatedAt,
	)
	return mapDBError(err)
}

func (r *KnowledgeEntityRepository) Get(ctx context.Context, id string) (*persistence.KnowledgeEntity, error) {
	if id == "" {
		return nil, fmt.Errorf("KnowledgeEntityRepository.Get: id required")
	}
	row := r.db.QueryRowContext(ctx, knowledgeEntitySelect+`WHERE id = $1`, id)
	return scanKnowledgeEntity(row)
}

func (r *KnowledgeEntityRepository) GetByCanonical(ctx context.Context, projectID, entityType, canonicalName string) (*persistence.KnowledgeEntity, error) {
	if projectID == "" || entityType == "" || canonicalName == "" {
		return nil, fmt.Errorf("KnowledgeEntityRepository.GetByCanonical: project_id, type, canonical_name required")
	}
	row := r.db.QueryRowContext(ctx,
		knowledgeEntitySelect+`WHERE project_id = $1 AND type = $2 AND canonical_name = $3`,
		projectID, entityType, canonicalName)
	return scanKnowledgeEntity(row)
}

func (r *KnowledgeEntityRepository) List(ctx context.Context, filter persistence.KnowledgeEntityFilter) ([]*persistence.KnowledgeEntity, error) {
	if filter.ProjectID == "" {
		return nil, fmt.Errorf("KnowledgeEntityRepository.List: project_id required")
	}
	limit := filter.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	lifecycle := filter.Lifecycle
	if len(lifecycle) == 0 {
		lifecycle = []string{"published"}
	}

	q := knowledgeEntitySelect + `WHERE project_id = $1 AND lifecycle_state = ANY($2)`
	args := []any{filter.ProjectID, pq.Array(lifecycle)}
	pos := 3
	if len(filter.Types) > 0 {
		q += fmt.Sprintf(` AND type = ANY($%d)`, pos)
		args = append(args, pq.Array(filter.Types))
		pos++
	}
	if filter.NameLike != "" {
		q += fmt.Sprintf(` AND canonical_name ILIKE $%d`, pos)
		args = append(args, "%"+filter.NameLike+"%")
		pos++
	}
	q += fmt.Sprintf(` ORDER BY canonical_name ASC LIMIT $%d OFFSET $%d`, pos, pos+1)
	args = append(args, limit, filter.Offset)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*persistence.KnowledgeEntity, 0)
	for rows.Next() {
		e, err := scanKnowledgeEntity(rows)
		if err != nil {
			return nil, err
		}
		if e != nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

func (r *KnowledgeEntityRepository) SimilarByEmbedding(ctx context.Context, projectID, entityType string, embedding []float32, limit int) ([]*persistence.KnowledgeEntity, error) {
	if len(embedding) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := knowledgeEntitySelect + `
		WHERE project_id = $1 AND type = $2 AND lifecycle_state = 'published' AND embedding IS NOT NULL
		ORDER BY embedding <=> $3
		LIMIT $4`
	rows, err := r.db.QueryContext(ctx, q, projectID, entityType, encodeVector(embedding), limit)
	if err != nil {
		// pgvector may not be installed — degrade gracefully.
		if strings.Contains(err.Error(), "operator does not exist") {
			return nil, nil
		}
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*persistence.KnowledgeEntity, 0, limit)
	for rows.Next() {
		e, err := scanKnowledgeEntity(rows)
		if err != nil {
			return nil, err
		}
		if e != nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

func (r *KnowledgeEntityRepository) UpdateLifecycle(ctx context.Context, id, newState string) error {
	if id == "" || newState == "" {
		return fmt.Errorf("UpdateLifecycle: id + newState required")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE knowledge_entities
		SET    lifecycle_state = $2, updated_at = now()
		WHERE  id = $1`,
		id, newState)
	return mapDBError(err)
}

func (r *KnowledgeEntityRepository) AddAlias(ctx context.Context, id, alias string) error {
	if id == "" || alias == "" {
		return fmt.Errorf("AddAlias: id + alias required")
	}
	// JSONB || a single-element array works only for objects;
	// for arrays we use the dedicated function jsonb_insert.
	// Use a defensive approach: only insert when the alias isn't
	// already in the array (no duplicates).
	_, err := r.db.ExecContext(ctx, `
		UPDATE knowledge_entities
		SET    aliases = CASE
		           WHEN aliases @> to_jsonb($2::text) THEN aliases
		           ELSE aliases || jsonb_build_array($2::text)
		       END,
		       updated_at = now()
		WHERE  id = $1`,
		id, alias)
	return mapDBError(err)
}

// ---------- helpers ----------

const knowledgeEntitySelect = `
	SELECT id, project_id, type, canonical_name,
	       aliases, description, properties, embedding,
	       extracted_by, resolved_by, confidence,
	       lifecycle_state, validation_status, epoch_id, expires_at, supersedes_id,
	       created_at, updated_at
	FROM knowledge_entities `

func scanKnowledgeEntity(scanner interface {
	Scan(dest ...any) error
}) (*persistence.KnowledgeEntity, error) {
	var (
		e            persistence.KnowledgeEntity
		aliases      sql.NullString
		properties   sql.NullString
		embedding    sql.NullString
		extractedBy  sql.NullString
		resolvedBy   sql.NullString
		epochID      sql.NullString
		expiresAt    sql.NullTime
		supersedesID sql.NullString
	)
	err := scanner.Scan(
		&e.ID, &e.ProjectID, &e.Type, &e.CanonicalName,
		&aliases, &e.Description, &properties, &embedding,
		&extractedBy, &resolvedBy, &e.Confidence,
		&e.LifecycleState, &e.ValidationStatus, &epochID, &expiresAt, &supersedesID,
		&e.CreatedAt, &e.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, mapDBError(err)
	}
	if aliases.Valid {
		e.Aliases = []byte(aliases.String)
	}
	if properties.Valid {
		e.Properties = []byte(properties.String)
	}
	if embedding.Valid {
		e.Embedding = decodeVector(embedding.String)
	}
	if extractedBy.Valid {
		e.ExtractedBy = extractedBy.String
	}
	if resolvedBy.Valid {
		e.ResolvedBy = resolvedBy.String
	}
	if epochID.Valid {
		e.EpochID = &epochID.String
	}
	if expiresAt.Valid {
		e.ExpiresAt = &expiresAt.Time
	}
	if supersedesID.Valid {
		e.SupersedesID = &supersedesID.String
	}
	return &e, nil
}

// encodeVector renders a []float32 as the pgvector text format
// `[0.1,0.2,…]`. Empty / nil slice returns nil so the column
// lands as SQL NULL.
func encodeVector(v []float32) any {
	if len(v) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%g", f)
	}
	sb.WriteByte(']')
	return sb.String()
}

// decodeVector parses the pgvector text format back into a
// []float32. Defensive: returns nil on any malformed input
// (caller treats nil as "no embedding available").
func decodeVector(s string) []float32 {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil
	}
	body := s[1 : len(s)-1]
	if body == "" {
		return nil
	}
	parts := strings.Split(body, ",")
	out := make([]float32, 0, len(parts))
	for _, p := range parts {
		var f float32
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%g", &f); err != nil {
			return nil
		}
		out = append(out, f)
	}
	return out
}

func strOrNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}
