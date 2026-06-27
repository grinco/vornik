package sqlite

import (
	"context"
	"database/sql"

	"vornik.io/vornik/internal/persistence"
)

// TelegramPollerStateRepository is the SQLite stub. Single-
// process deployments don't have multi-replica failover, so
// the in-memory offset in the bot is authoritative — every Get
// returns ErrNotFound (caller starts at 0) and every Set is a
// no-op. SQLite operators tolerate the brief duplicate-replay
// window after a daemon restart (it's the legacy behaviour
// pre-leader-election).
type TelegramPollerStateRepository struct {
	_ *sql.DB
}

// NewTelegramPollerStateRepository returns the stub.
func NewTelegramPollerStateRepository(db *sql.DB) *TelegramPollerStateRepository {
	return &TelegramPollerStateRepository{}
}

// Get always returns ErrNotFound — callers fall back to
// offset=0 (pre-feature behaviour).
func (r *TelegramPollerStateRepository) Get(_ context.Context, _ string) (*persistence.TelegramPollerState, error) {
	return nil, persistence.ErrNotFound
}

// Set is a no-op for the stub.
func (r *TelegramPollerStateRepository) Set(_ context.Context, _ *persistence.TelegramPollerState) error {
	return nil
}
