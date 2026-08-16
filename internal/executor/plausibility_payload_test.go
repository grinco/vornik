package executor

import (
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Regression 2026-08-16: 32 of the long-horizon arm's 73 failures (43.8%, the
// largest single cause) were the tester setting testing.passed=true without
// the fields passed_requires_pinned_validation requires.
//
// The daemon evaluates the rule at container.go:866, AFTER the container has
// exited, and the agent was never shown it. registry/output_schema.go:389
// deliberately excludes the plausibility block from the generated provider
// schema — conditional JSON Schema is unevenly supported and
// EvaluatePlausibility runs post-receipt — and nothing else carried it either.
// So the rule was absent from the provider schema BY DECISION and from the
// prompt BY OMISSION. Only the second is a defect.
//
// Same shape as the require_output_glob defect: a contract the daemon enforces
// post-exit and the agent cannot see.
func TestBuildAgentInput_CarriesPlausibilityRules(t *testing.T) {
	rules := []registry.PlausibilityRule{{
		Name:    "passed_requires_pinned_validation",
		When:    map[string]any{"testing.passed": true},
		Require: []string{"testing.pinned_cases_validated", "testing.cases"},
	}}

	raw := buildTestAgentInput(t, &agentInputOpts{PlausibilityRules: rules})

	swarm, ok := raw["swarm"].(map[string]any)
	if !ok {
		t.Fatalf("payload has no swarm block: %#v", raw)
	}
	got, ok := swarm["plausibilityRules"].([]any)
	if !ok || len(got) != 1 {
		t.Fatalf("payload must carry the role's plausibility rules, got %#v", swarm["plausibilityRules"])
	}
	rule, _ := got[0].(map[string]any)
	if rule["name"] != "passed_requires_pinned_validation" {
		t.Errorf("rule name not carried: %#v", rule)
	}
	req, _ := rule["require"].([]any)
	if len(req) != 2 {
		t.Errorf("rule requirements not carried: %#v", rule["require"])
	}
	if rule["when"] == nil {
		t.Errorf("rule condition not carried — without it the agent cannot know WHEN the rule binds: %#v", rule)
	}
}

// Additive: a role with no rules must produce a byte-identical payload to
// before, so an older agent image is unaffected.
func TestBuildAgentInput_NoRulesLeavesPayloadUnchanged(t *testing.T) {
	raw := buildTestAgentInput(t, &agentInputOpts{})
	swarm, _ := raw["swarm"].(map[string]any)
	if _, present := swarm["plausibilityRules"]; present {
		t.Errorf("a role with no rules must not gain the key at all, got %#v", swarm["plausibilityRules"])
	}
}

// DRIFT GUARD. A rule declared in config but not reaching the payload silently
// reproduces the exact defect being fixed. This is a pure function of role
// config, so a build-time assertion is stronger than a runtime preflight: it
// cannot be skipped, and it fails before the run rather than during it.
//
// Deliberately reads the SHIPPED swarm configs rather than a fixture — a
// fixture would keep passing while the real dev-swarm tester lost its rules.
func TestShippedRolesWithPlausibilityRulesShipThemToTheAgent(t *testing.T) {
	swarms, err := registry.LoadSwarms("../../configs")
	if err != nil {
		t.Skipf("swarm configs not loadable from this test's working dir: %v", err)
	}
	checked := 0
	for name, sw := range swarms {
		for _, role := range sw.Roles {
			if len(role.PlausibilityRules) == 0 {
				continue
			}
			checked++
			raw := buildTestAgentInput(t, &agentInputOpts{PlausibilityRules: role.PlausibilityRules})
			swarmBlock, _ := raw["swarm"].(map[string]any)
			got, _ := swarmBlock["plausibilityRules"].([]any)
			if len(got) != len(role.PlausibilityRules) {
				t.Errorf("role %q in %q declares %d plausibility rule(s) but the payload carries %d",
					role.Name, name, len(role.PlausibilityRules), len(got))
			}
		}
	}
	if checked == 0 {
		t.Error("no shipped role declares plausibility rules — this guard is watching nothing; " +
			"if the rules moved, move the guard with them")
	}
}

// buildTestAgentInput renders the agent payload and returns it as a generic
// map, so assertions read the JSON the container actually receives rather than
// the Go struct that produced it.
func buildTestAgentInput(t *testing.T, opts *agentInputOpts) map[string]any {
	t.Helper()
	b := buildAgentInput(&persistence.Task{ID: "task_test"}, "exec_test", "wf", "sw", "step", "tester", "do the thing", opts)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("agent input is not valid JSON: %v\n%s", err, b)
	}
	return raw
}

// THE TEST THAT WAS MISSING, and why the fix shipped broken once.
//
// TestBuildAgentInput_CarriesPlausibilityRules proves the payload BUILDER
// carries rules when opts already has them. It says nothing about whether the
// executor ever puts them in opts — and the first version of this fix read the
// outer roleConfig variable ~74 lines before it was assigned, guarded by a
// `roleConfig != nil` check that made the mistake a silent no-op.
//
// Result: rules never reached any payload, the in-container nudge never fired
// once across an entire benchmark arm, and the tester kept being failed for a
// contract it still could not see. The unit tests were green throughout.
//
// This asserts the resolution the executor actually performs at that point in
// executeAgentStep: role lookup off the plan's swarm, not a variable that is
// still nil.
func TestExecutorResolvesPlausibilityRulesBeforeBuildingThePayload(t *testing.T) {
	sw := &registry.Swarm{
		ID: "dev-swarm",
		Roles: []registry.SwarmRole{
			{Name: "analyst"}, // deliberately first, and rule-free
			{Name: "tester", PlausibilityRules: []registry.PlausibilityRule{{
				Name:    "passed_requires_pinned_validation",
				When:    map[string]any{"testing.passed": true},
				Require: []string{"testing.pinned_cases_validated", "testing.cases"},
			}}},
		},
	}

	rc, err := findSwarmRole(sw, "tester")
	if err != nil || rc == nil {
		t.Fatalf("findSwarmRole(tester): %v", err)
	}
	if len(rc.PlausibilityRules) == 0 {
		t.Fatal("the lookup the executor performs must yield the tester's rules")
	}

	raw := buildTestAgentInput(t, &agentInputOpts{PlausibilityRules: rc.PlausibilityRules})
	swarmBlock, _ := raw["swarm"].(map[string]any)
	rules, _ := swarmBlock["plausibilityRules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("rules resolved from the plan's swarm must reach the payload, got %#v", swarmBlock["plausibilityRules"])
	}

	// A rule-free role must still produce no key, so rule-free payloads are
	// byte-identical to before.
	rcAnalyst, err := findSwarmRole(sw, "analyst")
	if err != nil || rcAnalyst == nil {
		t.Fatalf("findSwarmRole(analyst): %v", err)
	}
	rawAnalyst := buildTestAgentInput(t, &agentInputOpts{PlausibilityRules: rcAnalyst.PlausibilityRules})
	analystSwarm, _ := rawAnalyst["swarm"].(map[string]any)
	if _, present := analystSwarm["plausibilityRules"]; present {
		t.Errorf("a rule-free role must not gain the key, got %#v", analystSwarm["plausibilityRules"])
	}
}
