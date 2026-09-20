package executor

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// D6: the emitted enum binds at the provider only where the provider honours
// json_schema. These cover the daemon-side backstop, which binds everywhere.
//
// Incident of record: the 2026.9.4 scored arm, where the tester reported case
// ids it was never handed — invented on dp-01, renumbered on dp-09, a subset
// on dp-02/06/07 — while demonstrably holding the correct list, and every
// point of the 0.8414 mean below 1.000 was id bookkeeping rather than bad work.

func testerSchema(ids ...any) *registry.OutputSchema {
	return &registry.OutputSchema{
		Type: "object",
		Properties: map[string]*registry.OutputSchema{
			"testing": {
				Type: "object",
				Properties: map[string]*registry.OutputSchema{
					"cases": {
						Type: "array",
						Items: &registry.OutputSchema{
							Type: "object",
							Properties: map[string]*registry.OutputSchema{
								"id":     {Type: "string", Enum: ids},
								"status": {Type: "string", Enum: []any{"passed", "failed", "manual", "missing"}},
							},
						},
					},
				},
			},
		},
	}
}

func TestEnumValidate_PinnedIDOutsideEnumIsAViolation(t *testing.T) {
	// Test 50 — dp-09's shape: six reported ids, none of them pinned.
	result := []byte(`{"testing":{"cases":[{"id":"case_1","status":"passed"}]}}`)
	got := validateEnums(result, testerSchema("s1_case_1", "s1_case_2"))
	if len(got) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(got), got)
	}
	if got[0].Path != "testing.cases[0].id" {
		t.Errorf("path = %q, want testing.cases[0].id", got[0].Path)
	}
	if got[0].Mode != registry.ViolationFail {
		t.Errorf("mode = %q, want fail — a pinned id is a closed contract", got[0].Mode)
	}
}

func TestEnumValidate_InEnumIsClean(t *testing.T) {
	// Test 51 — the over-rejection guard. 15 of 20 tasks were exact.
	result := []byte(`{"testing":{"cases":[{"id":"s1_case_1","status":"passed"},{"id":"s1_case_2","status":"manual"}]}}`)
	if got := validateEnums(result, testerSchema("s1_case_1", "s1_case_2")); len(got) != 0 {
		t.Fatalf("a conforming report produced %d violations: %+v", len(got), got)
	}
}

func TestEnumValidate_MessageNamesValueAndTruncatesTheSet(t *testing.T) {
	// Test 52 — a retry told only "invalid enum" is a retry that guesses.
	// The allowed SET may truncate; the offending value never may.
	ids := make([]any, 0, 19)
	for _, id := range []string{
		"s1_case_1", "s1_case_2", "s1_case_3", "s1_case_4", "s1_case_5", "s1_case_6", "s1_case_7",
		"s2_case_1", "s2_case_2", "s2_case_3", "s2_case_4", "s2_case_5", "s2_case_6",
		"s3_case_1", "s3_case_2", "s3_case_3", "s3_case_4", "s3_case_5", "s3_case_6",
	} {
		ids = append(ids, id)
	}
	got := validateEnums([]byte(`{"testing":{"cases":[{"id":"totally_invented","status":"passed"}]}}`), testerSchema(ids...))
	if len(got) != 1 {
		t.Fatalf("got %d violations, want 1", len(got))
	}
	msg := enumViolationMessage(got, nil)
	if !strings.Contains(msg, "totally_invented") {
		t.Errorf("message lost the offending value, which is the one thing a retry needs: %q", msg)
	}
	if !strings.Contains(msg, "testing.cases[0].id") {
		t.Errorf("message lost the path: %q", msg)
	}
	if !strings.Contains(msg, "…") && !strings.Contains(msg, "more") {
		t.Errorf("a 19-id set rendered untruncated; the failure message is the retry prompt: %q", msg)
	}
	if len(msg) > 600 {
		t.Errorf("message is %d bytes; the cap exists so a 19-id enum does not blow it out", len(msg))
	}
}

func TestEnumValidate_StaticStatusEnumIsEnforcedWithNoPinning(t *testing.T) {
	// Test 54 — closes the parsed-but-unused Phase-2 gap for a declared enum.
	result := []byte(`{"testing":{"cases":[{"id":"s1_case_1","status":"skipped"}]}}`)
	got := validateEnums(result, testerSchema("s1_case_1"))
	if len(got) != 1 || got[0].Path != "testing.cases[0].status" {
		t.Fatalf("status outside its declared enum was not caught: %+v", got)
	}
}

func complexitySchema(mode string) *registry.OutputSchema {
	return &registry.OutputSchema{
		Type:     "object",
		Required: []string{"analysis"},
		Properties: map[string]*registry.OutputSchema{
			"analysis": {
				Type: "object",
				Properties: map[string]*registry.OutputSchema{
					"complexity": {
						Type:        "string",
						Enum:        []any{"trivial", "standard", "complex", "open_ended"},
						OnViolation: mode,
					},
				},
			},
		},
	}
}

