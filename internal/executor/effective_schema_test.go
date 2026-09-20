package executor

import (
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/quality"
	"vornik.io/vornik/internal/registry"
)

// D6.1a/D6.1b: the validator walks the schema the model was HANDED. Every site
// that writes opts.ResponseSchema must set EffectiveSchema beside it, and the
// recovery override — which replaces the emitted schema wholesale with a
// hand-built map — must clear it rather than leave the role's declaration
// standing against output produced under a different contract.

func TestApplyRoleSchemaOptsSetsEffectiveSchema(t *testing.T) {
	role := &registry.SwarmRole{
		Name: "analyst",
		OutputSchema: &registry.OutputSchema{
			Type: "object",
			Properties: map[string]*registry.OutputSchema{
				"tier": {Type: "string", Enum: []any{"a", "b"}},
			},
		},
	}
	opts := &agentInputOpts{}
	applyRoleSchemaOpts(opts, role)
	if opts.EffectiveSchema == nil {
		t.Fatal("EffectiveSchema is nil after applyRoleSchemaOpts; the validator would have nothing to walk")
	}
	if opts.EffectiveSchema.Properties["tier"].Enum[0] != "a" {
		t.Fatalf("EffectiveSchema is not the role's declaration: %+v", opts.EffectiveSchema)
	}
}

func TestApplyPinnedCaseEnumRetainsTheClone(t *testing.T) {
	// Test 53 — the load-bearing one. applyPinnedCaseEnum built the pinned
	// clone, generated the two provider copies from it and then DISCARDED it,
	// so a validator reaching for the dialect tree would find the role's
	// shared declaration and enforce the wrong thing — the exact
	// walk-the-declaration failure D6.1 warns about, one layer down.
	role := &registry.SwarmRole{
		Name:         "tester",
		OutputSchema: testerSchema(), // no enum on id in the DECLARATION
	}
	plan := &executionPlan{workflow: &registry.Workflow{
		QualityScoring: &quality.ScoringPolicy{
			Kind:         quality.ScoreKindPinnedCaseValidation,
			ProducerStep: "analyze",
			VerifierStep: "test",
		},
	}}
	state := &executionState{StepResults: map[string]json.RawMessage{
		"analyze": json.RawMessage(`{"analysis":{"test_case_ids":["s1_case_1","s1_case_2"]}}`),
	}}
	opts := &agentInputOpts{}
	applyRoleSchemaOpts(opts, role)
	applyPinnedCaseEnum(opts, plan, "test", role, state)

	if opts.EffectiveSchema == nil {
		t.Fatal("EffectiveSchema is nil after pinning")
	}
	pinned := opts.EffectiveSchema.Properties["testing"].Properties["cases"].Items.Properties["id"]
	if len(pinned.Enum) != 2 {
		t.Fatalf("EffectiveSchema carries %d pinned ids, want 2 — the clone was not retained", len(pinned.Enum))
	}
	if declared := role.OutputSchema.Properties["testing"].Properties["cases"].Items.Properties["id"]; len(declared.Enum) != 0 {
		t.Fatal("pinning leaked into the ROLE's shared declaration; the next task's tester would inherit these ids")
	}
}

func TestEffectiveSchemaValidatesAgainstThePinnedIDs(t *testing.T) {
	// The two halves joined: what the seam carries is what the validator binds.
	role := &registry.SwarmRole{Name: "tester", OutputSchema: testerSchema()}
	plan := &executionPlan{workflow: &registry.Workflow{
		QualityScoring: &quality.ScoringPolicy{
			Kind:         quality.ScoreKindPinnedCaseValidation,
			ProducerStep: "analyze",
			VerifierStep: "test",
		},
	}}
	state := &executionState{StepResults: map[string]json.RawMessage{
		"analyze": json.RawMessage(`{"analysis":{"test_case_ids":["s1_case_1"]}}`),
	}}
	opts := &agentInputOpts{}
	applyRoleSchemaOpts(opts, role)
	applyPinnedCaseEnum(opts, plan, "test", role, state)

	// dp-09's shape, against a declaration that constrains nothing.
	bad := []byte(`{"testing":{"cases":[{"id":"case_1","status":"passed"}]}}`)
	if got := validateEnums(bad, opts.EffectiveSchema); len(got) != 1 {
		t.Fatalf("the renumbered id was not caught through the seam: %+v", got)
	}
	good := []byte(`{"testing":{"cases":[{"id":"s1_case_1","status":"passed"}]}}`)
	if got := validateEnums(good, opts.EffectiveSchema); len(got) != 0 {
		t.Fatalf("a conforming report was rejected through the seam: %+v", got)
	}
}

func TestPinnedCaseEnumDoesNotFireForANonVerifierStep(t *testing.T) {
	role := &registry.SwarmRole{Name: "coder", OutputSchema: testerSchema()}
	plan := &executionPlan{workflow: &registry.Workflow{
		QualityScoring: &quality.ScoringPolicy{
			Kind:         quality.ScoreKindPinnedCaseValidation,
			ProducerStep: "analyze",
			VerifierStep: "test",
		},
	}}
	state := &executionState{StepResults: map[string]json.RawMessage{
		"analyze": json.RawMessage(`{"analysis":{"test_case_ids":["s1_case_1"]}}`),
	}}
	opts := &agentInputOpts{}
	applyRoleSchemaOpts(opts, role)
	applyPinnedCaseEnum(opts, plan, "implement", role, state)
	got := opts.EffectiveSchema.Properties["testing"].Properties["cases"].Items.Properties["id"]
	if len(got.Enum) != 0 {
		t.Fatalf("pinned ids were installed on a step that is not the verifier: %+v", got.Enum)
	}
}
