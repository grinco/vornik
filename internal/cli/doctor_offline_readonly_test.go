package cli

import (
	"context"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/storage"
)

// `vornikctl doctor --offline` MIGRATED the database it was diagnosing.
//
// runDoctorOffline -> storage.Open, whose SQLite branch calls db.Migrate on
// connect. A CLI one build ahead of the daemon therefore moved the schema
// forward while the operator believed they were only gathering evidence — and
// the command exists precisely for the case where the daemon will not start,
// which is when an unexpected migration is least welcome. Community defaults to
// SQLite, so this is CE's default path.
//
// Filed 2026-09-04 alongside the CE support bundle, which introduced
// storage.OpenReadOnly specifically to avoid adding a second instance of this.
func TestOfflineDoctor_DoesNotMigrateTheDatabaseItDiagnoses(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "vornik.db")
	cfg := &config.Config{Database: config.DatabaseConfig{Driver: "sqlite", Path: dbPath}}

	report := doctorReport{}
	offlineCheckDatabase(ctx, cfg, &report)

	// Opening a SQLite database creates the file; that is not a schema change.
	// What must NOT have happened is the schema moving forward.
	back, err := storage.OpenReadOnly(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = back.Close() }()

	// SQLite carries no migrations table — Migrate applies schemaSQL, a pile of
	// CREATE TABLE IF NOT EXISTS, plus an ALTER TABLE reconciler for columns
	// added after a table first shipped. So the evidence of a migration is the
	// tables themselves.
	var tables int
	if err := back.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`,
	).Scan(&tables); err != nil {
		t.Fatalf("inspect schema: %v", err)
	}
	if tables != 0 {
		t.Fatalf("doctor --offline migrated the database it was asked to diagnose: "+
			"%d table(s) exist after a read-only check on a fresh database", tables)
	}

	// The diagnosis itself must still happen — a check that stops reporting is
	// not a fix.
	var sawDatabase bool
	for _, c := range report.Checks {
		if c.Name == "database" {
			sawDatabase = true
			if c.Status != "ok" {
				t.Fatalf("database check = %s (%s), want ok", c.Status, c.Message)
			}
		}
	}
	if !sawDatabase {
		t.Fatal("no database check in the offline report")
	}
}

// The sharper case: an EXISTING operator database, which is what the command is
// actually pointed at. SQLite's Migrate runs an ALTER TABLE reconciler for
// columns added to a table after it first shipped, so a CLI one build ahead of
// the daemon rewrote the operator's schema mid-diagnosis.
func TestOfflineDoctor_DoesNotAlterAnExistingSchema(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "vornik.db")
	cfg := &config.Config{Database: config.DatabaseConfig{Driver: "sqlite", Path: dbPath}}

	// A project_skills table as it existed BEFORE the migration-154 columns.
	// storage.Open would reconcile it; a diagnosis must not.
	seed, err := storage.OpenReadOnly(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if _, err := seed.DB.ExecContext(ctx,
		`CREATE TABLE project_skills (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '')`,
	); err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	_ = seed.Close()

	report := doctorReport{}
	offlineCheckDatabase(ctx, cfg, &report)

	back, err := storage.OpenReadOnly(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = back.Close() }()

	rows, err := back.DB.QueryContext(ctx, `SELECT name FROM pragma_table_info('project_skills')`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if cols["embedding"] {
		t.Fatal("doctor --offline ALTERed the operator's project_skills table, adding the " +
			"migration-154 columns — the command exists for the case where the daemon " +
			"will not start, which is when an unexpected schema change is least welcome")
	}
}
