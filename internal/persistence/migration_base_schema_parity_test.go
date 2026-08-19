package persistence

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Measured 2026-08-18 on the bench daemon: the execution-quality reconciler
// logged `pq: column e.workflow_snapshot does not exist` every 30 seconds,
// forever, against a database sitting at migration 164 — the same version as
// production, which has the column.
//
// The column exists ONLY in deployments/postgres/schema/001_initial.sql, added
// there by a retrofitted DO block. No migration ever adds it. A database
// bootstrapped by the migration runner alone therefore lacks a column the Go
// code reads (execution_repository.go's workflow-snapshot pin, and migration
// 163's quality reconciler), and nothing detects it: the migrations table says
// fully migrated.
//
// The base SQL file and the migration list are two schema sources that must
// agree. This asserts every column the file retrofits onto a table is also
// reachable from migrations, so the next retrofit cannot land in one source
// only. It is a static check — no database required — because the failure it
// guards is precisely a deployment where nobody looked.
func TestBaseSchemaRetrofitsAreAlsoMigrations(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deployments", "postgres", "schema", "001_initial.sql"))
	if err != nil {
		t.Fatalf("read base schema: %v", err)
	}
	base := string(raw)

	// Columns the base file adds by ALTER rather than in its CREATE TABLE:
	// exactly the retrofits that can drift from the migration list.
	retrofit := regexp.MustCompile(`(?i)ALTER TABLE\s+(\w+)[\s\S]{0,400}?ADD COLUMN\s+(\w+)`)
	addColumn := regexp.MustCompile(`(?i)ADD COLUMN\s+(\w+)`)

	migrationSQL := &strings.Builder{}
	for _, m := range DefaultMigrations {
		migrationSQL.WriteString(m.Up)
		migrationSQL.WriteString("\n")
	}
	migrations := strings.ToLower(migrationSQL.String())

	checked := 0
	for _, stmt := range strings.Split(base, ";") {
		loc := retrofit.FindStringSubmatch(stmt)
		if loc == nil {
			continue
		}
		table := strings.ToLower(loc[1])
		// One ALTER may add several columns; check each.
		for _, col := range addColumn.FindAllStringSubmatch(stmt, -1) {
			column := strings.ToLower(col[1])
			checked++
			if !strings.Contains(migrations, column) {
				t.Errorf("deployments/postgres/schema/001_initial.sql retrofits %s.%s but no "+
					"migration adds it — a database bootstrapped from the migration runner "+
					"alone will be missing a column the daemon reads, while its migrations "+
					"table reports it fully migrated",
					table, column)
			}
		}
	}
	if checked == 0 {
		t.Error("no retrofitted columns found in the base schema — if the DO-block " +
			"pattern changed, move this guard with it rather than leaving it watching nothing")
	}
}
