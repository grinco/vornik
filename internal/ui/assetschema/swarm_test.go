package assetschema

import (
	"sort"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestSwarmSchema_DriftGuard is the anti-recurrence contract for swarms,
// in two halves:
//   - top-level: every registry.Swarm yaml leaf is either a scalar field
//     in SwarmSchema() or the key of a declared collection (roles).
//   - item-level: every registry.SwarmRole yaml leaf is either covered by
//     the roles collection's ItemSchema or explicitly deferred.
//
// A new struct field on either type fails CI until it gets a form entry
// or a conscious deferral — the structural guarantee that a swarm
// parameter can't silently become YAML-only.
func TestSwarmSchema_DriftGuard(t *testing.T) {
	schema := SwarmSchema()

	covered := map[string]bool{}
	for _, p := range schema.Paths() {
		covered[p] = true
	}
	for _, p := range schema.CollectionPaths() {
		covered[p] = true
	}
	var uncovered []string
	for _, leaf := range LeafPaths(registry.Swarm{}) {
		if !covered[leaf] {
			uncovered = append(uncovered, leaf)
		}
	}
	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		t.Errorf("registry.Swarm yaml field(s) have no schema entry (add a Field to SwarmSchema() or declare a collection): %v", uncovered)
	}

	roles, ok := schema.CollectionByPath("roles")
	if !ok {
		t.Fatal("SwarmSchema() must declare a roles collection")
	}
	itemCovered := map[string]bool{}
	for _, p := range roles.ItemSchema.Paths() {
		itemCovered[p] = true
	}
	for _, p := range roles.ItemDeferredPaths {
		itemCovered[p] = true
	}
	var uncoveredItems []string
	for _, leaf := range LeafPaths(registry.SwarmRole{}) {
		if !itemCovered[leaf] {
			uncoveredItems = append(uncoveredItems, leaf)
		}
	}
	if len(uncoveredItems) > 0 {
		sort.Strings(uncoveredItems)
		t.Errorf("registry.SwarmRole yaml leaf(s) have no item-schema entry and are not deferred "+
			"(give each a Field in the roles ItemSchema or add to its ItemDeferredPaths): %v", uncoveredItems)
	}
}

// TestSwarmSchema_NoBogusPaths is the inverse guard: every schema path,
// collection key, item-schema path, and deferred item path must resolve
// to a real struct leaf (catches typos / renamed fields).
func TestSwarmSchema_NoBogusPaths(t *testing.T) {
	schema := SwarmSchema()

	realSwarm := map[string]bool{}
	for _, leaf := range LeafPaths(registry.Swarm{}) {
		realSwarm[leaf] = true
	}
	for _, p := range schema.Paths() {
		if !realSwarm[p] {
			t.Errorf("swarm schema path %q does not resolve to a registry.Swarm yaml leaf", p)
		}
	}
	for _, p := range schema.CollectionPaths() {
		if !realSwarm[p] {
			t.Errorf("collection key %q does not resolve to a registry.Swarm yaml leaf", p)
		}
	}

	realRole := map[string]bool{}
	for _, leaf := range LeafPaths(registry.SwarmRole{}) {
		realRole[leaf] = true
	}
	roles, _ := schema.CollectionByPath("roles")
	for _, p := range roles.ItemSchema.Paths() {
		if !realRole[p] {
			t.Errorf("roles item-schema path %q does not resolve to a registry.SwarmRole yaml leaf", p)
		}
	}
	for _, p := range roles.ItemDeferredPaths {
		if !realRole[p] {
			t.Errorf("roles deferred path %q does not resolve to a registry.SwarmRole yaml leaf", p)
		}
	}
}

// TestSwarmSchema_NoItemOverlap ensures an item leaf isn't both covered
// and deferred.
func TestSwarmSchema_NoItemOverlap(t *testing.T) {
	roles, ok := SwarmSchema().CollectionByPath("roles")
	if !ok {
		t.Fatal("no roles collection")
	}
	covered := map[string]bool{}
	for _, p := range roles.ItemSchema.Paths() {
		covered[p] = true
	}
	for _, p := range roles.ItemDeferredPaths {
		if covered[p] {
			t.Errorf("item path %q is BOTH in the roles ItemSchema and ItemDeferredPaths", p)
		}
	}
}

// TestSwarmSchema_NoDuplicatePaths guards top-level and item paths
// against accidental duplication.
func TestSwarmSchema_NoDuplicatePaths(t *testing.T) {
	schema := SwarmSchema()
	seen := map[string]bool{}
	for _, p := range schema.Paths() {
		if seen[p] {
			t.Errorf("duplicate swarm schema path %q", p)
		}
		seen[p] = true
	}
	roles, _ := schema.CollectionByPath("roles")
	seenItem := map[string]bool{}
	for _, p := range roles.ItemSchema.Paths() {
		if seenItem[p] {
			t.Errorf("duplicate roles item-schema path %q", p)
		}
		seenItem[p] = true
	}
}
