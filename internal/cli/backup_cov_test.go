package cli

// Coverage sweep for the backup/restore command wiring that does NOT
// require Postgres or pg_dump/psql. The full runBackup / runRestore
// happy paths shell out to pg_dump/psql against a live DB and are out
// of scope for unit tests; here we pin the early guard branches.

import (
	"strings"
	"testing"
)

func backupCov_reset() {
	backupOut, restoreIn = "", ""
	restoreForce, restoreAllowNonEmpty, restoreClean = false, false, false
}

func TestRunRestore_RefusesWithoutForce(t *testing.T) {
	backupCov_reset()
	// --force not set → the very first guard returns before any config
	// load or DB connection, so this is fully hermetic.
	err := runRestore(restoreCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--force is required") {
		t.Fatalf("expected --force guard, got %v", err)
	}
}
