package executor

import (
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// The agent's schema finalization — the ONE turn on which a tool-using role's
// output schema can be enforced, because response_format and tools are mutually
// exclusive under guided decoding — is gated on a non-empty response_format:
//
//	if [ -n "$response_format" ] && ... SCHEMA_FINALIZE_PENDING==0 && TOOL_PHASE_HAPPENED==1 && tools>0
//
// Measured 2026-08-20 (bench arm 5, exec_20260820145138's report rung): that
// finalization did not fire. Its container log carries only startup and
// per-iteration lines. Three of the four conditions are provably satisfied — the
// rung made 25 tool calls so the tool phase happened, tools were offered, and it
// was the first pass — which leaves response_format empty as the only candidate.
//
// response_format comes from task.json's config.responseFormat, written by
// plan.go's optResponseFormat from opts.ResponseFormat. And opts.ResponseFormat is
// NOT set by applyRoleSchemaOpts (which sets ResponseSchema and
// ResultEmissionTool); it is set separately in resolveRoleOpts. Nothing asserted
// that the regular agent-step path actually does so, or that the value survives
// into the built input — the existing tests cover effectiveResponseFormat and
// applyRoleSchemaOpts in isolation.
//
// This closes that gap. If it passes, the daemon side is sound and the empty
// response_format must come from config or deployment; if it fails, the bug is
// here.
func TestResolveRoleOpts_SetsResponseFormatForASchemaBearingRole(t *testing.T) {
	role := registry.SwarmRole{
		Name: "analyst",
		OutputSchema: &registry.OutputSchema{
			Type:     "object",
			Required: []string{"analysis"},
			Properties: map[string]*registry.OutputSchema{
				"analysis": {Type: "object"},
			},
		},
	}
	plan := &executionPlan{swarm: &registry.Swarm{Roles: []registry.SwarmRole{role}}}
	opts := &agentInputOpts{}

	e := &Executor{}
	got := e.resolveRoleOpts(plan, registry.WorkflowStep{Type: "agent", Role: "analyst"}, nil, opts)
	if got == nil {
		t.Fatal("resolveRoleOpts returned no role config for a role that exists in the swarm")
	}

	if opts.ResponseFormat == "" {
		t.Error("opts.ResponseFormat is EMPTY for a role declaring an outputSchema. " +
			"config.responseFormat therefore reaches the agent empty, and the schema " +
			"finalization guard ([ -n \"$response_format\" ]) can never pass — so a " +
			"tool-using role's schema is never enforced on any turn.")
	}
	if opts.ResponseFormat != "json_schema" {
		t.Errorf("opts.ResponseFormat = %q, want \"json_schema\": a declared outputSchema is "+
			"what earns the strongest directive", opts.ResponseFormat)
	}
	// The schema itself must ride along, or json_schema degrades to json_object.
	if opts.ResponseSchema == nil {
		t.Error("opts.ResponseSchema is nil alongside a json_schema directive; the " +
			"request would fall back to the looser json_object form")
	}
}

// And the value must survive into the built input, not just the opts struct —
// config.responseFormat is what the agent actually reads.
func TestBuildAgentInput_CarriesResponseFormatToConfig(t *testing.T) {
	raw := buildAgentInput(&persistence.Task{ID: "t1"}, "exec_1", "wf", "swarm",
		"report", "analyst", "write the changelog",
		&agentInputOpts{ResponseFormat: "json_schema"})

	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatalf("built input is not JSON: %v", err)
	}
	cfg, ok := input["config"].(map[string]any)
	if !ok {
		t.Fatal("built input has no config object")
	}
	if got := cfg["responseFormat"]; got != "json_schema" {
		t.Errorf("config.responseFormat = %v, want \"json_schema\" — the agent reads this "+
			"key and gates its schema finalization on it being non-empty", got)
	}
}
