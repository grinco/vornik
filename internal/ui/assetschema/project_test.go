package assetschema

import (
	"sort"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestProjectSchema_DriftGuard is the anti-recurrence contract: every
// yaml-tagged leaf of registry.Project must be EITHER covered by
// ProjectSchema() OR explicitly listed in ProjectDeferredPaths. A new
// struct field therefore fails CI until someone consciously gives it a
// form entry or defers it — which is what structurally prevents a new
// parameter from silently becoming YAML-only (the gap this feature fixes).
func TestProjectSchema_DriftGuard(t *testing.T) {
	covered := map[string]bool{}
	for _, p := range ProjectSchema().Paths() {
		covered[p] = true
	}
	deferred := map[string]bool{}
	for _, p := range ProjectDeferredPaths {
		deferred[p] = true
	}

	var uncovered []string
	for _, leaf := range LeafPaths(registry.Project{}) {
		if !covered[leaf] && !deferred[leaf] {
			uncovered = append(uncovered, leaf)
		}
	}
	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		t.Errorf("registry.Project yaml field(s) have no form schema entry and are not in ProjectDeferredPaths "+
			"(give each a Field in ProjectSchema() or add to ProjectDeferredPaths so it is not silently YAML-only): %v", uncovered)
	}
}

// TestProjectSchema_NoBogusPaths is the inverse guard: every schema path
// and every deferred path must resolve to a REAL registry.Project leaf
// (catches typos and fields renamed/removed out from under the schema).
func TestProjectSchema_NoBogusPaths(t *testing.T) {
	real := map[string]bool{}
	for _, leaf := range LeafPaths(registry.Project{}) {
		real[leaf] = true
	}
	check := func(label string, paths []string) {
		for _, p := range paths {
			if !real[p] {
				t.Errorf("%s path %q does not resolve to a registry.Project yaml leaf (typo or stale entry?)", label, p)
			}
		}
	}
	check("schema", ProjectSchema().Paths())
	check("deferred", ProjectDeferredPaths)
}

// TestProjectSchema_NoOverlap ensures a path is not both covered and
// deferred (that would be a contradictory authoring mistake).
func TestProjectSchema_NoOverlap(t *testing.T) {
	covered := map[string]bool{}
	for _, p := range ProjectSchema().Paths() {
		covered[p] = true
	}
	for _, p := range ProjectDeferredPaths {
		if covered[p] {
			t.Errorf("path %q is BOTH in ProjectSchema() and ProjectDeferredPaths — pick one", p)
		}
	}
}

// TestProjectSchema_NoDuplicatePaths guards against the same path being
// declared twice in the schema (a copy-paste slip).
func TestProjectSchema_NoDuplicatePaths(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range ProjectSchema().Paths() {
		if seen[p] {
			t.Errorf("duplicate schema path %q", p)
		}
		seen[p] = true
	}
}
