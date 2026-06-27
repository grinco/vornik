package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"vornik.io/vornik/internal/registry"
)

// TestBuildShapeRetryHint_NamesMissingKeys — item 10 of
// https://docs.vornik.io When the validation
// failure was a "schema violation: missing required keys" error,
// the corrective hint must explicitly name the missing keys so the
// model can re-emit exactly what's needed. Pre-fix the hint only
// embedded the raw error string truncated to 400 chars and the
// model frequently failed to extract the key list from the prose.
func TestBuildShapeRetryHint_NamesMissingKeys(t *testing.T) {
	err := errors.New(`schema violation: role "writer" result.json is missing required keys: [writing produced_files]`)
	role := &registry.SwarmRole{
		Name: "writer",
		OutputSchema: &registry.OutputSchema{
			Required: []string{"writing", "produced_files", "message"},
			Properties: map[string]*registry.OutputSchema{
				"writing":        {Type: "object"},
				"produced_files": {Type: "array"},
				"message":        {Type: "string"},
			},
		},
	}

	hint := buildShapeRetryHint(err, shapeFailureJSON, nil, "", role)

	if !strings.Contains(hint, "Missing keys:") {
		t.Errorf("hint must contain 'Missing keys:' clause; got:\n%s", hint)
	}
	if !strings.Contains(hint, "writing") || !strings.Contains(hint, "produced_files") {
		t.Errorf("hint must name the missing keys; got:\n%s", hint)
	}
}

// TestBuildShapeRetryHint_IncludesRenderedSchema — when the role has
// an outputSchema, the corrective hint re-states the schema via
// RenderForPrompt so the model sees the canonical shape on retry
// instead of having to recall it from the original prompt. This
// matters because the original prompt may have been compacted/
// truncated by intermediate tool calls by the time the retry fires.
func TestBuildShapeRetryHint_IncludesRenderedSchema(t *testing.T) {
	err := errors.New(`schema violation: role "writer" result.json is missing required keys: [writing]`)
	role := &registry.SwarmRole{
		Name: "writer",
		OutputSchema: &registry.OutputSchema{
			Required: []string{"writing", "message"},
			Properties: map[string]*registry.OutputSchema{
				"writing": {Type: "object"},
				"message": {Type: "string"},
			},
		},
	}
	hint := buildShapeRetryHint(err, shapeFailureJSON, nil, "", role)

	if !strings.Contains(hint, "Required schema:") {
		t.Errorf("hint must contain 'Required schema:' clause; got:\n%s", hint)
	}
	// The schema render mentions the required keys' structure.
	if !strings.Contains(hint, "writing") {
		t.Errorf("hint must include rendered schema content; got:\n%s", hint)
	}
}

// TestBuildShapeRetryHint_FallsBackWhenNoSchema — roles using legacy
// requiredOutputKeys (no outputSchema) still get a corrective hint;
// the missing-keys clause covers them. The 'Required schema:' line
// is omitted (nothing to render).
func TestBuildShapeRetryHint_FallsBackWhenNoSchema(t *testing.T) {
	err := errors.New(`schema violation: role "legacy_role" result.json is missing required keys: [foo bar]`)
	role := &registry.SwarmRole{
		Name:               "legacy_role",
		RequiredOutputKeys: []string{"foo", "bar", "baz"},
		// No OutputSchema set — legacy path.
	}
	hint := buildShapeRetryHint(err, shapeFailureJSON, nil, "", role)

	if !strings.Contains(hint, "Missing keys:") {
		t.Errorf("legacy role still gets 'Missing keys:' clause; got:\n%s", hint)
	}
	// No schema → no rendered-schema clause.
	if strings.Contains(hint, "Required schema:") {
		t.Errorf("legacy role with no outputSchema must NOT get a rendered-schema clause; got:\n%s", hint)
	}
}

// TestBuildShapeRetryHint_PlausibilityUsesDifferentTemplate — when
// the failure was plausibility (JSON shape was fine, values were
// inconsistent), the hint uses the plausibility template instead of
// the shape template. Missing-keys / schema-render clauses are
// suppressed because the model's JSON keys were already correct.
func TestBuildShapeRetryHint_PlausibilityUsesDifferentTemplate(t *testing.T) {
	err := errors.New(`plausibility violation: role "reviewer" failed 1 rule(s): approved_needs_feedback: feedback empty when approved=false`)
	role := &registry.SwarmRole{
		Name: "reviewer",
		OutputSchema: &registry.OutputSchema{
			Required: []string{"approved", "feedback"},
			Properties: map[string]*registry.OutputSchema{
				"approved": {Type: "bool"},
				"feedback": {Type: "string"},
			},
		},
	}
	hint := buildShapeRetryHint(err, shapeFailurePlausibility, nil, "", role)

	if !strings.Contains(hint, "plausibility") {
		t.Errorf("plausibility failure must use plausibility template; got:\n%s", hint)
	}
	// Missing-keys clause is irrelevant for plausibility (the JSON keys
	// WERE present; the values just contradicted). Don't include it.
	if strings.Contains(hint, "Missing keys:") {
		t.Errorf("plausibility hint should not name missing keys; got:\n%s", hint)
	}
}

