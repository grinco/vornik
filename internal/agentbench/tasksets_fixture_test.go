package agentbench

import (
	"os"
	"path/filepath"
	"testing"
)

// requireTaskSets returns the task-set paths matching pattern, or SKIPS the
// test when the tasksets directory is absent entirely.
//
// `internal/agentbench/tasksets/` is EE-only by design:
// scripts/export-public-ce.sh strips it because a benchmark whose tasks are
// public can be trained against and tuned toward, so its value as a release
// gate depends on the set not being visible to anything under test
// (2026-08-13-agent-quality-benchmark-design.md §7). The harness ships to CE;
// what it measures against does not.
//
// The guards that read it therefore have nothing to assert in a CE checkout.
// Three of them failed the PUBLIC CI on 2026-08-19 and 2026-08-20 for exactly
// that reason — "no tasksets found" — and the reds were inherited by the next
// export rather than noticed as a CE-only condition.
//
// ABSENT means CE: skip, with a reason. PRESENT BUT EMPTY means a regression in
// the EE tree: fail. Collapsing those two states is what would let a genuinely
// emptied task-set directory pass unnoticed, so they are kept distinct.
//
// Centralised deliberately. Three separate tests had grown the same
// `filepath.Glob("tasksets/...")` + `t.Fatalf` pattern; per the repo's
// centralise-on-recurrence rule, the third occurrence is where the primitive
// gets extracted instead of the call site being patched again.
func requireTaskSets(t *testing.T, pattern string) []string {
	t.Helper()
	if _, err := os.Stat("tasksets"); os.IsNotExist(err) {
		t.Skip("no tasksets/ directory: EE-only benchmark ground truth, stripped by " +
			"the CE export. This guard has nothing to assert in a CE checkout.")
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	if len(matches) == 0 {
		t.Fatalf("tasksets/ exists but %s matched nothing — an emptied task-set "+
			"directory is a regression, not a CE checkout", pattern)
	}
	return matches
}
