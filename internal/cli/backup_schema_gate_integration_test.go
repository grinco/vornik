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

// freshTestDB creates a per-test database with a unique name + a
// DatabaseConfig pointed at it. Returns the config plus a cleanup
// callback that drops the DB. Skips the test when postgres isn't
// reachable so the suite stays green on hosts without a local
// daemon.
func freshTestDB(t *testing.T) (*config.DatabaseConfig, func()) {
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
	cleanup := func() {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + dbName)
		_ = admin.Close()
	}
	cfg := adminCfg
	cfg.Name = dbName
	return &cfg, cleanup
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
	cfg, cleanup := freshTestDB(t)
	defer cleanup()
	if err := checkTargetSchemaAbsent(cfg); err != nil {
		t.Fatalf("empty DB tripped schema gate: %v", err)
	}
}

// TestSchemaGate_PopulatedMigrationsTableFails (B-8): the daemon-
// startup path leaves the migrations table with one row per applied
// migration. The probe MUST refuse — that's the reproducible
// fresh-install failure mode the bug report described.
func TestSchemaGate_PopulatedMigrationsTableFails(t *testing.T) {
	cfg, cleanup := freshTestDB(t)
	defer cleanup()
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
	cfg, cleanup := freshTestDB(t)
	defer cleanup()
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
	cfg, cleanup := freshTestDB(t)
	defer cleanup()
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
