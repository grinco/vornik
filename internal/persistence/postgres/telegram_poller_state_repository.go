package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// TelegramPollerStateRepository persists the long-poll offset
// watermark across daemon restarts + replica failover. Single-
// row upserts keyed by bot_id; the table holds at most a
// handful of rows (one per bot the daemon proxies for).
type TelegramPollerStateRepository struct {
	db DBTX
}

// NewTelegramPollerStateRepository constructs the repo over db.
func NewTelegramPollerStateRepository(db DBTX) *TelegramPollerStateRepository {
	return &TelegramPollerStateRepository{db: db}
}

// Get returns the persisted offset for botID.
func (r *TelegramPollerStateRepository) Get(ctx context.Context, botID string) (*persistence.TelegramPollerState, error) {
	if botID == "" {
		return nil, fmt.Errorf("telegram_poller_state: bot_id required")
	}
	const q = `SELECT bot_id, offset_value FROM telegram_poller_state WHERE bot_id = $1`
	row := r.db.QueryRowContext(ctx, q, botID)
	var state persistence.TelegramPollerState
	if err := row.Scan(&state.BotID, &state.Offset); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("telegram_poller_state: get: %w", err)
	}
	return &state, nil
}

// Set upserts the row.
func (r *TelegramPollerStateRepository) Set(ctx context.Context, state *persistence.TelegramPollerState) error {
	if state == nil || state.BotID == "" {
		return fmt.Errorf("telegram_poller_state: bot_id required")
	}
	const q = `
INSERT INTO telegram_poller_state (bot_id, offset_value, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (bot_id) DO UPDATE
SET offset_value = EXCLUDED.offset_value,
    updated_at   = EXCLUDED.updated_at
WHERE telegram_poller_state.offset_value <= EXCLUDED.offset_value`
	if _, err := r.db.ExecContext(ctx, q, state.BotID, state.Offset); err != nil {
		return fmt.Errorf("telegram_poller_state: set: %w", err)
	}
	return nil
}
