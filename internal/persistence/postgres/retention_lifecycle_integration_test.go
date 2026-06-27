//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/retention"
)

// connectMigrated opens the integration DB (POSTGRES_* / :5433 throwaway) and
// runs all migrations so the full schema (ui_sessions, link_codes, …) exists.
func connectMigrated(t *testing.T) *DB {
	t.Helper()
	cfg := Config{
		Host:           getEnvOrDefault("POSTGRES_HOST", "localhost"),
		Port:           integrationPort(),
		Database:       getEnvOrDefault("POSTGRES_DB", integrationDBName),
		User:           getEnvOrDefault("POSTGRES_USER", "vornik"),
		Password:       getEnvOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:        "disable",
		ConnectTimeout: 10 * time.Second,
	}
	db, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		_ = db.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// TestIntegration_UISessionLifecycle verifies the ui_sessions lifecycle
// predicates against REAL Postgres (sqlmock can't validate `expires_at >
// now()` semantics): CountByStatus buckets, ListActiveByUser excluding
// expired/revoked, and DeleteExpiredSessions. Regression coverage for the
// 2026-06-23 session-count investigation (count and drill-down list must
// agree, and the now()-based predicate must actually exclude expired rows).
func TestIntegration_UISessionLifecycle(t *testing.T) {
	ctx := context.Background()
	db := connectMigrated(t).DB
	repo := NewUISessionRepository(db)

	// Clean slate (dedicated throwaway DB, serial -p 1 run).
	mustExec(t, db, "DELETE FROM ui_sessions")
	mustExec(t, db, "DELETE FROM users WHERE id = $1", "u-life")
	mustExec(t, db, "INSERT INTO users (id, display_name) VALUES ($1, $2)", "u-life", "Lifecycle User")
	t.Cleanup(func() {
		mustExec(t, db, "DELETE FROM ui_sessions")
		mustExec(t, db, "DELETE FROM users WHERE id = $1", "u-life")
	})

	now := time.Now().UTC()
	mk := func(id, hash string, expires time.Time) {
		t.Helper()
		if err := repo.CreateSession(ctx, &persistence.UISession{
			ID: id, TokenHash: hash, UserID: "u-life", Provider: "github",
			CreatedAt: now, LastSeenAt: now, ExpiresAt: expires,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("s-active", "h-active", now.Add(time.Hour))    // active
	mk("s-expired", "h-expired", now.Add(-time.Hour)) // expired, not revoked
	mk("s-revoked", "h-revoked", now.Add(time.Hour))  // will be revoked
	if err := repo.RevokeSession(ctx, "s-revoked"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// CountByStatus: one of each bucket.
	c, err := repo.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if c.Active != 1 || c.ExpiredNotRevoked != 1 || c.Revoked != 1 {
		t.Errorf("CountByStatus = %+v, want {Active:1 ExpiredNotRevoked:1 Revoked:1}", c)
	}

	// ListActiveByUser excludes both the expired and the revoked row.
	active, err := repo.ListActiveByUser(ctx, "u-life")
	if err != nil {
		t.Fatalf("ListActiveByUser: %v", err)
	}
	if len(active) != 1 || active[0].ID != "s-active" {
		t.Errorf("ListActiveByUser = %d rows (want only s-active): %+v", len(active), active)
	}

	// DeleteExpiredSessions(cutoff): removes the expired + the revoked row
	// (revoked just now < cutoff), keeps the active one.
	n, err := repo.DeleteExpiredSessions(ctx, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 2 {
		t.Errorf("DeleteExpiredSessions removed %d, want 2", n)
	}
	c2, _ := repo.CountByStatus(ctx)
	if c2.Active != 1 || c2.ExpiredNotRevoked != 0 || c2.Revoked != 0 {
		t.Errorf("post-delete CountByStatus = %+v, want {Active:1 0 0}", c2)
	}
}

// TestIntegration_RetentionSweepLinkCodes verifies the link_codes retention
// prune against REAL Postgres — the predicate is a destructive DELETE only
// pinned by sqlmock until now. Seeds rows across the 7-day grace boundary and
// asserts exactly the stale ones are removed.
func TestIntegration_RetentionSweepLinkCodes(t *testing.T) {
	ctx := context.Background()
	db := connectMigrated(t).DB

	mustExec(t, db, "DELETE FROM link_codes")
	mustExec(t, db, "DELETE FROM users WHERE id = $1", "u-link")
	mustExec(t, db, "INSERT INTO users (id, display_name) VALUES ($1, $2)", "u-link", "Link User")
	t.Cleanup(func() {
		mustExec(t, db, "DELETE FROM link_codes")
		mustExec(t, db, "DELETE FROM users WHERE id = $1", "u-link")
	})

	now := time.Now().UTC()
	stale := now.AddDate(0, 0, -10)      // past the 7-day grace
	withinGrace := now.AddDate(0, 0, -3) // expired but inside the grace
	future := now.Add(time.Hour)

	insert := func(code string, expires time.Time, used *time.Time) {
		t.Helper()
		mustExec(t, db,
			`INSERT INTO link_codes (code_hash, user_id, expires_at, used_at) VALUES ($1, $2, $3, $4)`,
			code, "u-link", expires, used)
	}
	insert("c-expired-old", stale, nil)          // DELETE (expired past grace)
	insert("c-used-old", future, &stale)         // DELETE (used past grace)
	insert("c-fresh", future, nil)               // KEEP
	insert("c-expired-recent", withinGrace, nil) // KEEP (within grace)

	sweeper := retention.New(db, zerolog.Nop())
	counts, err := sweeper.SweepGlobal(ctx, 0) // responseCacheDays=0 → cache prune skipped
	if err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	if counts.LinkCodes != 2 {
		t.Errorf("swept LinkCodes = %d, want 2", counts.LinkCodes)
	}

	var remaining int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM link_codes").Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 2 {
		t.Errorf("remaining link_codes = %d, want 2 (c-fresh + c-expired-recent)", remaining)
	}
	// The two survivors are the fresh + within-grace codes.
	for _, code := range []string{"c-fresh", "c-expired-recent"} {
		var n int
		_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM link_codes WHERE code_hash = $1", code).Scan(&n)
		if n != 1 {
			t.Errorf("expected %s to survive the sweep", code)
		}
	}
}
