package ui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconcileYAMLSequence covers the collection save contract in one
// shot: reorder + per-item patch + add + remove, with comments on a
// surviving element and unrelated top-level keys preserved.
func TestReconcileYAMLSequence(t *testing.T) {
	content := []byte(`displayName: Demo
roles:
  - name: coder
    model: old-model
  # reviewer does QA — comment must survive reorder
  - name: reviewer
    model: rev-model
leadRole: reviewer
`)

	items := []collectionItem{
		// reviewer first (reorder), model changed.
		{ID: "reviewer", Patches: []yamlPatch{
			{Path: []string{"name"}, Value: "reviewer"},
			{Path: []string{"model"}, Value: "new-rev"},
		}},
		// newbie added at the end.
		{ID: "newbie", Patches: []yamlPatch{
			{Path: []string{"name"}, Value: "newbie"},
			{Path: []string{"model"}, Value: "n-model"},
		}},
		// coder omitted → removed.
	}

	out, err := reconcileYAMLSequence(content, "roles", "name", items)
	require.NoError(t, err)
	got := string(out)

	// Order: reviewer before newbie; coder gone. (The patcher quotes
	// string scalars, so match the bare identifiers, not `name: x`.)
	ri := strings.Index(got, "reviewer")
	ni := strings.Index(got, "newbie")
	require.Greater(t, ri, 0)
	require.Greater(t, ni, 0)
	assert.Less(t, ri, ni, "reviewer should come before newbie")
	assert.NotContains(t, got, "coder", "coder removed")
	assert.NotContains(t, got, "old-model")

	// Surviving element patched + comment preserved.
	assert.Contains(t, got, "new-rev")
	assert.Contains(t, got, "reviewer does QA")

	// New element present.
	assert.Contains(t, got, "newbie")
	assert.Contains(t, got, "n-model")

	// Unrelated top-level keys survive.
	assert.Contains(t, got, "displayName: Demo")
	assert.Contains(t, got, "leadRole: reviewer")
}

// TestReconcileYAMLSequence_EmptyClearsList reconciling to no items
// leaves an empty sequence (every element removed).
func TestReconcileYAMLSequence_EmptyClearsList(t *testing.T) {
	content := []byte("roles:\n  - name: a\n  - name: b\n")
	out, err := reconcileYAMLSequence(content, "roles", "name", nil)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "name: a")
	assert.NotContains(t, string(out), "name: b")
}

// TestReconcileYAMLMapping covers the map collection save contract:
// rename (delete+add by key), per-item patch, add, remove, with comments
// on a surviving key and sibling top-level keys preserved.
func TestReconcileYAMLMapping(t *testing.T) {
	content := []byte(`displayName: WF
steps:
  build:
    type: agent
    role: coder
  # review gate — comment must survive
  review:
    type: gate
entrypoint: build
`)

	items := []mappingItem{
		// review kept (reordered first), type changed.
		{Key: "review", Patches: []yamlPatch{{Path: []string{"type"}, Value: "approval"}}},
		// compile added.
		{Key: "compile", Patches: []yamlPatch{
			{Path: []string{"type"}, Value: "agent"},
			{Path: []string{"role"}, Value: "builder"},
		}},
		// build omitted → removed.
	}

	out, err := reconcileYAMLMapping(content, "steps", items)
	require.NoError(t, err)
	got := string(out)

	assert.NotContains(t, got, "build:", "build removed")
	assert.NotContains(t, got, "coder")
	assert.Contains(t, got, "review:")
	assert.Contains(t, got, "review gate — comment must survive", "surviving key's comment preserved")
	assert.Contains(t, got, "approval")
	assert.Contains(t, got, "compile:")
	assert.Contains(t, got, "builder")
	// Order: review before compile.
	assert.Less(t, strings.Index(got, "review:"), strings.Index(got, "compile:"))
	// Siblings survive.
	assert.Contains(t, got, "displayName: WF")
	assert.Contains(t, got, "entrypoint: build")
}

// TestReconcileYAMLMapping_NotAMapping — a scalar where a mapping is
// expected is a structural error.
func TestReconcileYAMLMapping_NotAMapping(t *testing.T) {
	content := []byte("steps: nope\n")
	_, err := reconcileYAMLMapping(content, "steps", []mappingItem{{Key: "x"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a mapping")
}

// TestReconcileYAMLMapping_CreatesAbsentKey — a missing `steps:` mapping
// is synthesised.
func TestReconcileYAMLMapping_CreatesAbsentKey(t *testing.T) {
	content := []byte("displayName: WF\n")
	items := []mappingItem{{Key: "only", Patches: []yamlPatch{{Path: []string{"type"}, Value: "agent"}}}}
	out, err := reconcileYAMLMapping(content, "steps", items)
	require.NoError(t, err)
	assert.Contains(t, string(out), "only:")
	assert.Contains(t, string(out), "displayName: WF")
}

// TestReconcileYAMLSequence_NullSequenceCoerced — a bare `roles:` (null
// value) is coerced to a sequence so items can be added.
func TestReconcileYAMLSequence_NullSequenceCoerced(t *testing.T) {
	content := []byte("roles:\n")
	items := []collectionItem{{ID: "x", Patches: []yamlPatch{{Path: []string{"name"}, Value: "x"}}}}
	out, err := reconcileYAMLSequence(content, "roles", "name", items)
	require.NoError(t, err)
	assert.Contains(t, string(out), `name: "x"`)
}

// TestReconcileYAMLSequence_NotASequence — a scalar where a sequence is
// expected is a structural error, not silently overwritten.
func TestReconcileYAMLSequence_NotASequence(t *testing.T) {
	content := []byte("roles: just-a-string\n")
	_, err := reconcileYAMLSequence(content, "roles", "name", []collectionItem{{ID: "x"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a sequence")
}

// TestReconcileYAMLSequence_CreatesAbsentKey reconciles into a document
// that has no sequence key yet — the key is synthesised.
func TestReconcileYAMLSequence_CreatesAbsentKey(t *testing.T) {
	content := []byte("displayName: Demo\n")
	items := []collectionItem{{ID: "x", Patches: []yamlPatch{{Path: []string{"name"}, Value: "x"}}}}
	out, err := reconcileYAMLSequence(content, "roles", "name", items)
	require.NoError(t, err)
	assert.Contains(t, string(out), `name: "x"`)
	assert.Contains(t, string(out), "displayName: Demo")
}
