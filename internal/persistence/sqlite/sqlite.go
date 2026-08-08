// Package sqlite provides a SQLite-backed implementation of the
// persistence-package repository interfaces. The backend is intended
// for local development and integration tests — production deploys
// stay on Postgres.
//
// SQLite-specific concessions documented in
// https://docs.vornik.io:
//
//   - Single-writer at the database level: all writes serialize.
//     Fine for tests + dev; not for multi-tenant production.
//   - No SKIP LOCKED: the lease query degrades to BEGIN IMMEDIATE +
//     a row-level update under a held transaction. Concurrent
//     LeaseTask callers serialize; the single-scheduler-goroutine
//     deployment shape we ship doesn't notice.
//   - TEXT arrays (`completed_steps TEXT[]`, etc.) → JSON-encoded
//     TEXT columns; helper sqliteStringArray drives the round-trip.
//   - Enum types → TEXT + CHECK constraint.
//   - `NOW()` → `datetime('now')` (UTC text).
//   - Placeholder style: SQLite accepts `?` only (no `$N`); each repo
//     ships its own SQL string distinct from the postgres sibling.
//
// WAL journal mode is enabled at Connect so concurrent reads don't
// block writes — critical for any test that touches the scheduler's
// lease path.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Config holds SQLite connection configuration. Mirrors
// internal/config.DatabaseConfig.Path; additional knobs are
// hard-coded sensibly for the test/dev use case.
type Config struct {
	// Path is the on-disk database file. Pass ":memory:" for an
	// ephemeral test DB; one connection only (SQLite in-memory
	// databases are not shared across connections by default).
	Path string

	// MaxOpenConns caps the connection pool. Defaults to 1 for
	// :memory: (otherwise the pool would create multiple
	// independent in-memory DBs) and 5 for file-backed paths.
	MaxOpenConns int

	// ConnectTimeout bounds the initial open + ping. Defaults to 5s.
	ConnectTimeout time.Duration
}

// DefaultConfig returns a Config tuned for ephemeral test usage.
func DefaultConfig() Config {
	return Config{
		Path:           ":memory:",
		MaxOpenConns:   1,
		ConnectTimeout: 5 * time.Second,
	}
}

// DB wraps the sql.DB plus configuration metadata for the
// storage.Backend integration. Mirrors postgres.DB for symmetry —
// callers that hold a *sqlite.DB pointer can call Migrate / Close /
// IsReady the same way they would on Postgres.
type DB struct {
	*sql.DB
	config Config
}

// Connect opens a SQLite database at cfg.Path, verifies connectivity,
// and applies the consolidated schema. Returns a ready-to-use *DB.
//
// Unlike Postgres there's no incremental migration history — the
// schema is single-version + idempotent (CREATE TABLE IF NOT EXISTS
// throughout) so multiple Connect calls on the same file converge.
// This trades the historical reproducibility of Postgres migrations
// for simpler test fixtures: every test starts from the latest schema
// with no migration ordering to worry about.
func Connect(ctx context.Context, cfg Config) (*DB, error) {
	if cfg.Path == "" {
		cfg.Path = ":memory:"
	}
	// Create the DB file's parent directory if it's missing. SQLite
	// fails with SQLITE_CANTOPEN (error 14) rather than creating
	// missing parents — a common footgun for file-backed paths like
	// ./.dev/vornik.db. Skip the in-memory DB (no filesystem path).
	if cfg.Path != ":memory:" {
		if dir := filepath.Dir(cfg.Path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("sqlite: create db dir %q: %w", dir, err)
			}
		}
	}
	if cfg.MaxOpenConns <= 0 {
		if cfg.Path == ":memory:" {
			cfg.MaxOpenConns = 1
		} else {
			cfg.MaxOpenConns = 5
		}
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 5 * time.Second
	}

	// The modernc.org/sqlite driver registers under the name
	// "sqlite". Pass file-mode pragmas via the connection string so
	// they apply on every fresh connection in the pool.
	// foreign_keys intentionally OFF for phase 2: only 4 of the 28
	// repos are implemented, so a write into artifacts.task_id (for
	// example) has no parent row in tasks yet. Once TaskRepository
	// + others land, flip this back to ON and seed parent rows in
	// the shared test setup.
	dsn := cfg.Path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(OFF)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", cfg.Path, err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: ping %q: %w", cfg.Path, err)
	}

	return &DB{DB: db, config: cfg}, nil
}

