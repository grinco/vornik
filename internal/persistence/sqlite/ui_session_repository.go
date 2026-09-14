package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// UISessionRepository implements persistence.UISessionRepository on SQLite
// (identity-core parity, 2026-09-13 config-assistant plan §2 / review R1).
// A port of the Postgres repository: token-hash lookup filters revoked rows
// in SQL; expiry and idle-timeout checks belong to the session store. Every
// NOW() becomes the caller's clock (sqliteTime), and the "active" predicate
// `revoked_at IS NULL AND expires_at > now` is the same one ListUsers'
// session count uses, so the drill-down list length equals the count.
type UISessionRepository struct {
	db DBTX
}

// NewUISessionRepository constructs the repository.
func NewUISessionRepository(db DBTX) *UISessionRepository {
	return &UISessionRepository{db: db}
}

// Compile-time interface pin.
var _ persistence.UISessionRepository = (*UISessionRepository)(nil)

const uiSessionColumns = `id, token_hash, user_id, provider, created_at, last_seen_at, expires_at, revoked_at, ip, user_agent`

// CreateSession inserts a new session row. Empty IP/UserAgent are stored
// as NULL so retention filters over them behave as on Postgres.
func (r *UISessionRepository) CreateSession(ctx context.Context, s *persistence.UISession) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO ui_sessions (id, token_hash, user_id, provider, created_at, last_seen_at, expires_at, ip, user_agent)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.TokenHash, s.UserID, s.Provider,
		sqliteTime(s.CreatedAt), sqliteTime(s.LastSeenAt), sqliteTime(s.ExpiresAt),
		nullStr(s.IP), nullStr(s.UserAgent))
	return mapIdentityDBError(err)
}

// GetActiveByTokenHash returns the non-revoked session matching tokenHash,
// or ErrSessionNotFound (which satisfies errors.Is(err, ErrNotFound)).
func (r *UISessionRepository) GetActiveByTokenHash(ctx context.Context, tokenHash string) (*persistence.UISession, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+uiSessionColumns+` FROM ui_sessions WHERE token_hash = ? AND revoked_at IS NULL`,
		tokenHash)
	s, err := scanUISession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrSessionNotFound
	}
	if err != nil {
		return nil, mapIdentityDBError(err)
	}
	return s, nil
}

// TouchSession updates last_seen_at; best-effort (zero rows is not an
// error). A non-empty ip is refreshed alongside it; an empty ip leaves the
// stored value untouched.
func (r *UISessionRepository) TouchSession(ctx context.Context, id, ip string) error {
	var err error
	if ip == "" {
		_, err = r.db.ExecContext(ctx,
			`UPDATE ui_sessions SET last_seen_at = ? WHERE id = ?`, nowText(), id)
	} else {
		_, err = r.db.ExecContext(ctx,
			`UPDATE ui_sessions SET last_seen_at = ?, ip = ? WHERE id = ?`, nowText(), ip, id)
	}
	return mapIdentityDBError(err)
}

// RevokeSession soft-deletes an active session; ErrSessionNotFound when it
// is already revoked or missing.
func (r *UISessionRepository) RevokeSession(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE ui_sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, nowText(), id)
	return revokeResult(res, err)
}

// RevokeSessionForUser revokes an active session only when it belongs to
// userID; absent, already revoked and foreign sessions are all
// ErrSessionNotFound (indistinguishable by design).
func (r *UISessionRepository) RevokeSessionForUser(ctx context.Context, userID, sessionID string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE ui_sessions SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		nowText(), sessionID, userID)
	return revokeResult(res, err)
}

func revokeResult(res sql.Result, err error) error {
	if err != nil {
		return mapIdentityDBError(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return persistence.ErrSessionNotFound
	}
	return nil
}

// ListActiveByUser returns the user's non-revoked, non-expired sessions,
// newest last-seen first.
func (r *UISessionRepository) ListActiveByUser(ctx context.Context, userID string) ([]*persistence.UISession, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+uiSessionColumns+`
FROM ui_sessions
WHERE user_id = ? AND revoked_at IS NULL AND expires_at > ?
ORDER BY last_seen_at DESC`, userID, nowText())
	if err != nil {
		return nil, mapIdentityDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.UISession
	for rows.Next() {
		s, err := scanUISession(rows)
		if err != nil {
			return nil, mapIdentityDBError(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, mapIdentityDBError(err)
	}
	return out, nil
}

// DeleteExpiredSessions hard-deletes sessions whose expires_at or
// revoked_at falls before cutoff; returns the rows deleted.
func (r *UISessionRepository) DeleteExpiredSessions(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM ui_sessions WHERE expires_at < ?1 OR (revoked_at IS NOT NULL AND revoked_at < ?1)`,
		sqliteTime(cutoff))
	if err != nil {
		return 0, mapIdentityDBError(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountByStatus returns ui_sessions counts bucketed by lifecycle status in
// one scan: active, expired-but-not-revoked (the leak class reported
// 2026-06-23), and revoked.
func (r *UISessionRepository) CountByStatus(ctx context.Context) (persistence.UISessionStatusCounts, error) {
	var c persistence.UISessionStatusCounts
	err := r.db.QueryRowContext(ctx, `SELECT
  COUNT(*) FILTER (WHERE revoked_at IS NULL AND expires_at > ?1)  AS active,
  COUNT(*) FILTER (WHERE revoked_at IS NULL AND expires_at <= ?1) AS expired_not_revoked,
  COUNT(*) FILTER (WHERE revoked_at IS NOT NULL)                  AS revoked
FROM ui_sessions`, nowText()).Scan(&c.Active, &c.ExpiredNotRevoked, &c.Revoked)
	if err != nil {
		return persistence.UISessionStatusCounts{}, mapIdentityDBError(err)
	}
	return c, nil
}

// scanUISession reads one uiSessionColumns row from a *sql.Row or
// *sql.Rows (rowScanner, mcp_oauth_token_repository.go). Times round-trip
// through the TEXT helpers (modernc does not scan TEXT into time.Time).
func scanUISession(sc rowScanner) (*persistence.UISession, error) {
	var s persistence.UISession
	var created, lastSeen, expires sqlTime
	var revoked sqlNullTime
	var ip, ua sql.NullString
	if err := sc.Scan(&s.ID, &s.TokenHash, &s.UserID, &s.Provider,
		&created, &lastSeen, &expires, &revoked, &ip, &ua); err != nil {
		return nil, err
	}
	s.CreatedAt, s.LastSeenAt, s.ExpiresAt = created.Time, lastSeen.Time, expires.Time
	if revoked.Valid {
		t := revoked.Time
		s.RevokedAt = &t
	}
	s.IP, s.UserAgent = ip.String, ua.String
	return &s, nil
}
