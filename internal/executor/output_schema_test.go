package executor

import (
	"slices"
	"testing"
)

// TestValidateRequiredOutputKeys_BackwardCompatBareKeys — existing
// role yamls that list bare top-level keys ("plan", "research")
// must keep working unchanged. This is the regression guard for the
// rung-3a schema-syntax extension.
func TestValidateRequiredOutputKeys_BackwardCompatBareKeys(t *testing.T) {
	body := []byte(`{"plan":[],"extra":1}`)
	if got := validateRequiredOutputKeys(body, []string{"plan"}); len(got) != 0 {
		t.Errorf("bare key present: expected no missing, got %v", got)
	}
	if got := validateRequiredOutputKeys(body, []string{"missing"}); !slices.Equal(got, []string{"missing"}) {
		t.Errorf("bare key absent: expected [missing], got %v", got)
	}
}

// TestValidateRequiredOutputKeys_NestedPath — dot-paths walk into
// child objects. This is the headline new capability.
func TestValidateRequiredOutputKeys_NestedPath(t *testing.T) {
	body := []byte(`{"review":{"approved":true,"summary":"ok"}}`)
	cases := []struct {
		entries []string
		missing []string
	}{
		{[]string{"review.approved"}, nil},
		{[]string{"review.summary"}, nil},
		{[]string{"review.feedback"}, []string{"review.feedback"}},
		{[]string{"review.approved", "review.feedback"}, []string{"review.feedback"}},
	}
	for _, tc := range cases {
		got := validateRequiredOutputKeys(body, tc.entries)
		if !slices.Equal(got, tc.missing) {
			t.Errorf("entries=%v: got %v, want %v", tc.entries, got, tc.missing)
		}
	}
}

// TestValidateRequiredOutputKeys_FlatKeyForm — the validator must
// accept dot-paths against payloads whose producer emitted the whole
// dotted name as a single top-level key. This is the flat-key form
// the gate evaluator and plausibility checker both accept; without
// the consolidation through lookupJSONPath, the validator would
// reject what the gate accepts and the executor pipeline would
// disagree with itself. Stability item 2 of the post-2026.5.3
// roadmap (path-walking pattern audit).
func TestValidateRequiredOutputKeys_FlatKeyForm(t *testing.T) {
	body := []byte(`{"review.approved":true,"review.summary":"ok"}`)
	cases := []struct {
		entries []string
		missing []string
	}{
		{[]string{"review.approved"}, nil},
		{[]string{"review.summary"}, nil},
		{[]string{"review.feedback"}, []string{"review.feedback"}},
		// Mixed: type assertion still works against flat-key form.
		{[]string{"review.approved:bool"}, nil},
	}
	for _, tc := range cases {
		got := validateRequiredOutputKeys(body, tc.entries)
		if !slices.Equal(got, tc.missing) {
			t.Errorf("flat-key form entries=%v: got %v, want %v", tc.entries, got, tc.missing)
		}
	}
}

// TestValidateRequiredOutputKeys_TypeAssertion — "path:type" rejects
// values whose JSON type doesn't match. Catches the "review.approved
// is the string \"true\"" failure mode where the model emitted a
// stringified bool.
func TestValidateRequiredOutputKeys_TypeAssertion(t *testing.T) {
	body := []byte(`{
		"review": {
			"approved": true,
			"summary": "ok",
			"checked_commit": "abc1234",
			"files_changed": 3,
			"tags": ["a","b"]
		}
	}`)
	cases := []struct {
		entry string
		ok    bool
	}{
		{"review:object", true},
		{"review.approved:bool", true},
		{"review.summary:string", true},
		{"review.files_changed:number", true},
		{"review.tags:array", true},
		// Wrong-type cases — must be reported as missing.
		{"review.approved:string", false},
		{"review.summary:bool", false},
		{"review.tags:object", false},
	}
	for _, tc := range cases {
		got := validateRequiredOutputKeys(body, []string{tc.entry})
		if tc.ok && len(got) != 0 {
			t.Errorf("%q: expected pass, got missing %v", tc.entry, got)
		}
		if !tc.ok && len(got) == 0 {
			t.Errorf("%q: expected type-mismatch failure, got pass", tc.entry)
		}
	}
}

// TestValidateRequiredOutputKeys_StringifiedBoolCaught — the
// motivating case for type assertions. An LLM that emits
// "approved": "true" (string) instead of true (bool) was previously
// indistinguishable from the success case at the schema layer.
func TestValidateRequiredOutputKeys_StringifiedBoolCaught(t *testing.T) {
	body := []byte(`{"review":{"approved":"true"}}`)
	got := validateRequiredOutputKeys(body, []string{"review.approved:bool"})
	if len(got) != 1 {
		t.Fatalf("expected stringified bool to fail, got %v", got)
	}
}

// TestValidateRequiredOutputKeys_NonObjectBodyFails — the existing
// guarantee: a body that doesn't unmarshal into an object reports
// every required entry as missing.
func TestValidateRequiredOutputKeys_NonObjectBodyFails(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`"a string"`),
		[]byte(`[1,2,3]`),
		[]byte(`null`),
		[]byte(`not even json`),
	} {
		got := validateRequiredOutputKeys(body, []string{"plan", "scout.context_written:bool"})
		if len(got) != 2 {
			t.Errorf("body %q: expected all entries missing, got %v", body, got)
		}
	}
}

// TestValidateRequiredOutputKeys_EmptyArgsSkip — the contract: empty
// body or empty required list returns nil so callers don't have to
// guard themselves.
func TestValidateRequiredOutputKeys_EmptyArgsSkip(t *testing.T) {
	if got := validateRequiredOutputKeys(nil, []string{"x"}); got != nil {
		t.Errorf("nil body: expected nil, got %v", got)
	}
	if got := validateRequiredOutputKeys([]byte(`{"x":1}`), nil); got != nil {
		t.Errorf("empty required: expected nil, got %v", got)
	}
}

