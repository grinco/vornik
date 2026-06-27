//go:build integration

package cli

// Shared harness for the DB-backed CLI integration coverage tests.
//
// These tests drive the real run* cobra handlers (runMemoryScopeList,
// runMemoryWipe, runRetention, …) end-to-end against a live test
// Postgres. The seam every command shares is config.Load() →
// storage.Open(ctx, cfg.Database); we exploit it by writing a temp
// vornik config whose `database` block points at the test DB, pointing
// VORNIK_CONFIG at it, and migrating the schema once per process.
//
// Run with (the container the operator provisioned):
//   TEST_DATABASE_URL=postgres://vornik:vornik@localhost:5433/vornik_integration_test?sslmode=disable \
//   POSTGRES_HOST=localhost POSTGRES_PORT=5433 POSTGRES_USER=vornik \
//   POSTGRES_PASSWORD=vornik POSTGRES_DB=vornik_integration_test \
//   go test -tags=integration ./internal/cli/... -count=1
//
// NOTE: the package's other integration file (backup_schema_gate_*)
// owns envOrDefault + openDB; we reuse envOrDefault here and avoid
// re-declaring it.

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence/postgres"
)

// dbcovTestPort resolves the port for the test Postgres from
// POSTGRES_PORT (the operator's container runs on 5433), defaulting
// to the standard 5432 when unset.
func dbcovTestPort(t *testing.T) int {
	t.Helper()
	raw := envOrDefault("POSTGRES_PORT", "5432")
	p, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("bad POSTGRES_PORT %q: %v", raw, err)
	}
	return p
}

// dbcovDBConfig returns a config.DatabaseConfig pointed at the test
// Postgres described by the POSTGRES_* env (or localhost defaults).
func dbcovDBConfig(t *testing.T) config.DatabaseConfig {
	t.Helper()
	return config.DatabaseConfig{
		Driver:   "postgres",
		Host:     envOrDefault("POSTGRES_HOST", "localhost"),
		Port:     dbcovTestPort(t),
		Name:     envOrDefault("POSTGRES_DB", "vornik_integration_test"),
		User:     envOrDefault("POSTGRES_USER", "vornik"),
		Password: envOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:  "disable",
	}
}

var (
	dbcovMigrateOnce sync.Once
	dbcovMigrateErr  error
)

// dbcovOpen returns a live *sql.DB for direct seeding/assertions,
// migrating the schema exactly once per test process. It Skips (not
// fails) when the test Postgres is unreachable so the suite stays
// green on hosts without the throwaway container.
func dbcovOpen(t *testing.T) *sql.DB {
	t.Helper()
	cfg := dbcovDBConfig(t)
	connStr := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Name, cfg.SSLMode)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Skipf("postgres open failed, skipping: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("postgres unreachable, skipping: %v", err)
	}

	dbcovMigrateOnce.Do(func() {
		pgCfg := postgres.Config{
			Host:           cfg.Host,
			Port:           cfg.Port,
			Database:       cfg.Name,
			User:           cfg.User,
			Password:       cfg.Password,
			SSLMode:        cfg.SSLMode,
			ConnectTimeout: 10 * time.Second,
		}
		mctx, mcancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer mcancel()
		pg, err := postgres.Connect(mctx, pgCfg)
		if err != nil {
			dbcovMigrateErr = fmt.Errorf("connect for migrate: %w", err)
			return
		}
		defer func() { _ = pg.Close() }()
		if err := pg.Migrate(mctx); err != nil {
			dbcovMigrateErr = fmt.Errorf("migrate: %w", err)
		}
	})
	if dbcovMigrateErr != nil {
		_ = db.Close()
		t.Fatalf("schema migration failed: %v", dbcovMigrateErr)
	}

	t.Cleanup(func() { _ = db.Close() })
	return db
}

