package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/quality"
	"vornik.io/vornik/internal/registry"
)

// D5 (agent-quality-benchmark design, amendment 2026-09-18 and its retraction).
//
// The measured defect is that the verifier reports case ids that are not the
// ones it was given — inventing 3 on dp-01, renumbering all 15 on dp-09,
// omitting most on dp-02/06/07 — while demonstrably HOLDING the correct list:
// the analyst wrote the same ids into both result.json and CURRENT_TASK.md and
// the tester read that file every time.
//
// So telling it the ids more conveniently (D1) is still telling. §12.11.7
// already established the lever that binds: "the `required` list is what
// actually binds at tool-call decoding time — naming a field in a plausibility
// `require:` constrains nothing during generation". The same holds one level
// down. An enum of the pinned ids on cases[].id makes a wrong id
// UNREPRESENTABLE at decode time rather than merely forbidden in prose, and
// 2026.8.7 measured that exact move (prose obligation -> schema `required`)
// taking tester plausibility violations from 37/50 to 0/6.
func testerSchemaWithFreeformCaseID() *registry.OutputSchema {
	return &registry.OutputSchema{
		Type: "object",
		Properties: map[string]*registry.OutputSchema{
			"testing": {
				Type:     "object",
				Required: []string{"passed", "cases"},
				Properties: map[string]*registry.OutputSchema{
					"passed": {Type: "boolean"},
					"cases": {
						Type: "array",
						Items: &registry.OutputSchema{
							Type:     "object",
							Required: []string{"id", "status"},
							Properties: map[string]*registry.OutputSchema{
								"id":     {Type: "string"},
								"status": {Type: "string", Enum: []any{"passed", "failed", "manual", "missing"}},
							},
						},
					},
				},
			},
		},
	}
}

func TestPinCaseIDEnum_ConstrainsIDToThePinnedSet(t *testing.T) {
	schema := testerSchemaWithFreeformCaseID()
	producer := json.RawMessage(`{"analysis":{"test_case_ids":["s1_case_1","s1_case_2","s3_case_9"]}}`)

	got, n := pinCaseIDEnum(schema, producer)
	if n != 3 {
		t.Fatalf("pinned %d ids into the schema, want 3", n)
	}

	idNode := got.Properties["testing"].Properties["cases"].Items.Properties["id"]
	if len(idNode.Enum) != 3 {
		t.Fatalf("cases[].id enum = %#v, want the 3 pinned ids", idNode.Enum)
	}
	for i, want := range []string{"s1_case_1", "s1_case_2", "s3_case_9"} {
		if idNode.Enum[i] != want {
			t.Errorf("enum[%d] = %v, want %q", i, idNode.Enum[i], want)
		}
	}
	// The enum must survive into the JSON Schema the provider is handed,
	// because that is the only copy that binds anything.
	js := got.ToJSONSchema()
	raw, _ := json.Marshal(js)
	if !json.Valid(raw) || !strings.Contains(string(raw), `"s1_case_1"`) {
		t.Errorf("pinned ids did not reach the provider schema: %s", raw)
	}
}

// The caller's schema is shared across every execution of the role, so pinning
// must never mutate it — the next task's tester would inherit this task's ids.
func TestPinCaseIDEnum_DoesNotMutateTheRoleSchema(t *testing.T) {
	schema := testerSchemaWithFreeformCaseID()
	producer := json.RawMessage(`{"analysis":{"test_case_ids":["s1_case_1"]}}`)

	_, _ = pinCaseIDEnum(schema, producer)

	if got := schema.Properties["testing"].Properties["cases"].Items.Properties["id"].Enum; len(got) != 0 {
		t.Fatalf("the role's shared schema was mutated: id enum = %#v", got)
	}
}

