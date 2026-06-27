//go:build integration

package postgres

import (
	"strings"
	"testing"
)

// TestPurgeRepotestLeftovers_PredicateCoversKnownLiterals pins the
// integration-test cleanup predicate so future contributors don't
// silently drop coverage for the literal project IDs that repotest.go
// hardcodes at ~30 call sites.
//
// History (2026-05-28): the operator noticed ~250 rows leaking into
// the shared vornik_test DB under project_ids "p" and "p1" — created
// by repotest call sites that pass the literal string straight to
// Create() instead of going through uniqueID(). The original
// purgeRepotestLeftovers() only matched LIKE 'proj-%' / 'other-%',
// so the literals accumulated across every CI run. The predicate
// now also IN-matches those literals; this test ensures it stays
// that way.
//
// If you intentionally drop a literal from the IN list (e.g. because
// repotest.go stopped using it), update BOTH the predicate and this
// test in the same commit — silent drift here costs $1100 of fake
// task_llm_usage rows, per the comment on purgeRepotestLeftovers.
func TestPurgeRepotestLeftovers_PredicateCoversKnownLiterals(t *testing.T) {
	mustContain := []string{
		// uniqueID-prefixed namespaces.
		"proj-%",
		"other-%",
		// Short literals hardcoded by repotest.go.
		"'p'",
		"'p1'",
		"'p2'",
		"'proj-1'",
		"'proj-2'",
	}
	for _, want := range mustContain {
		if !strings.Contains(repotestLeftoverPredicate, want) {
			t.Errorf("repotestLeftoverPredicate is missing %q — adding a hardcoded test fixture without updating the predicate leaks rows into the shared DB across every CI run; predicate is:\n  %s",
				want, repotestLeftoverPredicate)
		}
	}

	// Defence-in-depth: _external is the real attribution-fallback
	// bucket. Matching it would wipe genuine audit history.
	if strings.Contains(repotestLeftoverPredicate, "_external") {
		t.Errorf("repotestLeftoverPredicate must NOT mention `_external` — that's the real attribution-fallback bucket; predicate is:\n  %s",
			repotestLeftoverPredicate)
	}
}
