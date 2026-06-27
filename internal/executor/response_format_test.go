package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestEffectiveResponseFormat pins the default-to-json_object policy
// described in https://docs.vornik.io item 8.
//
// Three rules, each anchored to a current production failure mode:
//
//  1. Explicit role.ResponseFormat wins. Operators who set the
//     field expect their value passed through; defaulting around
//     them would silently override.
//  2. Empty role.ResponseFormat + any output validation declared
//     → "json_object". Any role that promises a JSON shape via
//     RequiredOutputKeys or PlausibilityRules gets gateway-level
//     json-mode automatically — closes the "model wrapped JSON in
//     a markdown fence" failure class without operator action.
//  3. Empty role.ResponseFormat + no validation declared → "".
//     Free-form roles (vision, dispatcher) keep returning prose;
//     json-mode would be a behaviour change for them.
func TestEffectiveResponseFormat(t *testing.T) {
	tests := []struct {
		name string
		role *registry.SwarmRole
		want string
	}{
		{
			name: "nil role returns empty",
			role: nil,
			want: "",
		},
		{
			name: "explicit responseFormat wins over default",
			role: &registry.SwarmRole{
				ResponseFormat:     "json_object",
				RequiredOutputKeys: []string{"plan"},
			},
			want: "json_object",
		},
		{
			name: "explicit non-default value preserved",
			role: &registry.SwarmRole{
				// Future-friendly: an operator setting an unknown
				// value (e.g. a json_schema flavour) gets the value
				// passed through to the gateway, not silently
				// rewritten to "json_object".
				ResponseFormat:     "json_schema",
				RequiredOutputKeys: []string{"writing"},
			},
			want: "json_schema",
		},
		{
			name: "empty + RequiredOutputKeys → json_object",
			role: &registry.SwarmRole{
				RequiredOutputKeys: []string{"analysis"},
			},
			want: "json_object",
		},
		{
			name: "empty + PlausibilityRules → json_object",
			role: &registry.SwarmRole{
				PlausibilityRules: []registry.PlausibilityRule{
					{Name: "approved-needs-feedback"},
				},
			},
			want: "json_object",
		},
		{
			name: "empty + no validation → empty (free-form)",
			role: &registry.SwarmRole{
				Name: "vision",
			},
			want: "",
		},
		{
			name: "empty RequiredOutputKeys list does not trigger default",
			role: &registry.SwarmRole{
				RequiredOutputKeys: []string{},
			},
			want: "",
		},
		{
			// Item 7 of https://docs.vornik.io:
			// when the role declares an outputSchema, the executor
			// should prefer the typed json_schema directive so
			// provider-side enforcement (OpenAI / Bedrock json_schema
			// mode, Anthropic tool-use forcing) kicks in instead of
			// the looser json_object nudge. Pin this default so the
			// daemon → entrypoint → chat-proxy → provider path lands
			// on json_schema for every migrated role automatically.
			name: "outputSchema present → json_schema (item 7 default)",
			role: &registry.SwarmRole{
				OutputSchema: &registry.OutputSchema{
					Type:     "object",
					Required: []string{"writing"},
					Properties: map[string]*registry.OutputSchema{
						"writing": {Type: "object"},
					},
				},
			},
			want: "json_schema",
		},
		{
			// Operator-set responseFormat still wins even when
			// outputSchema is present — escape hatch for a role
			// whose provider rejects strict schema enforcement and
			// the operator wants to fall back to json_object
			// without dropping the schema entirely (the schema
			// still drives validateRequiredOutputKeys +
			// plausibility post-receipt).
			name: "explicit responseFormat wins over outputSchema default",
			role: &registry.SwarmRole{
				ResponseFormat: "json_object",
				OutputSchema: &registry.OutputSchema{
					Type:     "object",
					Required: []string{"x"},
				},
			},
			want: "json_object",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveResponseFormat(tc.role)
			if got != tc.want {
				t.Errorf("effectiveResponseFormat = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAppendSchemaPromptIfEnabled covers the prompt-injection wiring
// for item 6 phase 2. The flag is opt-in per role (gate a one-role-at-
// a-time migration without flipping the default for every shipped
// swarm), and the helper is called from every agentInputOpts
// construction site so a migrated role behaves identically whether
// it's invoked via a workflow step, a lead-spawned plan, or the
// lead's own planning step.
func TestAppendSchemaPromptIfEnabled(t *testing.T) {
	schema := &registry.OutputSchema{
		Type:     "object",
		Required: []string{"writing"},
		Properties: map[string]*registry.OutputSchema{
			"writing": {Type: "object"},
		},
	}

	t.Run("nil opts is a no-op (no panic)", func(t *testing.T) {
		appendSchemaPromptIfEnabled(nil, &registry.SwarmRole{
			OutputSchema:           schema,
			InjectSchemaIntoPrompt: true,
		})
	})

	t.Run("nil role is a no-op", func(t *testing.T) {
		opts := &agentInputOpts{StepPrompt: "do the thing"}
		appendSchemaPromptIfEnabled(opts, nil)
		if opts.StepPrompt != "do the thing" {
			t.Errorf("StepPrompt mutated when role nil: %q", opts.StepPrompt)
		}
	})

	t.Run("schema set but flag off → unchanged", func(t *testing.T) {
		opts := &agentInputOpts{StepPrompt: "do the thing"}
		appendSchemaPromptIfEnabled(opts, &registry.SwarmRole{
			OutputSchema: schema,
			// InjectSchemaIntoPrompt: false (zero value)
		})
		if opts.StepPrompt != "do the thing" {
			t.Errorf("StepPrompt mutated when flag off: %q", opts.StepPrompt)
		}
	})

	t.Run("flag on → render appended after step prompt", func(t *testing.T) {
		opts := &agentInputOpts{StepPrompt: "do the thing"}
		appendSchemaPromptIfEnabled(opts, &registry.SwarmRole{
			OutputSchema:           schema,
			InjectSchemaIntoPrompt: true,
		})
		if !strings.HasPrefix(opts.StepPrompt, "do the thing\n\n") {
			t.Errorf("expected schema appended after blank line; got: %q",
				opts.StepPrompt)
		}
		if !strings.Contains(opts.StepPrompt, "Your response must match this JSON shape:") {
			t.Errorf("expected rendered schema in prompt; got: %q",
				opts.StepPrompt)
		}
	})

	t.Run("empty step prompt → schema becomes the whole prompt", func(t *testing.T) {
		opts := &agentInputOpts{}
		appendSchemaPromptIfEnabled(opts, &registry.SwarmRole{
			OutputSchema:           schema,
			InjectSchemaIntoPrompt: true,
		})
		if !strings.HasPrefix(opts.StepPrompt, "Respond with ONLY a JSON object") {
			t.Errorf("expected schema as full prompt; got: %q", opts.StepPrompt)
		}
	})

	t.Run("empty schema render → unchanged", func(t *testing.T) {
		// A schema with no required keys + no plausibility renders
		// to "" — the helper must not append a stray blank line.
		emptyish := &registry.OutputSchema{Type: "object"}
		opts := &agentInputOpts{StepPrompt: "do the thing"}
		appendSchemaPromptIfEnabled(opts, &registry.SwarmRole{
			OutputSchema:           emptyish,
			InjectSchemaIntoPrompt: true,
		})
		if opts.StepPrompt != "do the thing" {
			t.Errorf("StepPrompt mutated by empty render: %q", opts.StepPrompt)
		}
	})
}

// TestApplyRoleSchemaOpts pins the cross-wiring helper that every
// agentInputOpts construction site uses. Item 7 of the
// deterministic-output-schema delivery plan: when the role has an
// outputSchema, the JSON Schema must reach the agent runtime via
// agentInputOpts.ResponseSchema, AND when the role opted into prompt
// injection, the rendered prose must reach the prompt. Both behaviours
// must come from one helper so the three call sites can't drift.
func TestApplyRoleSchemaOpts(t *testing.T) {
	schema := &registry.OutputSchema{
		Type:     "object",
		Required: []string{"writing"},
		Properties: map[string]*registry.OutputSchema{
			"writing": {Type: "object"},
		},
	}

	t.Run("role with no outputSchema → no-op", func(t *testing.T) {
		opts := &agentInputOpts{StepPrompt: "x"}
		applyRoleSchemaOpts(opts, &registry.SwarmRole{Name: "vision"})
		if opts.ResponseSchema != nil {
			t.Errorf("ResponseSchema set when role had no schema: %#v", opts.ResponseSchema)
		}
		if opts.StepPrompt != "x" {
			t.Errorf("StepPrompt mutated when role had no schema: %q", opts.StepPrompt)
		}
	})

	t.Run("schema present, inject off → schema wired but prompt untouched", func(t *testing.T) {
		opts := &agentInputOpts{StepPrompt: "x"}
		applyRoleSchemaOpts(opts, &registry.SwarmRole{
			OutputSchema: schema,
			// InjectSchemaIntoPrompt: false (zero value)
		})
		if opts.ResponseSchema == nil {
			t.Error("ResponseSchema should be wired regardless of inject flag")
		}
		if opts.StepPrompt != "x" {
			t.Errorf("StepPrompt mutated when inject flag off: %q", opts.StepPrompt)
		}
	})

	t.Run("schema + inject → both wired", func(t *testing.T) {
		opts := &agentInputOpts{StepPrompt: "x"}
		applyRoleSchemaOpts(opts, &registry.SwarmRole{
			OutputSchema:           schema,
			InjectSchemaIntoPrompt: true,
		})
		if opts.ResponseSchema == nil {
			t.Error("ResponseSchema not wired")
		}
		if !strings.Contains(opts.StepPrompt, "Your response must match this JSON shape:") {
			t.Errorf("rendered prompt not appended; got: %q", opts.StepPrompt)
		}
	})
}

// TestBuildAgentInput_ResponseSchemaSurface confirms the schema
// reaches task.json's config.responseSchema field — the contract the
// agent runtime reads. Without this end-to-end test, an internal-only
// refactor could silently drop the field and the daemon-side change
// would still pass its narrower unit tests.
func TestBuildAgentInput_ResponseSchemaSurface(t *testing.T) {
	task := &persistence.Task{ID: "t", ProjectID: "p"}
	schema := map[string]any{
		"type":     "object",
		"required": []string{"writing"},
	}
	out := buildAgentInput(task, "e1", "wf", "s", "step", "writer", "stub prompt",
		&agentInputOpts{
			ResponseFormat: "json_object",
			ResponseSchema: schema,
		})

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("buildAgentInput emitted invalid JSON: %v\nfull: %s", err, out)
	}
	cfg, ok := parsed["config"].(map[string]any)
	if !ok {
		t.Fatalf("config missing or wrong type in task.json")
	}
	// responseFormat preserved so back-compat with runtimes that
	// don't yet read responseSchema holds.
	if cfg["responseFormat"] != "json_object" {
		t.Errorf("config.responseFormat = %v, want json_object", cfg["responseFormat"])
	}
	// responseSchema surfaces in the same config block.
	got, ok := cfg["responseSchema"].(map[string]any)
	if !ok {
		t.Fatalf("config.responseSchema missing or wrong type: %#v", cfg["responseSchema"])
	}
	if got["type"] != "object" {
		t.Errorf("schema.type = %v, want object", got["type"])
	}
}

// TestBuildAgentInput_NoResponseSchemaWhenNotSet pins the back-compat
// invariant: when the opts don't carry a schema, task.json must NOT
// include the responseSchema key. A runtime that does string-match on
// the field's presence (legitimate path during migration) shouldn't
// see a phantom empty value.
func TestBuildAgentInput_NoResponseSchemaWhenNotSet(t *testing.T) {
	task := &persistence.Task{ID: "t", ProjectID: "p"}
	out := buildAgentInput(task, "e1", "wf", "s", "step", "writer", "stub prompt",
		&agentInputOpts{ResponseFormat: "json_object"})
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	cfg := parsed["config"].(map[string]any)
	if _, present := cfg["responseSchema"]; present {
		t.Errorf("config.responseSchema should be absent when opts.ResponseSchema is nil; got: %#v",
			cfg["responseSchema"])
	}
	if _, present := cfg["resultEmissionTool"]; present {
		t.Errorf("config.resultEmissionTool should be absent when opts.ResultEmissionTool is nil; got: %#v",
			cfg["resultEmissionTool"])
	}
}

// TestApplyRoleSchemaOpts_WiresFullEndToEnd is the integration-shaped
// proof for item 7: a single role with an outputSchema produces an
// agentInputOpts whose ResponseFormat, ResponseSchema, and
// ResultEmissionTool fields are all populated, and the resulting
// task.json hands the agent runtime everything it needs to enforce
// the schema at the provider boundary.
//
// Without this end-to-end test, any internal refactor could
// silently drop one of the three signals and the narrower unit
// tests would still pass — the agent would build a request without
// schema enforcement and we'd be back to post-validate retries.
//
// This test is the daemon-side anchor; the chat-side
// response_schema_enforcement_test.go covers the wire-level
// translation per provider.
func TestApplyRoleSchemaOpts_WiresFullEndToEnd(t *testing.T) {
	role := &registry.SwarmRole{
		Name: "writer",
		OutputSchema: &registry.OutputSchema{
			Type:     "object",
			Required: []string{"writing"},
			Properties: map[string]*registry.OutputSchema{
				"writing": {
					Type:     "object",
					Required: []string{"written"},
					Properties: map[string]*registry.OutputSchema{
						"written": {Type: "bool"},
					},
				},
			},
		},
	}

	// Stage 1: applyRoleSchemaOpts populates opts (the helper every
	// agentInputOpts construction site delegates to).
	opts := &agentInputOpts{}
	opts.ResponseFormat = effectiveResponseFormat(role)
	applyRoleSchemaOpts(opts, role)

	if opts.ResponseFormat != "json_schema" {
		t.Errorf("ResponseFormat = %q, want json_schema (item 7 default)", opts.ResponseFormat)
	}
	if opts.ResponseSchema == nil {
		t.Fatal("ResponseSchema not populated; provider-side enforcement bypassed")
	}
	if opts.ResultEmissionTool == nil {
		t.Fatal("ResultEmissionTool not populated; tool-use enforcement bypassed")
	}
	if opts.ResultEmissionTool.Name != "emit_writer_result" {
		t.Errorf("tool name = %q, want emit_writer_result", opts.ResultEmissionTool.Name)
	}

	// Stage 2: task.json round-trip — the contract surface the
	// agent reads. Pin every field a runtime might consult so a
	// silent drop in any of them surfaces as a test failure here.
	task := &persistence.Task{ID: "t", ProjectID: "p"}
	out := buildAgentInput(task, "e1", "wf", "s", "step", "writer", "stub", opts)
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("buildAgentInput emitted invalid JSON: %v", err)
	}
	cfg := parsed["config"].(map[string]any)
	if cfg["responseFormat"] != "json_schema" {
		t.Errorf("config.responseFormat = %v, want json_schema", cfg["responseFormat"])
	}
	schema, ok := cfg["responseSchema"].(map[string]any)
	if !ok {
		t.Fatalf("config.responseSchema missing or wrong type: %#v", cfg["responseSchema"])
	}
	if schema["type"] != "object" {
		t.Errorf("schema.type = %v, want object", schema["type"])
	}
	tool, ok := cfg["resultEmissionTool"].(map[string]any)
	if !ok {
		t.Fatalf("config.resultEmissionTool missing: %#v", cfg["resultEmissionTool"])
	}
	if tool["name"] != "emit_writer_result" {
		t.Errorf("tool name = %v, want emit_writer_result", tool["name"])
	}
}

// TestApplyRoleSchemaOpts_FallsBackCleanlyWhenSchemaEmpty pins the
// no-regression invariant: a role without an outputSchema — or one
// with a schema that produces no usable JSON Schema body (no type,
// no required, no properties) — must not leave the runtime
// configured for json_schema enforcement against an empty schema.
// effectiveResponseFormat should fall through to "" or "json_object"
// per the legacy rules, and the daemon should not surface a
// responseSchema field at all.
func TestApplyRoleSchemaOpts_FallsBackCleanlyWhenSchemaEmpty(t *testing.T) {
	tests := []struct {
		name string
		role *registry.SwarmRole
		want string
	}{
		{
			name: "no outputSchema at all → fallback chain",
			role: &registry.SwarmRole{
				Name:               "writer",
				RequiredOutputKeys: []string{"message"},
			},
			want: "json_object",
		},
		{
			name: "outputSchema that produces nil JSON Schema → fallback",
			role: &registry.SwarmRole{
				Name:         "noop",
				OutputSchema: &registry.OutputSchema{},
			},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveResponseFormat(tc.role)
			if got != tc.want {
				t.Errorf("effectiveResponseFormat = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildAgentInput_ResultEmissionToolSurface confirms item 9's
// daemon-side contract: when the role has an outputSchema, task.json
// includes a config.resultEmissionTool block with the synthetic tool
// spec ready for the agent runtime to register with the LLM gateway.
func TestBuildAgentInput_ResultEmissionToolSurface(t *testing.T) {
	task := &persistence.Task{ID: "t", ProjectID: "p"}
	out := buildAgentInput(task, "e1", "wf", "s", "step", "writer", "stub prompt",
		&agentInputOpts{
			ResponseFormat: "json_object",
			ResultEmissionTool: &registry.ToolSpec{
				Name:        "emit_writer_result",
				Description: "Emit the role's structured result.",
				Parameters: map[string]any{
					"type": "object",
				},
			},
		})
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	cfg := parsed["config"].(map[string]any)
	tool, ok := cfg["resultEmissionTool"].(map[string]any)
	if !ok {
		t.Fatalf("config.resultEmissionTool missing or wrong type: %#v", cfg["resultEmissionTool"])
	}
	if tool["name"] != "emit_writer_result" {
		t.Errorf("tool.name = %v, want emit_writer_result", tool["name"])
	}
	if tool["description"] == "" {
		t.Error("tool.description must round-trip non-empty")
	}
	params, ok := tool["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Errorf("tool.parameters round-trip lost: %#v", tool["parameters"])
	}
}
