package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// UISessionRepository implements persistence.UISessionRepository on
// Postgres. Token-hash lookup filters revoked rows in SQL; expiry and
// idle-timeout checks belong to the session store (it owns the clock).
// One row per browser login session (migration 91).
type UISessionRepository struct {
	db DBTX
}

// NewUISessionRepository constructs the repository.
func NewUISessionRepository(db DBTX) *UISessionRepository {
	return &UISessionRepository{db: db}
}

// CreateSession inserts a new session row. Empty IP/UserAgent are stored
// as NULL so future retention filters (WHERE ip IS NULL) work correctly.
func (r *UISessionRepository) CreateSession(ctx context.Context, s *persistence.UISession) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO ui_sessions (id, token_hash, user_id, provider, created_at, last_seen_at, expires_at, ip, user_agent) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		s.ID, s.TokenHash, s.UserID, s.Provider,
		s.CreatedAt, s.LastSeenAt, s.ExpiresAt,
		sql.NullString{String: s.IP, Valid: s.IP != ""},
		sql.NullString{String: s.UserAgent, Valid: s.UserAgent != ""})
	return mapDBError(err)
}

// GetActiveByTokenHash returns the non-revoked session matching
// tokenHash, or ErrSessionNotFound when no row matches.
func (r *UISessionRepository) GetActiveByTokenHash(ctx context.Context, tokenHash string) (*persistence.UISession, error) {
	var s persistence.UISession
	var ip, ua sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT id, token_hash, user_id, provider, created_at, last_seen_at, expires_at, revoked_at, ip, user_agent FROM ui_sessions WHERE token_hash = $1 AND revoked_at IS NULL`,
		tokenHash).Scan(
		&s.ID, &s.TokenHash, &s.UserID, &s.Provider,
		&s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt, &s.RevokedAt,
		&ip, &ua)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrSessionNotFound
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	s.IP = ip.String
	s.UserAgent = ua.String
	return &s, nil
}

// TouchSession updates last_seen_at to NOW(). Best-effort: zero rows
// affected is not an error (the session may have been concurrently
// revoked between the caller's auth check and this async touch).
//
// A non-empty ip is refreshed alongside last_seen_at so the admin
// session viewer reflects the session's current source rather than the
// frozen login-time IP (the "stale IP" reported 2026-06-23). An empty ip
// leaves the stored value untouched — a non-middleware/test caller with
// no resolved IP must not blank out a real one.
func (r *UISessionRepository) TouchSession(ctx context.Context, id, ip string) error {
	var err error
	if ip == "" {
		_, err = r.db.ExecContext(ctx,
			`UPDATE ui_sessions SET last_seen_at = NOW() WHERE id = $1`,
			id)
	} else {
		_, err = r.db.ExecContext(ctx,
			`UPDATE ui_sessions SET last_seen_at = NOW(), ip = $2 WHERE id = $1`,
			id, ip)
	}
	return mapDBError(err)
}

// RevokeSession soft-deletes an active session. Returns
// ErrSessionNotFound when the session is already revoked or missing.
func (r *UISessionRepository) RevokeSession(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE ui_sessions SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`,
		id)
	if err != nil {
		return mapDBError(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return persistence.ErrSessionNotFound
	}
	return nil
}

// RevokeSessionForUser revokes an active session only when it belongs
// to userID (admin session viewer ownership invariant). ErrSessionNotFound
// when no active row matches (absent, already revoked, or owned by
// another user — not distinguished, by design).
func (r *UISessionRepository) RevokeSessionForUser(ctx context.Context, userID, sessionID string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE ui_sessions SET revoked_at = NOW() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		sessionID, userID)
	if err != nil {
		return mapDBError(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return persistence.ErrSessionNotFound
	}
	return nil
}

// ListActiveByUser returns the user's non-revoked, non-expired sessions,
// newest last-seen first (admin session viewer). Matches the Users-page
// count predicate (ListUsers query C: revoked_at IS NULL AND
// expires_at > now()) so the list length equals the count shown.
func (r *UISessionRepository) ListActiveByUser(ctx context.Context, userID string) ([]*persistence.UISession, error) {
	// expires_at > now() excludes expired-but-not-revoked rows: this query
	// is read directly by the admin session viewer and does NOT pass through
	// session.Store.Validate (where the Go-side expiry/idle check lives), so
	// the predicate must be in the SQL. Without it, expired sessions linger
	// in the "active" list forever (worsened when retention is disabled and
	// nothing hard-deletes them), inflating the count and surfacing stale
	// login-time IPs. Reported 2026-06-23.
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, token_hash, user_id, provider, created_at, last_seen_at, expires_at, revoked_at, ip, user_agent
FROM ui_sessions
WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()
ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.UISession
	for rows.Next() {
		var s persistence.UISession
		var ip, ua sql.NullString
		if err := rows.Scan(&s.ID, &s.TokenHash, &s.UserID, &s.Provider,
			&s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt, &s.RevokedAt, &ip, &ua); err != nil {
			return nil, mapDBError(err)
		}
		s.IP = ip.String
		s.UserAgent = ua.String
		out = append(out, &s)
	}
	if err := rows.Err(); err != nil {
		return nil, mapDBError(err)
	}
	return out, nil
}

// DeleteExpiredSessions hard-deletes sessions whose expires_at or
// revoked_at falls before cutoff. Returns the count of deleted rows.
func (r *UISessionRepository) DeleteExpiredSessions(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM ui_sessions WHERE expires_at < $1 OR (revoked_at IS NOT NULL AND revoked_at < $1)`,
		cutoff)
	if err != nil {
		return 0, mapDBError(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountByStatus returns ui_sessions counts bucketed by lifecycle status in a
// single scan. The buckets mirror the admin surfaces' "active" predicate
// (revoked_at IS NULL AND expires_at > now()), the expired-but-not-revoked
// leak class (counted active until retention deletes it — reported
// 2026-06-23), and revoked. Powers the ui_sessions observability gauge.
func (r *UISessionRepository) CountByStatus(ctx context.Context) (persistence.UISessionStatusCounts, error) {
	var c persistence.UISessionStatusCounts
	err := r.db.QueryRowContext(ctx, `SELECT
  COUNT(*) FILTER (WHERE revoked_at IS NULL AND expires_at > now())  AS active,
  COUNT(*) FILTER (WHERE revoked_at IS NULL AND expires_at <= now()) AS expired_not_revoked,
  COUNT(*) FILTER (WHERE revoked_at IS NOT NULL)                     AS revoked
FROM ui_sessions`).Scan(&c.Active, &c.ExpiredNotRevoked, &c.Revoked)
	if err != nil {
		return persistence.UISessionStatusCounts{}, mapDBError(err)
	}
	return c, nil
}

// Compile-time interface pin.
var _ persistence.UISessionRepository = (*UISessionRepository)(nil)