// dbcovWriteConfig writes a temp vornik config whose database block
// points at the test Postgres, sets VORNIK_CONFIG to it (auto-reset
// by t.Setenv at test end), and returns the path. auth_enabled:false
// keeps config.Validate happy without needing static API keys.
func dbcovWriteConfig(t *testing.T, db config.DatabaseConfig) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := fmt.Sprintf(`server:
  address: ":8080"
database:
  driver: postgres
  host: %q
  port: %d
  name: %q
  user: %q
  password: %q
  sslmode: %q
storage:
  artifacts_path: %q
api:
  auth_enabled: false
`, db.Host, db.Port, db.Name, db.User, db.Password, db.SSLMode,
		filepath.Join(dir, "artifacts"))
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("VORNIK_CONFIG", path)
	return path
}

// dbcovSetup is the common per-test prologue: open+migrate the DB and
// point VORNIK_CONFIG at it. Returns the live handle for seeding.
func dbcovSetup(t *testing.T) *sql.DB {
	t.Helper()
	db := dbcovOpen(t)
	dbcovWriteConfig(t, dbcovDBConfig(t))
	return db
}

// dbcovResetFlags swaps flag.CommandLine for a fresh FlagSet so the
// next config.Load() can re-register its --config/--version flags
// without panicking ("flag redefined"). config.Load() calls
// flag.String on the process-global CommandLine; running more than
// one DB-backed handler per test binary therefore requires a reset
// between invocations. Safe in tests: we don't consume the parsed
// flags, only VORNIK_CONFIG.
func dbcovResetFlags() {
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	// config.Load() calls flag.Parse() on os.Args, which under `go
	// test` carry -test.* flags this fresh set doesn't know about.
	// ContinueOnError means Parse returns an error (which Load
	// ignores) instead of os.Exit; discard the usage spew so it
	// doesn't pollute test output.
	fs.SetOutput(io.Discard)
	flag.CommandLine = fs
}

// dbcovCapture redirects os.Stdout for the duration of fn and returns
// everything fn printed. The handlers write straight to os.Stdout
// (fmt.Printf / tabwriter on os.Stdout), so this is the only way to
// assert their human-facing output. flag.CommandLine is reset first
// so the nested config.Load() can re-register its flags.
func dbcovCapture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	dbcovResetFlags()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	outCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		outCh <- string(b)
	}()

	runErr := fn()

	_ = w.Close()
	os.Stdout = orig
	out := <-outCh
	_ = r.Close()
	return out, runErr
}

// dbcovUniqueProject returns a project ID unique to this test run so
// concurrent suites and re-runs against the shared DB never collide.
func dbcovUniqueProject(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// dbcovSeedChunk inserts one project_memory_chunks row, filling the
// NOT-NULL columns the schema requires. repoScope may be "" to leave
// repo_scope NULL (the uncategorized partition).
func dbcovSeedChunk(t *testing.T, db *sql.DB, project, sourceName, content, repoScope string) string {
	t.Helper()
	id := fmt.Sprintf("chunk-%d", time.Now().UnixNano())
	// Stagger so two seeds in the same microsecond get distinct IDs +
	// content hashes.
	time.Sleep(time.Microsecond)
	hash := fmt.Sprintf("hash-%s-%s", id, content)
	var scope any
	if repoScope != "" {
		scope = repoScope
	}
	_, err := db.Exec(`
		INSERT INTO project_memory_chunks
		    (id, project_id, source_name, chunk_index, content, content_hash, created_at, repo_scope)
		VALUES ($1, $2, $3, 0, $4, $5, now(), $6)`,
		id, project, sourceName, content, hash, scope)
	if err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	return id
}

// dbcovCleanupProject deletes every seeded row for a project across
// the wipe-relevant tables. Registered via t.Cleanup so the shared DB
// doesn't accumulate cross-test residue (project IDs are unique, so
// this is belt-and-suspenders).
func dbcovCleanupProject(t *testing.T, db *sql.DB, project string) {
	t.Helper()
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM knowledge_edges WHERE project_id = $1`,
			`DELETE FROM knowledge_entities WHERE project_id = $1`,
			`DELETE FROM project_memory_chunks WHERE project_id = $1`,
			`DELETE FROM project_memory_quarantine WHERE project_id = $1`,
			`DELETE FROM project_ingest_queue WHERE project_id = $1`,
			`DELETE FROM corpus_epochs WHERE project_id = $1`,
			`DELETE FROM memory_retrieval_audit WHERE project_id = $1`,
		} {
			_, _ = db.Exec(q, project)
		}
	})
}
