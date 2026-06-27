package cli

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"vornik.io/vornik/internal/archiveutil"
	"vornik.io/vornik/internal/config"

	_ "github.com/lib/pq"
)

var (
	backupOut            string
	restoreIn            string
	restoreForce         bool
	restoreAllowNonEmpty bool
	restoreClean         bool
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Create a one-file archive of the vornik deployment",
	Long: `Create a tarball containing:
  - pg_dump of the database
  - the artifacts directory
  - the project workspaces directory (per-project git repos)
  - a snapshot of the main config file + configs/ directory

Output path defaults to ./vornik-backup-YYYYMMDD-HHMMSS.tgz. The archive
is portable across hosts when restored via 'vornikctl restore'.

Requires: pg_dump on PATH with credentials resolved from config.

Examples:
  vornikctl backup
  vornikctl backup --out /backups/daily.tgz
`,
	RunE: runBackup,
}

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Restore a vornik deployment from a backup archive",
	Long: `Restore the database, artifacts directory, project workspaces, and
configs from an archive produced by 'vornikctl backup'. The daemon
MUST NOT be running. Requires psql on PATH. Refuses to run unless
--force is set.

Two safety gates run before the restore proceeds:
  1. Schema-presence gate (B-8): if the target already carries a
     vornik-owned schema (migrations rows OR canonical PG types like
     artifact_class), the restore is refused. Override with --clean
     (drops the schema first) or --allow-non-empty (proceeds anyway,
     usually fails on CREATE TYPE collisions).
  2. Row-count gate: refuses targets with non-empty projects/tasks
     tables. Override with --allow-non-empty.

Examples:
  # Fresh target — no vornik schema yet
  vornikctl restore --from vornik-backup-20260501-040000.tgz --force

  # Daemon already migrated the target; wipe + restore
  vornikctl restore --from <archive> --force --clean

  # Explicitly accept collisions (rarely useful)
  vornikctl restore --from <archive> --force --allow-non-empty

See https://docs.vornik.io "Backup and Restore" for the full matrix.
`,
	RunE: runRestore,
}

func init() {
	backupCmd.Flags().StringVar(&backupOut, "out", "", "output archive path (default auto-generated)")
	restoreCmd.Flags().StringVar(&restoreIn, "from", "", "archive path (required)")
	restoreCmd.Flags().BoolVar(&restoreForce, "force", false, "required — confirms the daemon is stopped and the DB can be overwritten")
	restoreCmd.Flags().BoolVar(&restoreAllowNonEmpty, "allow-non-empty", false, "permit restore into a DB with existing project/task rows")
	restoreCmd.Flags().BoolVar(&restoreClean, "clean", false, "DROP SCHEMA public CASCADE before restore — wipes any vornik schema already loaded on the target")
	_ = restoreCmd.MarkFlagRequired("from")
	rootCmd.AddCommand(backupCmd)
	rootCmd.AddCommand(restoreCmd)
}

