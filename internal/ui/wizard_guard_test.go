package ui

import "testing"

// TestWizardRepointGuard pins the wizard repoint invariant: it may
// rewrite only the swarmId + defaultWorkflowId pointers, never any
// other project-YAML key. A stray patch (e.g. projectId, autonomy) on
// the repoint path must be refused.
func TestWizardRepointGuard(t *testing.T) {
	for _, ok := range []string{"swarmId", "defaultWorkflowId"} {
		if !wizardRepointGuard.Allows(ok) {
			t.Errorf("wizard repoint must allow %q", ok)
		}
	}
	for _, protected := range []string{"projectId", "autonomy", "budget", "permissions"} {
		if wizardRepointGuard.Allows(protected) {
			t.Errorf("wizard repoint must NOT be able to write %q", protected)
		}
	}

	// The real repoint patch set passes.
	repoint := []yamlPatch{
		{Path: []string{"swarmId"}, Value: "p-swarm"},
		{Path: []string{"defaultWorkflowId"}, Value: "p-wf"},
	}
	if err := wizardRepointGuard.Check(topLevelPatchKeys(repoint)); err != nil {
		t.Errorf("legitimate repoint patch set must pass: %v", err)
	}

	// A repoint that strays into identity is refused.
	stray := append(repoint, yamlPatch{Path: []string{"projectId"}, Value: "renamed"})
	if err := wizardRepointGuard.Check(topLevelPatchKeys(stray)); err == nil {
		t.Error("a repoint touching projectId must be refused")
	}
}
