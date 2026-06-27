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
func (r *EntityMentionRepository) ListByEntity(ctx context.Context, entityID string, limit int) ([]*persistence.EntityMention, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT chunk_id, entity_id, char_start, char_end, surface
		FROM entity_mentions
		WHERE entity_id = ?
		ORDER BY chunk_id DESC
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