func runBackup(cmd *cobra.Command, args []string) error {
	cfg, configPath, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	outPath := backupOut
	if outPath == "" {
		outPath = fmt.Sprintf("vornik-backup-%s.tgz", time.Now().Format("20060102-150405"))
	}

	// Stage everything into a temp dir, then tar it up. pg_dump goes in
	// first because it's the most likely to fail (missing tool, bad creds)
	// and we want to fail fast before touching anything else.
	staging, err := os.MkdirTemp("", "vornik-backup-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	// 1. pg_dump
	dumpPath := filepath.Join(staging, "db.sql")
	// Use a short-lived 0600 .pgpass file instead of PGPASSWORD in the env:
	// PGPASSWORD leaks into /proc/<pid>/environ where any process running as
	// the same user can read it, and into ps output on some platforms.
	pgpassPath, cleanup, err := writePGPassFile(&cfg.Database)
	if err != nil {
		return fmt.Errorf("write pgpass: %w", err)
	}
	defer cleanup()
	pgEnv := append(os.Environ(), "PGPASSFILE="+pgpassPath)
	pgArgs := []string{
		"-h", cfg.Database.Host,
		"-p", fmt.Sprint(cfg.Database.Port),
		"-U", cfg.Database.User,
		"-d", cfg.Database.Name,
		"--no-owner", "--no-privileges",
		"-f", dumpPath,
	}
	pgCmd := exec.Command("pg_dump", pgArgs...)
	pgCmd.Env = pgEnv
	if out, err := pgCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pg_dump failed: %w\n%s", err, out)
	}
	fmt.Printf("✔ database dumped (%d bytes)\n", fileSize(dumpPath))

	// 2. artifacts dir (optional — may not exist on fresh deployments).
	artifactsSrc := cfg.Storage.ArtifactsPath
	if artifactsSrc != "" {
		if info, err := os.Stat(artifactsSrc); err == nil && info.IsDir() {
			artifactsDst := filepath.Join(staging, "artifacts")
			if err := copyDir(artifactsSrc, artifactsDst); err != nil {
				return fmt.Errorf("copy artifacts: %w", err)
			}
			fmt.Printf("✔ artifacts copied from %s\n", artifactsSrc)
		} else {
			fmt.Printf("  (skipping artifacts: %s not found)\n", artifactsSrc)
		}
	}

	// 3. project workspaces dir (optional — base dir for per-project git repos).
	workspacesSrc := cfg.Runtime.ProjectWorkspacePath
	if workspacesSrc != "" {
		if info, err := os.Stat(workspacesSrc); err == nil && info.IsDir() {
			workspacesDst := filepath.Join(staging, "workspaces")
			if err := copyDir(workspacesSrc, workspacesDst); err != nil {
				return fmt.Errorf("copy project workspaces: %w", err)
			}
			fmt.Printf("✔ project workspaces copied from %s\n", workspacesSrc)
		} else {
			fmt.Printf("  (skipping project workspaces: %s not found)\n", workspacesSrc)
		}
	}

	// 4. config snapshot: main file + configs/ sibling tree.
	if configPath != "" {
		if err := copyFile(configPath, filepath.Join(staging, "config.yaml")); err != nil {
			return fmt.Errorf("copy config: %w", err)
		}
		fmt.Printf("✔ config file copied\n")

		if dir := resolveConfigsDir(configPath); dir != "" {
			if err := copyDir(dir, filepath.Join(staging, "configs")); err != nil {
				return fmt.Errorf("copy configs dir: %w", err)
			}
			fmt.Printf("✔ configs dir copied from %s\n", dir)
		}
	}

	// 5. Marker file so restore can validate it's a real vornik archive.
	if err := os.WriteFile(filepath.Join(staging, "VORNIK_BACKUP"), []byte("vornik-backup-v1\n"), 0o644); err != nil {
		return err
	}

	// 6. tar.gz the staging dir.
	if err := tarGzDir(staging, outPath); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	fmt.Printf("\nbackup written: %s (%d bytes)\n", outPath, fileSize(outPath))
	return nil
}

