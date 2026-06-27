package registry

import (
	"strings"
	"testing"
)

// TestRenderForPrompt_UsesJSONSkeletonNotDotPaths — the regression
// guard for the schema-render bug observed 2026-05-08.
//
// Pre-fix RenderForPrompt produced a bullet list with dot-notation
// paths:
//
//	Required top-level keys:
//	  - research (object)
//	    - research.written (bool, required)
//	  - produced_files (array)
//
// Some models (especially open-weight ones via Bedrock) trained on
// that notation by emitting flat keys: {"research.written": true,
// "produced_files": [...]}. The validator's flat-key fast path then
// passed the "research.written:bool" check (literal key match) but
// failed the "research:object" check (no `research` key existed),
// producing the operator-visible
//
//	"missing required keys: [research:object research.written:bool produced_files:array]"
//
// failure mode the user reported.
//
// The new render emits a JSON skeleton showing the ACTUAL nested
// structure: models see `{"research": {"written": <bool>, ...}}`
// directly, matching the format they're trained on (millions of JSON
// examples in pretraining vs. vornik's bespoke dot-path notation).
//
// The render MUST NOT contain dot-notation paths anywhere — the
// dot is the literal trigger for the flat-key failure mode.
func TestRenderForPrompt_UsesJSONSkeletonNotDotPaths(t *testing.T) {
	schema := &OutputSchema{
		Type:     "object",
		Required: []string{"research", "produced_files"},
		Properties: map[string]*OutputSchema{
			"research": {
				Type:     "object",
				Required: []string{"written"},
				Properties: map[string]*OutputSchema{
					"written": {Type: "bool"},
					"sources": {Type: "array"},
				},
			},
			"produced_files": {Type: "array"},
		},
	}
	got := schema.RenderForPrompt()

	// Must contain the JSON skeleton header, not the legacy bullet header.
	if !strings.Contains(got, "Your response must match this JSON shape:") {
		t.Errorf("missing JSON skeleton header; got:\n%s", got)
	}
	if strings.Contains(got, "Required top-level keys:") {
		t.Errorf("legacy bullet-list header re-introduced; got:\n%s", got)
	}

	// Must contain the nested object structure verbatim.
	if !strings.Contains(got, `"research": {`) {
		t.Errorf("nested object structure missing; got:\n%s", got)
	}
	if !strings.Contains(got, `"written": <bool>`) {
		t.Errorf("nested type placeholder missing; got:\n%s", got)
	}

	// Must NOT contain dot-notation paths — these are the
	// flat-key training signal.
	if strings.Contains(got, "research.written") {
		t.Errorf("dot-notation path leaked into prompt — this is the bug. got:\n%s", got)
	}
	if strings.Contains(got, "research.sources") {
		t.Errorf("dot-notation path leaked into prompt; got:\n%s", got)
	}
}

// TestRenderForPrompt_RequiredMarkerOnEachField — the skeleton
// shows /* required */ next to keys whose schema is in the
// parent's required[] list. Pin this so a refactor doesn't lose
// the per-field signal (without it, the model can't tell which
// keys are mandatory).
func TestRenderForPrompt_RequiredMarkerOnEachField(t *testing.T) {
	schema := &OutputSchema{
		Type:     "object",
		Required: []string{"research"},
		Properties: map[string]*OutputSchema{
			"research": {
				Type:     "object",
				Required: []string{"written"},
				Properties: map[string]*OutputSchema{
					"written": {Type: "bool"},
					"sources": {Type: "array"}, // optional
				},
			},
		},
	}
	got := schema.RenderForPrompt()

	if !strings.Contains(got, "/* required */") {
		t.Errorf("required marker missing; got:\n%s", got)
	}
	// The optional `sources` field must NOT have the required marker
	// — count occurrences. Top-level research is required (1), nested
	// written is required (1). Total 2. If sources got a required
	// marker by mistake, count would be 3.
	count := strings.Count(got, "/* required */")
	if count != 2 {
		t.Errorf("expected exactly 2 /* required */ markers (research + research.written), got %d in:\n%s", count, got)
	}
}

// TestRenderForPrompt_NestedDeterministicOrder — required keys are
// rendered in the operator-declared order (matches Required slice
// order); optional keys are rendered alphabetically after them.
// Across two structurally-identical schemas the output must be
// byte-identical for the prompt cache to land consistently.
func TestRenderForPrompt_NestedDeterministicOrder(t *testing.T) {
	mk := func() *OutputSchema {
		return &OutputSchema{
			Type:     "object",
			Required: []string{"alpha", "beta"},
			Properties: map[string]*OutputSchema{
				"alpha":   {Type: "string"},
				"beta":    {Type: "number"},
				"zeta":    {Type: "string"}, // optional, alphabetically last
				"charlie": {Type: "string"}, // optional, between alpha/beta if alphabetical
			},
		}
	}
	a := mk().RenderForPrompt()
	b := mk().RenderForPrompt()
	if a != b {
		t.Errorf("non-deterministic render output:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
	// Required-first ordering: alpha and beta must come BEFORE
	// charlie and zeta.
	alphaIdx := strings.Index(a, `"alpha"`)
	betaIdx := strings.Index(a, `"beta"`)
	charlieIdx := strings.Index(a, `"charlie"`)
	zetaIdx := strings.Index(a, `"zeta"`)
	if alphaIdx < 0 || betaIdx < 0 || charlieIdx < 0 || zetaIdx < 0 {
		t.Fatalf("missing keys in render: %s", a)
	}
	if alphaIdx > charlieIdx || betaIdx > charlieIdx {
		t.Errorf("required keys (alpha, beta) must render before optional charlie; got order alpha=%d beta=%d charlie=%d", alphaIdx, betaIdx, charlieIdx)
	}
	if charlieIdx > zetaIdx {
		t.Errorf("optional keys must render alphabetically; got charlie=%d zeta=%d", charlieIdx, zetaIdx)
	}
}
