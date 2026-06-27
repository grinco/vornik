package schema

import (
	"os"
	"strings"
	"testing"
)

func readSchema(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("001_initial.sql")
	if err != nil {
		t.Fatalf("failed to read schema file: %v", err)
	}
	return string(data)
}

func TestInitialSchemaCreatesMigrationTable(t *testing.T) {
	sql := readSchema(t)
	if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS migrations") {
		t.Fatal("expected initial schema to create migrations")
	}
	if !strings.Contains(sql, "COMMENT ON TABLE migrations") {
		t.Fatal("expected initial schema to document migrations")
	}
}

func TestSchemaExecReconcileColumns(t *testing.T) {
	sql := readSchema(t) // existing helper that slurps 001_initial.sql
	for _, want := range []string{
		"ADD COLUMN filled_qty",
		"trading_fills",
		"ADD COLUMN exec_id",
		"ADD COLUMN account_id",
		"ADD COLUMN source_detail",
		"idx_trading_fills_exec",
		"idx_trading_fills_filled_at",
		"CREATE TABLE IF NOT EXISTS trading_fills_shadow",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("schema missing %q", want)
		}
	}
}
