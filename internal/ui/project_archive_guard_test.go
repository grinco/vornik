package ui

import "testing"

// TestLifecyclePatchGuard_OnlyLifecycleKey pins the invariant the
// archive/unarchive/delete-now paths rely on: lifecycle writes may
// touch only the top-level `lifecycle:` key. A patch targeting project
// config or identity (projectId, swarmId, autonomy) must be refused, so
// a typo or an accidentally-added patch can't ride the archive path
// into a protected field.
func TestLifecyclePatchGuard_OnlyLifecycleKey(t *testing.T) {
	if !lifecyclePatchGuard.Allows("lifecycle") {
		t.Fatal("lifecycle guard must allow the lifecycle key")
	}
	for _, protected := range []string{"projectId", "swarmId", "autonomy", "tenant_id"} {
		if lifecyclePatchGuard.Allows(protected) {
			t.Errorf("lifecycle path must NOT be able to write %q", protected)
		}
	}

	// A real archive patch set (only lifecycle.* leaves) passes.
	archivePatches := []yamlPatch{
		{Path: []string{"lifecycle", "status"}, Value: "archived"},
		{Path: []string{"lifecycle", "archivedAt"}, Value: "2026-05-30T00:00:00Z"},
		{Path: []string{"lifecycle", "reason"}, Value: "x", RemoveIfEmpty: true},
	}
	if err := lifecyclePatchGuard.Check(topLevelPatchKeys(archivePatches)); err != nil {
		t.Errorf("a lifecycle-only patch set must pass the guard: %v", err)
	}

	// A patch set that strays into project config is refused.
	strayPatches := []yamlPatch{
		{Path: []string{"lifecycle", "status"}, Value: "archived"},
		{Path: []string{"projectId"}, Value: "renamed"},
	}
	if err := lifecyclePatchGuard.Check(topLevelPatchKeys(strayPatches)); err == nil {
		t.Error("a lifecycle patch set touching projectId must be refused")
	}
}

// TestTopLevelPatchKeys covers the shared extraction helper.
func TestTopLevelPatchKeys(t *testing.T) {
	got := topLevelPatchKeys([]yamlPatch{
		{Path: []string{"lifecycle", "status"}},
		{Path: []string{"autonomy", "enabled"}},
		{Path: nil}, // skipped
		{Path: []string{"displayName"}},
	})
	want := []string{"lifecycle", "autonomy", "displayName"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %q, want %q", i, got[i], want[i])
		}
	}
}
