package executor

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestInterpolateOutputs_ExactMatchPreservesType asserts a
// payload field whose entire value IS the reference picks up
// the resolved JSON shape verbatim — a list reference yields
// a Go slice, a number reference yields a float64, etc. This
// is what makes typed envelopes feed cleanly into downstream
// JSON-shaped payloads.
func TestInterpolateOutputs_ExactMatchPreservesType(t *testing.T) {
	steps := map[string]json.RawMessage{
		"gather": json.RawMessage(`{"brief":"launch Q3 campaign","kpis":["click_rate","conversion"],"count":42}`),
	}
	in := map[string]any{
		"brief":     "${outputs.gather.brief}",
		"kpis":      "${outputs.gather.kpis}",
		"count":     "${outputs.gather.count}",
		"unrelated": "literal value",
	}
	out := interpolateOutputs(in, steps).(map[string]any)

	if out["brief"] != "launch Q3 campaign" {
		t.Errorf("brief = %v, want literal string", out["brief"])
	}
	if kpis, ok := out["kpis"].([]any); !ok || len(kpis) != 2 || kpis[0] != "click_rate" {
		t.Errorf("kpis lost type fidelity: %#v", out["kpis"])
	}
	if cnt, ok := out["count"].(float64); !ok || cnt != 42 {
		t.Errorf("count = %v, want 42.0", out["count"])
	}
	if out["unrelated"] != "literal value" {
		t.Errorf("non-reference field mutated: %v", out["unrelated"])
	}
}

// TestInterpolateOutputs_EmbeddedReferences asserts a string
// containing a reference as a SUBSTRING substitutes the scalar
// form. Lists/maps in this case round-trip as compact JSON
// (no other sensible choice in a string template).
func TestInterpolateOutputs_EmbeddedReferences(t *testing.T) {
	steps := map[string]json.RawMessage{
		"x": json.RawMessage(`{"name":"q3","price":9.99,"tags":["a","b"]}`),
	}
	cases := []struct {
		in   string
		want string
	}{
		{"campaign-${outputs.x.name}", "campaign-q3"},
		{"price: $${outputs.x.price}", "price: $9.99"},
		{"tags = ${outputs.x.tags}", `tags = ["a","b"]`},
		{"two ${outputs.x.name} refs ${outputs.x.name}", "two q3 refs q3"},
	}
	for _, c := range cases {
		got := interpolateStringRefs(c.in, steps)
		if got != c.want {
			t.Errorf("interpolate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestInterpolateOutputs_AbsentReferenceResolvesEmpty matches
// the "missing reference falls back to empty" semantics in the
// production helper. Operators see broken values in the
// payload rather than a crash — surfaces the bad reference
// loudly.
func TestInterpolateOutputs_AbsentReferenceResolvesEmpty(t *testing.T) {
	steps := map[string]json.RawMessage{
		"present": json.RawMessage(`{"name":"x"}`),
	}
	// Exact-match against absent step → empty string.
	if v := interpolateStringRefs("${outputs.missing.x}", steps); v != "" {
		t.Errorf("missing step exact-match = %v, want empty", v)
	}
	// Exact-match against absent field of a present step → "".
	if v := interpolateStringRefs("${outputs.present.absent}", steps); v != "" {
		t.Errorf("missing field exact-match = %v, want empty", v)
	}
	// Embedded with absent reference still works.
	if v := interpolateStringRefs("pre-${outputs.missing.x}-post", steps); v != "pre--post" {
		t.Errorf("embedded missing = %q, want pre--post", v)
	}
}

// TestInterpolateOutputs_NestedFieldPaths asserts dot-separated
// paths drill into nested JSON. The LLD's worked example uses
// `${outputs.spec.data.kpis}` — this test pins that shape.
func TestInterpolateOutputs_NestedFieldPaths(t *testing.T) {
	steps := map[string]json.RawMessage{
		"spec": json.RawMessage(`{"schema":"spec_envelope.v1","data":{"kpis":["click","conv"],"region":"EU"}}`),
	}
	got := interpolateOutputs(map[string]any{
		"kpis":   "${outputs.spec.data.kpis}",
		"region": "${outputs.spec.data.region}",
	}, steps).(map[string]any)

	if kpis, ok := got["kpis"].([]any); !ok || len(kpis) != 2 {
		t.Errorf("nested kpis lost shape: %#v", got["kpis"])
	}
	if got["region"] != "EU" {
		t.Errorf("nested region = %v, want EU", got["region"])
	}
}

// TestInterpolateOutputs_RecursiveStructures covers maps inside
// lists inside maps — the workflow author can nest references
// arbitrarily.
func TestInterpolateOutputs_RecursiveStructures(t *testing.T) {
	steps := map[string]json.RawMessage{
		"x": json.RawMessage(`{"items":[1,2,3]}`),
	}
	in := map[string]any{
		"outer": []any{
			map[string]any{"items": "${outputs.x.items}"},
			"literal",
		},
	}
	out := interpolateOutputs(in, steps).(map[string]any)
	outer := out["outer"].([]any)
	first := outer[0].(map[string]any)
	if items, ok := first["items"].([]any); !ok || len(items) != 3 {
		t.Errorf("nested list lost: %#v", first["items"])
	}
	if outer[1] != "literal" {
		t.Errorf("literal mutated: %v", outer[1])
	}
}

// TestInterpolateOutputs_NilInputsAreSafe guards the degraded
// configuration where either side is empty — the helper is
// called on every step entry whether or not it has anything
// to do, so a nil input must short-circuit cleanly.
func TestInterpolateOutputs_NilInputsAreSafe(t *testing.T) {
	if v := interpolateOutputs(nil, nil); v != nil {
		t.Errorf("nil input = %v, want nil", v)
	}
	// Empty steps map = every reference resolves empty.
	got := interpolateOutputs(map[string]any{"x": "${outputs.gone.y}"}, nil).(map[string]any)
	if got["x"] != "" {
		t.Errorf("empty stepResults: x = %v, want empty", got["x"])
	}
	// No-reference input passes through untouched.
	in := map[string]any{"x": "y", "n": float64(5)}
	got2 := interpolateOutputs(in, map[string]json.RawMessage{}).(map[string]any)
	if !reflect.DeepEqual(got2, in) {
		t.Errorf("no-ref input mutated: %#v vs %#v", got2, in)
	}
}
