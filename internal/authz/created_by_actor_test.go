package authz

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// §5.7: "`created_by_actor` is unchanged by any claim — asserted directly,
// because the whole read-time-resolution argument rests on it."
//
// The argument it rests on is §5.4's: the observed actor is STORED
// (`tasks.created_by_actor` = `api_key:<id>`, a fact about the request that
// never changes) and the person is RESOLVED AT READ TIME. That is what makes
// a claim roll up a key's entire history with no backfill, and what makes a
// wrong mapping correctable — rewriting the stored actor would destroy the
// record of what was actually observed, and could not be undone.
//
// Nothing asserted it. It was true by construction, which is exactly what the
// §5.0 API-key door was before it turned out not to be hung.

// TestClaimService_CannotReachTheTaskRecord — the structural assertion, and
// the one that would actually catch the regression. A behavioural test can
// only show that today's code does not rewrite the actor; this shows it
// CANNOT, because the service holds nothing that could.
//
// The regression this guards is concrete and tempting: someone adds
// "backfill attribution on claim" and reaches for a task repository. That
// change fails here, in the package whose design says the opposite.
func TestClaimService_CannotReachTheTaskRecord(t *testing.T) {
	typ := reflect.TypeOf(Accounts{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := f.Type.String()
		if strings.Contains(strings.ToLower(name), "task") {
			t.Errorf("Accounts holds %s %s — the claim path must not be able to reach the task "+
				"record at all; §5.4's read-time resolution exists so history rolls up WITHOUT "+
				"rewriting what was observed", f.Name, name)
		}
	}
}

// TestClaimPackage_NeverWritesTheObservedActor is the source half: no
// non-test file in this package may name the column, in any statement.
//
// Both halves are needed. The reflection test catches a dependency added to
// the struct; this catches a raw SQL string or a repository reached some other
// way. Neither alone is the claim §5.7 makes.
func TestClaimPackage_NeverWritesTheObservedActor(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			// Comments may name the column — this file's own doc comments do.
			// The contract is about STATEMENTS that could write it.
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, "created_by_actor") {
				t.Errorf("%s:%d names created_by_actor; the observed actor is stored once and "+
					"resolved at read time, and rewriting it destroys the record of what was "+
					"actually observed:\n\t%s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}
