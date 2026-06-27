package schemaregistry

import (
	"sort"
	"testing"
)

// TestIDs_ReturnsSortedOrder pins the IDs() doc contract ("sorted list").
// Regression guard for the drift where IDs() returned map-iteration order.
func TestIDs_ReturnsSortedOrder(t *testing.T) {
	dir := t.TempDir()
	// Registered deliberately out of alphabetical order.
	mustWrite(t, dir, "zeta.v1.json", "{}")
	mustWrite(t, dir, "alpha.v1.json", "{}")
	mustWrite(t, dir, "mu.v1.json", "{}")

	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ids := reg.IDs()
	if len(ids) != 3 {
		t.Fatalf("want 3 ids, got %v", ids)
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("IDs() must return a sorted slice (doc contract), got %v", ids)
	}
}
