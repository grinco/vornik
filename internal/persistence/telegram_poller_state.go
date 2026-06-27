package persistence

import "context"

// TelegramPollerState is the persisted long-poll watermark
// telegram's pollLoop reads on leader-acquired and writes after
// each batch of getUpdates returns. A single-row table keyed
// by `bot_id` (typically the bot's @username — same value
// across replicas) so a multi-replica failover resumes from
// the last-confirmed offset rather than replaying everything
// Telegram has queued for the bot since the last confirm.
//
// Why this lives next to leader-election: the gate alone
// achieves steady-state at-most-once delivery, but a leader-
// failover would reset the in-memory offset to 0 and Telegram
// would replay every queued update (bounded ~24h / 100 updates
// but still visibly duplicated for the user). The persisted
// offset closes that window.
type TelegramPollerState struct {
	// BotID identifies the telegram bot — typically the bot's
	// @username or a stable operator-supplied label. Single-bot
	// deployments can leave this as a fixed sentinel (the
	// container picks one and reuses it).
	BotID string
	// Offset is the next getUpdates offset to request. Matches
	// Telegram's confirmed-offset semantics — passing this back
	// in `getUpdates?offset=N` confirms everything < N and
	// returns updates with id >= N.
	Offset int64
}

// TelegramPollerStateRepository persists the long-poll
// watermark. SQLite stub returns ErrNotFound on Get + no-op
// Set (single-process deployments don't need cross-restart
// persistence — the in-memory offset is sufficient).
type TelegramPollerStateRepository interface {
	// Get returns the persisted state for the bot. ErrNotFound
	// when no row exists yet — caller starts from offset=0 (the
	// pre-feature behaviour).
	Get(ctx context.Context, botID string) (*TelegramPollerState, error)

	// Set upserts the row. Called after each batch of
	// getUpdates returns successfully — best-effort on write
	// errors (the in-memory offset advances regardless, so a
	// brief DB outage doesn't stall message processing).
	Set(ctx context.Context, state *TelegramPollerState) error
}
