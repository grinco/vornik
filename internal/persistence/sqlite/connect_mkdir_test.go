package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestConnect_CreatesMissingParentDir pins the fix for the SQLite
// SQLITE_CANTOPEN (error 14) footgun: a file-backed path whose parent
// directory does not exist yet — e.g. ./.dev/vornik.db on a fresh
// checkout via `make dev` — must open rather than error. Connect now
// creates the parent dir.
func TestConnect_CreatesMissingParentDir(t *testing.T) {
	ctx := context.Background()
	parent := filepath.Join(t.TempDir(), "nested", "sub")
	if _, err := os.Stat(parent); !os.IsNotExist(err) {
		t.Fatalf("precondition: parent dir should not exist yet, stat err=%v", err)
	}
	dbPath := filepath.Join(parent, "vornik.db")

	db, err := sqlite.Connect(ctx, sqlite.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("Connect with missing parent dir should succeed, got: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("db file should exist after Connect: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate after auto-created dir: %v", err)
	}
}

// TestConnect_InMemoryUnaffected confirms the ":memory:" path skips the
// parent-dir creation (no filesystem path) and still opens.
func TestConnect_InMemoryUnaffected(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Connect(ctx, sqlite.Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Connect(:memory:): %v", err)
	}
	defer func() { _ = db.Close() }()
}
