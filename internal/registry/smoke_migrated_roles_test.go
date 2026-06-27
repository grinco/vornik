package registry

import (
	"strings"
	"testing"
)

// findRole is the test-helper we use across the migrated-role smoke
// tests below — by-name lookup with a clear failure message rather
// than a nil panic when the role moved.
func findRole(t *testing.T, swarm *Swarm, name string) *SwarmRole {
	t.Helper()
	for i := range swarm.Roles {
		if swarm.Roles[i].Name == name {
			return &swarm.Roles[i]
		}
	}
	t.Fatalf("role %q missing from swarm %s", name, swarm.ID)
	return nil
}

// assertMigrated holds the migration invariant for every role that's
// moved to outputSchema: the schema is set, the inject flag is on,
// the legacy fields are derived (so consumers see them populated),
// and the rendered prompt contains the required-keys header. Each
// per-role smoke test below adds role-specific assertions on top.
func assertMigrated(t *testing.T, role *SwarmRole, wantRequiredKeys []string) {
	t.Helper()
	if role.OutputSchema == nil {
		t.Fatalf("role %q OutputSchema unset", role.Name)
	}
	if !role.InjectSchemaIntoPrompt {
		t.Fatalf("role %q InjectSchemaIntoPrompt should be true", role.Name)
	}
	for _, want := range wantRequiredKeys {
		found := false
		for _, k := range role.RequiredOutputKeys {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("role %q derived RequiredOutputKeys missing %q; got %v",
				role.Name, want, role.RequiredOutputKeys)
		}
	}
	rendered := role.OutputSchema.RenderForPrompt()
	// New render format: a JSON shape skeleton, not a bullet list.
	// Pre-fix the bullet list used dot-paths ("research.written")
	// which trained models to emit flat keys instead of nested
	// objects.
	if !strings.Contains(rendered, "Your response must match this JSON shape:") {
		t.Errorf("role %q render missing JSON shape header; got:\n%s", role.Name, rendered)
	}
}

// TestSmokeLoad_AssistantSwarmMigratedRoles covers the assistant-
// swarm migration in one pass: writer (the original phase-2 role)
// plus researcher (added later). Both share the produced-files
// pattern; the writer adds the message:string non-empty guarantee.
func TestSmokeLoad_AssistantSwarmMigratedRoles(t *testing.T) {
	swarms, err := LoadSwarms("../../configs")
	if err != nil {
		t.Fatalf("LoadSwarms: %v", err)
	}
	a := swarms["assistant-swarm"]
	if a == nil {
		t.Fatal("assistant-swarm not found")
	}

	t.Run("writer", func(t *testing.T) {
		w := findRole(t, a, "writer")
		assertMigrated(t, w, []string{
			"writing:object", "writing.written:bool",
			"produced_files:array", "message:string",
		})
		// The rendered prompt must surface the message:string non-
		// empty guarantee — that's the contract autonomy/UI rely on.
		if !strings.Contains(w.OutputSchema.RenderForPrompt(), "non-empty") {
			t.Error("writer render missing non-empty marker")
		}
	})
	t.Run("researcher", func(t *testing.T) {
		r := findRole(t, a, "researcher")
		assertMigrated(t, r, []string{
			"research:object", "research.written:bool",
			"produced_files:array",
		})
	})
}

