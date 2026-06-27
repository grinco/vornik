//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "github.com/lib/pq"
)

// integrationDBName is the canonical default for the integration
// test database. It is intentionally distinct from the daemon's
// "vornik_test" so a running daemon cannot race test fixtures via
// the scheduler's project-wide LeaseTask. Override with POSTGRES_DB
// when running in a CI image that provisions its own database.
const integrationDBName = "vornik_integration_test"

// getEnvOrDefault returns the environment variable value or a default.
// This is used by integration tests to allow environment-based configuration.
func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// integrationPort reads POSTGRES_PORT (default 5432) so the integration
// suite can be pointed at a throwaway instance on a non-default port —
// the same parameterization test/integration already uses. Without this
// the tests were hardcoded to 5432, forcing them onto whatever (possibly
// production) server listens there.
func integrationPort() int {
	if v := os.Getenv("POSTGRES_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			return p
		}
	}
	return 5432
}

// ensureIntegrationDB makes sure POSTGRES_DB (default
// "vornik_integration_test") exists AND has the 001_initial.sql
// bootstrap schema applied. The MigrationRunner used by Connect
// expects 001_initial.sql to have run out-of-band; on a clean host
// nothing else seeds it, so we do it here.
//
// Both the test/integration package and this package call into
// roughly the same shape — they race on the same fresh DB. A
// pg_advisory_lock around the schema application serialises them.
// Idempotent — once the `tasks` table exists the bootstrap is a
// no-op and Connect proceeds.
func ensureIntegrationDB() error {
	host := getEnvOrDefault("POSTGRES_HOST", "localhost")
	port := getEnvOrDefault("POSTGRES_PORT", "5432")
	user := getEnvOrDefault("POSTGRES_USER", "vornik")
	pass := getEnvOrDefault("POSTGRES_PASSWORD", "vornik")
	target := getEnvOrDefault("POSTGRES_DB", integrationDBName)

	adminDSN := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=postgres sslmode=disable",
		host, port, user, pass)
	adminDB, err := sql.Open("postgres", adminDSN)
	if err != nil {
		return fmt.Errorf("open admin DB: %w", err)
	}
	defer func() { _ = adminDB.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := adminDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping admin DB: %w", err)
	}
	var exists bool
	if err := adminDB.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, target,
	).Scan(&exists); err != nil {
		return fmt.Errorf("check db existence: %w", err)
	}
	if !exists {
		quoted, err := quoteIdent(target)
		if err != nil {
			return fmt.Errorf("invalid POSTGRES_DB value: %w", err)
		}
		if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+quoted); err != nil {
			return fmt.Errorf("create database %s: %w", target, err)
		}
	}
	return applyBootstrapSchema(host, port, user, pass, target)
}

// applyBootstrapSchema runs deployments/postgres/schema/001_initial.sql
// against the target database when the `tasks` table isn't already
// present. Serialised via pg_advisory_lock so cross-package callers
// (test/integration TestMain runs in parallel) don't double-apply.
func applyBootstrapSchema(host, port, user, pass, target string) error {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		user, pass, host, port, target)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("open target DB for bootstrap: %w", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const bootstrapLockKey int64 = 7717437012345
	if _, err := db.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, bootstrapLockKey); err != nil {
		return fmt.Errorf("acquire bootstrap lock: %w", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, bootstrapLockKey)
	}()

	var hasTasks bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = 'tasks')`,
	).Scan(&hasTasks); err != nil {
		return fmt.Errorf("check bootstrap state: %w", err)
	}
	if hasTasks {
		return nil
	}

	body, err := os.ReadFile(bootstrapSchemaPath())
	if err != nil {
		return fmt.Errorf("read bootstrap schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("apply bootstrap schema: %w", err)
	}
	return nil
}

// bootstrapSchemaPath walks up from the test binary's cwd looking
// for deployments/postgres/schema/001_initial.sql. Works whether
// tests run from the repo root or a subdirectory.
func bootstrapSchemaPath() string {
	const rel = "deployments/postgres/schema/001_initial.sql"
	dir, err := os.Getwd()
	if err != nil {
		return rel
	}
	for {
		candidate := filepath.Join(dir, rel)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return rel
		}
		dir = parent
	}
}

// quoteIdent returns a safely-quoted SQL identifier. Refuses names
// that contain a double-quote or NUL byte — both can break out of
// the quoted literal.
func quoteIdent(name string) (string, error) {
	for i := 0; i < len(name); i++ {
		if name[i] == '"' || name[i] == 0 {
			return "", fmt.Errorf("identifier contains unsafe character at position %d", i)
		}
	}
	return `"` + name + `"`, nil
}