func TestEnumValidate_WarnModeDoesNotFailTheStep(t *testing.T) {
	// Test 54a — dynamic-tool-budget-design.md §4 resolves an absent, empty or
	// unrecognised tier to 1.0x, a decision taken on 2026-06-13 after coercing
	// it to `standard` silently halved every dev-pipeline budget and timed out
	// a 15-minute implement step. Failing the step here would invert that.
	got := validateEnums([]byte(`{"analysis":{"complexity":"medium"}}`), complexitySchema("warn"))
	if len(got) != 1 {
		t.Fatalf("got %d violations, want 1 (it must still be COUNTED)", len(got))
	}
	if got[0].Mode != registry.ViolationWarn {
		t.Fatalf("mode = %q, want warn", got[0].Mode)
	}
	if fatalEnumViolations(got) != nil {
		t.Fatal("a warn-mode violation was reported as fatal; §4's 1.0x degradation is inverted")
	}
}

func TestEnumValidate_AbsentOptionalComplexityIsNotAViolation(t *testing.T) {
	// Test 57 — complexity is OPTIONAL in the shipped schema (dev-swarm.md
	// required: [analysis] only), which is what keeps §4's "absent -> 1.0x"
	// reachable even under a provider that enforces the emitted enum. An enum
	// constrains a value, never a field's presence.
	if got := validateEnums([]byte(`{"analysis":{"ready":true}}`), complexitySchema("warn")); len(got) != 0 {
		t.Fatalf("an omitted optional field was treated as an enum violation: %+v", got)
	}
}

func TestEnumValidate_EmptyComplexityWarnsAndIsNotFatal(t *testing.T) {
	// Test 57a — the empty leg is a different path from absent, and §4 covers
	// both. It is a value, so it IS out of enum; it must warn, never fail.
	got := validateEnums([]byte(`{"analysis":{"complexity":""}}`), complexitySchema("warn"))
	if len(got) != 1 || got[0].Mode != registry.ViolationWarn {
		t.Fatalf("empty complexity: got %+v, want exactly one warn-mode violation", got)
	}
	if fatalEnumViolations(got) != nil {
		t.Fatal("empty complexity was fatal; §4 resolves it to 1.0x")
	}
}

func TestEnumValidate_NilSchemaIsANoOp(t *testing.T) {
	// Test 55 (half) — nil EffectiveSchema suppresses the ENUM sub-check only.
	// The requiredOutputKeys half is asserted at the container seam.
	if got := validateEnums([]byte(`{"testing":{"cases":[{"id":"anything"}]}}`), nil); got != nil {
		t.Fatalf("a nil schema enforced something: %+v", got)
	}
}

func TestEnumValidate_NoEnumsAnywhereIsANoOp(t *testing.T) {
	schema := &registry.OutputSchema{
		Type:       "object",
		Properties: map[string]*registry.OutputSchema{"plan": {Type: "string"}},
	}
	if got := validateEnums([]byte(`{"plan":"whatever"}`), schema); got != nil {
		t.Fatalf("a schema with no enums produced violations: %+v", got)
	}
}

func TestEnumValidate_UnparseableResultIsNotAnEnumViolation(t *testing.T) {
	// The structural check owns "this is not JSON". Reporting it as an enum
	// violation would misattribute a shape failure to a contract failure.
	if got := validateEnums([]byte(`not json at all`), testerSchema("s1_case_1")); got != nil {
		t.Fatalf("unparseable output produced enum violations: %+v", got)
	}
}

func TestEnumValidate_NonStringEnumValueComparesByValue(t *testing.T) {
	schema := &registry.OutputSchema{
		Type: "object",
		Properties: map[string]*registry.OutputSchema{
			"count": {Type: "number", Enum: []any{1, 2, 3}},
		},
	}
	if got := validateEnums([]byte(`{"count":2}`), schema); len(got) != 0 {
		t.Fatalf("a numeric in-enum value was rejected (JSON decodes to float64): %+v", got)
	}
	if got := validateEnums([]byte(`{"count":9}`), schema); len(got) != 1 {
		t.Fatalf("a numeric out-of-enum value was not caught: %+v", got)
	}
}

func TestEnumViolationMessageRedactsTheOffendingValue(t *testing.T) {
	// The enum check runs on rawResultBytes — the UNREDACTED form — because a
	// redaction splice that breaks the JSON would otherwise turn a real
	// violation into a clean pass, which is the "examined and clean vs never
	// examined" control CLAUDE.md §4 forbids. The 2026-08-20 validate-before-
	// redact rule allows that only for reads that emit structure, never
	// payload content. An enum violation's VALUE is payload content, from the
	// very fields that design names as redaction triggers ("case ids, commit
	// SHAs, generated identifiers"), and this message becomes agentError —
	// executions table, dashboard, Telegram, downstream prompts.
	violations := []enumViolation{{
		Path:    "testing.cases[0].id",
		Value:   "sk-live-SECRETVALUE",
		Allowed: []any{"s1_case_1"},
		Mode:    registry.ViolationFail,
	}}
	redact := func(string) string { return "[REDACTED]" }
	msg := enumViolationMessage(violations, redact)
	if strings.Contains(msg, "SECRETVALUE") {
		t.Fatalf("the raw payload value reached the persisted diagnostic: %q", msg)
	}
	if !strings.Contains(msg, "[REDACTED]") {
		t.Fatalf("the redactor was not applied: %q", msg)
	}
	if !strings.Contains(msg, "testing.cases[0].id") || !strings.Contains(msg, "s1_case_1") {
		t.Fatalf("path and allowed set come from the SCHEMA and must survive redaction: %q", msg)
	}
}
