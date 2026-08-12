package architecture

import (
	"path/filepath"
	"runtime"
	"testing"
)

// moduleRoot returns the absolute path of the module root (the directory that
// contains go.mod). It works regardless of the working directory from which the
// test is invoked.
//
// IT LIVES IN ITS OWN FILE ON PURPOSE. It used to sit in import_law_test.go,
// which the CE export EXCLUDES and replaces with a template (that file names the
// EE module path, so it cannot ship). Every law added afterwards — the
// provider-spend law, the ledger-wiring law, the Disabled() allowlist law — used
// this helper and shipped to CE without it, which broke the CE tree's
// artifact-purity gate: the laws referenced an undefined symbol in the one tree
// where nobody looks until an export refuses to publish. Caught on 2026-08-12 by
// the export doing exactly that, before the public push.
//
// Anything shared by laws that DO ship belongs here, not in the excluded file.
func moduleRoot(t *testing.T) string {
	t.Helper()
	// __file__ → .../internal/architecture/module_root_test.go
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// Walk up two levels: architecture/ → internal/ → repo root.
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	return abs
}
