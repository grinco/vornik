package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// ChannelSessionRepository implements
// persistence.ChannelSessionRepository against PostgreSQL. The
// (kind, session_id) composite PK keeps webchat/email/slack/github
// namespaces isolated within a single table; Save uses ON CONFLICT
// DO UPDATE so the post-turn write is one round trip.
type ChannelSessionRepository struct {
	db DBTX
}

// NewChannelSessionRepository constructs the repo over db.
func NewChannelSessionRepository(db DBTX) *ChannelSessionRepository {
	return &ChannelSessionRepository{db: db}
}

// Load returns the session row. ErrNotFound when no row exists —
// callers treat that as a fresh empty session.
//
// History is returned as the raw JSONB bytes; the caller
// unmarshals into []chat.Message at the channel boundary so this
// package doesn't import internal/chat.
func (r *ChannelSessionRepository) Load(ctx context.Context, kind, sessionID string) (*persistence.ChannelSession, error) {
	if kind == "" || sessionID == "" {
		return nil, fmt.Errorf("channel_session: kind + sessionID required")
	}
	const q = `
SELECT kind, session_id, COALESCE(active_project, ''), history, created_at, updated_at, expires_at
FROM channel_sessions
WHERE kind = $1 AND session_id = $2`
	row := r.db.QueryRowContext(ctx, q, kind, sessionID)
	var (
		s         persistence.ChannelSession
		expiresAt sql.NullTime
	)
	if err := row.Scan(&s.Kind, &s.SessionID, &s.ActiveProject, &s.History, &s.CreatedAt, &s.UpdatedAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("channel_session: load: %w", err)
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		s.ExpiresAt = &t
	}
	return &s, nil
}

// Save upserts the session. The DO UPDATE branch always wins
// because the post-turn write is authoritative — there is no
// per-session contention semantics to honour (a session is
// driven by one inbound channel message at a time).
func (r *ChannelSessionRepository) Save(ctx context.Context, kind, sessionID, activeProject string, historyJSON []byte) error {
	if kind == "" || sessionID == "" {
		return fmt.Errorf("channel_session: kind + sessionID required")
	}
	// Empty payload becomes the JSONB empty array so the column's
	// NOT NULL constraint is satisfied and downstream readers see
	// a sane default rather than a Postgres NULL.
	if len(historyJSON) == 0 {
		historyJSON = []byte("[]")
	}
	var ap sql.NullString
	if activeProject != "" {
		ap = sql.NullString{String: activeProject, Valid: true}
	}
	const q = `
INSERT INTO channel_sessions (kind, session_id, active_project, history, created_at, updated_at)
VALUES ($1, $2, $3, $4, NOW(), NOW())
ON CONFLICT (kind, session_id) DO UPDATE
SET active_project = EXCLUDED.active_project,
    history = EXCLUDED.history,
    updated_at = NOW()`
	if _, err := r.db.ExecContext(ctx, q, kind, sessionID, ap, historyJSON); err != nil {
		return fmt.Errorf("channel_session: save: %w", err)
	}
	return nil
}

// Delete removes the session. Idempotent — no error when the row
// doesn't exist; the caller's "clear chat" affordance shouldn't
// fail for a session that was never persisted.
func (r *ChannelSessionRepository) Delete(ctx context.Context, kind, sessionID string) error {
	const q = `DELETE FROM channel_sessions WHERE kind = $1 AND session_id = $2`
	if _, err := r.db.ExecContext(ctx, q, kind, sessionID); err != nil {
		return fmt.Errorf("channel_session: delete: %w", err)
	}
	return nil
}
