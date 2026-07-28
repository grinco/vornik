package executor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression for the T-1089 third defect: a require_output_glob violation was
// never treated as a shape failure, so the ONE corrective retry the primitive
// is documented to get never fired.
//
// container.go's guard emits:
//
//	"schema violation: output contract for step %q not met — no file matching
//	 %q was written during this step. You MUST write the declared output file
//	 before finishing."
//
// but classifyShapeFailure matched the narrower literal "schema violation: role"
// (the missing-required-keys message). The output-contract message has no
// "role", matched no other case, and fell through to shapeFailureNone — so
// isShapeFailure was false, no shape retry ran, and isModelShapedFailure (which
// keys off shape failures) skipped the model fallback too. The step went
// straight to on_fail on its first attempt.
//
// That contradicted three places that already assumed the broad prefix:
//   - registry.WorkflowStep.RequireOutputGlob's doc: "fails with a
//     'schema violation:' error (which the shape-retry layer corrects once
//     before giving up)"
//   - container.go's guard comment: "one corrective shape retry, then on_fail"
//   - shapeFailureMetricKind, which already labels any "schema violation:" as
//     "schema_violation" — so the metric claimed a schema violation while the
//     classifier reported no shape failure at all.
//
// Observed in production on fork task_20260728220230_f59791b80ccc36fe: the
// synthesize step produced exactly one response artifact and failed, with no
// *_shape_retry / *_model_fallback attempt — the writer that ran out of prompt
// budget never received the "You MUST write the declared output file" hint.

// outputContractErr builds the exact error container.go's require_output_glob
// guard produces, so the test breaks if that message is reworded.
// The message is built via Sprintf and then wrapped, rather than passed as a
// literal to fmt.Errorf: it must stay byte-identical to container.go's guard
// (which assembles it with Sprintf into a string), and that wording ends in a
// full stop that revive's error-strings rule rejects in an inline literal.
func outputContractErr(stepID, glob string) error {
	msg := fmt.Sprintf(
		"schema violation: output contract for step %q not met — no file matching %q was written during this step. You MUST write the declared output file before finishing.",
		stepID, glob)
	return errors.New(msg)
}

// The incident case: an output-contract violation MUST be a shape failure so
// the corrective retry fires.
func TestClassifyShapeFailure_OutputContractIsShapeFailure_T1089(t *testing.T) {
	err := outputContractErr("synthesize", "artifacts/out/deliverable.md")

	assert.Equal(t, shapeFailureJSON, classifyShapeFailure(err),
		"require_output_glob violation must classify as a shape failure (T-1089)")
	assert.True(t, isShapeFailure(err),
		"isShapeFailure must be true or no corrective retry runs")
	assert.True(t, isModelShapedFailure(err),
		"model fallback keys off shape failures; an output-contract miss should reach it")
}

// The classifier and the metric labeller must agree — they disagreed before the
// fix (metric said schema_violation, classifier said none).
func TestShapeFailureMetricKind_AgreesWithClassifier_OnOutputContract(t *testing.T) {
	err := outputContractErr("research", "artifacts/out/findings.md")
	kind := classifyShapeFailure(err)

	require.NotEqual(t, shapeFailureNone, kind,
		"classifier must not report 'no shape failure' for a message the metric labels schema_violation")
	assert.Equal(t, "schema_violation", shapeFailureMetricKind(err, kind))
}

// The pre-existing missing-required-keys path must keep classifying exactly as
// before — the fix broadens the matcher, it must not change this case.
func TestClassifyShapeFailure_RoleMissingKeysUnchanged(t *testing.T) {
	err := fmt.Errorf(`schema violation: role %q result.json is missing required keys: [message produced_files]`, "writer")

	assert.Equal(t, shapeFailureJSON, classifyShapeFailure(err))
	assert.Equal(t, "schema_violation", shapeFailureMetricKind(err, classifyShapeFailure(err)))
}

// Broadening the prefix must not swallow unrelated errors into the retry
// ladder: a container crash or a content-verification rejection is NOT a shape
// failure and must not earn a corrective re-run.
func TestClassifyShapeFailure_UnrelatedErrorsStillNotShapeFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"container exit", errors.New("container exited with code 137")},
		{"mtime floor", errors.New("claimed file report.md was not modified during this step")},
		{"plain timeout", errors.New("context deadline exceeded")},
		{"nil", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, shapeFailureNone, classifyShapeFailure(tc.err),
				"must not be pulled into the shape-retry ladder")
			assert.False(t, isShapeFailure(tc.err))
		})
	}
}

// Plausibility keeps its own kind (it selects a different corrective hint), and
// must not be reclassified as JSON by the broadened prefix.
func TestClassifyShapeFailure_PlausibilityStillDistinct(t *testing.T) {
	err := errors.New(`plausibility violation: role "writer" failed 1 rule(s): not_written_implies_reason: under condition writing.written=false, field "writing.reason" is missing or empty`)

	assert.Equal(t, shapeFailurePlausibility, classifyShapeFailure(err))
	assert.Equal(t, "plausibility", shapeFailureMetricKind(err, classifyShapeFailure(err)))
}
