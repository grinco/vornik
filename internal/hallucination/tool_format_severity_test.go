package hallucination

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// INCIDENT 2026-09-16 — task_20260916163809_c16163e002783786.
// `exec_20260916163809_287b70afe781a3b2` ran a design review for 8m08s,
// produced its output, and was then failed by its own gate:
//
//	hallucination detector found 1 unsupported claim(s):
//	tool_format="file_write</tool>"
//
// `hallucinated_tool_format` is not a claim about the world. It is an
// observation about the MODEL'S TOOL-CALL SYNTAX, and Severity's own contract
// reserves High for "grounded-by-construction claims: a URL the model quoted
// that doesn't appear in any tool input/output, a task_id that doesn't exist,
// a project_id outside the registry", warning in the same sentence that
// "false positives here trigger expensive retries". This rule was placed at
// High against that definition, and the expensive retry is exactly what
// happened: three attempts, 12m17s, nothing salvaged.
//
// Blocking also has no path to success. A retry cannot teach a model to format
// tool calls, so the rung re-runs the whole step and the same malformed name
// comes back — which is what the ledger shows.
//
// Demoted to Warn: recorded, surfaced in the UI, not blocking. Same move, and
// the same reason, as url_not_fetched on 2026-08-26 (design §4a).
func TestHallucinatedToolFormat_IsWarnNotBlocking(t *testing.T) {
	gc := &GroundingContext{ToolCallNames: []string{`file_write</tool>`}}
	d := NewDefault()

	sigs := d.Scan("", gc)
	require.Len(t, sigs, 1, "the rule must still FIRE — demotion is about the consequence, not the detection")
	assert.Equal(t, "hallucinated_tool_format", sigs[0].Detector)
	assert.Equal(t, SeverityWarn, sigs[0].Severity,
		"a malformed tool NAME is a harness-syntax observation, not a grounded claim")
	assert.False(t, d.ShouldBlock(sigs),
		"an 8-minute review must not be destroyed by a signal no retry can fix")
}

// The argument half of the same rule stays HIGH, and the asymmetry is the
// point (review-20260916-2c6c, finding 3). Same detection class, different
// blast radius:
//
//   - a malformed NAME does not dispatch. Nothing ran, so the step's output
//     contract sees the consequence and the demotion above costs nothing that
//     was not already covered.
//   - a malformed ARGUMENT dispatches. The name parsed, the runtime ran the
//     call, and a wrapper or tokenizer token went through inside a value. The
//     file IS written and the output contract IS satisfied — with corrupt
//     content. No downstream control sees that unless the corruption happens
//     to surface as a named artifact or a number.
//
// The 2026-09-16 incident is evidence about names. Demoting the argument half
// on that evidence was an over-generalisation, corrected here before it
// shipped anywhere.
func TestHallucinatedToolArgsFormat_StaysBlocking(t *testing.T) {
	gc := &GroundingContext{
		ToolCallNames:  []string{"run_shell"},
		ToolCallInputs: []string{`{"command":"ls<arg_value>"}`},
	}
	d := NewDefault()

	sigs := d.Scan("", gc)
	require.NotEmpty(t, sigs)
	var args []Signal
	for _, s := range sigs {
		if s.ClaimType == "tool_args_format" {
			args = append(args, s)
		}
	}
	require.NotEmpty(t, args, "the argument half must still fire")
	for _, s := range args {
		assert.Equal(t, SeverityHigh, s.Severity,
			"a dispatched call with a corrupt argument has no other gate")
	}
	assert.True(t, d.ShouldBlock(args))
}

// The demotion must not reach the rules that ARE grounded by construction.
// Those are the ones the severity contract reserves High for, and the reason
// the detector blocks at all.
func TestGroundedRulesStillBlock(t *testing.T) {
	gc := &GroundingContext{KnownTaskIDs: map[string]struct{}{}}
	d := NewDefault()

	sigs := d.Scan("I checked task_20260101000000_aaaaaaaaaaaaaaaa for you.", gc)
	require.NotEmpty(t, sigs, "task_id_not_found must still fire")
	assert.True(t, d.ShouldBlock(sigs),
		"a task_id that does not exist is a grounded-by-construction claim and must still block")
}
