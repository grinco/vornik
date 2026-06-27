package sqlite_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestMigration108_AllowPush_Idempotent verifies that the sqlite schema (which
// bakes migration 108's allow_push column into schemaSQL) applies cleanly and
// is idempotent. Uses an in-memory SQLite DB — never touches vornik_test.
func TestMigration108_AllowPush_Idempotent(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Connect(ctx, sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// First apply: full schema including allow_push column.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	// Verify allow_push column exists and defaults to 0.
	_, err = db.ExecContext(ctx, `INSERT INTO api_keys
		(id, project_id, name, key_hash, key_prefix, created_at)
		VALUES ('akey-m108', 'proj-m108', 'n', 'h_m108', 'pre', datetime('now'))`)
	if err != nil {
		t.Fatalf("insert without allow_push: %v", err)
	}

	var allowPush int
	row := db.QueryRowContext(ctx, `SELECT allow_push FROM api_keys WHERE id = 'akey-m108'`)
	if err := row.Scan(&allowPush); err != nil {
		t.Fatalf("scan allow_push: %v", err)
	}
	if allowPush != 0 {
		t.Errorf("allow_push default = %d, want 0", allowPush)
	}

	// Verify the column can store 1.
	_, err = db.ExecContext(ctx, `UPDATE api_keys SET allow_push = 1 WHERE id = 'akey-m108'`)
	if err != nil {
		t.Fatalf("update allow_push=1: %v", err)
	}
	row = db.QueryRowContext(ctx, `SELECT allow_push FROM api_keys WHERE id = 'akey-m108'`)
	if err := row.Scan(&allowPush); err != nil {
		t.Fatalf("scan allow_push after update: %v", err)
	}
	if allowPush != 1 {
		t.Errorf("allow_push = %d, want 1", allowPush)
	}

	// Second apply (idempotency): Migrate again must not error.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate (idempotency): %v", err)
	}
}
