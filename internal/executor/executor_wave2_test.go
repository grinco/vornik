package executor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/verifier"
)

// Wave-2 second pass: HIGH-VALUE tests for pure/deterministic helpers in
// internal/executor that the first pass (pure_helpers_coverage_test.go +
// tool_budget_*_test.go) left under-covered. No tool-budget / lead-outcome /
// intField / resolveStepField duplication. Tests target failure/outcome
// classification, gate-condition evaluation, recovery routing, artifact-flow
// interpolation, retryable-error marking, prompt/context assembly, and
// env/state helpers.

// --- valueMatchesType: the JSON-type predicate the schema validator uses ---

func TestW2ExecValueMatchesType_AllBranches(t *testing.T) {
	cases := []struct {
		name string
		v    any
		typ  string
		want bool
	}{
		{"string ok", "x", "string", true},
		{"string mismatch", 1.0, "string", false},
		{"bool ok", true, "bool", true},
		{"boolean alias ok", false, "boolean", true},
		{"bool mismatch", "true", "bool", false},
		{"number float64", 3.14, "number", true},
		{"number int", 7, "integer", true},
		{"number int64", int64(9), "int", true},
		{"number int32", int32(2), "float", true},
		{"number float32", float32(1.5), "number", true},
		{"number mismatch", "5", "number", false},
		{"array ok", []any{1, 2}, "array", true},
		{"array mismatch", map[string]any{}, "array", false},
		{"object ok", map[string]any{"k": 1}, "object", true},
		{"object mismatch", []any{}, "object", false},
		{"any non-nil", "anything", "any", true},
		{"any nil", nil, "any", false},
		{"empty-type non-nil", 0.0, "", true},
		{"empty-type nil", nil, "", false},
		// Unknown type names are permissive (a typo in role yaml must
		// not fail the whole step) — this is the load-bearing default.
		{"unknown type permissive", "x", "stringy", true},
		{"unknown type permissive nil", nil, "widget", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := valueMatchesType(tc.v, tc.typ); got != tc.want {
				t.Fatalf("valueMatchesType(%v, %q) = %v, want %v", tc.v, tc.typ, got, tc.want)
			}
		})
	}
}

// --- gate-condition evaluation: routing the workflow off the producer JSON ---

func TestW2ExecEvaluateSingleCondition_Types(t *testing.T) {
	payload := map[string]any{
		"review":   map[string]any{"approved": true, "score": float64(9)},
		"name":     "alice",
		"done":     false,
		"flat.key": "v", // pre-flattened key the LLM sometimes emits
	}
	cases := []struct {
		name      string
		condition string
		want      bool
		wantErr   bool
	}{
		{"nested bool true", "review.approved == true", true, false},
		{"nested number match", "review.score == 9", true, false},
		{"nested number mismatch", "review.score == 8", false, false},
		{"string match quoted", `name == "alice"`, true, false},
		{"string mismatch", `name == "bob"`, false, false},
		{"bool false match", "done == false", true, false},
		{"flat dotted key match", `flat.key == "v"`, true, false},
		// Missing path: not an error, just no match.
		{"missing path", "missing.field == true", false, false},
		// No == operator at all: a structural error.
		{"no operator", "review.approved", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evaluateSingleCondition(tc.condition, payload)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.condition)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.condition, err)
			}
			if got != tc.want {
				t.Fatalf("evaluateSingleCondition(%q) = %v, want %v", tc.condition, got, tc.want)
			}
		})
	}
}

func TestW2ExecEvaluateGateCondition_Compound(t *testing.T) {
	payload := map[string]any{
		"a": true,
		"b": "yes",
		"c": false,
	}
	// All sub-terms true → matched.
	if got, err := evaluateGateCondition(`a == true && b == "yes"`, payload); err != nil || !got {
		t.Fatalf("compound all-true: got=%v err=%v", got, err)
	}
	// One sub-term false → no match, no error.
	if got, err := evaluateGateCondition(`a == true && c == true`, payload); err != nil || got {
		t.Fatalf("compound one-false: got=%v err=%v", got, err)
	}
	// Empty sub-term (trailing &&) is a loud config error, NOT vacuous
	// truth — this is the 2026-05-06 regression guard.
	if _, err := evaluateGateCondition(`a == true &&`, payload); err == nil {
		t.Fatalf("trailing && should error, got nil")
	}
	if _, err := evaluateGateCondition(`a == true && && b == "yes"`, payload); err == nil {
		t.Fatalf("double && should error, got nil")
	}
	// A sub-term that itself is malformed bubbles its error up.
	if _, err := evaluateGateCondition(`a == true && bogus`, payload); err == nil {
		t.Fatalf("malformed sub-term should error, got nil")
	}
}