// Migrate applies the consolidated schema. Idempotent.
func (d *DB) Migrate(ctx context.Context) error {
	// Additive columns FIRST. schemaSQL is CREATE TABLE IF NOT EXISTS, which
	// is a no-op on a table that already exists — so a column added to
	// schemaSQL after a database was created never lands there. That is
	// survivable for a plain column (queries error) but fatal when schemaSQL
	// also indexes it: the CREATE INDEX fails and Migrate returns an error,
	// so the daemon will not start at all against an existing database.
	// Reconciling before schemaSQL means the index has its column by the time
	// it is created.
	if err := d.applyAdditiveColumns(ctx); err != nil {
		return err
	}
	if _, err := d.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("sqlite: apply schema: %w", err)
	}
	return nil
}

// additiveColumn is a column added to an EXISTING table after that table first
// shipped in schemaSQL.
//
// Postgres gets these through numbered migrations; SQLite has no migration
// runner, only the idempotent starter schema, so they need this reconciler.
// Any future column added to an existing table in schemaSQL must be registered
// here too — otherwise it silently exists on fresh databases only.
type additiveColumn struct {
	table  string
	column string
	// ddl is the type + constraints, e.g. `TEXT NOT NULL DEFAULT ''`. SQLite's
	// ADD COLUMN requires a non-null default when the column is NOT NULL.
	ddl string
}

// sqliteAdditiveColumns is the registry. Keep it append-only and in the same
// order as the corresponding Postgres migrations, so the two backends can be
// diffed by eye.
var sqliteAdditiveColumns = []additiveColumn{
	// Postgres migration 154 — knowledge-skill dedup preflight (LLD §12.2).
	{"project_skills", "embedding", `TEXT NOT NULL DEFAULT ''`},
	{"project_skills", "embedding_model", `TEXT NOT NULL DEFAULT ''`},
	{"project_skills", "supersedes_id", `TEXT NOT NULL DEFAULT ''`},
	{"project_skills", "distinct_justification", `TEXT NOT NULL DEFAULT ''`},
}

// applyAdditiveColumns adds any registered column missing from an existing
// table. Tables that don't exist yet are skipped — schemaSQL creates them with
// the column already present, so a fresh database needs nothing here.
func (d *DB) applyAdditiveColumns(ctx context.Context) error {
	for _, c := range sqliteAdditiveColumns {
		exists, err := d.tableExists(ctx, c.table)
		if err != nil {
			return err
		}
		if !exists {
			continue // fresh database; schemaSQL will create it complete
		}
		has, err := d.columnExists(ctx, c.table, c.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		// SQLite has no ADD COLUMN IF NOT EXISTS, hence the check above.
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.ddl)
		if _, err := d.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: add column %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

func (d *DB) tableExists(ctx context.Context, table string) (bool, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("sqlite: probe table %s: %w", table, err)
	}
	return n > 0, nil
}

func (d *DB) columnExists(ctx context.Context, table, column string) (bool, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("sqlite: probe column %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}

// IsReady checks that the connection is alive and the schema has
// landed. The "schema present" check is a SELECT against the
// migrations table sentinel — mirrors postgres.DB.IsReady's shape.
func (d *DB) IsReady(ctx context.Context) error {
	if err := d.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlite: ping: %w", err)
	}
	var one int
	if err := d.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='tasks' LIMIT 1").Scan(&one); err != nil {
		return fmt.Errorf("sqlite: schema not applied: %w", err)
	}
	return nil
}

// Close closes the underlying connection pool.
func (d *DB) Close() error {
	return d.DB.Close()
}
