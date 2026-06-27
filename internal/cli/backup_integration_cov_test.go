//go:build integration

package cli

// Integration coverage for backup.go's DB-touching paths. pg_dump and
// psql ARE available in this environment (verified: /usr/bin/pg_dump,
// /usr/bin/psql), so we exercise the runBackup happy path end-to-end
// against the live test Postgres and the schema/empty gates against
// the real (migrated) target.
//
// The runRestore happy path is intentionally NOT exercised here: the
// only reachable target is the shared, fully-migrated test DB, and a
// real restore would DROP/replace its schema mid-suite, breaking every
// other test that shares the database. The restore exec surface is
// covered indirectly — checkTargetSchemaAbsent / checkTargetEmpty /
// dropTargetSchema are the gates runRestore calls, and those are
// tested directly here and in backup_schema_gate_integration_test.go.

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func backupInt_reset() {
	backupOut, restoreIn = "", ""
	restoreForce, restoreAllowNonEmpty, restoreClean = false, false, false
}

func TestIntegration_RunBackup_ProducesValidArchive(t *testing.T) {
	if _, err := os.Stat("/usr/bin/pg_dump"); err != nil {
		t.Skip("pg_dump unavailable; skipping runBackup happy path")
	}
	dbcovSetup(t) // migrates + points VORNIK_CONFIG at the test DB
	backupInt_reset()

	outPath := filepath.Join(t.TempDir(), "backup.tgz")
	backupOut = outPath

	out, err := dbcovCapture(t, func() error { return runBackup(backupCmd, nil) })
	if err != nil {
		t.Fatalf("runBackup: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "database dumped") {
		t.Errorf("expected pg_dump success line:\n%s", out)
	}
	if !strings.Contains(out, "backup written") {
		t.Errorf("expected backup-written line:\n%s", out)
	}

	// Archive must exist and contain db.sql + the VORNIK_BACKUP marker.
	if fileSize(outPath) == 0 {
		t.Fatalf("archive not written or empty: %s", outPath)
	}
	names := backupIntListTar(t, outPath)
	for _, want := range []string{"db.sql", "VORNIK_BACKUP"} {
		if !names[want] {
			t.Errorf("archive missing %q; entries: %v", want, keys(names))
		}
	}
}

// backupIntListTar returns the set of entry names in a .tgz archive.
func backupIntListTar(t *testing.T, archive string) map[string]bool {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		names[hdr.Name] = true
	}
	return names
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestIntegration_CheckTargetSchemaAbsent_RefusesMigratedDB(t *testing.T) {
	dbcovSetup(t) // ensures the schema is migrated
	cfg := dbcovDBConfig(t)
	// The migrated test DB has a populated migrations table AND the
	// canonical vornik types → the gate must refuse.
	err := checkTargetSchemaAbsent(&cfg)
	if err == nil {
		t.Fatal("schema gate accepted a fully-migrated DB")
	}
	if !strings.Contains(err.Error(), "schema already loaded") &&
		!strings.Contains(err.Error(), "vornik type") {
		t.Errorf("unexpected gate error: %v", err)
	}
}

func TestIntegration_CheckTargetEmpty_PassesOnFreshTables(t *testing.T) {
	db := dbcovSetup(t)
	cfg := dbcovDBConfig(t)
	// projects + tasks empty right after migration → gate passes.
	// (Guard: if a prior suite left rows, this would flake; assert
	// the precondition so a failure is self-explaining.)
	var pN, tN int
	_ = db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&pN)
	_ = db.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&tN)
	if pN != 0 || tN != 0 {
		t.Skipf("shared DB has projects=%d tasks=%d rows; checkTargetEmpty precondition not met", pN, tN)
	}
	if err := checkTargetEmpty(&cfg); err != nil {
		t.Fatalf("checkTargetEmpty refused an empty target: %v", err)
	}
}

func TestIntegration_CheckTargetEmpty_RefusesPopulatedTasks(t *testing.T) {
	db := dbcovSetup(t)
	cfg := dbcovDBConfig(t)
	// `projects` is config-file-based (no DB table); the gate's other
	// probe table is `tasks`. Seed one task row, assert the gate trips.
	tid := dbcovUniqueProject("bk-task")
	pid := dbcovUniqueProject("bk-proj")
	if _, err := db.Exec(`INSERT INTO tasks (id, project_id) VALUES ($1, $2)`, tid, pid); err != nil {
		t.Fatalf("seed tasks row: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE id=$1`, tid) })

	err := checkTargetEmpty(&cfg)
	if err == nil || !strings.Contains(err.Error(), "tasks row(s)") {
		t.Fatalf("expected non-empty tasks refusal, got %v", err)
	}
}
