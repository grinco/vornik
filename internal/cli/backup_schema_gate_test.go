package cli

import (
	"testing"
)

// TestRestoreCommandHasCleanFlag (B-8) — the schema-gate add-on
// surface lives behind --clean. Smoke-test the cobra wiring so a
// future refactor can't drop the flag without the regression
// catching it. The behavioural tests for dropTargetSchema /
// checkTargetSchemaAbsent live in
// backup_schema_gate_integration_test.go (real-DB).
func TestRestoreCommandHasCleanFlag(t *testing.T) {
	if f := restoreCmd.Flags().Lookup("clean"); f == nil {
		t.Fatal("restoreCmd is missing the --clean flag (B-8)")
	}
}
