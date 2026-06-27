package executor

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests target deterministic, pure helpers in package executor
// that the slow integration-flavoured suite exercises only
// incidentally (intField at 40%, scalarStringForm/resolveStepField at
// ~69%, extractRepoScopeFromPayload + truncateWithMarker with no direct
// test, plus a handful of ParseLeadOutcome error branches). Each test
// asserts real behaviour and an edge the existing tests miss — no
// padding. Prefix: TestPureHelper / TestPureLeadOutcome.

// --- intField (plan.go) -----------------------------------------------

// TestPureHelper_IntField_AllNumericShapes pins the type-tolerance
// contract: the dispatcher persists payloads as raw JSON (numbers
// arrive as float64 after a generic unmarshal) but in-process callers
// may hand it native int/int64. All three must coerce; anything else —
// including a numeric STRING, which json does NOT widen — falls to 0.
func TestPureHelper_IntField_AllNumericShapes(t *testing.T) {
	m := map[string]any{
		"native_int": 7,
		"int64_val":  int64(9),
		"float_json": float64(42), // json.Unmarshal default for "42"
		"truncates":  float64(3.9),
		"as_string":  "5", // NOT coerced — string is not numeric here
		"as_bool":    true,
		"as_nil":     nil,
	}
	assert.Equal(t, 7, intField(m, "native_int"))
	assert.Equal(t, 9, intField(m, "int64_val"))
	assert.Equal(t, 42, intField(m, "float_json"))
	// float→int is a Go conversion: truncation toward zero, no rounding.
	assert.Equal(t, 3, intField(m, "truncates"), "float must truncate not round")
	assert.Equal(t, 0, intField(m, "as_string"), "numeric string is not coerced")
	assert.Equal(t, 0, intField(m, "as_bool"))
	assert.Equal(t, 0, intField(m, "as_nil"))
	assert.Equal(t, 0, intField(m, "absent_key"), "missing key returns zero value")
}

// TestPureHelper_IntField_NegativeAndZero confirms negatives and exact
// zero survive the float path (no abs / no clamp).
func TestPureHelper_IntField_NegativeAndZero(t *testing.T) {
	m := map[string]any{"neg": float64(-12), "zero": float64(0)}
	assert.Equal(t, -12, intField(m, "neg"))
	assert.Equal(t, 0, intField(m, "zero"))
}

// --- scalarStringForm (outputs_interpolate.go) ------------------------

