package postgres

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ChannelDisclosureRepository is the Postgres implementation of the EU AI Act
// Art 50 disclosure record (migration 139).
type ChannelDisclosureRepository struct {
	db persistence.DBTX
}

// NewChannelDisclosureRepository builds the repository.
func NewChannelDisclosureRepository(db persistence.DBTX) *ChannelDisclosureRepository {
	return &ChannelDisclosureRepository{db: db}
}

var _ persistence.ChannelDisclosureRepository = (*ChannelDisclosureRepository)(nil)

// WasServed reports whether this (channel, session) has been disclosed to.
func (r *ChannelDisclosureRepository) WasServed(ctx context.Context, channel, sessionID string) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(
		     SELECT 1 FROM channel_disclosure_log
		      WHERE channel = $1 AND session_id = $2)`,
		channel, sessionID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("postgres: channel_disclosure_log exists: %w", err)
	}
	return exists, nil
}

// MarkServed records the disclosure. ON CONFLICT DO NOTHING makes the write
// idempotent, so two concurrent first turns in one session cannot produce a
// second row — the PK is the serialisation point.
func (r *ChannelDisclosureRepository) MarkServed(ctx context.Context, channel, sessionID, textHash string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO channel_disclosure_log (channel, session_id, served_at, text_hash)
		      VALUES ($1, $2, NOW(), $3)
		 ON CONFLICT (channel, session_id) DO NOTHING`,
		channel, sessionID, textHash,
	)
	if err != nil {
		return fmt.Errorf("postgres: channel_disclosure_log insert: %w", err)
	}
	return nil
}

// ServedBetween is the Art 99 enforcement-response query.
func (r *ChannelDisclosureRepository) ServedBetween(ctx context.Context, from, to time.Time) ([]persistence.ChannelDisclosure, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT channel, session_id, served_at, text_hash
		   FROM channel_disclosure_log
		  WHERE served_at BETWEEN $1 AND $2
		  ORDER BY served_at`,
		from.UTC(), to.UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: channel_disclosure_log select: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []persistence.ChannelDisclosure
	for rows.Next() {
		var d persistence.ChannelDisclosure
		if err := rows.Scan(&d.Channel, &d.SessionID, &d.ServedAt, &d.TextHash); err != nil {
			return nil, fmt.Errorf("postgres: channel_disclosure_log scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: channel_disclosure_log rows: %w", err)
	}
	return out, nil
}
