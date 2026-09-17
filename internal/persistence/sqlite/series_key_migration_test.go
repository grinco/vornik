package sqlite_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite"
)

// The SQLite half of Postgres migration 190 — recurring-series membership on
// project_memory_chunks (2026-09-16-retrieval-recency-design.md §6).
//
// SQLite has no migration runner: the schema arrives as one idempotent
// schemaSQL blob plus the sqliteAdditiveColumns reconciler for databases that
// predate a column. So "migration 190 landed on SQLite" is two separate claims
// — the fresh-database shape and the existing-database reconciliation — and
// they fail in different ways. Both are asserted below.
//
// Uses an in-memory database; never touches a live deployment database.

func migrate190DB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func seriesIndexSQL(ctx context.Context, t *testing.T, db *sqlite.DB) (string, bool) {
	t.Helper()
	var ddl string
	err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_memory_chunks_series'`).Scan(&ddl)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("read index ddl: %v", err)
	}
	return ddl, true
}

// TestMigration190_SeriesKey_FreshDatabase: a database created today has the
// column, the index, and the nullable semantics the design relies on — absent
// means "not part of a series", which is what every pre-existing row is.
func TestMigration190_SeriesKey_FreshDatabase(t *testing.T) {
	ctx := context.Background()
	db := migrate190DB(t)

	if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
		(id, project_id, content, content_hash, created_at, series_key)
		VALUES ('chunk-m190-a', 'proj-m190', 'todays headlines', 'h-a', '2026-09-16T08:00:00Z', 'czech-news')`); err != nil {
		t.Fatalf("insert with series_key: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
		(id, project_id, content, content_hash, created_at)
		VALUES ('chunk-m190-b', 'proj-m190', 'a one-off note', 'h-b', '2026-09-16T08:00:00Z')`); err != nil {
		t.Fatalf("insert without series_key: %v", err)
	}

	var got sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT series_key FROM project_memory_chunks WHERE id = 'chunk-m190-a'`).Scan(&got); err != nil {
		t.Fatalf("scan series_key: %v", err)
	}
	if !got.Valid || got.String != "czech-news" {
		t.Errorf("series_key = %v, want czech-news", got)
	}

	// NULL, not '': the partial index and the query-time EXISTS both key on
	// IS NOT NULL, so an empty-string default would enrol every chunk in the
	// store into one enormous series that supersedes itself.
	if err := db.QueryRowContext(ctx,
		`SELECT series_key FROM project_memory_chunks WHERE id = 'chunk-m190-b'`).Scan(&got); err != nil {
		t.Fatalf("scan absent series_key: %v", err)
	}
	if got.Valid {
		t.Errorf("series_key on a non-series chunk = %q, want NULL", got.String)
	}
}

// TestMigration190_SeriesIndexShape pins the index the query-time EXISTS
// depends on. The key order is not cosmetic: id is last so the index serves
// the row-value tuple comparison (created_at, id) that breaks ties between two
// members ingested in the same second, and the measurement that justified
// keeping that tuple form (design §5.3, both drivers) was taken against this
// exact shape.
func TestMigration190_SeriesIndexShape(t *testing.T) {
	ctx := context.Background()
	db := migrate190DB(t)

	ddl, ok := seriesIndexSQL(ctx, t, db)
	if !ok {
		t.Fatal("idx_memory_chunks_series missing after Migrate")
	}
	normalised := strings.Join(strings.Fields(ddl), " ")
	for _, want := range []string{
		"project_memory_chunks(project_id, series_key, created_at DESC, id DESC)",
		"WHERE series_key IS NOT NULL",
	} {
		if !strings.Contains(normalised, want) {
			t.Errorf("index ddl missing %q, got %q", want, normalised)
		}
	}
}

// TestMigration190_SeriesKey_Idempotent: Migrate twice must not error. The
// additive reconciler probes pragma_table_info before issuing ADD COLUMN —
// SQLite has no ADD COLUMN IF NOT EXISTS — and a probe that got the table or
// column name wrong would surface here as a duplicate-column error on the
// second pass.
func TestMigration190_SeriesKey_Idempotent(t *testing.T) {
	db := migrate190DB(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate (idempotency): %v", err)
	}
}

// TestMigration190_SeriesKey_ReconciledOnExistingDatabase is the test that
// actually earns its place.
//
// schemaSQL is CREATE TABLE IF NOT EXISTS, so a column added to it never lands
// on a database that already has the table — and this column is INDEXED, which
// turns that from a query-level failure into a startup-level one: the CREATE
// INDEX fails, Migrate returns an error, and the daemon does not start at all.
// That is why series_key is registered in sqliteAdditiveColumns and why the
// reconciler runs before schemaSQL.
//
// Simulated by taking a migrated database back to the pre-190 shape (index
// first — SQLite refuses to drop an indexed column) and re-running Migrate.
func TestMigration190_SeriesKey_ReconciledOnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	db := migrate190DB(t)

	if _, err := db.ExecContext(ctx, `DROP INDEX idx_memory_chunks_series`); err != nil {
		t.Fatalf("drop index to simulate a pre-190 database: %v", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE project_memory_chunks DROP COLUMN series_key`); err != nil {
		t.Fatalf("drop column to simulate a pre-190 database: %v", err)
	}

	// Precondition: we really are back at the old shape, otherwise the
	// re-Migrate below would pass without proving anything.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('project_memory_chunks') WHERE name = 'series_key'`).Scan(&n); err != nil {
		t.Fatalf("probe column: %v", err)
	}
	if n != 0 {
		t.Fatalf("simulated pre-190 database still has series_key")
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate against a pre-190 database: %v — series_key must be registered in "+
			"sqliteAdditiveColumns AND reconciled before schemaSQL, or the indexed column "+
			"fails CREATE INDEX and the daemon will not start", err)
	}

	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('project_memory_chunks') WHERE name = 'series_key'`).Scan(&n); err != nil {
		t.Fatalf("probe column after reconcile: %v", err)
	}
	if n != 1 {
		t.Errorf("series_key not restored by the additive reconciler (count=%d)", n)
	}
	if _, ok := seriesIndexSQL(ctx, t, db); !ok {
		t.Error("idx_memory_chunks_series not restored by schemaSQL after the column was reconciled")
	}
}