func runRestore(cmd *cobra.Command, args []string) error {
	if !restoreForce {
		return fmt.Errorf("--force is required. stop the vornik daemon and confirm you want to overwrite the target database")
	}
	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// B-8: schema-presence guard. The fresh-install path
	// (`make infra-create` → `vornik starts` → migrations run →
	// operator runs `vornikctl restore`) leaves the target with a
	// full migration-applied schema but ZERO business rows. The
	// row-count gate below (projects / tasks) passes, then psql
	// hits `CREATE TYPE artifact_class already exists` ~line 74 of
	// the dump and dies, leaving a half-restored target. The
	// schema-presence probe closes that gap.
	//
	// Three behaviours:
	//   - --clean: drop the public schema first, then proceed. The
	//     documented "wipe + restore" path for the fresh-install
	//     failure mode.
	//   - --allow-non-empty: explicit operator override. Restore
	//     proceeds even with schema loaded; psql still likely
	//     errors but the operator chose to try.
	//   - neither: refuse with a clear error naming both recovery
	//     paths.
	if restoreClean {
		if err := dropTargetSchema(&cfg.Database); err != nil {
			return fmt.Errorf("--clean: %w", err)
		}
		fmt.Println("✔ target schema wiped (--clean)")
	} else if !restoreAllowNonEmpty {
		if err := checkTargetSchemaAbsent(&cfg.Database); err != nil {
			return fmt.Errorf("%w\n\nrecovery paths:\n"+
				"  - re-run with --clean to drop + recreate the schema, OR\n"+
				"  - manually `DROP SCHEMA public CASCADE; CREATE SCHEMA public AUTHORIZATION %s;`, OR\n"+
				"  - re-run with --allow-non-empty if the schema came from the same vornik version\n"+
				"    (will likely still fail on CREATE TYPE collisions — see https://docs.vornik.io)",
				err, cfg.Database.User)
		}
	}

	// Non-empty-target guard: refuse to overwrite a populated DB
	// unless --allow-non-empty is set. The old behaviour was that
	// psql -f would happily clobber existing tables; an operator
	// running `vornikctl restore --force` against the wrong DB had
	// no safety net. Now we count rows in projects + tasks; either
	// > 0 trips the guard. (Migrations / schema_version are
	// expected; we don't probe them.)
	//
	// Skipped after --clean since we just emptied the schema.
	if !restoreAllowNonEmpty && !restoreClean {
		if err := checkTargetEmpty(&cfg.Database); err != nil {
			return fmt.Errorf("%w (pass --allow-non-empty to override)", err)
		}
	}

	staging, err := os.MkdirTemp("", "vornik-restore-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := untarGz(restoreIn, staging); err != nil {
		return fmt.Errorf("extract archive: %w", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "VORNIK_BACKUP")); err != nil {
		return fmt.Errorf("not a vornik backup archive (missing VORNIK_BACKUP marker)")
	}
	fmt.Println("✔ archive validated")

	// Restore the database with psql in a single transaction so a
	// mid-restore failure leaves the target consistent (--set
	// ON_ERROR_STOP=on aborts on the first error; -1 wraps the
	// whole file in BEGIN/COMMIT). Atomicity goal from
	// saas-readiness §3.13.
	dumpPath := filepath.Join(staging, "db.sql")
	pgpassPath, cleanup, err := writePGPassFile(&cfg.Database)
	if err != nil {
		return fmt.Errorf("write pgpass: %w", err)
	}
	defer cleanup()
	pgEnv := append(os.Environ(), "PGPASSFILE="+pgpassPath)
	pgArgs := []string{
		"-h", cfg.Database.Host,
		"-p", fmt.Sprint(cfg.Database.Port),
		"-U", cfg.Database.User,
		"-d", cfg.Database.Name,
		"--single-transaction",
		"--set", "ON_ERROR_STOP=on",
		"-f", dumpPath,
	}
	pgCmd := exec.Command("psql", pgArgs...)
	pgCmd.Env = pgEnv
	if out, err := pgCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("psql restore failed: %w\n%s", err, out)
	}
	fmt.Println("✔ database restored (single-transaction)")

	// Restore artifacts.
	artifactsSrc := filepath.Join(staging, "artifacts")
	if info, err := os.Stat(artifactsSrc); err == nil && info.IsDir() {
		if cfg.Storage.ArtifactsPath == "" {
			fmt.Println("  (skipping artifacts: no storage.artifacts_path configured)")
		} else {
			if err := copyDir(artifactsSrc, cfg.Storage.ArtifactsPath); err != nil {
				return fmt.Errorf("restore artifacts: %w", err)
			}
			fmt.Printf("✔ artifacts restored to %s\n", cfg.Storage.ArtifactsPath)
		}
	}

	// Restore project workspaces.
	workspacesSrc := filepath.Join(staging, "workspaces")
	if info, err := os.Stat(workspacesSrc); err == nil && info.IsDir() {
		if cfg.Runtime.ProjectWorkspacePath == "" {
			fmt.Println("  (skipping project workspaces: no runtime.project_workspace_path configured)")
		} else {
			if err := copyDir(workspacesSrc, cfg.Runtime.ProjectWorkspacePath); err != nil {
				return fmt.Errorf("restore project workspaces: %w", err)
			}
			fmt.Printf("✔ project workspaces restored to %s\n", cfg.Runtime.ProjectWorkspacePath)
		}
	}

	fmt.Println("\nrestore complete. Config files in the archive were NOT applied — compare against your current deployment manually and merge as needed.")
	return nil
}

