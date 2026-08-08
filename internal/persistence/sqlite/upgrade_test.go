package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

// oldProjectSkillsDDL is the project_skills shape as it existed BEFORE
// Postgres migration 154 added the dedup-preflight columns. Kept verbatim so
// the test reproduces a real pre-upgrade database rather than an approximation.
const oldProjectSkillsDDL = `CREATE TABLE IF NOT EXISTS project_skills (
	id TEXT PRIMARY KEY, project_id TEXT NOT NULL, repo_scope TEXT,
	name TEXT NOT NULL, description TEXT NOT NULL, body TEXT NOT NULL,
	body_sha256 TEXT NOT NULL, domain TEXT, tags TEXT NOT NULL DEFAULT '[]',
	roles TEXT NOT NULL DEFAULT '[]', maturity TEXT NOT NULL DEFAULT 'draft',
	version INTEGER NOT NULL DEFAULT 1, origin_client TEXT, origin_task TEXT,
	author TEXT, usage_fired INTEGER NOT NULL DEFAULT 0,
	usage_worked INTEGER NOT NULL DEFAULT 0, usage_corrected INTEGER NOT NULL DEFAULT 0,
	last_fired_at TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
	is_global INTEGER NOT NULL DEFAULT 0,
	UNIQUE (project_id, repo_scope, name))`

// TestMigrateUpgradesExistingDatabase is the regression test for a defect
// introduced 2026-08-07 and caught before release.
//
// schemaSQL is CREATE TABLE IF NOT EXISTS, so adding a column to an existing
// table there is a no-op on any database that already has the table. Adding
// migration 154's columns plus an index over supersedes_id made Migrate FAIL
// on every pre-existing SQLite database — `no such column: supersedes_id` —
// which means the daemon would not start at all, not merely misbehave.
//
// Run this against the code as it stood before applyAdditiveColumns and it
// fails at Migrate.
func TestMigrateUpgradesExistingDatabase(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "existing.db")
	db, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Stand up a database as it looked before the new columns existed, with a
	// row in it so we also prove data survives the upgrade.
	if _, err := db.Exec(oldProjectSkillsDDL); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO project_skills
		(id, project_id, name, description, body, body_sha256, maturity, version, created_at, updated_at)
		VALUES ('sk-legacy','proj','legacy-skill','d','# B','abc','active',2,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate on an existing database failed — the daemon would not start: %v", err)
	}

	for _, col := range []string{"embedding", "embedding_model", "supersedes_id", "distinct_justification"} {
		has, err := db.columnExists(context.Background(), "project_skills", col)
		if err != nil {
			t.Fatalf("columnExists(%s): %v", col, err)
		}
		if !has {
			t.Errorf("column %q missing after Migrate on an existing database", col)
		}
	}

	// The pre-existing row must still be readable through the full column list.
	repo := NewSkillRepository(db.DB)
	got, err := repo.GetByID(context.Background(), "sk-legacy")
	if err != nil {
		t.Fatalf("legacy row unreadable after upgrade: %v", err)
	}
	if got.Body != "# B" || got.Version != 2 || got.Maturity != "active" {
		t.Errorf("legacy row altered by upgrade: %+v", got)
	}
	if len(got.Embedding) != 0 || got.EmbeddingModel != "" {
		t.Errorf("legacy row should read back un-embedded, got embedding=%v model=%q", got.Embedding, got.EmbeddingModel)
	}

	// Idempotent: a second Migrate must not fail trying to re-add columns.
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate was not idempotent: %v", err)
	}
}

// TestMigrateFreshDatabaseUnaffected: the reconciler must not disturb the
// fresh-install path, where schemaSQL already creates every column.
func TestMigrateFreshDatabaseUnaffected(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "fresh.db")
	db, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate on a fresh database: %v", err)
	}
	for _, col := range []string{"embedding", "supersedes_id"} {
		has, err := db.columnExists(context.Background(), "project_skills", col)
		if err != nil {
			t.Fatalf("columnExists(%s): %v", col, err)
		}
		if !has {
			t.Errorf("fresh database missing %q", col)
		}
	}
}
