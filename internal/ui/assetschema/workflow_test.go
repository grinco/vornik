package assetschema

import (
	"sort"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestWorkflowSchema_DriftGuard — every registry.Workflow yaml leaf is a
// scalar field in WorkflowSchema(), a declared collection key (steps), or
// an explicit WorkflowDeferredPaths entry; every registry.WorkflowStep
// leaf is an item-schema field or a deferred item path. A new struct
// field on either type fails CI until consciously covered or deferred.
func TestWorkflowSchema_DriftGuard(t *testing.T) {
	schema := WorkflowSchema()

	covered := map[string]bool{}
	for _, p := range schema.Paths() {
		covered[p] = true
	}
	for _, p := range schema.CollectionPaths() {
		covered[p] = true
	}
	for _, p := range WorkflowDeferredPaths {
		covered[p] = true
	}
	var uncovered []string
	for _, leaf := range LeafPaths(registry.Workflow{}) {
		if !covered[leaf] {
			uncovered = append(uncovered, leaf)
		}
	}
	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		t.Errorf("registry.Workflow yaml field(s) have no schema entry (field, collection, or WorkflowDeferredPaths): %v", uncovered)
	}

	steps, ok := schema.CollectionByPath("steps")
	if !ok {
		t.Fatal("WorkflowSchema() must declare a steps collection")
	}
	if !steps.KeyIsMapKey {
		t.Error("steps collection must be KeyIsMapKey (steps{} is a map)")
	}
	itemCovered := map[string]bool{}
	for _, p := range steps.ItemSchema.Paths() {
		itemCovered[p] = true
	}
	for _, p := range steps.ItemDeferredPaths {
		itemCovered[p] = true
	}
	var uncoveredItems []string
	for _, leaf := range LeafPaths(registry.WorkflowStep{}) {
		if !itemCovered[leaf] {
			uncoveredItems = append(uncoveredItems, leaf)
		}
	}
	if len(uncoveredItems) > 0 {
		sort.Strings(uncoveredItems)
		t.Errorf("registry.WorkflowStep yaml leaf(s) have no item-schema entry and are not deferred: %v", uncoveredItems)
	}
}

// TestWorkflowSchema_NoBogusPaths — every schema/collection/item/deferred
// path resolves to a real struct leaf.
func TestWorkflowSchema_NoBogusPaths(t *testing.T) {
	schema := WorkflowSchema()

	realWF := map[string]bool{}
	for _, leaf := range LeafPaths(registry.Workflow{}) {
		realWF[leaf] = true
	}
	for _, p := range schema.Paths() {
		if !realWF[p] {
			t.Errorf("workflow schema path %q does not resolve to a registry.Workflow yaml leaf", p)
		}
	}
	for _, p := range schema.CollectionPaths() {
		if !realWF[p] {
			t.Errorf("collection key %q does not resolve to a registry.Workflow yaml leaf", p)
		}
	}
	for _, p := range WorkflowDeferredPaths {
		if !realWF[p] {
			t.Errorf("WorkflowDeferredPaths %q does not resolve to a registry.Workflow yaml leaf", p)
		}
	}

	realStep := map[string]bool{}
	for _, leaf := range LeafPaths(registry.WorkflowStep{}) {
		realStep[leaf] = true
	}
	steps, _ := schema.CollectionByPath("steps")
	for _, p := range steps.ItemSchema.Paths() {
		if !realStep[p] {
			t.Errorf("steps item-schema path %q does not resolve to a registry.WorkflowStep yaml leaf", p)
		}
	}
	for _, p := range steps.ItemDeferredPaths {
		if !realStep[p] {
			t.Errorf("steps deferred path %q does not resolve to a registry.WorkflowStep yaml leaf", p)
		}
	}
}

// TestWorkflowSchema_NoOverlap — a path isn't both covered and deferred.
func TestWorkflowSchema_NoOverlap(t *testing.T) {
	schema := WorkflowSchema()
	covered := map[string]bool{}
	for _, p := range schema.Paths() {
		covered[p] = true
	}
	for _, p := range WorkflowDeferredPaths {
		if covered[p] {
			t.Errorf("workflow path %q is BOTH in WorkflowSchema() and WorkflowDeferredPaths", p)
		}
	}
	steps, _ := schema.CollectionByPath("steps")
	itemCovered := map[string]bool{}
	for _, p := range steps.ItemSchema.Paths() {
		itemCovered[p] = true
	}
	for _, p := range steps.ItemDeferredPaths {
		if itemCovered[p] {
			t.Errorf("step item path %q is BOTH in the steps ItemSchema and ItemDeferredPaths", p)
		}
	}
}
