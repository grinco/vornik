package sqlite

import (
	"context"
	"database/sql"

	"vornik.io/vornik/internal/persistence"
)

// EntityMentionRepository persists chunk ↔ entity links.
type EntityMentionRepository struct {
	db DBTX
}

func NewEntityMentionRepository(db DBTX) *EntityMentionRepository {
	return &EntityMentionRepository{db: db}
}

// Insert writes one mention. INSERT OR IGNORE on the composite PK
// makes duplicate writes a no-op (matches the postgres semantics).
func (r *EntityMentionRepository) Insert(ctx context.Context, m *persistence.EntityMention) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO entity_mentions (chunk_id, entity_id, char_start, char_end, surface)
		VALUES (?, ?, ?, ?, ?)`,
		m.ChunkID, m.EntityID, m.CharStart, m.CharEnd, m.Surface)
	return err
}

// ListByEntity returns the mentions for one entity, newest chunk first.
//
// Joins project_memory_chunks for the ORDER and the FILTER, as the Postgres
// twin does. Until 2026-09-09 this ordered by chunk_id — which is not time,
// so "newest first" held only when ids happened to sort that way — and read
// entity_mentions alone, so a mention whose chunk row was gone (foreign keys
// are OFF on this driver; any deletion path that does not sweep mentions
// strands one) was listed with nothing behind it. The shared repotest suite
// now pins the order on both backends; the orphan case is pinned here.
func (r *EntityMentionRepository) ListByEntity(ctx context.Context, entityID string, limit int) ([]*persistence.EntityMention, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT m.chunk_id, m.entity_id, m.char_start, m.char_end, m.surface
		FROM entity_mentions m
		JOIN project_memory_chunks c ON c.id = m.chunk_id
		WHERE m.entity_id = ?
		ORDER BY c.created_at DESC
		LIMIT ?`, entityID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanMentions(rows)
}

// ListByChunk returns the entities mentioned in one chunk.
func (r *EntityMentionRepository) ListByChunk(ctx context.Context, chunkID string) ([]*persistence.EntityMention, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT chunk_id, entity_id, char_start, char_end, surface
		FROM entity_mentions
		WHERE chunk_id = ?
		ORDER BY char_start ASC`, chunkID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanMentions(rows)
}

// DeleteForChunk removes every mention for one chunk.
func (r *EntityMentionRepository) DeleteForChunk(ctx context.Context, chunkID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM entity_mentions WHERE chunk_id = ?`, chunkID)
	return err
}

func scanMentions(rows *sql.Rows) ([]*persistence.EntityMention, error) {
	var out []*persistence.EntityMention
	for rows.Next() {
		var (
			m       persistence.EntityMention
			charEnd sql.NullInt64
		)
		if err := rows.Scan(&m.ChunkID, &m.EntityID, &m.CharStart, &charEnd, &m.Surface); err != nil {
			return nil, err
		}
		if charEnd.Valid {
			v := int(charEnd.Int64)
			m.CharEnd = &v
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}