// Unreadable or absent producer evidence must leave the schema alone rather
// than pin an empty enum — an empty enum admits NOTHING, which would fail
// every tester report instead of constraining it.
func TestPinCaseIDEnum_LeavesSchemaAloneWithoutPinnedIDs(t *testing.T) {
	for name, producer := range map[string]json.RawMessage{
		"absent":      nil,
		"unparseable": json.RawMessage(`not json`),
		"no ids":      json.RawMessage(`{"analysis":{}}`),
		"empty list":  json.RawMessage(`{"analysis":{"test_case_ids":[]}}`),
	} {
		t.Run(name, func(t *testing.T) {
			schema := testerSchemaWithFreeformCaseID()
			got, n := pinCaseIDEnum(schema, producer)
			if n != 0 {
				t.Fatalf("pinned %d ids from %s evidence", n, name)
			}
			if len(got.Properties["testing"].Properties["cases"].Items.Properties["id"].Enum) != 0 {
				t.Errorf("%s evidence produced a non-empty enum", name)
			}
		})
	}
}

// The wiring test: a verifier step in a workflow declaring pinned_case_validation
// gets its case-id enum pinned from the producer step's recorded result, so the
// constraint reaches opts.ResponseSchema / opts.ResultEmissionTool — the copies
// the provider is actually handed. Without this the enum exists only in a
// helper nobody calls.
func TestApplyPinnedCaseEnum_PinsFromTheProducerStep(t *testing.T) {
	role := &registry.SwarmRole{Name: "tester", OutputSchema: testerSchemaWithFreeformCaseID()}
	opts := &agentInputOpts{}
	applyRoleSchemaOpts(opts, role)

	plan := &executionPlan{workflow: &registry.Workflow{
		ID: "dev-pipeline",
		QualityScoring: &quality.ScoringPolicy{
			Kind:         quality.ScoreKindPinnedCaseValidation,
			ProducerStep: "analyze",
			VerifierStep: "test",
		},
	}}
	state := &executionState{StepResults: map[string]json.RawMessage{
		"analyze": json.RawMessage(`{"analysis":{"test_case_ids":["s1_case_1","s1_case_2"]}}`),
	}}

	applyPinnedCaseEnum(opts, plan, "test", role, state)

	raw, err := json.Marshal(opts.ResponseSchema)
	if err != nil {
		t.Fatalf("marshal response schema: %v", err)
	}
	if !strings.Contains(string(raw), `"s1_case_1"`) {
		t.Errorf("pinned ids did not reach opts.ResponseSchema: %s", raw)
	}
	toolRaw, err := json.Marshal(opts.ResultEmissionTool)
	if err != nil {
		t.Fatalf("marshal emission tool: %v", err)
	}
	if !strings.Contains(string(toolRaw), `"s1_case_1"`) {
		t.Errorf("pinned ids did not reach the emission tool spec: %s", toolRaw)
	}
}

// A step that is not the declared verifier must be left alone — the enum is a
// contract between one producer and one verifier, not a workflow-wide rule.
func TestApplyPinnedCaseEnum_IgnoresNonVerifierSteps(t *testing.T) {
	role := &registry.SwarmRole{Name: "tester", OutputSchema: testerSchemaWithFreeformCaseID()}
	opts := &agentInputOpts{}
	applyRoleSchemaOpts(opts, role)
	before, _ := json.Marshal(opts.ResponseSchema)

	plan := &executionPlan{workflow: &registry.Workflow{
		ID: "dev-pipeline",
		QualityScoring: &quality.ScoringPolicy{
			Kind:         quality.ScoreKindPinnedCaseValidation,
			ProducerStep: "analyze",
			VerifierStep: "test",
		},
	}}
	state := &executionState{StepResults: map[string]json.RawMessage{
		"analyze": json.RawMessage(`{"analysis":{"test_case_ids":["s1_case_1"]}}`),
	}}

	applyPinnedCaseEnum(opts, plan, "implement", role, state)

	after, _ := json.Marshal(opts.ResponseSchema)
	if string(before) != string(after) {
		t.Errorf("a non-verifier step's schema was specialised:\nbefore %s\nafter  %s", before, after)
	}
}
