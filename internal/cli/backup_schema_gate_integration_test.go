//go:build integration

package cli

// Integration tests for B-8's schema-gate primitives. Each test
// spins up a fresh database against the operator's local postgres
// so the SQL ran in real conditions; the helpers themselves don't
// portage to sqlite (pg_type, to_regclass, DROP SCHEMA public
// CASCADE are PG-specific).
//
// Run with:
//   POSTGRES_HOST=localhost POSTGRES_PASSWORD=vornik \
//     go test -tags=integration ./internal/cli/... -run SchemaGate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"vornik.io/vornik/internal/config"
)

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// freshTestDB creates a per-test database with a unique name and returns a
// DatabaseConfig pointed at it. Teardown is registered with t.Cleanup, so callers
// do NOT get (and must not need) a cleanup callback — see the comment at the
// registration below for why that ordering is load-bearing.
//
// Skips the test when postgres isn't reachable so the suite stays green on hosts
// without a local daemon.
func freshTestDB(t *testing.T) *config.DatabaseConfig {
	t.Helper()
	adminCfg := config.DatabaseConfig{
		Host:     envOrDefault("POSTGRES_HOST", "localhost"),
		Port:     5432,
		Name:     envOrDefault("POSTGRES_DB", "vornik_test"),
		User:     envOrDefault("POSTGRES_USER", "vornik"),
		Password: envOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:  "disable",
	}
	adminConnStr := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		adminCfg.Host, adminCfg.Port, adminCfg.User, adminCfg.Password,
		adminCfg.Name, adminCfg.SSLMode,
	)
	admin, err := sql.Open("postgres", adminConnStr)
	if err != nil {
		t.Skipf("postgres unreachable, skipping: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Skipf("postgres ping failed, skipping: %v", err)
	}
	dbName := fmt.Sprintf("vornik_b8_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		_ = admin.Close()
		t.Fatalf("create test DB: %v", err)
	}

	// Drop via t.Cleanup, NOT a returned defer.
	//
	// This leaked 138 databases before being fixed. Deferred functions in a test
	// body run BEFORE any t.Cleanup callback, so a `defer cleanup()` here fired
	// DROP DATABASE while openDB's connection to that same database was still
	// open — and Postgres refuses to drop a database with a live session. The
	// error was discarded by `_, _ =`, so every run silently left a database
	// behind.
	//
	// t.Cleanup callbacks run last-added-first, and this one is registered at
	// creation time — before any openDB call — so it runs AFTER their connection
	// closes. That ordering is the fix; terminating stragglers below is belt and
	// braces for pooled connections that outlive a Close.
	t.Cleanup(func() {
		// A *sql.DB is a pool: Close returns before the server has necessarily
		// reaped every backend, and one straggler is enough to block the drop.
		if _, err := admin.Exec(
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			  WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName,
		); err != nil {
			t.Logf("terminate backends on %s: %v", dbName, err)
		}
		// Reported, never discarded. A silent drop failure is exactly how this
		// leaked unnoticed for months; a visible one fails the test that caused it.
		if _, err := admin.Exec("DROP DATABASE IF EXISTS " + dbName); err != nil {
			t.Errorf("leaked test database %s: %v", dbName, err)
		}
		_ = admin.Close()
	})

	cfg := adminCfg
	cfg.Name = dbName
	return &cfg
}

func openDB(t *testing.T, cfg *config.DatabaseConfig) *sql.DB {
	t.Helper()
	connStr := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Name, cfg.SSLMode,
	)
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestSchemaGate_EmptyDBPassesProbe (B-8): a freshly-created
// database with no schema returns nil — exactly the state the
// row-count gate already permits, so the new probe doesn't
// regress the documented "restore into a brand-new DB" path.
func TestSchemaGate_EmptyDBPassesProbe(t *testing.T) {
	cfg := freshTestDB(t)
	if err := checkTargetSchemaAbsent(cfg); err != nil {
		t.Fatalf("empty DB tripped schema gate: %v", err)
	}
}