// TestPureHelper_ScalarStringForm_TypeRendering walks every branch of
// the scalar renderer used for EMBEDDED ${outputs.x.y} substitution.
// The float branch is the load-bearing one: integers-as-float64 (the
// json default) must render "5" not "5.000000", which is exactly why
// the production code routes through json.Marshal rather than fmt.
func TestPureHelper_ScalarStringForm_TypeRendering(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, ""},
		{"string_passthrough", "hello", "hello"},
		{"empty_string", "", ""},
		{"bool_true", true, "true"},
		{"bool_false", false, "false"},
		{"int_valued_float", float64(5), "5"},
		{"fractional_float", float64(9.99), "9.99"},
		{"negative_float", float64(-3), "-3"},
		{"slice_marshals", []any{"a", "b"}, `["a","b"]`},
		{"map_marshals", map[string]any{"k": "v"}, `{"k":"v"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, scalarStringForm(c.in))
		})
	}
}

// --- resolveStepField (outputs_interpolate.go) ------------------------

// TestPureHelper_ResolveStepField_NilPaths covers the four nil-return
// branches the embedded/exact-match tests don't isolate: absent step,
// empty raw body, unparseable JSON, and a path segment that descends
// into a NON-object (scalar). Returning nil (vs erroring) is what lets
// a broken reference render as an empty string downstream.
func TestPureHelper_ResolveStepField_NilPaths(t *testing.T) {
	steps := map[string]json.RawMessage{
		"present":    json.RawMessage(`{"a":{"b":"deep"}}`),
		"empty":      json.RawMessage(``),
		"garbage":    json.RawMessage(`not json at all`),
		"scalar_a":   json.RawMessage(`{"a":"i-am-a-string"}`),
		"toplvl_arr": json.RawMessage(`["x","y"]`),
	}
	assert.Nil(t, resolveStepField(steps, "missing", "a"), "absent step")
	assert.Nil(t, resolveStepField(steps, "empty", "a"), "empty body")
	assert.Nil(t, resolveStepField(steps, "garbage", "a"), "unparseable body")
	// Descending PAST a scalar: a.b where a is a string, not a map.
	assert.Nil(t, resolveStepField(steps, "scalar_a", "a.b"),
		"cannot descend into a scalar segment")
	// Top-level document is an array, not an object → first segment misses.
	assert.Nil(t, resolveStepField(steps, "toplvl_arr", "a"),
		"non-object root yields nil")
	// Sanity: the deep happy-path still resolves so the nil cases above
	// are genuinely the failure branches and not a broken fixture.
	assert.Equal(t, "deep", resolveStepField(steps, "present", "a.b"))
}

// --- extractRepoScopeFromPayload (workflow.go) ------------------------

// TestPureHelper_ExtractRepoScope covers all four return paths:
// empty/invalid payload → "", context.repo_scope taking precedence over
// top-level, top-level fallback, and whitespace trimming on both. The
// precedence rule (nested context wins) is the non-obvious behaviour.
func TestPureHelper_ExtractRepoScope(t *testing.T) {
	assert.Equal(t, "", extractRepoScopeFromPayload(nil), "nil payload")
	assert.Equal(t, "", extractRepoScopeFromPayload([]byte{}), "empty payload")
	assert.Equal(t, "", extractRepoScopeFromPayload([]byte(`{not json`)), "malformed JSON")
	assert.Equal(t, "", extractRepoScopeFromPayload([]byte(`{}`)), "no repo_scope anywhere")

	// Top-level only.
	assert.Equal(t, "owner/repo",
		extractRepoScopeFromPayload([]byte(`{"repo_scope":"owner/repo"}`)))

	// context.repo_scope wins over top-level when both present.
	both := []byte(`{"repo_scope":"top/level","context":{"repo_scope":"nested/wins"}}`)
	assert.Equal(t, "nested/wins", extractRepoScopeFromPayload(both),
		"nested context.repo_scope must take precedence")

	// Whitespace is trimmed; a blank nested value falls through to top-level.
	assert.Equal(t, "owner/repo",
		extractRepoScopeFromPayload([]byte(`{"repo_scope":"  owner/repo  ","context":{"repo_scope":"   "}}`)),
		"blank nested value falls back to trimmed top-level")
}

// --- truncateWithMarker (canonical_context.go) ------------------------

// TestPureHelper_TruncateWithMarker covers the under-limit passthrough,
// the exact-boundary passthrough (<= is inclusive), and the truncated
// path whose marker must report the EXACT dropped-byte count. Byte
// length, not rune length, drives the cut.
func TestPureHelper_TruncateWithMarker(t *testing.T) {
	// Under and at the limit: returned verbatim, no marker.
	assert.Equal(t, "abc", truncateWithMarker([]byte("abc"), 10))
	assert.Equal(t, "abcde", truncateWithMarker([]byte("abcde"), 5),
		"len == maxBytes is inclusive (<=), no truncation")
	assert.Equal(t, "", truncateWithMarker([]byte(""), 0),
		"empty input under zero limit returns empty, no marker")

	// Over the limit: keep first maxBytes bytes + a marker stating the
	// dropped count (here 10 - 4 = 6).
	got := truncateWithMarker([]byte("0123456789"), 4)
	assert.True(t, strings.HasPrefix(got, "0123"), "keeps first maxBytes bytes")
	assert.Contains(t, got, fmt.Sprintf("truncated %d bytes", 6),
		"marker reports exact dropped byte count")

	// maxBytes 0 on non-empty input drops everything.
	allDropped := truncateWithMarker([]byte("xyz"), 0)
	assert.True(t, strings.HasPrefix(allDropped, "\n\n...[truncated 3 bytes]"),
		"zero limit drops all content but keeps marker")
}

// --- isTranscriptArtifact / IsTranscriptArtifact (artifacts.go) -------

// TestPureHelper_IsTranscriptArtifact pins the disambiguation-aware
// transcript classifier and its exported twin. The 2026-05-15 incident
// (issue: 79 transcript chunks leaked into RAG) hinges on the
// disambiguated form `route-response-20260515-0f96.md` still matching;
// guard both that and the negatives.
func TestPureHelper_IsTranscriptArtifact(t *testing.T) {
	// Plain convention.
	assert.True(t, isTranscriptArtifact("route-response.md"))
	assert.True(t, isTranscriptArtifact("write-response.md"))
	// Disambiguated form — the 2026-05-15 regression case.
	assert.True(t, isTranscriptArtifact("route-response-20260515-0f96.md"),
		"disambiguated transcript must still classify")
	// Negatives: real project artifacts, not transcripts.
	assert.False(t, isTranscriptArtifact("CHANGES.md"))
	assert.False(t, isTranscriptArtifact("plan.md"))
	assert.False(t, isTranscriptArtifact("response.md"), "needs the <step>- prefix")
	// Exported wrapper must agree byte-for-byte with the private form.
	for _, n := range []string{"route-response.md", "route-response-20260515-0f96.md", "CHANGES.md", "plan.md"} {
		assert.Equal(t, isTranscriptArtifact(n), IsTranscriptArtifact(n),
			"exported wrapper must mirror private impl for %q", n)
	}
}

// --- splitArtifactStem (artifacts.go) ---------------------------------

// TestPureHelper_SplitArtifactStem locks the documented contracts:
// last-dot-wins, leading-dot files are extension-less, no-dot files are
// extension-less, and the stem+ext == name invariant holds for all.
func TestPureHelper_SplitArtifactStem(t *testing.T) {
	cases := []struct {
		name     string
		wantStem string
		wantExt  string
	}{
		{"report.md", "report", ".md"},
		{"report.tar.gz", "report.tar", ".gz"}, // last dot wins
		{"CHANGELOG", "CHANGELOG", ""},         // no dot
		{".env", ".env", ""},                   // leading-dot only
		{".dockerignore", ".dockerignore", ""},
		{"a.b.c.d", "a.b.c", ".d"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stem, ext := splitArtifactStem(c.name)
			assert.Equal(t, c.wantStem, stem)
			assert.Equal(t, c.wantExt, ext)
			// Documented idempotent contract.
			assert.Equal(t, c.name, stem+ext, "stem+ext must reconstruct name")
		})
	}
}

// --- stripDisambiguationSuffix (artifacts.go) -------------------------

// TestPureHelper_StripDisambiguationSuffix asserts the inverse of
// disambiguateArtifactName and its safety guards: a date segment that
// is NOT at the end (anchored regex) must not be stripped, and names
// from pre-disambiguation builds pass through unchanged.
func TestPureHelper_StripDisambiguationSuffix(t *testing.T) {
	// Round-trip: the suffix disambiguateArtifactName appends is removed.
	assert.Equal(t, "route-response.md",
		stripDisambiguationSuffix("route-response-20260515-0f96.md"))
	// Extension preserved, hex case-insensitive.
	assert.Equal(t, "report.md",
		stripDisambiguationSuffix("report-20260101-ABCD.md"))
	// Date segment NOT at the end → not stripped (anchored to stem end).
	assert.Equal(t, "request-20260516-cycle.md",
		stripDisambiguationSuffix("request-20260516-cycle.md"),
		"only the trailing disambig segment matches")
	// Pre-disambiguation names pass through.
	assert.Equal(t, "plain.md", stripDisambiguationSuffix("plain.md"))
	assert.Equal(t, "CHANGELOG", stripDisambiguationSuffix("CHANGELOG"))
	// Wrong-length short code (3 hex, not 4) → not a match.
	assert.Equal(t, "x-20260101-abc.md",
		stripDisambiguationSuffix("x-20260101-abc.md"),
		"short code must be exactly 4 hex chars")
}

// --- ParseLeadOutcome error branches (lead_outcome.go) ----------------

// TestPureLeadOutcome_EmptyAndMalformed covers the two earliest error
// returns the happy-path-heavy existing suite skips: empty input and
// non-JSON input. Both must error and return ok=false / nil outcome.
func TestPureLeadOutcome_EmptyAndMalformed(t *testing.T) {
	out, ok, err := ParseLeadOutcome(nil)
	assert.Nil(t, out)
	assert.False(t, ok)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty lead output")

	out, ok, err = ParseLeadOutcome([]byte(`{"outcome": continue`)) // truncated/invalid
	assert.Nil(t, out)
	assert.False(t, ok)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid lead JSON")
}

// TestPureLeadOutcome_OutcomeCaseInsensitive pins the documented
// lower-casing of the outcome discriminator: "CHECKPOINT" / "Continue"
// must be accepted as their canonical kinds, not rejected as unknown.
func TestPureLeadOutcome_OutcomeCaseInsensitive(t *testing.T) {
	in := []byte(`{"outcome":"CONTINUE","plan":{"steps":["research"]}}`)
	out, ok, err := ParseLeadOutcome(in)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, LeadOutcomeContinue, out.Outcome)
	// Version defaults to 1 when the envelope omits it.
	assert.Equal(t, 1, out.Version, "absent version defaults to 1")
}

// TestPureLeadOutcome_ReviewCheckpoint exercises the review checkpoint
// kind (its own validateLeadOutcome branch) plus its missing-draft
// failure — neither is covered by the existing decision/action_required
// cases.
func TestPureLeadOutcome_ReviewCheckpoint(t *testing.T) {
	ok := []byte(`{"outcome":"checkpoint","checkpoint":{"kind":"review","draft":"please review this draft"}}`)
	out, parsed, err := ParseLeadOutcome(ok)
	require.NoError(t, err)
	require.True(t, parsed)
	require.NotNil(t, out.Checkpoint)
	assert.Equal(t, CheckpointKindReview, out.Checkpoint.Kind)

	missing := []byte(`{"outcome":"checkpoint","checkpoint":{"kind":"review"}}`)
	_, _, err = ParseLeadOutcome(missing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "draft")
}

// TestPureLeadOutcome_UnknownCheckpointKind covers the default arm of
// the checkpoint.kind switch — a typo'd or omitted kind must fail the
// step rather than silently produce a malformed checkpoint.
func TestPureLeadOutcome_UnknownCheckpointKind(t *testing.T) {
	in := []byte(`{"outcome":"checkpoint","checkpoint":{"kind":"approval","question":"go?"}}`)
	_, _, err := ParseLeadOutcome(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown checkpoint.kind")

	// Empty kind also lands in the default arm.
	empty := []byte(`{"outcome":"checkpoint","checkpoint":{"question":"go?"}}`)
	_, _, err = ParseLeadOutcome(empty)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown checkpoint.kind")
}
