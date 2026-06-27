package executor

import "testing"

// TestValidateEnvelopeShape covers the Phase D envelope-shape
// contract (LLD §4.2). The validator runs in the resolve hook
// before MarkCompleted writes the envelope back; a malformed
// shape resolves the CPC as rejected so the caller's on_fail
// branch fires.
//
// Phase D ships shape-only validation (schema+status required,
// schema must match expected). Full JSON-Schema validation
// against schema_registry rows is deferred to a follow-on.
func TestValidateEnvelopeShape(t *testing.T) {
	cases := []struct {
		name       string
		envelope   map[string]any
		expected   string
		wantReason string // empty = accept
	}{
		{
			name:       "happy_path",
			envelope:   map[string]any{"schema": "spec_envelope.v1", "status": "ok", "data": map[string]any{}},
			expected:   "spec_envelope.v1",
			wantReason: "",
		},
		{
			name:       "missing_schema",
			envelope:   map[string]any{"status": "ok"},
			expected:   "spec_envelope.v1",
			wantReason: "envelope missing required field: schema",
		},
		{
			name:       "wrong_schema",
			envelope:   map[string]any{"schema": "assets_envelope.v1", "status": "ok"},
			expected:   "spec_envelope.v1",
			wantReason: "envelope.schema = assets_envelope.v1, caller expected spec_envelope.v1",
		},
		{
			name:       "missing_status",
			envelope:   map[string]any{"schema": "spec_envelope.v1"},
			expected:   "spec_envelope.v1",
			wantReason: "envelope missing required field: status",
		},
		{
			name:       "schema_not_string",
			envelope:   map[string]any{"schema": 42, "status": "ok"},
			expected:   "spec_envelope.v1",
			wantReason: "envelope.schema must be a string",
		},
		{
			name:       "nil_envelope",
			envelope:   nil,
			expected:   "spec_envelope.v1",
			wantReason: "envelope is not a JSON object",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := validateEnvelopeShape(c.envelope, c.expected)
			if got != c.wantReason {
				t.Errorf("validateEnvelopeShape = %q, want %q", got, c.wantReason)
			}
		})
	}
}