// TestParseSchemaEntry — the path/type splitter. Multiple colons
// (path "a:b:c"+type "x") must take the LAST colon as separator so
// future use of namespaced fields doesn't break.
func TestParseSchemaEntry(t *testing.T) {
	cases := []struct {
		in   string
		path string
		typ  string
	}{
		{"plan", "plan", ""},
		{"review.approved", "review.approved", ""},
		{"review.approved:bool", "review.approved", "bool"},
		{"a.b.c:string", "a.b.c", "string"},
		{"  spaced  :  bool  ", "spaced", "bool"},
		{"", "", ""},
	}
	for _, tc := range cases {
		path, typ := parseSchemaEntry(tc.in)
		if path != tc.path || typ != tc.typ {
			t.Errorf("%q: got (%q, %q), want (%q, %q)", tc.in, path, typ, tc.path, tc.typ)
		}
	}
}

// TestValueMatchesType_NumbersAcceptIntAndFloat — JSON has one
// number type; both int and float decoded values must satisfy the
// "number" assertion. Operator-side schemas don't need to know
// whether the LLM emitted 5 or 5.0.
func TestValueMatchesType_NumbersAcceptIntAndFloat(t *testing.T) {
	for _, v := range []any{float64(5), int(5), int64(5), float32(5)} {
		if !valueMatchesType(v, "number") {
			t.Errorf("number: rejected %T", v)
		}
	}
	if !valueMatchesType(true, "boolean") {
		t.Error("boolean alias rejected true")
	}
	if !valueMatchesType("x", "string") {
		t.Error("string rejected")
	}
}

// TestExtractLastJSONObject — balanced-brace scanner handles plain
// objects, prose-wrapped objects, multi-object spans, and brace-in-
// string distractors. The motivating case is the agent harness
// pass-3 extraction bug: when the model emits
// `<think>{scratch}</think>{final}`, the greedy `grep -o '{.*}'`
// captures the whole span (including the closing think tag) and
// fails jq's type check, leaving result.json without the model's
// keys at top level.
func TestExtractLastJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain object", `{"a":1}`, `{"a":1}`},
		{"prose then object", `model thinks: {"a":1}`, `{"a":1}`},
		{"multi object — last wins", `<think>{"scratch":true}</think>{"final":42}`, `{"final":42}`},
		{"nested braces", `{"a":{"b":{"c":1}}}`, `{"a":{"b":{"c":1}}}`},
		{"brace inside string", `prefix {"s":"a } b"} suffix`, `{"s":"a } b"}`},
		{"escaped quote in string", `{"q":"a\"}b"}`, `{"q":"a\"}b"}`},
		{"empty input", ``, ``},
		{"no object", `just words`, ``},
		{"unbalanced — no match", `{"a":1`, ``},
	}
	for _, tc := range cases {
		got := extractLastJSONObject([]byte(tc.in))
		if string(got) != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, string(got), tc.want)
		}
	}
}

// TestNormalizedResultPayload_HoistsMessageJSON — when the agent
// harness wraps the model's structured output as a JSON-encoded
// string in `message` (because its pass-3 extraction failed to
// merge), validate that the validator still finds the model's keys.
// This is the deterministic safety net for the
// exec_20260507164414_31fc381f97738812 regression class.
func TestNormalizedResultPayload_HoistsMessageJSON(t *testing.T) {
	// Envelope where the agent merge step silently dropped the
	// model's JSON; `approved` is buried inside `message` as a
	// stringified JSON object.
	envelope := []byte(`{
		"status": "COMPLETED",
		"message": "Some prose. <think>{\"scratch\": true}</think> Final: {\"approved\": [{\"symbol\":\"AAPL\"}], \"rejected\": [], \"has_approvals\": true}",
		"toolAudit": []
	}`)
	got := validateRequiredOutputKeys(envelope, []string{"approved", "rejected", "has_approvals:bool"})
	if len(got) != 0 {
		t.Errorf("expected no missing keys after message-hoist, got %v", got)
	}
}

// TestNormalizedResultPayload_EnvelopeKeysWinOnCollision — when
// both the envelope and the inner message JSON define the same
// key, the envelope wins. This protects audit/secret-scan fields
// (toolAudit, status) from being silently overwritten by a model
// that decided to emit those names too.
func TestNormalizedResultPayload_EnvelopeKeysWinOnCollision(t *testing.T) {
	envelope := []byte(`{
		"status": "COMPLETED",
		"message": "{\"status\": \"FAILED\", \"approved\": []}"
	}`)
	parsed, err := normalizedResultPayload(envelope)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if parsed["status"] != "COMPLETED" {
		t.Errorf("envelope status overwritten by message — got %v, want COMPLETED", parsed["status"])
	}
	if _, ok := parsed["approved"]; !ok {
		t.Errorf("non-conflicting message keys must still be hoisted")
	}
}

// TestNormalizedResultPayload_RawProseFallback — when even the
// envelope doesn't parse (no `{...}` wrapper at all), the trailing
// JSON object should still be recovered. This is the second layer
// of recovery, for the rare case where the agent harness writes
// raw model output without enveloping it.
func TestNormalizedResultPayload_RawProseFallback(t *testing.T) {
	body := []byte(`prose without envelope. final: {"plan":["a","b"]}`)
	got := validateRequiredOutputKeys(body, []string{"plan"})
	if len(got) != 0 {
		t.Errorf("expected plan recovered from raw prose, got missing %v", got)
	}
}
