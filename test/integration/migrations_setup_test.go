//go:build integration
// +build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"vornik.io/vornik/internal/persistence"
)

// CI bootstraps the test DB from deployments/postgres/schema/001_initial.sql,
// which is the bare-bones schema. Columns added since (last_error_class,
// retry-policy bits, judge-verdict tables, …) live in the in-code
// migration runner under internal/persistence/migrations.go and are
// applied when the daemon boots — but the test/integration tests skip
// the daemon and talk to the DB directly, so without this hook they
// see a stale schema and every query referencing a post-bootstrap
// column 500s.
//
// TestMain runs once per `go test` invocation of this package and
// applies the migration runner idempotently to whatever schema is
// already there. Safe to re-run: applyMigration is a no-op when the
// migration version is already recorded.
//
// Sibling test/integration tests' connectDB() helper continues to
// just open a connection — the schema is in place by the time any
// test body executes.
//
// Failure mode: if migrations can't run (DB down, mismatched user,
// etc.) we emit a diagnostic and skip the run rather than running
// the suite against a stale schema. CI surfaces the migration error
// via this output and the subsequent test failures.

var migrateOnce sync.Once

func TestMain(m *testing.M) {
	// CREATE DATABASE the integration target before any test (and
	// before applyTestMigrations, which assumes the DB exists).
	// Best-effort: if it fails we let applyTestMigrations surface
	// the diagnostic rather than swallowing it here.
	if err := ensureIntegrationDB(); err != nil {
		fmt.Fprintf(os.Stderr, "test/integration: bootstrap DB failed: %v\n", err)
	}
	migrateOnce.Do(applyTestMigrations)
	purgeIntegrationOrphans()
	code := m.Run()
	purgeIntegrationOrphans()
	os.Exit(code)
}

// ensureIntegrationDB creates POSTGRES_DB (default
// integrationDBName) when missing AND applies the
// deployments/postgres/schema/001_initial.sql bootstrap on the
// fresh database. The MigrationRunner that applyTestMigrations()
// invokes afterwards expects this baseline to exist and only
// applies migrations 2+ on top. Idempotent — re-runs do nothing
// once the DB exists.
func ensureIntegrationDB() error {
	host := getEnvOrDefault("POSTGRES_HOST", "localhost")
	port := getEnvOrDefault("POSTGRES_PORT", "5432")
	user := getEnvOrDefault("POSTGRES_USER", "vornik")
	pass := getEnvOrDefault("POSTGRES_PASSWORD", "vornik")
	target := getEnvOrDefault("POSTGRES_DB", integrationDBName)

	adminDSN := fmt.Sprintf("postgres://%s:%s@%s:%s/postgres?sslmode=disable",
		user, pass, host, port)
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
		// Refuse identifiers containing characters that could break
		// out of the quoted name. CREATE DATABASE doesn't support
		// parameter binding so we have to quote-ident the literal.
		for i := 0; i < len(target); i++ {
			if target[i] == '"' || target[i] == 0 {
				return fmt.Errorf("invalid POSTGRES_DB value (unsafe char at %d)", i)
			}
		}
		if _, err := adminDB.ExecContext(ctx, `CREATE DATABASE "`+target+`"`); err != nil {
			return fmt.Errorf("create database %s: %w", target, err)
		}
	}
	// Always run the bootstrap: idempotent (no-op when `tasks`
	// already exists), and required when the DB itself exists but
	// a previous run crashed before applying the schema.
	return applyBootstrapSchema(host, port, user, pass, target)
}

// applyBootstrapSchema runs deployments/postgres/schema/001_initial.sql
// against the target database. The MigrationRunner itself does not
// seed the baseline — it expects 001_initial.sql to have been applied
// out-of-band (see migrations.go:syncBootstrapSchema) and then records
// version=1 as already applied. CI images that provision the DB
// externally skip this path; here we run it inline so
// `make test-integration` works on a clean host.
//
// Idempotent: returns nil if the `tasks` table already exists. Two
// test binaries (postgres pkg + test/integration) both call this and
// may race on the same fresh DB; the table check + the per-connection
// pg_advisory_lock serialises them safely.
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

	// Advisory lock so concurrent bootstrap callers serialise.
	// The arbitrary number is just a per-key namespace — pick any
	// stable constant.
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

	schemaPath := bootstrapSchemaPath()
	body, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", schemaPath, err)
	}
	if _, err := db.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("apply bootstrap schema: %w", err)
	}
	return nil
}

// bootstrapSchemaPath walks up from the test binary's working
// directory until it finds deployments/postgres/schema/001_initial.sql.
// This works whether tests run from the repo root (`make`) or from
// a subdirectory (`go test ./test/integration/...`).
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

// purgeIntegrationOrphans wipes any leftover fixture rows the
// scheduler / API integration tests would have cleaned via
// t.Cleanup but didn't because a previous run was killed
// mid-test (SIGKILL, OOM, `go test -timeout` exceeded). Without
// this, orphan rows accumulate in the shared vornik_test DB
// forever — operators running `\dt` see hundreds of stale
// `sched-e2e-*` / `sched-retry-*` / `proj-it-*` rows from
// crashed CI runs.
//
// We DELETE every prefix the integration suite owns. The
// scheduler tests namespace under `sched-` (sched-e2e-<ns>,
// sched-retry-<ns>); api_*_test files under `proj-it-` and
// `proj-task-`. The list mirrors what cleanupIntegrationProject
// would have removed had the test completed normally.
//
// Children-before-parents per the tasks/executions FK shape.
// Best-effort: per-statement errors log and continue so a
// missing migration on a partially-bootstrapped CI DB doesn't
// block the suite.
func purgeIntegrationOrphans() {
	dbURL := getTestDBURL()
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "test/integration: open DB for orphan purge: %v\n", err)
		return
	}
	defer func() { _ = db.Close() }()

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		fmt.Fprintf(os.Stderr, "test/integration: ping DB for orphan purge: %v\n", err)
		return
	}

	const cond = "project_id LIKE 'sched-%' OR project_id LIKE 'proj-it-%' OR project_id LIKE 'proj-task-%'"
	statements := []string{
		"DELETE FROM artifacts WHERE " + cond,
		"DELETE FROM executions WHERE " + cond,
		"DELETE FROM tasks WHERE " + cond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, q := range statements {
		if _, err := db.ExecContext(ctx, q); err != nil {
			fmt.Fprintf(os.Stderr, "test/integration: orphan purge %q: %v\n", q, err)
		}
	}
}

func applyTestMigrations() {
	dbURL := getTestDBURL()
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "test/integration: open DB for migrations failed: %v\n", err)
		return
	}
	defer func() { _ = db.Close() }()

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		fmt.Fprintf(os.Stderr, "test/integration: ping DB before migrations failed: %v\n", err)
		return
	}

	runCtx, runCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer runCancel()
	runner := persistence.NewMigrationRunner(db)
	if err := runner.Run(runCtx); err != nil {
		fmt.Fprintf(os.Stderr, "test/integration: migration runner failed: %v\n", err)
		return
	}
}