// The tar.gz packaging + safe-extraction helpers moved to
// internal/archiveutil so the support-report bundle builder
// (internal/api + internal/cli/support_report.go) can reuse the SAME
// path-traversal / symlink guards rather than duplicating them (see
// https://docs.vornik.io §4.1/§7). These thin
// wrappers keep the existing call sites + tests in this package
// unchanged.

func copyFile(src, dst string) error { return archiveutil.CopyFile(src, dst) }

func copyDir(src, dst string) error { return archiveutil.CopyDir(src, dst) }

func tarGzDir(dir, out string) error { return archiveutil.TarGzDir(dir, out) }

func untarGz(archive, dir string) error { return archiveutil.UntarGz(archive, dir) }

func fileSize(p string) int64 { return archiveutil.FileSize(p) }

// checkTargetEmpty connects to the target DB and refuses the
// restore when projects or tasks already hold rows. Returns nil
// when the DB looks fresh — either both probe tables are empty
// or the tables don't exist yet (brand-new install before
// migrations).
//
// Why these two tables: projects is the operator's primary
// identity surface (anything in it is operator-curated content),
// and tasks is the only other table that could realistically
// hold legitimate work in a "fresh" install. Counting both keeps
// the guard tight without enumerating every table.
//
// Implementation: two queries per table. First a to_regclass
// probe (returns NULL when the table doesn't exist); then a
// COUNT only when the table exists. Keeps the SQL readable and
// avoids the awkward "COALESCE around a subquery in a WHERE
// clause" form.
func checkTargetEmpty(db *config.DatabaseConfig) error {
	connStr := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		db.Host, db.Port, db.User, db.Password, db.Name, fallbackSSLMode(db.SSLMode))
	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("open target database for non-empty probe: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// Allowlist for defensive table-name interpolation. The two
	// names are compile-time constants in this function; the
	// allowlist exists so a future caller can't widen the call
	// site without thinking about SQL-injection safety.
	allow := map[string]struct{}{"projects": {}, "tasks": {}}
	for _, table := range []string{"projects", "tasks"} {
		if _, ok := allow[table]; !ok {
			return fmt.Errorf("internal: refusing to probe unknown table %q", table)
		}
		var exists bool
		if err := conn.QueryRow(
			"SELECT to_regclass('public.' || $1) IS NOT NULL",
			table,
		).Scan(&exists); err != nil {
			return fmt.Errorf("probe %s existence: %w", table, err)
		}
		if !exists {
			continue
		}
		var n int
		if err := conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			return fmt.Errorf("probe %s row count: %w", table, err)
		}
		if n > 0 {
			return fmt.Errorf("refusing restore: target database has %d %s row(s)", n, table)
		}
	}
	return nil
}