// TestBuildShapeRetryHint_PreservesPriorMessage — when there's
// substantive prior content (the model produced 2KB of approval
// reasoning before failing shape validation), the hint includes the
// prior-attempt anchor so the retry can re-format the prior work
// rather than abandoning it.
func TestBuildShapeRetryHint_PreservesPriorMessage(t *testing.T) {
	err := errors.New(`schema violation: role "risk" result.json is missing required keys: [approved]`)
	role := &registry.SwarmRole{
		Name: "risk",
		OutputSchema: &registry.OutputSchema{
			Required: []string{"approved"},
			Properties: map[string]*registry.OutputSchema{
				"approved": {Type: "array"},
			},
		},
	}
	priorMsg := "I approve AAPL qty=3 stop $254.63, MSFT qty=2 stop $365.86, NVO qty=2 stop $80.50 — all within risk caps."
	hint := buildShapeRetryHint(err, shapeFailureJSON, []byte(`{"message":`+jsonString(priorMsg)+`}`), "", role)

	if !strings.Contains(hint, "AAPL") {
		t.Errorf("hint must preserve prior substantive content; got:\n%s", hint)
	}
}

// TestBuildShapeRetryHint_AppendsRoleGuidance — the role-specific
// shapeRetryHint from swarm YAML lands at the end of the hint so
// empirical "preserve approvals on retry" guidance reaches the model
// alongside the missing-keys + rendered-schema clauses.
func TestBuildShapeRetryHint_AppendsRoleGuidance(t *testing.T) {
	err := errors.New(`schema violation: role "x" result.json is missing required keys: [y]`)
	role := &registry.SwarmRole{Name: "x"}
	hint := buildShapeRetryHint(err, shapeFailureJSON, nil, "Preserve approvals from prior attempt.", role)

	if !strings.Contains(hint, "Preserve approvals") {
		t.Errorf("hint must include role guidance; got:\n%s", hint)
	}
	if !strings.Contains(hint, "ROLE GUIDANCE") {
		t.Errorf("hint must label role guidance with ROLE GUIDANCE marker; got:\n%s", hint)
	}
}

// TestExtractMissingKeysFromError — the parser must extract the
// missing-keys slice from the error message format emitted by
// container.go ("schema violation: role %q result.json is missing
// required keys: [a b c]"). Returns empty when the error message
// doesn't match the expected format.
func TestExtractMissingKeysFromError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "single missing key",
			err:  errors.New(`schema violation: role "writer" result.json is missing required keys: [writing]`),
			want: []string{"writing"},
		},
		{
			name: "multiple missing keys",
			err:  errors.New(`schema violation: role "writer" result.json is missing required keys: [writing produced_files message]`),
			want: []string{"writing", "produced_files", "message"},
		},
		{
			name: "dotted keys",
			err:  errors.New(`schema violation: role "researcher" result.json is missing required keys: [research.written research.sources]`),
			want: []string{"research.written", "research.sources"},
		},
		{
			name: "nil error",
			err:  nil,
			want: nil,
		},
		{
			name: "non-matching error",
			err:  errors.New(`plausibility violation: role "x" failed 1 rule(s)`),
			want: nil,
		},
		{
			name: "empty key list",
			err:  errors.New(`schema violation: role "x" result.json is missing required keys: []`),
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractMissingKeysFromError(tc.err)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d keys (%v), want %d (%v)", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestRecordShapeRetryOutcome_IncrementsByOutcome — the new
// outcome-labelled counter (item 10 visibility ask). One increment
// per retry decision-point so operators can read attempted /
// recovered / failed ratios by role without joining two counters.
func TestRecordShapeRetryOutcome_IncrementsByOutcome(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.RecordShapeRetryOutcome("writer", "attempted")
	m.RecordShapeRetryOutcome("writer", "recovered")
	m.RecordShapeRetryOutcome("writer", "attempted")
	m.RecordShapeRetryOutcome("writer", "failed")

	if got := testutil.ToFloat64(m.ShapeRetryByOutcomeTotal.WithLabelValues("writer", "attempted")); got != 2 {
		t.Errorf("attempted = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.ShapeRetryByOutcomeTotal.WithLabelValues("writer", "recovered")); got != 1 {
		t.Errorf("recovered = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ShapeRetryByOutcomeTotal.WithLabelValues("writer", "failed")); got != 1 {
		t.Errorf("failed = %v, want 1", got)
	}
}

// TestRecordShapeRetryOutcome_NilSafe — defensive: the counter
// helper must no-op cleanly when the Metrics struct is nil (some
// tests construct executors without a metrics registry) and when
// role/outcome are empty (no useful telemetry to record).
func TestRecordShapeRetryOutcome_NilSafe(t *testing.T) {
	var m *Metrics
	m.RecordShapeRetryOutcome("writer", "attempted") // must not panic

	reg := prometheus.NewRegistry()
	m = NewMetrics(reg)
	m.RecordShapeRetryOutcome("", "attempted") // empty role → skip
	m.RecordShapeRetryOutcome("writer", "")    // empty outcome → skip

	if got := testutil.ToFloat64(m.ShapeRetryByOutcomeTotal.WithLabelValues("writer", "attempted")); got != 0 {
		t.Errorf("expected no increment for empty-label call, got %v", got)
	}
}
