package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// KnowledgeEntityRepository persists typed nouns derived from chunks.
//
// SimilarByEmbedding requires pgvector; SQLite has no equivalent
// operator, so the method returns ErrUnimplemented. The other
// surface (Insert/Get/List/UpdateLifecycle/AddAlias) is full
// CRUD on a single table.
type KnowledgeEntityRepository struct {
	db DBTX
}

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
		// (parity with the Postgres repo; UNIQUE(project_id, type,
		// canonical_name) stays the authoritative dedup key).
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
	now := time.Now().UTC()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
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
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ProjectID, e.Type, e.CanonicalName,
		nullableBlob(e.Aliases), e.Description, nullableBlob(e.Properties), encodeVectorBlob(e.Embedding),
		e.ExtractedBy, e.ResolvedBy, e.Confidence,
		e.LifecycleState, e.ValidationStatus, e.EpochID, sqliteTimePtr(e.ExpiresAt), e.SupersedesID,
		sqliteTime(e.CreatedAt), sqliteTime(e.UpdatedAt),
	)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return persistence.ErrDuplicateKey
	}
	return err
}

func (r *KnowledgeEntityRepository) Get(ctx context.Context, id string) (*persistence.KnowledgeEntity, error) {
	if id == "" {
		return nil, fmt.Errorf("KnowledgeEntityRepository.Get: id required")
	}
	row := r.db.QueryRowContext(ctx, knowledgeEntitySelect+` WHERE id = ?`, id)
	return scanKnowledgeEntity(row)
}

func (r *KnowledgeEntityRepository) GetByCanonical(ctx context.Context, projectID, entityType, canonicalName string) (*persistence.KnowledgeEntity, error) {
	if projectID == "" || entityType == "" || canonicalName == "" {
		return nil, fmt.Errorf("KnowledgeEntityRepository.GetByCanonical: project_id, type, canonical_name required")
	}
	row := r.db.QueryRowContext(ctx, knowledgeEntitySelect+`
		WHERE project_id = ? AND type = ? AND canonical_name = ?`,
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
	var b strings.Builder
	b.WriteString(knowledgeEntitySelect)
	b.WriteString(" WHERE project_id = ?")
	args := []any{filter.ProjectID}

	lifecyclePlaceholders := strings.Repeat("?,", len(lifecycle))
	lifecyclePlaceholders = lifecyclePlaceholders[:len(lifecyclePlaceholders)-1]
	b.WriteString(" AND lifecycle_state IN (" + lifecyclePlaceholders + ")")
	for _, l := range lifecycle {
		args = append(args, l)
	}
	if len(filter.Types) > 0 {
		typePlaceholders := strings.Repeat("?,", len(filter.Types))
		typePlaceholders = typePlaceholders[:len(typePlaceholders)-1]
		b.WriteString(" AND type IN (" + typePlaceholders + ")")
		for _, t := range filter.Types {
			args = append(args, t)
		}
	}
	if filter.NameLike != "" {
		b.WriteString(" AND canonical_name LIKE ?")
		args = append(args, "%"+filter.NameLike+"%")
	}
	b.WriteString(" ORDER BY canonical_name ASC LIMIT ? OFFSET ?")
	args = append(args, limit, filter.Offset)

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
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

// SimilarByEmbedding has no SQLite analog (no pgvector). Returns
// ErrUnimplemented so callers can opt to skip on SQLite without a
// silent empty-result false positive.
func (r *KnowledgeEntityRepository) SimilarByEmbedding(ctx context.Context, projectID, entityType string, embedding []float32, limit int) ([]*persistence.KnowledgeEntity, error) {
	return nil, ErrUnimplemented
}

func (r *KnowledgeEntityRepository) UpdateLifecycle(ctx context.Context, id, newState string) error {
	if id == "" || newState == "" {
		return fmt.Errorf("UpdateLifecycle: id + newState required")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE knowledge_entities
		SET lifecycle_state = ?, updated_at = ?
		WHERE id = ?`,
		newState, sqliteTime(time.Now().UTC()), id)
	return err
}

// AddAlias appends one alias atomically without duplicates. SQLite's
// json_insert + json_each pattern doesn't have a clean append-if-
// missing; we Get → mutate in Go → write back. Race window is fine
// for the test/demo workload.
func (r *KnowledgeEntityRepository) AddAlias(ctx context.Context, id, alias string) error {
	if id == "" || alias == "" {
		return fmt.Errorf("AddAlias: id + alias required")
	}
	var raw sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT aliases FROM knowledge_entities WHERE id = ?`, id).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistence.ErrNotFound
		}
		return err
	}
	var aliases []string
	if raw.Valid && raw.String != "" {
		if err := json.Unmarshal([]byte(raw.String), &aliases); err != nil {
			aliases = nil
		}
	}
	for _, a := range aliases {
		if a == alias {
			return nil // already present
		}
	}
	aliases = append(aliases, alias)
	enc, _ := json.Marshal(aliases)
	_, err = r.db.ExecContext(ctx, `
		UPDATE knowledge_entities
		SET aliases = ?, updated_at = ?
		WHERE id = ?`,
		string(enc), sqliteTime(time.Now().UTC()), id)
	return err
}

const knowledgeEntitySelect = `
SELECT id, project_id, type, canonical_name,
       aliases, description, properties, embedding,
       extracted_by, resolved_by, confidence,
       lifecycle_state, validation_status, epoch_id, expires_at, supersedes_id,
       created_at, updated_at
FROM knowledge_entities`

func scanKnowledgeEntity(scanner interface{ Scan(dest ...any) error }) (*persistence.KnowledgeEntity, error) {
	e := &persistence.KnowledgeEntity{}
	var (
		aliases, properties, embedding sql.NullString
		extractedBy, resolvedBy        sql.NullString
		epochID, supersedesID          sql.NullString
		expiresAt                      sqlNullTime
		createdAt, updatedAt           sqlTime
	)
	err := scanner.Scan(
		&e.ID, &e.ProjectID, &e.Type, &e.CanonicalName,
		&aliases, &e.Description, &properties, &embedding,
		&extractedBy, &resolvedBy, &e.Confidence,
		&e.LifecycleState, &e.ValidationStatus, &epochID, &expiresAt, &supersedesID,
		&createdAt, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	if aliases.Valid {
		e.Aliases = []byte(aliases.String)
	}
	if properties.Valid {
		e.Properties = []byte(properties.String)
	}
	if embedding.Valid {
		e.Embedding = decodeVectorBlob([]byte(embedding.String))
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
	if supersedesID.Valid {
		e.SupersedesID = &supersedesID.String
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		e.ExpiresAt = &t
	}
	e.CreatedAt = createdAt.Time
	e.UpdatedAt = updatedAt.Time
	return e, nil
}

// encodeVectorBlob/decodeVectorBlob persist a []float32 as a packed
// little-endian byte sequence. Not searchable on SQLite (no pgvector)
// but round-trips cleanly so an entity created on SQLite can be
// re-read with the original vector intact — useful when integration
// tests want to assert vector-passthrough.
func encodeVectorBlob(v []float32) any {
	if len(v) == 0 {
		return nil
	}
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func decodeVectorBlob(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}