// checkTargetSchemaAbsent (B-8) returns an error when the target
// database already carries a vornik-owned schema — either because
// the daemon started and ran its migrations, or because a previous
// restore left tables behind. The detection is two-pronged so it
// catches both the migrations-path (a populated `migrations` table)
// and the bootstrap-SQL-path (typed enum like artifact_class
// already exists even when `migrations` is empty).
//
// Returns nil when the target looks unconditionally safe to
// restore into: no `migrations` rows AND none of the canonical
// vornik-owned types present.
func checkTargetSchemaAbsent(db *config.DatabaseConfig) error {
	connStr := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		db.Host, db.Port, db.User, db.Password, db.Name, fallbackSSLMode(db.SSLMode))
	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("open target database for schema probe: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Probe 1: the migrations bookkeeping table. If it exists with
	// rows, the daemon has applied at least one migration against
	// this database — the schema is loaded.
	var migrationsExists bool
	if err := conn.QueryRow(
		"SELECT to_regclass('public.migrations') IS NOT NULL",
	).Scan(&migrationsExists); err != nil {
		return fmt.Errorf("probe migrations existence: %w", err)
	}
	if migrationsExists {
		var n int
		if err := conn.QueryRow("SELECT COUNT(*) FROM migrations").Scan(&n); err != nil {
			return fmt.Errorf("probe migrations row count: %w", err)
		}
		if n > 0 {
			return fmt.Errorf("refusing restore: target database has %d applied migration(s) — schema already loaded", n)
		}
	}

	// Probe 2: a small allowlist of vornik-owned PG types. These
	// come from the bootstrap SQL (deployments/postgres/schema/
	// 001_initial.sql) and a `CREATE TYPE` in the dump can't be
	// idempotent — psql hits "already exists" and bails out.
	// Allowlist of canonical types kept narrow so future operator
	// types in the same DB don't trip the gate.
	vornikTypes := []string{"artifact_class", "task_status", "execution_status"}
	for _, typ := range vornikTypes {
		var exists bool
		if err := conn.QueryRow(
			"SELECT EXISTS (SELECT 1 FROM pg_type WHERE typname = $1)",
			typ,
		).Scan(&exists); err != nil {
			return fmt.Errorf("probe pg_type %s: %w", typ, err)
		}
		if exists {
			return fmt.Errorf("refusing restore: target database already defines vornik type %q", typ)
		}
	}
	return nil
}

// dropTargetSchema (B-8) wipes the public schema on the target
// database — the documented "wipe + restore" path triggered by the
// --clean flag. The drop + recreate runs in one tx so a partial
// failure leaves either the original schema OR an empty schema,
// never a half-dropped one. The recreated schema is granted to the
// configured connection user so the subsequent psql restore can
// actually populate it.
func dropTargetSchema(db *config.DatabaseConfig) error {
	connStr := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		db.Host, db.Port, db.User, db.Password, db.Name, fallbackSSLMode(db.SSLMode))
	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("open target for --clean: %w", err)
	}
	defer func() { _ = conn.Close() }()

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("begin --clean tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec("DROP SCHEMA public CASCADE"); err != nil {
		return fmt.Errorf("DROP SCHEMA public CASCADE: %w", err)
	}
	// The user is interpolated, not parameterised, because PG
	// AUTHORIZATION accepts an identifier (not a string literal).
	// Interpolation safety: the user name comes from operator-
	// authored config, the same source that authenticated the
	// connection above. If they can inject SQL here they could
	// already DROP DATABASE.
	stmt := fmt.Sprintf("CREATE SCHEMA public AUTHORIZATION %s", db.User)
	if _, err := tx.Exec(stmt); err != nil {
		return fmt.Errorf("CREATE SCHEMA public AUTHORIZATION %s: %w", db.User, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit --clean tx: %w", err)
	}
	return nil
}

// fallbackSSLMode picks a safe default when the config didn't set
// one. Mirrors the daemon's connection-builder default.
func fallbackSSLMode(in string) string {
	if strings.TrimSpace(in) == "" {
		return "disable"
	}
	return in
}

// writePGPassFile writes a short-lived .pgpass-format file holding the
// database password with 0600 permissions, and returns the path + a
// cleanup callback the caller must defer. pg_dump / psql pick the file up
// via PGPASSFILE, so the password never travels through the process
// environment.
func writePGPassFile(db *config.DatabaseConfig) (string, func(), error) {
	f, err := os.CreateTemp("", "vornik-pgpass-*")
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, err
	}
	// .pgpass format: host:port:database:user:password. Colons and
	// backslashes in any field must be backslash-escaped.
	esc := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `:`, `\:`)
		return s
	}
	line := fmt.Sprintf("%s:%d:%s:%s:%s\n",
		esc(db.Host), db.Port, esc(db.Name), esc(db.User), esc(db.Password))
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}
