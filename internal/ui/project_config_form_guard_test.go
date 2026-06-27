package ui

import "testing"

// TestProjectConfigFormGuard_AllowsEveryEmittedKey pins the invariant
// the save handler relies on: every top-level key the form actually
// emits (buildFormPatches + buildMCPPatches) is in the allowlist. If a
// future edit adds a patch under a new top-level key without updating
// projectConfigFormGuard, this fails — instead of every real save
// breaking with a "Refused" error in production.
func TestProjectConfigFormGuard_AllowsEveryEmittedKey(t *testing.T) {
	data := &ProjectConfigFormData{}
	patches := buildFormPatches(data)
	patches = append(patches, buildMCPPatches(data)...)

	touched := make([]string, 0, len(patches))
	for _, p := range patches {
		if len(p.Path) > 0 {
			touched = append(touched, p.Path[0])
		}
	}
	if err := projectConfigFormGuard.Check(touched); err != nil {
		t.Fatalf("guard rejected a key the form legitimately emits — allowlist drifted from buildFormPatches: %v", err)
	}
}

// TestProjectConfigFormGuard_ProtectsIdentity is the security half:
// the form must never be able to write the project's identity or other
// protected keys, even if a patch for one is somehow constructed.
func TestProjectConfigFormGuard_ProtectsIdentity(t *testing.T) {
	for _, protected := range []string{"projectId", "tenant_id", "lifecycle", "created_at"} {
		if projectConfigFormGuard.Allows(protected) {
			t.Errorf("protected key %q must NOT be writable via the config form", protected)
		}
	}
	if err := projectConfigFormGuard.Check([]string{"displayName", "projectId"}); err == nil {
		t.Error("a patch set touching projectId must be refused")
	}
}
