package registry

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestDeriveRequiredOutputKeys pins the schema → path:type derivation
// described in https://docs.vornik.io item 6. The
// legacy validator (executor's validateRequiredOutputKeys) consumes
// `path[:type]` strings; the schema must emit exactly that shape so
// the rest of the codebase needs no consumer-side changes.
func TestDeriveRequiredOutputKeys(t *testing.T) {
	tests := []struct {
		name   string
		schema *OutputSchema
		want   []string
	}{
		{
			name:   "nil schema",
			schema: nil,
			want:   nil,
		},
		{
			name:   "empty schema",
			schema: &OutputSchema{Type: "object"},
			want:   nil,
		},
		{
			name: "flat object: each top-level required becomes path:type",
			schema: &OutputSchema{
				Type:     "object",
				Required: []string{"plan", "message"},
				Properties: map[string]*OutputSchema{
					"plan":    {Type: "object"},
					"message": {Type: "string"},
				},
			},
			// Sorted output for deterministic comparison.
			want: []string{"message:string", "plan:object"},
		},
		{
			name: "nested required path: writing.written:bool sits alongside writing:object",
			schema: &OutputSchema{
				Type:     "object",
				Required: []string{"writing", "produced_files", "message"},
				Properties: map[string]*OutputSchema{
					"writing": {
						Type:     "object",
						Required: []string{"written"},
						Properties: map[string]*OutputSchema{
							"written": {Type: "bool"},
							"path":    {Type: "string"},
						},
					},
					"produced_files": {Type: "array"},
					"message":        {Type: "string"},
				},
			},
			want: []string{
				"message:string",
				"produced_files:array",
				"writing.written:bool",
				"writing:object",
			},
		},
		{
			name: "required name without a properties entry emits path-only (no type)",
			schema: &OutputSchema{
				Type:     "object",
				Required: []string{"opaque"},
				// Properties is nil — the required name must still
				// produce an entry, just without the type assertion.
				// Mirrors the legacy "bare key in requiredOutputKeys"
				// behaviour the validator already supports.
			},
			want: []string{"opaque"},
		},
		{
			name: "required name with empty type emits path-only",
			schema: &OutputSchema{
				Type:     "object",
				Required: []string{"data"},
				Properties: map[string]*OutputSchema{
					"data": {}, // type unset
				},
			},
			want: []string{"data"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.schema.DeriveRequiredOutputKeys()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DeriveRequiredOutputKeys = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDerivePlausibilityRules covers both the explicit pass-through of
// `plausibility:` and the implicit rules that minLength:1 generates.
// The implicit rules are how schema-level "non-empty string" enforcement
// reaches the existing PlausibilityRule evaluator without touching the
// validator's type check (which accepts ""). See
// https://docs.vornik.io item 3 for the writer-message
// regression that motivates this.
func TestDerivePlausibilityRules(t *testing.T) {
	tests := []struct {
		name   string
		schema *OutputSchema
		want   []PlausibilityRule
	}{
		{
			name:   "nil schema",
			schema: nil,
			want:   nil,
		},
		{
			name: "explicit plausibility block passes through",
			schema: &OutputSchema{
				Plausibility: []PlausibilityRule{
					{
						Name:    "writing_written_path",
						When:    map[string]any{"writing.written": true},
						Require: []string{"writing.path"},
					},
				},
			},
			want: []PlausibilityRule{
				{
					Name:    "writing_written_path",
					When:    map[string]any{"writing.written": true},
					Require: []string{"writing.path"},
				},
			},
		},
		{
			name: "minLength:1 on a string generates a require-non-empty rule",
			schema: &OutputSchema{
				Type:     "object",
				Required: []string{"message"},
				Properties: map[string]*OutputSchema{
					"message": {Type: "string", MinLength: 1},
				},
			},
			want: []PlausibilityRule{
				{
					Name:    "min_length_message",
					Require: []string{"message"},
				},
			},
		},
		{
			name: "minLength on a non-string is ignored (numeric bounds belong to phase 2)",
			schema: &OutputSchema{
				Type: "object",
				Properties: map[string]*OutputSchema{
					"count": {Type: "number", MinLength: 1},
				},
			},
			want: nil,
		},
		{
			name: "nested minLength path is dotted in the rule name + require",
			schema: &OutputSchema{
				Type: "object",
				Properties: map[string]*OutputSchema{
					"writing": {
						Type: "object",
						Properties: map[string]*OutputSchema{
							"summary": {Type: "string", MinLength: 1},
						},
					},
				},
			},
			want: []PlausibilityRule{
				{
					Name:    "min_length_writing_summary",
					Require: []string{"writing.summary"},
				},
			},
		},
		{
			name: "explicit + implicit are concatenated, explicit first",
			schema: &OutputSchema{
				Type: "object",
				Properties: map[string]*OutputSchema{
					"message": {Type: "string", MinLength: 1},
				},
				Plausibility: []PlausibilityRule{
					{Name: "explicit_first", Require: []string{"a"}},
				},
			},
			want: []PlausibilityRule{
				{Name: "explicit_first", Require: []string{"a"}},
				{Name: "min_length_message", Require: []string{"message"}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.schema.DerivePlausibilityRules()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DerivePlausibilityRules = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestRenderForPrompt covers the prompt-rendering side of the schema —
// the prose the agent sees in lieu of a hand-written `Output on
// success: { ... }` block. Asserts:
//   - empty / nil schemas render to "" so callers can `if s := ...; s != ""`
//   - the header instruction appears before any per-key clause
//   - each top-level required key appears with its type summary
//   - nested required paths appear under their parent, indented
//   - explicit plausibility rules appear in a labelled block with
//     deterministic when-clause ordering
//   - minLength:1 surfaces as "non-empty" in the type summary AND as
//     an implicit conditional (via DerivePlausibilityRules)
//   - rendering is deterministic: same schema → same bytes regardless
//     of map iteration order
func TestRenderForPrompt(t *testing.T) {
	t.Run("nil schema", func(t *testing.T) {
		if got := (*OutputSchema)(nil).RenderForPrompt(); got != "" {
			t.Errorf("nil schema rendered: %q", got)
		}
	})
	t.Run("empty schema", func(t *testing.T) {
		s := &OutputSchema{Type: "object"}
		if got := s.RenderForPrompt(); got != "" {
			t.Errorf("empty schema rendered: %q", got)
		}
	})

	t.Run("writer-shaped schema renders all sections", func(t *testing.T) {
		schema := &OutputSchema{
			Type:     "object",
			Required: []string{"writing", "produced_files", "message"},
			Properties: map[string]*OutputSchema{
				"writing": {
					Type:     "object",
					Required: []string{"written"},
					Properties: map[string]*OutputSchema{
						"written": {Type: "bool"},
						"path":    {Type: "string"},
						"reason":  {Type: "string"},
					},
				},
				"produced_files": {Type: "array"},
				"message":        {Type: "string", MinLength: 1},
			},
			Plausibility: []PlausibilityRule{
				{
					Name:    "written_implies_path",
					When:    map[string]any{"writing.written": true},
					Require: []string{"writing.path"},
				},
			},
		}
		got := schema.RenderForPrompt()

		// Header — eliminates the "model wrapped JSON in a fence"
		// failure class at the prompt level, mirroring the gateway-
		// side json_object default (item 8).
		expectContains(t, got, "Respond with ONLY a JSON object")

		// JSON skeleton showing the actual nested structure. Pre-fix
		// this section was a bullet list of dotted paths — models
		// trained that into flat keys ("research.written":true)
		// which then failed parent-object validation. The skeleton
		// shows the structural nesting AND the type expectations.
		expectContains(t, got, "Your response must match this JSON shape:")
		expectContains(t, got, `"message": <string, non-empty>`)
		expectContains(t, got, `"produced_files": <array> /* required */`)
		expectContains(t, got, `"writing": {`)

		// Nested required path under its parent — value is rendered
		// with /* required */ marker, not a separate "X.Y" path.
		expectContains(t, got, `"written": <bool> /* required */`)

		// Plausibility section: explicit rule first, then the
		// implicit min_length rule generated by minLength:1.
		expectContains(t, got, "Conditional requirements")
		expectContains(t, got, `when writing.written=true: writing.path must be present and non-empty (rule "written_implies_path")`)
		expectContains(t, got, `always: message must be present and non-empty (rule "min_length_message")`)
	})

	t.Run("rendering is deterministic across runs", func(t *testing.T) {
		// Two structurally-identical schemas built independently —
		// map iteration order in walkMinLength / renderNested could
		// otherwise surface as a diff.
		mk := func() *OutputSchema {
			return &OutputSchema{
				Type:     "object",
				Required: []string{"a", "b"},
				Properties: map[string]*OutputSchema{
					"a": {Type: "string", MinLength: 1},
					"b": {Type: "string", MinLength: 1},
				},
			}
		}
		a := mk().RenderForPrompt()
		b := mk().RenderForPrompt()
		if a != b {
			t.Fatalf("non-deterministic render:\nfirst:\n%s\nsecond:\n%s", a, b)
		}
	})
}

func expectContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("render missing expected fragment %q\nfull render:\n%s", want, got)
	}
}

// TestToJSONSchema covers the dialect → JSON Schema converter (item 7
// of https://docs.vornik.io). The converter feeds
// LLM gateways that accept response_format: { type: "json_schema" } —
// provider-side schema enforcement, not just post-validation.
//
// Specific invariants pinned here:
//   - bool → "boolean" (JSON Schema's name) but no other type renames.
//   - object types get additionalProperties: false so providers reject
//     unexpected keys — the dialect's plausibility layer can't catch
//     emitted-extra-key cases on its own.
//   - plausibility: {} block is dropped (provider-side conditionals
//     aren't uniformly supported; EvaluatePlausibility runs
//     post-receipt anyway).
//   - sorted property names → byte-identical output across runs.
//   - nil receiver → nil result (callers gate on this to skip
//     provider-schema enforcement when no schema is declared).
func TestToJSONSchema(t *testing.T) {
	t.Run("nil receiver returns nil", func(t *testing.T) {
		var s *OutputSchema
		if got := s.ToJSONSchema(); got != nil {
			t.Errorf("nil receiver got non-nil schema: %#v", got)
		}
	})

	t.Run("empty schema (no type, no required, no properties) returns nil", func(t *testing.T) {
		s := &OutputSchema{}
		if got := s.ToJSONSchema(); got != nil {
			t.Errorf("empty schema produced output: %#v", got)
		}
	})

	t.Run("bool type renamed to boolean", func(t *testing.T) {
		s := &OutputSchema{
			Type: "object",
			Properties: map[string]*OutputSchema{
				"flag": {Type: "bool"},
			},
		}
		got := s.ToJSONSchema()
		props, ok := got["properties"].(map[string]any)
		if !ok {
			t.Fatalf("properties missing or wrong type: %#v", got)
		}
		flag, ok := props["flag"].(map[string]any)
		if !ok {
			t.Fatalf("flag missing or wrong type: %#v", props)
		}
		if flag["type"] != "boolean" {
			t.Errorf("bool not renamed to boolean: got %v", flag["type"])
		}
	})

	t.Run("writer-shaped schema converts losslessly", func(t *testing.T) {
		schema := &OutputSchema{
			Type:     "object",
			Required: []string{"writing", "produced_files", "message"},
			Properties: map[string]*OutputSchema{
				"writing": {
					Type:     "object",
					Required: []string{"written"},
					Properties: map[string]*OutputSchema{
						"written": {Type: "bool"},
						"path":    {Type: "string"},
					},
				},
				"produced_files": {
					Type:  "array",
					Items: &OutputSchema{Type: "string"},
				},
				"message": {Type: "string", MinLength: 1},
			},
			Plausibility: []PlausibilityRule{
				{Name: "x", Require: []string{"writing.path"}},
			},
		}
		got := schema.ToJSONSchema()

		// Top-level shape.
		if got["type"] != "object" {
			t.Errorf("top type = %v, want object", got["type"])
		}
		if got["additionalProperties"] != false {
			t.Errorf("top additionalProperties = %v, want false", got["additionalProperties"])
		}
		// Plausibility block must NOT leak into JSON Schema.
		if _, present := got["plausibility"]; present {
			t.Error("plausibility leaked into JSON Schema output")
		}

		props := got["properties"].(map[string]any)

		writing := props["writing"].(map[string]any)
		if writing["additionalProperties"] != false {
			t.Errorf("nested object additionalProperties = %v, want false", writing["additionalProperties"])
		}
		writingProps := writing["properties"].(map[string]any)
		if writingProps["written"].(map[string]any)["type"] != "boolean" {
			t.Error("nested bool not renamed")
		}

		// Array items recurse through the converter too.
		producedFiles := props["produced_files"].(map[string]any)
		items := producedFiles["items"].(map[string]any)
		if items["type"] != "string" {
			t.Errorf("array items type = %v, want string", items["type"])
		}

		// minLength survives.
		message := props["message"].(map[string]any)
		if message["minLength"] != 1 {
			t.Errorf("minLength = %v, want 1", message["minLength"])
		}
	})

	t.Run("conversion is deterministic across runs", func(t *testing.T) {
		mk := func() *OutputSchema {
			return &OutputSchema{
				Type:     "object",
				Required: []string{"a", "b"},
				Properties: map[string]*OutputSchema{
					"a": {Type: "string"},
					"b": {Type: "number"},
				},
			}
		}
		// Marshal both to JSON and compare bytes — Go's map
		// iteration order would otherwise hide nondeterminism.
		first, _ := mkJSON(mk().ToJSONSchema())
		second, _ := mkJSON(mk().ToJSONSchema())
		if first != second {
			t.Fatalf("non-deterministic output:\nfirst:  %s\nsecond: %s", first, second)
		}
	})
}

// mkJSON helper for the determinism check — encoding/json marshal
// follows map-key alphabetical order, so byte equality of the
// marshalled output is the cleanest determinism assertion available.
func mkJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// TestToToolSpec covers the synthetic-tool-spec generator (item 9 of
// https://docs.vornik.io). The agent runtime
// surfaces this tool to the LLM gateway so the model produces its
// result via a validated tool call instead of a free-form JSON
// envelope — strongest portable schema enforcement.
func TestToToolSpec(t *testing.T) {
	t.Run("nil schema returns nil", func(t *testing.T) {
		var s *OutputSchema
		if got := s.ToToolSpec("anything"); got != nil {
			t.Errorf("nil schema produced tool: %#v", got)
		}
	})
	t.Run("empty schema returns nil (no body to enforce)", func(t *testing.T) {
		s := &OutputSchema{}
		if got := s.ToToolSpec("writer"); got != nil {
			t.Errorf("empty schema produced tool: %#v", got)
		}
	})
	t.Run("populated schema returns stable tool name + JSON-Schema params", func(t *testing.T) {
		s := &OutputSchema{
			Type:     "object",
			Required: []string{"writing"},
			Properties: map[string]*OutputSchema{
				"writing": {Type: "object"},
			},
		}
		got := s.ToToolSpec("writer")
		if got == nil {
			t.Fatal("expected tool spec")
		}
		// Stable role-derived name — replay corpora and audit
		// trails depend on it not having a random suffix.
		if got.Name != "emit_writer_result" {
			t.Errorf("Name = %q, want emit_writer_result", got.Name)
		}
		if got.Description == "" {
			t.Error("Description must be non-empty (LLMs route on tool descriptions)")
		}
		// Parameters IS the JSON Schema — same body the
		// responseSchema path emits.
		if got.Parameters["type"] != "object" {
			t.Errorf("Parameters.type = %v, want object", got.Parameters["type"])
		}
		if got.Parameters["additionalProperties"] != false {
			t.Errorf("Parameters.additionalProperties = %v, want false", got.Parameters["additionalProperties"])
		}
	})
}

// TestSchemaVersionSurfaces pins the three places version: N flows
// into when set: the rendered prompt prose, the JSON Schema output
// (as the x-vornik-version extension key), and — implicitly — the
// task.json config.responseSchema (since that's the JSON Schema
// passed through verbatim).
//
// Item 13 of https://docs.vornik.io Schema
// versioning is metadata only today; this test pins the
// surface-everywhere invariant so a future migration trail
// implementation can rely on the version flowing through unchanged.
func TestSchemaVersionSurfaces(t *testing.T) {
	schema := &OutputSchema{
		Version:  3,
		Type:     "object",
		Required: []string{"x"},
		Properties: map[string]*OutputSchema{
			"x": {Type: "string"},
		},
	}
	t.Run("rendered prompt includes schema version line", func(t *testing.T) {
		got := schema.RenderForPrompt()
		if !strings.Contains(got, "(schema v3)") {
			t.Errorf("render missing schema version marker; got:\n%s", got)
		}
	})
	t.Run("JSON Schema carries x-vornik-version", func(t *testing.T) {
		got := schema.ToJSONSchema()
		if got["x-vornik-version"] != 3 {
			t.Errorf("x-vornik-version = %v, want 3", got["x-vornik-version"])
		}
	})
	t.Run("version 0 (unset) is suppressed everywhere", func(t *testing.T) {
		zero := &OutputSchema{
			Type:     "object",
			Required: []string{"x"},
			Properties: map[string]*OutputSchema{
				"x": {Type: "string"},
			},
		}
		js := zero.ToJSONSchema()
		if _, present := js["x-vornik-version"]; present {
			t.Error("x-vornik-version present when Version=0 (should be omitted)")
		}
		render := zero.RenderForPrompt()
		if strings.Contains(render, "schema v") {
			t.Errorf("render contains version marker when Version=0; got:\n%s", render)
		}
	})
}
