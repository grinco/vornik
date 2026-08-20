package executor

import (
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Regression, 2026-08-20. A `require_output_glob` miss got a JSON-formatting
// correction, so no rung of the retry ladder could fix it.
//
// T-1089 (1e349167e) made an output-contract miss classify as a shape failure,
// which turned the corrective retry and the model fallback back on. The
// companion review of that very commit flagged what remained: "the KIND
// classification (shapeFailureJSON) selects a JSON-centric corrective hint for
// a file-write failure, which may produce suboptimal agent guidance." Nobody
// acted on it, because until now nothing exercised the path often enough to
// show it.
//
// Measured in the 2026.8.8 validation arm, once fixing review.all_done let the
// pipeline reach `report` for the first time: 5 schema_violation on `report`
// plus a full ladder — report_shape_retry, report_model_fallback,
// report_model_fallback_shape_retry, report_shape_retry_infra_retry1 — failing
// with schema_violation, degenerate_loop, iteration_exhausted and timeout. The
// step's real failure was never writing artifacts/out/CHANGELOG.md, and every
// retry told it to fix its JSON.
//
// shapeRetryHint says "Respond ONLY with the required JSON object... No
// prose... No markdown code fences... must parse with `jq .`", and the JSON
// branch also appends the whole role schema — wrong advice plus token noise on
// a step already exhausting its iteration budget. The plausibility branch
// carries this exact reasoning already ("Pointing at JSON formatting (as
// shapeRetryHint does) would confuse the model"); it was never carried over.
//
// Classification tests for this message live in output_contract_retry_test.go
// (T-1089). This file covers the KIND and the HINT.

func contractRole() *registry.SwarmRole {
	return &registry.SwarmRole{
		Name: "analyst",
		OutputSchema: &registry.OutputSchema{
			Type:     "object",
			Required: []string{"analysis"},
			Properties: map[string]*registry.OutputSchema{
				"analysis": {Type: "object"},
			},
		},
	}
}

func TestClassifyShapeFailure_outputContractHasItsOwnKind(t *testing.T) {
	err := outputContractErr("report", "artifacts/out/CHANGELOG.md")

	got := classifyShapeFailure(err)
	if got == shapeFailureJSON {
		t.Fatal("an unwritten output file classified as a JSON-shape failure, so the " +
			"corrective hint tells the agent to fix its JSON instead of writing the file")
	}
	if got != shapeFailureOutputContract {
		t.Errorf("classifyShapeFailure = %v, want shapeFailureOutputContract", got)
	}

	// T-1089's invariants must survive the refinement: still a shape failure,
	// still reaches the model fallback, still labelled schema_violation.
	if !isShapeFailure(err) {
		t.Error("T-1089 regression: no corrective retry would run")
	}
	if !isModelShapedFailure(err) {
		t.Error("T-1089 regression: the model fallback would not fire")
	}
	if got := shapeFailureMetricKind(err, classifyShapeFailure(err)); got != "schema_violation" {
		t.Errorf("metric kind = %q, want schema_violation — classifier and metric must agree", got)
	}
}

func TestBuildShapeRetryHint_outputContractTellsTheAgentToWriteTheFile(t *testing.T) {
	err := outputContractErr("report", "artifacts/out/CHANGELOG.md")
	hint := buildShapeRetryHint(err, shapeFailureOutputContract, nil, "", contractRole())

	for _, want := range []string{"write", "artifacts/out/CHANGELOG.md"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint does not mention %q: %s", want, hint)
		}
	}
	for _, unwanted := range []string{"No markdown code fences", "jq .", "Respond ONLY with the required JSON object"} {
		if strings.Contains(hint, unwanted) {
			t.Errorf("hint carries JSON-formatting advice %q for a file-write failure: %s", unwanted, hint)
		}
	}
	if strings.Contains(hint, "Required schema:") {
		t.Error("hint appends the role schema for a file-write failure; noise on a step " +
			"already exhausting its iteration budget")
	}
}

// The fix must not strip guidance from the case that legitimately needs it.
func TestBuildShapeRetryHint_jsonBranchKeepsItsSchema(t *testing.T) {
	err := errors.New(`schema violation: role "analyst" result.json is missing required keys: [analysis:object]`)
	hint := buildShapeRetryHint(err, shapeFailureJSON, nil, "", contractRole())
	if !strings.Contains(hint, "Required schema:") {
		t.Error("the JSON branch lost its rendered schema clause")
	}
	if !strings.Contains(hint, "Missing keys: [analysis:object]") {
		t.Error("the JSON branch lost its missing-keys clause")
	}
}