// TestSmokeLoad_DevSwarmMigratedRoles covers the dev-swarm migration.
// Touches a representative member of each conflict-class the migration
// addressed:
//   - workflow-step-pinned: analyst, coder use loose object schemas;
//     workflow step prompts pin the sub-fields per call.
//   - gate-driven: tester pins testing.passed; reviewer pins
//     review.approved.
//   - lead-plan-only: feasibility, scout, architect declare full
//     sub-field schemas because no per-step prompt competes.
func TestSmokeLoad_DevSwarmMigratedRoles(t *testing.T) {
	swarms, err := LoadSwarms("../../configs")
	if err != nil {
		t.Fatalf("LoadSwarms: %v", err)
	}
	d := swarms["dev-swarm"]
	if d == nil {
		t.Fatal("dev-swarm not found")
	}

	t.Run("analyst (loose object)", func(t *testing.T) {
		a := findRole(t, d, "analyst")
		assertMigrated(t, a, []string{"analysis:object"})
		// Loose schema by design — no nested required so workflow
		// step prompt can pin sub-fields per call without conflict.
		if len(a.OutputSchema.Properties["analysis"].Required) != 0 {
			t.Errorf("analyst.analysis should not pin sub-fields; got required %v",
				a.OutputSchema.Properties["analysis"].Required)
		}
	})
	t.Run("tester (gate-driven)", func(t *testing.T) {
		te := findRole(t, d, "tester")
		assertMigrated(t, te, []string{"testing:object", "testing.passed:bool"})
		// Plausibility derives a failure_explained rule via the
		// explicit when=passed:false clause in the schema YAML.
		foundFailure := false
		for _, rule := range te.PlausibilityRules {
			if rule.Name == "failure_explained" {
				foundFailure = true
				break
			}
		}
		if !foundFailure {
			t.Errorf("tester missing failure_explained plausibility rule; got %v",
				te.PlausibilityRules)
		}
	})
	t.Run("reviewer (gate-driven)", func(t *testing.T) {
		r := findRole(t, d, "reviewer")
		assertMigrated(t, r, []string{"review:object", "review.approved:bool"})
	})
	t.Run("feasibility (lead-plan-only)", func(t *testing.T) {
		f := findRole(t, d, "feasibility")
		assertMigrated(t, f, []string{"feasibility:object", "feasibility.feasible:bool"})
		// Both conditional-explanation rules from the schema YAML
		// must surface — these are what stop a thumbs-up/thumbs-down
		// without a reason from passing validation.
		var sawFeasible, sawBlocked bool
		for _, rule := range f.PlausibilityRules {
			if rule.Name == "feasible_explained" {
				sawFeasible = true
			}
			if rule.Name == "blocked_explained" {
				sawBlocked = true
			}
		}
		if !sawFeasible || !sawBlocked {
			t.Errorf("feasibility missing conditional rules; got %v", f.PlausibilityRules)
		}
	})
	t.Run("scout (produced_files-driven)", func(t *testing.T) {
		s := findRole(t, d, "scout")
		assertMigrated(t, s, []string{
			"scout:object", "scout.project_context_written:bool",
			"produced_files:array",
		})
	})
	t.Run("architect (lead-plan-only)", func(t *testing.T) {
		a := findRole(t, d, "architect")
		assertMigrated(t, a, []string{"architect:object", "architect.committed:bool"})
	})
}

// TestSmokeLoad_NoLegacyConflictsRemain belt-and-braces: walks every
// shipped swarm + preset, and asserts that every role with
// requiredOutputKeys populated also either:
//
//	(a) has outputSchema set + InjectSchemaIntoPrompt true (migrated),
//	(b) has outputSchema unset (legacy, fine for now).
//
// The case we want to NEVER see is outputSchema set + the inject flag
// off, because that means the validator side migrated but the agent
// is still seeing whatever stale prose example the systemPrompt may
// carry. Catching this at config-load is what item 11 (config-load
// compat check) escalates to a hard refusal; this lint is the test-
// time analog.
func TestSmokeLoad_NoLegacyConflictsRemain(t *testing.T) {
	roots := []string{"../../configs", "../cli/presets"}
	for _, root := range roots {
		swarms, err := LoadSwarms(root)
		if err != nil {
			// Some roots only have presets, not swarms — skip on
			// "no swarms directory" but fail on real load errors.
			if strings.Contains(err.Error(), "no such file or directory") {
				continue
			}
			t.Fatalf("LoadSwarms(%s): %v", root, err)
		}
		for id, swarm := range swarms {
			for _, role := range swarm.Roles {
				if role.OutputSchema != nil && !role.InjectSchemaIntoPrompt {
					t.Errorf("%s/%s: outputSchema set but injectSchemaIntoPrompt false — "+
						"the validator migrated but the prompt is still operator-prose. "+
						"Either flip injectSchemaIntoPrompt: true, or remove outputSchema "+
						"and keep the legacy shape.",
						id, role.Name)
				}
			}
		}
	}
}
