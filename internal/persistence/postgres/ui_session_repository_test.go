package postgres

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

func newUISessionMock(t *testing.T) (*UISessionRepository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewUISessionRepository(db), mock, func() { _ = db.Close() }
}

func TestUISessionRepository_CreateSession(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	now := time.Now().UTC()
	s := &persistence.UISession{
		ID:         "sess_1",
		TokenHash:  "abc123",
		UserID:     "user_1",
		Provider:   "google",
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(24 * time.Hour),
		IP:         "127.0.0.1",
		UserAgent:  "Mozilla/5.0",
	}
	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO ui_sessions (id, token_hash, user_id, provider, created_at, last_seen_at, expires_at, ip, user_agent) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`)).
		WithArgs(s.ID, s.TokenHash, s.UserID, s.Provider,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			s.IP, s.UserAgent).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.CreateSession(context.Background(), s)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestUISessionRepository_CreateSession_EmptyIPUA(t *testing.T) {
	// Empty ip/user_agent must reach the driver as nil (NULL), not as
	// an empty string, so that future WHERE ip IS NULL retention filters
	// work correctly.
	repo, mock, done := newUISessionMock(t)
	defer done()

	now := time.Now().UTC()
	s := &persistence.UISession{
		ID:         "sess_2",
		TokenHash:  "def456",
		UserID:     "user_2",
		Provider:   "github",
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(24 * time.Hour),
		IP:         "",
		UserAgent:  "",
	}
	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO ui_sessions (id, token_hash, user_id, provider, created_at, last_seen_at, expires_at, ip, user_agent) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`)).
		WithArgs(s.ID, s.TokenHash, s.UserID, s.Provider,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			nil, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.CreateSession(context.Background(), s)
	if err != nil {
		t.Fatalf("CreateSession empty ip/ua: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestUISessionRepository_CreateSession_Error(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	now := time.Now().UTC()
	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO ui_sessions (id, token_hash, user_id, provider, created_at, last_seen_at, expires_at, ip, user_agent) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`)).
		WillReturnError(sql.ErrConnDone)

	err := repo.CreateSession(context.Background(), &persistence.UISession{
		ID: "sess_err", TokenHash: "h", UserID: "u", Provider: "p",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now,
	})
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestUISessionRepository_GetActiveByTokenHash(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	now := time.Now().UTC()
	expires := now.Add(24 * time.Hour)
	rows := sqlmock.NewRows([]string{
		"id", "token_hash", "user_id", "provider",
		"created_at", "last_seen_at", "expires_at", "revoked_at",
		"ip", "user_agent",
	}).AddRow(
		"sess_1", "abc123", "user_1", "google",
		now, now, expires, nil,
		"127.0.0.1", "Mozilla/5.0",
	)
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT id, token_hash, user_id, provider, created_at, last_seen_at, expires_at, revoked_at, ip, user_agent FROM ui_sessions WHERE token_hash = $1 AND revoked_at IS NULL`)).
		WithArgs("abc123").
		WillReturnRows(rows)

	got, err := repo.GetActiveByTokenHash(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("GetActiveByTokenHash: %v", err)
	}
	if got.ID != "sess_1" {
		t.Errorf("ID = %q, want %q", got.ID, "sess_1")
	}
	if got.TokenHash != "abc123" {
		t.Errorf("TokenHash = %q, want %q", got.TokenHash, "abc123")
	}
	if got.UserID != "user_1" {
		t.Errorf("UserID = %q, want %q", got.UserID, "user_1")
	}
	if got.Provider != "google" {
		t.Errorf("Provider = %q, want %q", got.Provider, "google")
	}
	if got.RevokedAt != nil {
		t.Errorf("RevokedAt = %v, want nil", got.RevokedAt)
	}
	if got.IP != "127.0.0.1" {
		t.Errorf("IP = %q, want %q", got.IP, "127.0.0.1")
	}
	if got.UserAgent != "Mozilla/5.0" {
		t.Errorf("UserAgent = %q, want %q", got.UserAgent, "Mozilla/5.0")
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, now)
	}
	if !got.LastSeenAt.Equal(now) {
		t.Errorf("LastSeenAt = %v, want %v", got.LastSeenAt, now)
	}
	if !got.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, expires)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestUISessionRepository_GetActiveByTokenHash_NotFound(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT id, token_hash, user_id, provider, created_at, last_seen_at, expires_at, revoked_at, ip, user_agent FROM ui_sessions WHERE token_hash = $1 AND revoked_at IS NULL`)).
		WithArgs("missing").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "token_hash", "user_id", "provider",
			"created_at", "last_seen_at", "expires_at", "revoked_at",
			"ip", "user_agent",
		}))

	_, err := repo.GetActiveByTokenHash(context.Background(), "missing")
	if err != persistence.ErrSessionNotFound {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestUISessionRepository_GetActiveByTokenHash_Error(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT id, token_hash, user_id, provider, created_at, last_seen_at, expires_at, revoked_at, ip, user_agent FROM ui_sessions WHERE token_hash = $1 AND revoked_at IS NULL`)).
		WithArgs("h").
		WillReturnError(sql.ErrConnDone)

	_, err := repo.GetActiveByTokenHash(context.Background(), "h")
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestUISessionRepository_TouchSession(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE ui_sessions SET last_seen_at = NOW() WHERE id = $1`)).
		WithArgs("sess_1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.TouchSession(context.Background(), "sess_1", "")
	if err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestUISessionRepository_TouchSession_RefreshesIP pins the IP-refresh
// branch (stale-IP fix, 2026-06-23): a non-empty IP is written alongside
// last_seen_at so the admin viewer tracks the session's current source.
func TestUISessionRepository_TouchSession_RefreshesIP(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE ui_sessions SET last_seen_at = NOW(), ip = $2 WHERE id = $1`)).
		WithArgs("sess_1", "203.0.113.9").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.TouchSession(context.Background(), "sess_1", "203.0.113.9")
	if err != nil {
		t.Fatalf("TouchSession with IP: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// countByStatusSQL pins the status-bucketed count query verbatim so the test
// fails if the SQL drifts. The three buckets must match the lifecycle the
// admin surfaces use: active (live), expired-but-not-revoked (the leak class
// the operator reported 2026-06-23 — counted but not yet retention-deleted),
// and revoked.
const countByStatusSQL = `SELECT
  COUNT(*) FILTER (WHERE revoked_at IS NULL AND expires_at > now())  AS active,
  COUNT(*) FILTER (WHERE revoked_at IS NULL AND expires_at <= now()) AS expired_not_revoked,
  COUNT(*) FILTER (WHERE revoked_at IS NOT NULL)                     AS revoked
FROM ui_sessions`

func TestUISessionRepository_CountByStatus(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	mock.ExpectQuery(regexp.QuoteMeta(countByStatusSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"active", "expired_not_revoked", "revoked"}).
			AddRow(int64(7), int64(3), int64(12)))

	got, err := repo.CountByStatus(context.Background())
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if got.Active != 7 || got.ExpiredNotRevoked != 3 || got.Revoked != 12 {
		t.Errorf("counts = %+v, want {Active:7 ExpiredNotRevoked:3 Revoked:12}", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestUISessionRepository_CountByStatus_Error(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	mock.ExpectQuery(regexp.QuoteMeta(countByStatusSQL)).WillReturnError(sql.ErrConnDone)

	if _, err := repo.CountByStatus(context.Background()); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestUISessionRepository_TouchSession_ZeroRowsOK(t *testing.T) {
	// Zero rows affected is not an error (async caller; session may
	// have been concurrently revoked).
	repo, mock, done := newUISessionMock(t)
	defer done()

	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE ui_sessions SET last_seen_at = NOW() WHERE id = $1`)).
		WithArgs("sess_gone").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.TouchSession(context.Background(), "sess_gone", "")
	if err != nil {
		t.Fatalf("TouchSession zero rows: %v (want nil)", err)
	}
}

func TestUISessionRepository_TouchSession_Error(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE ui_sessions SET last_seen_at = NOW() WHERE id = $1`)).
		WithArgs("sess_1").
		WillReturnError(sql.ErrConnDone)

	err := repo.TouchSession(context.Background(), "sess_1", "")
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestUISessionRepository_RevokeSession(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE ui_sessions SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`)).
		WithArgs("sess_1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.RevokeSession(context.Background(), "sess_1")
	if err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestUISessionRepository_RevokeSession_NotFound(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE ui_sessions SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`)).
		WithArgs("sess_missing").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.RevokeSession(context.Background(), "sess_missing")
	if err != persistence.ErrSessionNotFound {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestUISessionRepository_RevokeSession_Error(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE ui_sessions SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`)).
		WithArgs("sess_1").
		WillReturnError(sql.ErrConnDone)

	err := repo.RevokeSession(context.Background(), "sess_1")
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestUISessionRepository_DeleteExpiredSessions(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	cutoff := time.Now().UTC()
	mock.ExpectExec(regexp.QuoteMeta(
		`DELETE FROM ui_sessions WHERE expires_at < $1 OR (revoked_at IS NOT NULL AND revoked_at < $1)`)).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 7))

	n, err := repo.DeleteExpiredSessions(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 7 {
		t.Errorf("rows deleted = %d, want 7", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestUISessionRepository_DeleteExpiredSessions_Error(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()

	mock.ExpectExec(regexp.QuoteMeta(
		`DELETE FROM ui_sessions WHERE expires_at < $1 OR (revoked_at IS NOT NULL AND revoked_at < $1)`)).
		WillReturnError(sql.ErrConnDone)

	_, err := repo.DeleteExpiredSessions(context.Background(), time.Now())
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

// Must include `expires_at > now()`: the admin session viewer reads this
// query directly (it does NOT go through session.Store.Validate, which is
// where the Go-side clock check lives). Without the predicate, expired
// sessions show as "active" forever — the ever-growing count + stale
// login-time IPs the operator reported 2026-06-23.
const listActiveByUserSQL = `SELECT id, token_hash, user_id, provider, created_at, last_seen_at, expires_at, revoked_at, ip, user_agent
FROM ui_sessions
WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()
ORDER BY last_seen_at DESC`

func TestUISessionRepository_ListActiveByUser(t *testing.T) {
	repo, mock, done := newUISessionMock(t)
	defer done()
	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{
		"id", "token_hash", "user_id", "provider", "created_at",
		"last_seen_at", "expires_at", "revoked_at", "ip", "user_agent",
	}).
		AddRow("sess_1", "h1", "user_1", "github", now, now, now.Add(168*time.Hour), nil, "192.0.2.10", "Mozilla/5.0 Chrome/120").
		AddRow("sess_2", "h2", "user_1", "github", now, now, now.Add(168*time.Hour), nil, "10.0.0.5", "")
	mock.ExpectQuery(regexp.QuoteMeta(listActiveByUserSQL)).
		WithArgs("user_1").WillReturnRows(rows)

	got, err := repo.ListActiveByUser(context.Background(), "user_1")
	if err != nil {
		t.Fatalf("ListActiveByUser: %v", err)
	}
	if len(got) != 2 || got[0].ID != "sess_1" || got[1].ID != "sess_2" {
		t.Fatalf("got %d rows %+v, want sess_1, sess_2", len(got), got)
	}
	if got[0].IP != "192.0.2.10" || got[0].Provider != "github" {
		t.Errorf("row0 fields wrong: %+v", got[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestUISessionRepository_RevokeSessionForUser(t *testing.T) {
	const sql = `UPDATE ui_sessions SET revoked_at = NOW() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`
	t.Run("owned", func(t *testing.T) {
		repo, mock, done := newUISessionMock(t)
		defer done()
		mock.ExpectExec(regexp.QuoteMeta(sql)).WithArgs("sess_1", "user_1").
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := repo.RevokeSessionForUser(context.Background(), "user_1", "sess_1"); err != nil {
			t.Fatalf("RevokeSessionForUser: %v", err)
		}
	})
	t.Run("foreign-or-gone", func(t *testing.T) {
		repo, mock, done := newUISessionMock(t)
		defer done()
		mock.ExpectExec(regexp.QuoteMeta(sql)).WithArgs("sess_x", "user_1").
			WillReturnResult(sqlmock.NewResult(0, 0))
		if err := repo.RevokeSessionForUser(context.Background(), "user_1", "sess_x"); err != persistence.ErrSessionNotFound {
			t.Fatalf("err = %v, want ErrSessionNotFound", err)
		}
	})
}
