package memory

// Live-daemon DB shield for the memory integration suite. Lives in an
// untagged _test.go (no `//go:build integration`) so the predicate compiles
// and is unit-tested in the normal lane, while the integration suite
// (ingest_recall_isolation_integration_test.go) consumes it under the tag.
//
// Regression (2026-06-28): the original guard only refused "vornik_test", but
// product default is "vornik" (postgres.DefaultConfig). A run pointed at
// either would have sailed past the guard and run destructive per-test
// cleanup against live data.

import (
	"strings"
	"testing"
)

// protectedDaemonDBs are database names that back a LIVE Vornik daemon and must
// never receive the integration suite's destructive per-test cleanup:
//   - "vornik"      — product default (internal/persistence/postgres.DefaultConfig)
//   - "vornik_test" — post-rebrand daemon DB name
//
// A test pointed at any of these is refused unless VORNIK_TEST_ALLOW_DAEMON=1.
var protectedDaemonDBs = map[string]struct{}{
	"vornik":      {},
	"vornik_test": {},
}

// isProtectedDaemonDB reports whether dbName backs a live daemon and must be
// shielded from destructive integration cleanup. The match is case-insensitive
// and tolerates a leading "/" so a raw URL path ("/vornik_test") is handled.
func isProtectedDaemonDB(dbName string) bool {
	name := strings.ToLower(strings.TrimPrefix(dbName, "/"))
	_, ok := protectedDaemonDBs[name]
	return ok
}

func TestIsProtectedDaemonDB(t *testing.T) {
	// Every live-daemon DB must be shielded — incl. the historically-missed
	// "vornik" (default) and "vornik_test", case-insensitively
	// and with a leading-slash URL path.
	protected := []string{
		"vornik", "vornik_test",
		"Vornik", "VORNIK_TEST", "/vornik", "/vornik_test",
	}
	for _, name := range protected {
		if !isProtectedDaemonDB(name) {
			t.Errorf("isProtectedDaemonDB(%q) = false, want true (live daemon DB must be shielded)", name)
		}
	}

	// Throwaway/integration DBs must pass through so the suite can run.
	safe := []string{
		"vornik_ragtest", "vornik_integration_test", "swarmd_integration_test",
		"testdb", "postgres", "",
	}
	for _, name := range safe {
		if isProtectedDaemonDB(name) {
			t.Errorf("isProtectedDaemonDB(%q) = true, want false (throwaway DB must be allowed)", name)
		}
	}
}
