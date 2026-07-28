package persistence

import (
	"context"
	"time"
)

// ChannelSession is one row in the `channel_sessions` table —
// the per-channel conversation snapshot that survives daemon
// restarts and lets a replica pick up a conversation the
// originating replica started.
//
// History is the JSON-encoded chat.Message slice. The
// persistence layer holds raw bytes (not chat.Message) so
// channel_session.go doesn't need to import internal/chat —
// which already imports persistence, so the dependency must run
// in one direction. Callers marshal at their boundary.
//
// Channels:
//   - webchat: SessionID = cookie hash
//   - email:   SessionID = RFC822 thread root message-id
//   - slack:   SessionID = channel.thread_ts
//   - github:  SessionID = "owner/repo#issues/N" / "...#pulls/N"
//   - telegram (future phase): SessionID = chat_id (numeric, stringified)
type ChannelSession struct {
	Kind          string
	SessionID     string
	ActiveProject string
	History       []byte // JSON-encoded []chat.Message
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ExpiresAt     *time.Time
}

// ChannelSessionRepository persists per-channel session state.
// Implementations:
//   - Postgres: durable, multi-replica safe. Save uses ON CONFLICT
//     (kind, session_id) DO UPDATE so the post-turn write is one
//     round trip.
//   - SQLite: returns ErrNotFound on Load and is a no-op on Save —
//     single-process deployments keep their in-memory map as the
//     authoritative state. Wiring the interface uniformly means
//     channel implementations never have to branch on backend.
type ChannelSessionRepository interface {
	// Load returns the session for (kind, sessionID). ErrNotFound
	// for a session that has never been persisted — channels treat
	// that the same as "fresh session, empty history".
	Load(ctx context.Context, kind, sessionID string) (*ChannelSession, error)

	// Save upserts the session. activeProject may be empty (channel
	// hasn't pinned one yet). historyJSON is the JSON-encoded full
	// post-turn slice the caller marshalled — channels never store
	// partial history (Result.Messages is authoritative).
	Save(ctx context.Context, kind, sessionID, activeProject string, historyJSON []byte) error

	// Delete removes the session. Used by the webchat "clear chat"
	// affordance and the future stale-session sweeper. No error
	// when the row doesn't exist — idempotent.
	Delete(ctx context.Context, kind, sessionID string) error

	// ListByPrefix returns sessions whose SessionID starts with prefix,
	// most-recently-updated first, capped at limit (<=0 yields no rows).
	//
	// Exists because a SessionID encodes its own container: every Slack
	// thread in one channel shares the prefix "<team>/<channel>#", so
	// "the other conversations in this channel" is a prefix query rather
	// than a new data model. Used to surface what was discussed in a
	// channel's threads to a channel-level turn — Slack threads are
	// unfindable days later, so users follow up in the channel instead.
	//
	// prefix is matched literally: implementations MUST escape LIKE
	// metacharacters so a session id containing '%' or '_' cannot widen
	// the match.
	ListByPrefix(ctx context.Context, kind, prefix string, limit int) ([]*ChannelSession, error)
}
