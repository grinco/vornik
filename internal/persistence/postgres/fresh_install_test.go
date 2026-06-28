//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestIntegrationFreshInstallMigrations covers the failure mode
// the operator surfaced on 2026-05-27: a deployment that ran the
// migration runner WITHOUT first sourcing
// deployments/postgres/schema/001_initial.sql can hit
// "relation <name> does not exist" mid-migration when some later
// migration ALTERs a table that the migration runner never created
// (e.g. memory_retrieval_audit before the migration-72 hot-fix).
//
// The test creates a fresh database (no bootstrap SQL), runs every
// migration in DefaultMigrations from currentVersion=0, and asserts
// (a) every migration applied without error, and (b) every table
// that the daemon's repositories expect at runtime is present.
//
// If a future migration ALTERs a bootstrap-only table without first
// CREATE TABLE IF NOT EXISTS-ing it, this test fails on the
// migration that breaks, naming the table and the migration version
// in the error. Catches the entire class of bug.
//
// Run with: go test -tags=integration ./internal/persistence/postgres/... -run FreshInstall
func TestIntegrationFreshInstallMigrations(t *testing.T) {
	cfg := Config{
		Host:           getEnvOrDefault("POSTGRES_HOST", "localhost"),
		Port:           integrationPort(),
		Database:       getEnvOrDefault("POSTGRES_DB", integrationDBName),
		User:           getEnvOrDefault("POSTGRES_USER", "vornik"),
		Password:       getEnvOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:        "disable",
		ConnectTimeout: 10 * time.Second,
	}

	// Step 1: connect to the admin DB (the shared test DB is fine
	// — we just need a superuser-capable connection to issue
	// CREATE/DROP DATABASE).
	ctx := context.Background()
	admin, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to admin DB: %v", err)
	}
	defer func() { _ = admin.Close() }()

	// Step 2: spin up an isolated database for this test run. Naming
	// uses a unix-nano suffix so parallel test invocations don't
	// step on each other.
	freshDB := fmt.Sprintf("vornik_fresh_install_test_%d", time.Now().UnixNano())
	if _, err := admin.DB.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", freshDB)); err != nil {
		t.Fatalf("create fresh DB %s: %v", freshDB, err)
	}
	defer func() {
		// Always drop the test DB. Close the per-test connection
		// first so the DROP isn't blocked by an open session.
		_, _ = admin.DB.ExecContext(context.Background(),
			fmt.Sprintf("DROP DATABASE IF EXISTS %s", freshDB))
	}()

	// Step 3: connect to the fresh DB and run migrations from
	// scratch. NO bootstrap SQL is sourced — the test simulates
	// the "operator only has migrations.go" path that hit the
	// migration-72 failure.
	freshCfg := cfg
	freshCfg.Database = freshDB
	freshConn, err := Connect(ctx, freshCfg)
	if err != nil {
		t.Fatalf("connect to fresh DB: %v", err)
	}
	defer func() { _ = freshConn.Close() }()

	if err := freshConn.Migrate(ctx); err != nil {
		t.Fatalf("migrations failed on fresh install: %v\n\n"+
			"This usually means a later migration ALTERs a table that "+
			"no earlier migration CREATEd. The pattern is:\n"+
			"  - the table lives in deployments/postgres/schema/001_initial.sql\n"+
			"  - production has it because the bootstrap SQL was sourced\n"+
			"  - the migration runner alone never created it\n"+
			"Fix: prepend CREATE TABLE IF NOT EXISTS for that table to the "+
			"failing migration's Up SQL (verbatim from 001_initial.sql).\n"+
			"Reference fix: migration 72 hot-fix for memory_retrieval_audit.", err)
	}

	// Step 4: assert every table the daemon expects at runtime is
	// present. The list is derived from the consumer side (every
	// repository the daemon constructs in production), not from
	// 001_initial.sql — that way a future migration that DROPs a
	// bootstrap table without code-side cleanup is also caught.
	requiredTables := []string{
		// Core orchestration (migration 1 + early migrations)
		"tasks", "executions", "artifacts", "migrations",
		// Audit + observability (migrations 3, 8, 10, 11, 18, 72, 74)
		"tool_audit_log", "task_llm_usage", "execution_step_outcomes",
		"task_judge_verdicts", "webhook_events",
		"memory_retrieval_audit", "memory_ingest_audit",
		// Task lifecycle (migration 4 onward)
		"task_watchers",
		// Memory subsystem (migrations 7, 23+)
		"project_memory_chunks", "memory_embed_queue",
		"corpus_epochs", "corpus_epochs_active", "corpus_rollbacks",
		"project_memory_quarantine", "project_ingest_queue",
		// Identity (migration 32 + 70)
		"api_keys",
	}
	missing := []string{}
	for _, name := range requiredTables {
		var present bool
		err := freshConn.DB.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = $1
			)
		`, name).Scan(&present)
		if err != nil {
			t.Errorf("check table %s: %v", name, err)
			continue
		}
		if !present {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("missing required tables after fresh-install migrations: %s\n"+
			"Either a CREATE migration is missing for these, or a DROP migration "+
			"removed them without code-side cleanup.",
			strings.Join(missing, ", "))
	}

	// Step 5: assert the daemon's expected schema version is reached.
	// CurrentVersion should equal the max version in DefaultMigrations.
	runner := freshConn.MigrationRunner()
	status, err := runner.Status(ctx)
	if err != nil {
		t.Fatalf("migration status: %v", err)
	}
	maxVersion := 0
	for _, m := range persistence.DefaultMigrations {
		if m.Version > maxVersion {
			maxVersion = m.Version
		}
	}
	if status.CurrentVersion != maxVersion {
		t.Errorf("fresh-install schema version = %d, want %d (the highest version in DefaultMigrations)",
			status.CurrentVersion, maxVersion)
	}
	if len(status.Pending) != 0 {
		t.Errorf("fresh install left %d pending migrations: %v", len(status.Pending), status.Pending)
	}
}

// dbHandle is a narrow accessor for the *sql.DB inside a Postgres
// connection. Keeps the test's CREATE/DROP DATABASE administration
// out of the public Postgres surface. If a future refactor renames
// Postgres.DB(), update the helper rather than every test.
var _ = (*sql.DB)(nil)

// TestIntegrationGapMigrationReapplied is the regression for the
// MigrationRunner.Run rewrite (PR31): a migration whose version is below the
// max applied but that was never actually recorded (a gap — e.g. v23 added
// after the DB had already migrated past it, or an inversion that left a row
// unrecorded) must be re-applied idempotently on the next Run, not silently
// skipped. The old runner used MAX(version) and applied migrations with
// Version > max, so a gap migration below the max was never re-applied — the
// memory-hardening tables (corpus_epochs, project_memory_quarantine,
// project_ingest_queue) went missing this way.
//
// The test: fully migrate a fresh DB, DELETE a mid-version row (v23) from the
// migrations table to simulate the gap, re-run Migrate, and assert v23's row
// re-appears and v23's tables are still present (re-apply is idempotent:
// CREATE TABLE IF NOT EXISTS, DO$$-guarded constraints, zero-row backfill).
//
// Run with: go test -tags=integration ./internal/persistence/postgres/... -run GapMigrationReapplied
func TestIntegrationGapMigrationReapplied(t *testing.T) {
	cfg := Config{
		Host:           getEnvOrDefault("POSTGRES_HOST", "localhost"),
		Port:           integrationPort(),
		Database:       getEnvOrDefault("POSTGRES_DB", integrationDBName),
		User:           getEnvOrDefault("POSTGRES_USER", "vornik"),
		Password:       getEnvOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:        "disable",
		ConnectTimeout: 10 * time.Second,
	}
	ctx := context.Background()
	admin, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to admin DB: %v", err)
	}
	defer func() { _ = admin.Close() }()

	gapDB := fmt.Sprintf("vornik_gap_reapply_test_%d", time.Now().UnixNano())
	if _, err := admin.DB.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", gapDB)); err != nil {
		t.Fatalf("create gap DB %s: %v", gapDB, err)
	}
	defer func() {
		_, _ = admin.DB.ExecContext(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s", gapDB))
	}()

	gapCfg := cfg
	gapCfg.Database = gapDB
	conn, err := Connect(ctx, gapCfg)
	if err != nil {
		t.Fatalf("connect to gap DB: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Step 1: full apply.
	if err := conn.Migrate(ctx); err != nil {
		t.Fatalf("initial migrations failed: %v", err)
	}

	// Step 2: simulate a gap — delete v23's row (the memory-hardening
	// migration whose tables went missing in production). v23's tables
	// already exist; only its migrations-table row is removed.
	const gapVersion = 23
	if _, err := conn.DB.ExecContext(ctx,
		"DELETE FROM migrations WHERE version = $1", gapVersion); err != nil {
		t.Fatalf("delete gap migration row: %v", err)
	}

	// Sanity: the row is gone and the max version is still above the gap
	// (the condition under which the old MAX-based runner skipped it).
	var maxAfter int
	if err := conn.DB.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM migrations").Scan(&maxAfter); err != nil {
		t.Fatalf("read max version after gap: %v", err)
	}
	if maxAfter <= gapVersion {
		t.Fatalf("max version %d not above gap %d — test setup is wrong", maxAfter, gapVersion)
	}

	// Step 3: re-run Migrate. Under the old runner this was a no-op (v23 <=
	// max → skipped); under the rewrite it re-applies v23 idempotently and
	// records its row.
	if err := conn.Migrate(ctx); err != nil {
		t.Fatalf("re-run migrations after gap: %v\n"+
			"This means a gap-skipped migration is NOT idempotent on re-apply — "+
			"its Up SQL needs IF NOT EXISTS / DO$$ guards (see migration-discipline.md).", err)
	}

	// Step 4: the gap migration's row re-appeared.
	var gapRowExists bool
	if err := conn.DB.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)", gapVersion,
	).Scan(&gapRowExists); err != nil {
		t.Fatalf("check gap row re-appeared: %v", err)
	}
	if !gapRowExists {
		t.Errorf("gap migration v%d was not re-applied — its migrations-table row is still absent. "+
			"The runner must re-apply any migration not in the applied set, not skip by MAX(version).", gapVersion)
	}

	// Step 5: v23's tables are still present (idempotent re-apply didn't drop
	// or fail them).
	for _, table := range []string{"corpus_epochs", "project_memory_quarantine", "project_ingest_queue"} {
		var present bool
		if err := conn.DB.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = $1
			)
		`, table).Scan(&present); err != nil {
			t.Errorf("check table %s after gap re-apply: %v", table, err)
			continue
		}
		if !present {
			t.Errorf("v23 table %s missing after gap re-apply", table)
		}
	}
}
