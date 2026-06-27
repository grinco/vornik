package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// EntityMentionRepository is the Postgres backing for the
// entity_mentions table — chunk ↔ entity link rows.
type EntityMentionRepository struct {
	db DBTX
}

func NewEntityMentionRepository(db DBTX) *EntityMentionRepository {
	return &EntityMentionRepository{db: db}
}

func (r *EntityMentionRepository) Insert(ctx context.Context, m *persistence.EntityMention) error {
	if m == nil {
		return fmt.Errorf("EntityMentionRepository.Insert: nil mention")
	}
	if m.ChunkID == "" || m.EntityID == "" {
		return fmt.Errorf("Insert: chunk_id + entity_id required")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO entity_mentions (chunk_id, entity_id, char_start, char_end, surface)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (chunk_id, entity_id, char_start) DO NOTHING`,
		m.ChunkID, m.EntityID, m.CharStart, m.CharEnd, strOrNull(m.Surface),
	)
	return mapDBError(err)
}

func (r *EntityMentionRepository) ListByEntity(ctx context.Context, entityID string, limit int) ([]*persistence.EntityMention, error) {
	if entityID == "" {
		return nil, fmt.Errorf("ListByEntity: entity_id required")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT m.chunk_id, m.entity_id, m.char_start, m.char_end, m.surface
		FROM entity_mentions m
		JOIN project_memory_chunks c ON c.id = m.chunk_id
		WHERE m.entity_id = $1
		ORDER BY c.created_at DESC
		LIMIT $2`,
		entityID, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	return scanMentions(rows)
}

func (r *EntityMentionRepository) ListByChunk(ctx context.Context, chunkID string) ([]*persistence.EntityMention, error) {
	if chunkID == "" {
		return nil, fmt.Errorf("ListByChunk: chunk_id required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT chunk_id, entity_id, char_start, char_end, surface
		FROM entity_mentions
		WHERE chunk_id = $1
		ORDER BY char_start ASC`,
		chunkID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	return scanMentions(rows)
}

func (r *EntityMentionRepository) DeleteForChunk(ctx context.Context, chunkID string) error {
	if chunkID == "" {
		return fmt.Errorf("DeleteForChunk: chunk_id required")
	}
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM entity_mentions WHERE chunk_id = $1`, chunkID)
	return mapDBError(err)
}

func scanMentions(rows *sql.Rows) ([]*persistence.EntityMention, error) {
	out := make([]*persistence.EntityMention, 0)
	for rows.Next() {
		var m persistence.EntityMention
		var charEnd sql.NullInt32
		var surface sql.NullString
		if err := rows.Scan(&m.ChunkID, &m.EntityID, &m.CharStart, &charEnd, &surface); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, mapDBError(err)
		}
		if charEnd.Valid {
			ce := int(charEnd.Int32)
			m.CharEnd = &ce
		}
		if surface.Valid {
			m.Surface = surface.String
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}
