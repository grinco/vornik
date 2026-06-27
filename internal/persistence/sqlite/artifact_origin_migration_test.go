package sqlite_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestMigration107_ArtifactOrigin_Idempotent verifies that migration 107
// (artifact origin column) applies cleanly and is idempotent (re-running
// does not error). Uses an in-memory SQLite DB — never touches vornik_test.
func TestMigration107_ArtifactOrigin_Idempotent(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Connect(ctx, sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// First apply: full schema (includes origin column via Migrate).
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	// Verify origin column exists by inserting a row with a non-default origin.
	_, err = db.ExecContext(ctx, `INSERT INTO artifacts
		(id, project_id, name, artifact_class, storage_path, created_at, origin)
		VALUES ('art-m107', 'proj-m107', 'f.txt', 'OUTPUT', 'p', datetime('now'), 'task_output')`)
	if err != nil {
		t.Fatalf("insert with origin=task_output after migration: %v", err)
	}

	var origin string
	row := db.QueryRowContext(ctx, `SELECT origin FROM artifacts WHERE id = 'art-m107'`)
	if err := row.Scan(&origin); err != nil {
		t.Fatalf("scan origin: %v", err)
	}
	if origin != "task_output" {
		t.Errorf("origin = %q, want task_output", origin)
	}

	// Second apply (idempotency): Migrate again must not error.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate (idempotency): %v", err)
	}
}

// TestMigration107_ArtifactOrigin_DefaultUnknown verifies that rows
// inserted without an explicit origin value get 'unknown' from the column default.
func TestMigration107_ArtifactOrigin_DefaultUnknown(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Connect(ctx, sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	_, err = db.ExecContext(ctx, `INSERT INTO artifacts
		(id, project_id, name, artifact_class, storage_path, created_at)
		VALUES ('art-m107b', 'proj-m107b', 'g.txt', 'OUTPUT', 'p', datetime('now'))`)
	if err != nil {
		t.Fatalf("insert without origin: %v", err)
	}

	var origin string
	row := db.QueryRowContext(ctx, `SELECT origin FROM artifacts WHERE id = 'art-m107b'`)
	if err := row.Scan(&origin); err != nil {
		t.Fatalf("scan origin: %v", err)
	}
	if origin != "unknown" {
		t.Errorf("origin = %q, want unknown (default)", origin)
	}
}
