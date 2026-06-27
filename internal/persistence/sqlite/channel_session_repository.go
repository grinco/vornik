package sqlite

import (
	"context"
	"database/sql"

	"vornik.io/vornik/internal/persistence"
)

// ChannelSessionRepository is the SQLite stub. Single-process
// deployments keep their in-memory map as the authoritative
// state, so the repo presents itself as "always empty": Load
// returns ErrNotFound (channels fall back to their in-memory
// cache), Save is a no-op (the cache already holds the same
// data), Delete is a no-op.
//
// Wiring the interface uniformly across both backends means the
// channel implementations never branch on backend — they just call
// Load/Save and let the repo's semantics decide whether the call
// is durable.
type ChannelSessionRepository struct {
	_ *sql.DB
}

// NewChannelSessionRepository returns the stub.
func NewChannelSessionRepository(db *sql.DB) *ChannelSessionRepository {
	return &ChannelSessionRepository{}
}

// Load always returns ErrNotFound. Channels treat that the same
// as "session not in DB yet"; their in-memory cache supplies the
// history.
func (r *ChannelSessionRepository) Load(_ context.Context, _, _ string) (*persistence.ChannelSession, error) {
	return nil, persistence.ErrNotFound
}

// Save is a no-op. The caller's in-memory map already holds the
// post-turn state; no DB write would change behaviour.
func (r *ChannelSessionRepository) Save(_ context.Context, _, _, _ string, _ []byte) error {
	return nil
}

// Delete is a no-op for the same reason as Save.
func (r *ChannelSessionRepository) Delete(_ context.Context, _, _ string) error {
	return nil
}