// TestSchemaGate_PopulatedMigrationsTableFails (B-8): the daemon-
// startup path leaves the migrations table with one row per applied
// migration. The probe MUST refuse — that's the reproducible
// fresh-install failure mode the bug report described.
func TestSchemaGate_PopulatedMigrationsTableFails(t *testing.T) {
	cfg := freshTestDB(t)
	db := openDB(t, cfg)
	if _, err := db.Exec(`CREATE TABLE migrations (version INT)`); err != nil {
		t.Fatalf("create migrations: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO migrations VALUES (1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	err := checkTargetSchemaAbsent(cfg)
	if err == nil {
		t.Fatal("populated migrations did not trip the gate")
	}
	if !strings.Contains(err.Error(), "schema already loaded") {
		t.Errorf("error doesn't mention schema-loaded; got: %v", err)
	}
}

// TestSchemaGate_OwnedTypeFails (B-8): the bootstrap-SQL path
// creates PG types BEFORE any migration runs. With an empty
// migrations table but artifact_class defined, the dump's
// `CREATE TYPE artifact_class` will collide. Probe must catch it.
func TestSchemaGate_OwnedTypeFails(t *testing.T) {
	cfg := freshTestDB(t)
	db := openDB(t, cfg)
	if _, err := db.Exec(`CREATE TYPE artifact_class AS ENUM ('INPUT', 'OUTPUT')`); err != nil {
		t.Fatalf("create type: %v", err)
	}
	err := checkTargetSchemaAbsent(cfg)
	if err == nil {
		t.Fatal("artifact_class type did not trip the gate")
	}
	if !strings.Contains(err.Error(), "artifact_class") {
		t.Errorf("error doesn't name the type: %v", err)
	}
}

// TestSchemaGate_DropTargetSchemaWipes (B-8): --clean must drop
// the schema AND recreate it with the right owner so the
// subsequent psql -f actually has a schema to populate.
func TestSchemaGate_DropTargetSchemaWipes(t *testing.T) {
	cfg := freshTestDB(t)
	db := openDB(t, cfg)
	if _, err := db.Exec(`CREATE TABLE migrations (version INT)`); err != nil {
		t.Fatalf("create migrations: %v", err)
	}
	if _, err := db.Exec(`CREATE TYPE artifact_class AS ENUM ('INPUT', 'OUTPUT')`); err != nil {
		t.Fatalf("create type: %v", err)
	}
	// Sanity: the probe correctly refuses pre-clean.
	if err := checkTargetSchemaAbsent(cfg); err == nil {
		t.Fatal("pre-clean: schema gate should fail")
	}
	if err := dropTargetSchema(cfg); err != nil {
		t.Fatalf("dropTargetSchema: %v", err)
	}
	// Post-clean: schema present (so psql can write) but empty.
	if err := checkTargetSchemaAbsent(cfg); err != nil {
		t.Errorf("post-clean: schema gate must accept the empty schema; got %v", err)
	}
	// Confirm the new schema exists + we own it (the latter via
	// trying to CREATE TABLE which only succeeds with USAGE +
	// CREATE on the schema).
	if _, err := db.Exec(`CREATE TABLE post_clean_probe (id INT)`); err != nil {
		t.Errorf("post-clean: CREATE TABLE failed — schema not granted to user: %v", err)
	}
}

// TestFreshTestDB_DropsItsDatabase is the regression test for a leak that went
// unnoticed long enough to accumulate 138 orphaned databases.
//
// The original helper dropped the database from a `defer` in the caller. Deferred
// functions run BEFORE t.Cleanup callbacks, so the drop fired while openDB's
// connection to that database was still open; Postgres refuses to drop a database
// with a live session, and the error was discarded by `_, _ =`.
//
// This test reproduces the exact shape that failed — create the database, open a
// connection to it as the real tests do — then asserts from an INDEPENDENT
// connection that the database is gone once the subtest's cleanups have run.
// Asserting from outside the subtest is the whole point: nothing inside it can
// observe its own teardown.
func TestFreshTestDB_DropsItsDatabase(t *testing.T) {
	var dbName string

	t.Run("inner", func(t *testing.T) {
		cfg := freshTestDB(t)
		dbName = cfg.Name
		// Open a connection and actually use it, mirroring the real tests. A
		// lazily-opened *sql.DB would not reproduce the failure: the session that
		// blocks the drop only exists once a query has run.
		db := openDB(t, cfg)
		if _, err := db.Exec(`CREATE TABLE probe (id INT)`); err != nil {
			t.Fatalf("exec against fresh DB: %v", err)
		}
	})

	if dbName == "" {
		t.Skip("inner subtest skipped (postgres unreachable)")
	}

	admin, err := sql.Open("postgres", fmt.Sprintf(
		"host=%s port=5432 user=%s password=%s dbname=postgres sslmode=disable",
		envOrDefault("POSTGRES_HOST", "localhost"),
		envOrDefault("POSTGRES_USER", "swarmd"),
		envOrDefault("POSTGRES_PASSWORD", "swarmd"),
	))
	if err != nil {
		t.Skipf("admin connection unavailable: %v", err)
	}
	defer func() { _ = admin.Close() }()

	var n int
	if err := admin.QueryRow(
		`SELECT count(*) FROM pg_database WHERE datname = $1`, dbName,
	).Scan(&n); err != nil {
		t.Fatalf("query pg_database: %v", err)
	}
	if n != 0 {
		t.Errorf("test database %s survived its own cleanup — the suite is leaking "+
			"one database per run", dbName)
	}
}