func TestW2ExecParseGateValue(t *testing.T) {
	cases := []struct {
		raw  string
		want any
	}{
		{"true", true},
		{"false", false},
		{"null", nil},
		{`"hello"`, "hello"},
		{`""`, ""},
		{"42", float64(42)},
		{"3.5", float64(3.5)},
		// Bare unquoted non-numeric → returned verbatim as a string.
		{"PENDING", "PENDING"},
	}
	for _, tc := range cases {
		got, err := parseGateValue(tc.raw)
		if err != nil {
			t.Fatalf("parseGateValue(%q) err: %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("parseGateValue(%q) = %#v, want %#v", tc.raw, got, tc.want)
		}
	}
}

func TestW2ExecLookupJSONPath_FlatVsNested(t *testing.T) {
	// Flat key wins when present, even if a nested walk would also resolve.
	payload := map[string]any{
		"review.approved": "flat-wins",
		"review":          map[string]any{"approved": "nested"},
	}
	v, ok := lookupJSONPath(payload, "review.approved")
	if !ok || v != "flat-wins" {
		t.Fatalf("flat key should win: got=%v ok=%v", v, ok)
	}
	// Pure nested path.
	nested := map[string]any{"a": map[string]any{"b": map[string]any{"c": 1.0}}}
	v, ok = lookupJSONPath(nested, "a.b.c")
	if !ok || v != 1.0 {
		t.Fatalf("nested walk: got=%v ok=%v", v, ok)
	}
	// Non-object payload → not found.
	if _, ok := lookupJSONPath("not-an-object", "a"); ok {
		t.Fatalf("non-object payload should miss")
	}
	// Walk through a non-object intermediate → not found.
	if _, ok := lookupJSONPath(nested, "a.b.c.d"); ok {
		t.Fatalf("over-deep walk should miss")
	}
}

// --- classifyGateEvalError: maps evaluator errors to step outcome/class ---

func TestW2ExecClassifyGateEvalError(t *testing.T) {
	okOutcome, okClass := classifyGateEvalError(nil)
	if okOutcome == "" || okClass != "" {
		t.Fatalf("nil error: outcome=%q class=%q", okOutcome, okClass)
	}
	// Parse failure path.
	parseOutcome, parseClass := classifyGateEvalError(errors.New("failed to parse gate input as JSON: x"))
	if parseClass == "" || parseOutcome == "" {
		t.Fatalf("parse error: outcome=%q class=%q", parseOutcome, parseClass)
	}
	// No condition matched → downstream rejected (distinct outcome from default).
	noMatchOutcome, noMatchClass := classifyGateEvalError(errors.New("no gate condition matched (expected ...)"))
	if noMatchOutcome == "" || noMatchClass == "" {
		t.Fatalf("no-match: outcome=%q class=%q", noMatchOutcome, noMatchClass)
	}
	// Default (e.g. bad condition syntax).
	defOutcome, defClass := classifyGateEvalError(errors.New("unsupported gate condition \"x\""))
	if defOutcome == "" || defClass == "" {
		t.Fatalf("default: outcome=%q class=%q", defOutcome, defClass)
	}
	// The three error families must map to distinct outcomes.
	if parseOutcome == noMatchOutcome || noMatchOutcome == defOutcome || parseOutcome == defOutcome {
		t.Fatalf("outcomes should be distinct: parse=%q noMatch=%q default=%q",
			parseOutcome, noMatchOutcome, defOutcome)
	}
}

// --- shape-failure classification on the retry hot-path ---

func TestW2ExecClassifyShapeFailure_AllJSONTriggers(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want shapeFailureKind
	}{
		{"nil", nil, shapeFailureNone},
		{"plausibility", errors.New("plausibility violation: empty field"), shapeFailurePlausibility},
		{"schema violation role", errors.New(`schema violation: role "lead" bad`), shapeFailureJSON},
		{"missing required keys", errors.New("result.json is missing required keys: [a b]"), shapeFailureJSON},
		{"could not parse plan", errors.New("could not parse plan from lead output"), shapeFailureJSON},
		{"result.json parse", errors.New("result.json: failed to parse body"), shapeFailureJSON},
		{"result.json unmarshal", errors.New("result.json: cannot unmarshal number"), shapeFailureJSON},
		// result.json mentioned but no parse/unmarshal keyword → not a shape failure.
		{"result.json no verb", errors.New("result.json was empty"), shapeFailureNone},
		{"unrelated", errors.New("container exited 137"), shapeFailureNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyShapeFailure(tc.err); got != tc.want {
				t.Fatalf("classifyShapeFailure(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// --- extractMissingKeysFromError: structured recovery of validator keys ---

func TestW2ExecExtractMissingKeysFromError(t *testing.T) {
	// Space-separated (fmt %v default).
	got := extractMissingKeysFromError(errors.New(`role "x" result.json is missing required keys: [alpha beta gamma]`))
	if strings.Join(got, ",") != "alpha,beta,gamma" {
		t.Fatalf("space-separated: got %v", got)
	}
	// Comma-separated tolerance (future formatter drift).
	got = extractMissingKeysFromError(errors.New(`is missing required keys: [one, two]`))
	if strings.Join(got, ",") != "one,two" {
		t.Fatalf("comma-separated: got %v", got)
	}
	// Empty bracket → nil.
	if got := extractMissingKeysFromError(errors.New("is missing required keys: []")); got != nil {
		t.Fatalf("empty bracket should be nil, got %v", got)
	}
	// Whitespace-only bracket → nil.
	if got := extractMissingKeysFromError(errors.New("is missing required keys: [   ]")); got != nil {
		t.Fatalf("whitespace bracket should be nil, got %v", got)
	}
	// Non-matching error → nil.
	if got := extractMissingKeysFromError(errors.New("some other failure")); got != nil {
		t.Fatalf("non-match should be nil, got %v", got)
	}
	// Nil error → nil.
	if got := extractMissingKeysFromError(nil); got != nil {
		t.Fatalf("nil err should be nil, got %v", got)
	}
}

// --- markRetryable + isInfraFailure: the retryable-error marking primitive ---

func TestW2ExecMarkRetryableRoundTrips(t *testing.T) {
	base := errors.New("transient broker hiccup")
	wrapped := markRetryable(base)
	if wrapped == nil {
		t.Fatal("markRetryable(non-nil) returned nil")
	}
	// Unwraps back to the original sentinel.
	if !errors.Is(wrapped, base) {
		t.Fatalf("wrapped error should unwrap to base")
	}
	// A retryable-marked error is the task-level retry channel and must
	// NOT be re-classified as an infra failure — those are distinct retry
	// paths, and isInfraFailure deliberately excludes retryableError.
	if isInfraFailure(wrapped) {
		t.Fatalf("markRetryable output must not be treated as an infra failure")
	}
	// markRetryable(nil) is a nil-safe no-op.
	if markRetryable(nil) != nil {
		t.Fatalf("markRetryable(nil) should be nil")
	}
	// A plain error is not infra.
	if isInfraFailure(errors.New("plain")) {
		t.Fatalf("plain error must not be infra")
	}
	// nil is not infra.
	if isInfraFailure(nil) {
		t.Fatalf("nil must not be infra")
	}
	// A genuine transient gateway message IS an infra failure.
	if !isInfraFailure(errors.New("gateway error 503: upstream unavailable")) {
		t.Fatalf("gateway 503 should be an infra failure")
	}
}

// --- artifact-flow extraction from the task payload ---

func TestW2ExecExtractTaskInputArtifacts_ExtractedSkipped(t *testing.T) {
	// Equal counts, one file extracted (has extracted_document_id) → that
	// file is skipped (agent uses document_* tools), the other is staged.
	payload := []byte(`{"context":{
		"inputFiles":["/tmp/report.epub","/tmp/data.bin"],
		"inputExtractions":[{"extracted_document_id":"doc-1"},{}]
	}}`)
	got := extractTaskInputArtifacts(payload)
	if len(got) != 1 {
		t.Fatalf("expected 1 staged artifact, got %v", got)
	}
	if got[0]["name"] != "data.bin" || got[0]["sourcePath"] != "/tmp/data.bin" {
		t.Fatalf("unexpected staged artifact: %v", got[0])
	}
}

func TestW2ExecExtractTaskInputArtifacts_CountMismatchSkipsAll(t *testing.T) {
	// Mismatched extraction count → every basename flagged extracted →
	// nothing staged → nil.
	payload := []byte(`{"context":{
		"inputFiles":["/tmp/a.pdf","/tmp/b.pdf"],
		"inputExtractions":[{"extracted_document_id":"doc-1"}]
	}}`)
	if got := extractTaskInputArtifacts(payload); got != nil {
		t.Fatalf("count mismatch should stage nothing, got %v", got)
	}
}

func TestW2ExecExtractTaskInputArtifacts_NoExtractionsStagesAll(t *testing.T) {
	payload := []byte(`{"context":{"inputFiles":["/tmp/a.txt","","/tmp/b.txt"]}}`)
	got := extractTaskInputArtifacts(payload)
	if len(got) != 2 {
		t.Fatalf("expected 2 staged (empty path dropped), got %v", got)
	}
	if got[0]["name"] != "a.txt" || got[1]["name"] != "b.txt" {
		t.Fatalf("unexpected names: %v", got)
	}
}

func TestW2ExecExtractTaskInputArtifacts_EmptyAndBad(t *testing.T) {
	if got := extractTaskInputArtifacts(nil); got != nil {
		t.Fatalf("nil payload → nil, got %v", got)
	}
	if got := extractTaskInputArtifacts([]byte(`{not json`)); got != nil {
		t.Fatalf("bad json → nil, got %v", got)
	}
	if got := extractTaskInputArtifacts([]byte(`{"context":{"inputFiles":[]}}`)); got != nil {
		t.Fatalf("no files → nil, got %v", got)
	}
}

func TestW2ExecInputArtifactRefsFromPayload(t *testing.T) {
	// IDs zip positionally with inputFiles (by basename). Blank IDs skipped.
	payload := []byte(`{"context":{
		"inputArtifactIDs":["art-1","  ","art-3"],
		"inputFiles":["/tmp/one.csv","/tmp/two.csv","/tmp/three.csv"]
	}}`)
	refs := inputArtifactRefsFromPayload(payload)
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs (blank skipped), got %v", refs)
	}
	if refs[0].ID != "art-1" || refs[0].Name != "one.csv" {
		t.Fatalf("ref0 unexpected: %+v", refs[0])
	}
	// art-3 keeps its ID but pairs with the third filename.
	if refs[1].ID != "art-3" || refs[1].Name != "three.csv" {
		t.Fatalf("ref1 unexpected: %+v", refs[1])
	}
}

func TestW2ExecInputArtifactRefsFromPayload_Edges(t *testing.T) {
	// IDs longer than files → name falls back to "".
	refs := inputArtifactRefsFromPayload([]byte(`{"context":{"inputArtifactIDs":["x"],"inputFiles":[]}}`))
	if len(refs) != 1 || refs[0].ID != "x" || refs[0].Name != "" {
		t.Fatalf("missing filename should leave Name empty: %+v", refs)
	}
	// No IDs → nil.
	if got := inputArtifactRefsFromPayload([]byte(`{"context":{"inputFiles":["/a"]}}`)); got != nil {
		t.Fatalf("no IDs → nil, got %v", got)
	}
	// Empty / bad payloads → nil.
	if got := inputArtifactRefsFromPayload(nil); got != nil {
		t.Fatalf("nil → nil")
	}
	if got := inputArtifactRefsFromPayload([]byte(`{bad`)); got != nil {
		t.Fatalf("bad json → nil")
	}
}

// --- prose extraction from result JSON (hallucination detector input) ---

func TestW2ExecExtractProseFromResult_NestedAndArrays(t *testing.T) {
	// Pulls the known prose keys; concatenates string + array-of-string.
	in := []byte(`{
		"message":"hello",
		"summary":"world",
		"notes":["a","b",3,""],
		"reasoning":"",
		"toolHistory":"should be ignored"
	}`)
	got := extractProseFromResult(in)
	for _, want := range []string{"hello", "world", "a", "b"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in prose, got %q", want, got)
		}
	}
	// Non-prose / unknown key must not leak.
	if strings.Contains(got, "should be ignored") {
		t.Fatalf("unknown key leaked into prose: %q", got)
	}
	// Empty-string prose field and numeric array elements skipped.
	if strings.Contains(got, "3") {
		t.Fatalf("numeric array element leaked: %q", got)
	}
}

func TestW2ExecExtractProseFromResult_EmptyAndBad(t *testing.T) {
	if got := extractProseFromResult(nil); got != "" {
		t.Fatalf("nil → empty, got %q", got)
	}
	if got := extractProseFromResult([]byte(`not json`)); got != "" {
		t.Fatalf("bad json → empty, got %q", got)
	}
	// Valid JSON with no prose keys → empty.
	if got := extractProseFromResult([]byte(`{"status":"OK","count":5}`)); got != "" {
		t.Fatalf("no prose keys → empty, got %q", got)
	}
}

// --- prompt path rewriting: host paths → container artifact paths ---

func TestW2ExecRewriteInputPathsInPrompt(t *testing.T) {
	prompt := "Please read /home/user/uploads/spec.md and also ./spec.md and /tmp/spec.md."
	out := rewriteInputPathsInPrompt(prompt, []string{"/home/user/uploads/spec.md"})
	want := "/app/workspace/artifacts/in/spec.md"
	// Exact host path rewritten.
	if strings.Contains(out, "/home/user/uploads/spec.md") {
		t.Fatalf("exact host path not rewritten: %q", out)
	}
	// ./<base> and /tmp/<base> rewritten to container path.
	if strings.Count(out, want) < 3 {
		t.Fatalf("expected 3 container-path mentions, got %q", out)
	}
}

func TestW2ExecRewriteInputPathsInPrompt_NoOps(t *testing.T) {
	// Empty prompt unchanged.
	if got := rewriteInputPathsInPrompt("", []string{"/a"}); got != "" {
		t.Fatalf("empty prompt should pass through, got %q", got)
	}
	// No input files → unchanged.
	if got := rewriteInputPathsInPrompt("read /tmp/x", nil); got != "read /tmp/x" {
		t.Fatalf("no files should pass through, got %q", got)
	}
	// Bare basename mention is NOT rewritten (conservative — only path-like refs).
	out := rewriteInputPathsInPrompt("the spec.md file", []string{"/up/spec.md"})
	if strings.Contains(out, "/app/workspace") {
		t.Fatalf("bare basename should not be rewritten: %q", out)
	}
	// Empty source path entry is skipped without panicking.
	if got := rewriteInputPathsInPrompt("hi", []string{""}); got != "hi" {
		t.Fatalf("empty src entry should be skipped, got %q", got)
	}
}

// --- recovery context block assembly + learned-remediation overlay ---

func TestW2ExecBuildRecoveryContextBlock(t *testing.T) {
	rc := &RecoveryContext{
		FailedStep:    "plan_2_researcher",
		FailureClass:  "tool_blocked",
		FailureReason: "verifier rejected: blocked URL",
		BlockedURLs: []verifier.BlockedURL{
			{URL: "https://x.test/a", Reason: "denylist"},
		},
	}
	got := buildRecoveryContextBlock(rc)
	for _, want := range []string{
		"## RECOVERY_CONTEXT",
		"failed_step: plan_2_researcher",
		"failure_class: tool_blocked",
		"failure_reason: verifier rejected",
		"https://x.test/a",
		"denylist",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("recovery block missing %q in:\n%s", want, got)
		}
	}
	// No trailing newline (TrimRight).
	if strings.HasSuffix(got, "\n") {
		t.Fatalf("recovery block should not end with newline")
	}
}

func TestW2ExecBuildRecoveryContextBlock_Minimal(t *testing.T) {
	// Empty optional fields → only the banner, no field lines.
	got := buildRecoveryContextBlock(&RecoveryContext{})
	if !strings.Contains(got, "## RECOVERY_CONTEXT") {
		t.Fatalf("banner missing: %q", got)
	}
	if strings.Contains(got, "failed_step") || strings.Contains(got, "blocked_urls") {
		t.Fatalf("empty fields should not render: %q", got)
	}
}

// --- step-ID ↔ role helpers (recovery/attribution routing) ---

func TestW2ExecStepIDToRole(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"plan_2_researcher", "researcher"},
		{"plan_0_lead", "lead"},
		{"plan_lead_lead", "lead"},
		{"plan_lead_lead_shape_retry", "lead"},
		{"plan_3_writer_infra_retry1", "writer"},
		// Not the plan_ convention → "".
		{"gate_review", ""},
		{"plan_only", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := stepIDToRole(tc.id); got != tc.want {
			t.Fatalf("stepIDToRole(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestW2ExecStripRetryStepSuffix(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"plan_2_writer_shape_retry", "plan_2_writer"},
		{"plan_2_writer_model_fallback", "plan_2_writer"},
		{"plan_2_writer_refusal_retry", "plan_2_writer"},
		{"plan_2_writer_route_retry", "plan_2_writer"},
		{"plan_2_writer_infra_retry3", "plan_2_writer"},
		// No suffix → unchanged.
		{"plan_2_writer", "plan_2_writer"},
	}
	for _, tc := range cases {
		if got := stripRetryStepSuffix(tc.id); got != tc.want {
			t.Fatalf("stripRetryStepSuffix(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

// --- loadExecutionState: snapshot + column fallback merge ---

func TestW2ExecLoadExecutionState_NilAndEmpty(t *testing.T) {
	if got := loadExecutionState(nil); got.CurrentStepID != "" || len(got.CompletedSteps) != 0 {
		t.Fatalf("nil execution should yield zero state, got %+v", got)
	}
	// No snapshot, no columns → zero state.
	got := loadExecutionState(&persistence.Execution{})
	if got.CurrentStepID != "" || len(got.CompletedSteps) != 0 {
		t.Fatalf("empty execution → zero state, got %+v", got)
	}
}

func TestW2ExecLoadExecutionState_SnapshotWins(t *testing.T) {
	snap, _ := json.Marshal(executionState{
		CurrentStepID:  "from_snapshot",
		CompletedSteps: []string{"a", "b"},
	})
	step := "from_column"
	got := loadExecutionState(&persistence.Execution{
		StateSnapshot:  snap,
		CurrentStepID:  &step,
		CompletedSteps: []string{"x"},
	})
	// Snapshot is authoritative for both fields.
	if got.CurrentStepID != "from_snapshot" {
		t.Fatalf("snapshot CurrentStepID should win, got %q", got.CurrentStepID)
	}
	if strings.Join(got.CompletedSteps, ",") != "a,b" {
		t.Fatalf("snapshot CompletedSteps should win, got %v", got.CompletedSteps)
	}
}

func TestW2ExecLoadExecutionState_ColumnFallback(t *testing.T) {
	// Snapshot present but missing CurrentStepID/CompletedSteps → fall back
	// to the row columns.
	snap, _ := json.Marshal(executionState{Iterations: 2})
	step := "col_step"
	got := loadExecutionState(&persistence.Execution{
		StateSnapshot:  snap,
		CurrentStepID:  &step,
		CompletedSteps: []string{"c1", "c2"},
	})
	if got.CurrentStepID != "col_step" {
		t.Fatalf("expected column CurrentStepID fallback, got %q", got.CurrentStepID)
	}
	if strings.Join(got.CompletedSteps, ",") != "c1,c2" {
		t.Fatalf("expected column CompletedSteps fallback, got %v", got.CompletedSteps)
	}
	if got.Iterations != 2 {
		t.Fatalf("snapshot-only field lost: %+v", got)
	}
}

// --- buildCurrentDateTimeContext: timezone resolution (deterministic clock) ---

func TestW2ExecBuildCurrentDateTimeContext(t *testing.T) {
	// Pin the clock so the rendered strings are deterministic.
	fixed := time.Date(2026, 6, 17, 14, 30, 0, 0, time.UTC)
	orig := currentDateTimeNow
	currentDateTimeNow = func() time.Time { return fixed }
	defer func() { currentDateTimeNow = orig }()

	// Empty timezone defaults to UTC.
	utc := buildCurrentDateTimeContext("")
	if utc.Timezone != "UTC" {
		t.Fatalf("empty tz should default to UTC, got %q", utc.Timezone)
	}
	if utc.Date != "2026-06-17" || utc.Time != "14:30:00" {
		t.Fatalf("UTC render wrong: date=%q time=%q", utc.Date, utc.Time)
	}
	if !strings.Contains(utc.PromptLine, "2026") || !strings.Contains(utc.PromptLine, "UTC") {
		t.Fatalf("prompt line missing markers: %q", utc.PromptLine)
	}

	// A valid IANA zone shifts the local fields but UTC stays the same.
	ny := buildCurrentDateTimeContext("America/New_York")
	if ny.Timezone != "America/New_York" {
		t.Fatalf("valid tz not honored: %q", ny.Timezone)
	}
	if ny.UTC != utc.UTC {
		t.Fatalf("UTC field should be tz-independent: %q vs %q", ny.UTC, utc.UTC)
	}
	if ny.Time == utc.Time {
		t.Fatalf("New York local time should differ from UTC at 14:30")
	}

	// An invalid zone falls back to UTC (tz reset to "").
	bad := buildCurrentDateTimeContext("Not/AZone")
	if bad.Timezone != "UTC" || bad.Time != "14:30:00" {
		t.Fatalf("invalid tz should fall back to UTC: %+v", bad)
	}
}

// --- buildGatePromptSuffix: response-format instructions from gates ---

func TestW2ExecBuildGatePromptSuffix(t *testing.T) {
	gates := []registry.WorkflowGate{
		{Condition: "review.approved == true", Target: "complete"},
		{Condition: "review.approved == false", Target: "implement"},
	}
	got := buildGatePromptSuffix(gates)
	for _, want := range []string{
		"pure JSON object",
		"review.approved == true",
		"routes to: complete",
		"review.approved == false",
		"routes to: implement",
		"Do NOT wrap the JSON in markdown",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("gate suffix missing %q in:\n%s", want, got)
		}
	}
}

// --- previewJSON: truncation for trace/error lines ---

func TestW2ExecPreviewJSON(t *testing.T) {
	short := json.RawMessage(`{"k":1}`)
	if got := previewJSON(short); got != `{"k":1}` {
		t.Fatalf("short preview should be verbatim, got %q", got)
	}
	long := json.RawMessage(strings.Repeat("x", 400))
	got := previewJSON(long)
	if len(got) != 303 || !strings.HasSuffix(got, "...") {
		t.Fatalf("long preview should be 300+ellipsis, got len=%d suffix=%q", len(got), got[len(got)-3:])
	}
}

// --- evaluateGateStepTraced: integration of the gate evaluators above ---

func TestW2ExecEvaluateGateStepTraced_EnvelopeNormalization(t *testing.T) {
	// The gate input arrives wrapped in the harness envelope's `message`
	// field as a JSON string; normalization must still resolve the gate.
	step := registry.WorkflowStep{
		Gates: []registry.WorkflowGate{
			{Condition: "approved == true", Target: "complete"},
			{Condition: "approved == false", Target: "revise"},
		},
	}
	last := json.RawMessage(`{"message":"{\"approved\": false}"}`)
	target, trace, err := evaluateGateStepTraced(step, last)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target != "revise" {
		t.Fatalf("expected revise target, got %q", target)
	}
	// Trace should record both attempted gates, second matched.
	if len(trace.Entries) != 2 || !trace.Entries[1].Matched {
		t.Fatalf("trace unexpected: %+v", trace.Entries)
	}
}
