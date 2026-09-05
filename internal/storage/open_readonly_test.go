package storage

import (
	"context"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/config"
)

// A diagnostics collector must not move the schema. OpenReadOnly's SQLite
// branch is where that could happen silently: Open migrates on connect, so a
// CLI one build ahead of the daemon would migrate the daemon's database while
// the operator believed they were only collecting evidence
// (support-bundle-in-CE design §4).
func TestOpenReadOnly_SQLiteDoesNotMigrate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vornik.db")
	cfg := config.DatabaseConfig{Driver: "sqlite", Path: path}

	ro, err := OpenReadOnly(ctx, cfg)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer func() { _ = ro.Close() }()

	// No migration ran, so the schema is empty: a table Open would have
	// created is absent. Asserting on a table rather than on a call count
	// keeps this honest if the migration mechanism changes.
	var n int
	if err := ro.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='tasks'`).Scan(&n); err != nil {
		t.Fatalf("query schema: %v", err)
	}
	if n != 0 {
		t.Fatalf("OpenReadOnly migrated the database: found %d 'tasks' table(s)", n)
	}
	if ro.Repos == nil || ro.Repos.Tasks == nil {
		t.Fatal("OpenReadOnly must still build the repository set — the collector reads through it")
	}
}

// The contrast that gives the test above its meaning: Open DOES migrate, so
// the two functions are not interchangeable.
func TestOpen_SQLiteMigrates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vornik.db")

	b, err := Open(ctx, config.DatabaseConfig{Driver: "sqlite", Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = b.Close() }()

	var n int
	if err := b.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='tasks'`).Scan(&n); err != nil {
		t.Fatalf("query schema: %v", err)
	}
	if n == 0 {
		t.Fatal("Open no longer migrates; OpenReadOnly's reason to exist needs re-checking")
	}
}
