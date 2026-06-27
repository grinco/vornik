package executor

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin §9 "Structured recovery-checkpoint actions" of
// https://docs.vornik.io: an OPTIONAL typed
// `action` on a decision checkpoint option, parsed + validated by the
// lead-outcome parser. Invalid/unknown actions are DEMOTED to plain
// prose options (fail-safe), never a hard parse failure.

// decisionEnvelope builds a minimal valid outcome=checkpoint /
// kind=decision envelope wrapping the supplied raw options JSON, so a
// test can assert on how the action survives ParseLeadOutcome.
func decisionEnvelope(t *testing.T, optionsJSON string) []byte {
	t.Helper()
	return []byte(`{"outcome":"checkpoint","checkpoint":{"kind":"decision","question":"What now?","options":` + optionsJSON + `}}`)
}

func TestParseLeadOutcome_RerouteWorkflowActionParses(t *testing.T) {
	data := decisionEnvelope(t, `[
		{"id":"reroute","label":"Re-run via the planner workflow","action":{"type":"reroute_workflow","workflow":"research-planner"}},
		{"id":"skip","label":"Skip the step","action":{"type":"skip"}}
	]`)
	out, ok, err := ParseLeadOutcome(data)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, out.Checkpoint)
	require.Len(t, out.Checkpoint.Options, 2)

	reroute := out.Checkpoint.Options[0]
	require.NotNil(t, reroute.Action, "valid reroute_workflow action must survive parsing")
	assert.Equal(t, CheckpointActionRerouteWorkflow, reroute.Action.Type)
	assert.Equal(t, "research-planner", reroute.Action.Workflow)

	skip := out.Checkpoint.Options[1]
	require.NotNil(t, skip.Action)
	assert.Equal(t, CheckpointActionSkip, skip.Action.Type)
}

func TestParseLeadOutcome_ModelFallbackAndRetryActionsParse(t *testing.T) {
	data := decisionEnvelope(t, `[
		{"id":"fb","label":"Retry on the fallback model","action":{"type":"model_fallback"}},
		{"id":"retry","label":"Retry the step as-is","action":{"type":"retry"}}
	]`)
	out, ok, err := ParseLeadOutcome(data)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, out.Checkpoint.Options, 2)
	assert.Equal(t, CheckpointActionModelFallback, out.Checkpoint.Options[0].Action.Type)
	assert.Equal(t, CheckpointActionRetry, out.Checkpoint.Options[1].Action.Type)
}

func TestParseLeadOutcome_UnknownActionTypeDemotedToProse(t *testing.T) {
	// Fail-safe: an unknown action.type must DROP the action (demote to
	// a plain prose option) — NOT fail the whole recovery checkpoint.
	data := decisionEnvelope(t, `[
		{"id":"weird","label":"Do something else","action":{"type":"teleport"}},
		{"id":"skip","label":"Skip","action":{"type":"skip"}}
	]`)
	out, ok, err := ParseLeadOutcome(data)
	require.NoError(t, err, "unknown action type must not hard-fail the parse")
	require.True(t, ok)
	require.Len(t, out.Checkpoint.Options, 2)
	assert.Nil(t, out.Checkpoint.Options[0].Action,
		"unknown action.type must be demoted to a prose option (action dropped)")
	assert.Equal(t, "Do something else", out.Checkpoint.Options[0].Label,
		"demotion must preserve the option's id/label so the operator still sees the choice")
	require.NotNil(t, out.Checkpoint.Options[1].Action, "the sibling valid action stays")
}

func TestParseLeadOutcome_RerouteWithoutWorkflowDemotedToProse(t *testing.T) {
	// reroute_workflow REQUIRES a non-empty workflow; missing it demotes
	// to prose rather than rejecting.
	data := decisionEnvelope(t, `[
		{"id":"reroute","label":"Re-run on another workflow","action":{"type":"reroute_workflow"}},
		{"id":"retry","label":"Retry","action":{"type":"retry"}}
	]`)
	out, ok, err := ParseLeadOutcome(data)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, out.Checkpoint.Options[0].Action,
		"reroute_workflow with empty workflow must be demoted to prose")
	assert.Equal(t, "Re-run on another workflow", out.Checkpoint.Options[0].Label)
}

func TestParseLeadOutcome_NoActionIsBackwardCompatible(t *testing.T) {
	// Backward-compatible: an option with no action behaves exactly as
	// today — no action attached, no error.
	data := decisionEnvelope(t, `[
		{"id":"a","label":"Option A"},
		{"id":"b","label":"Option B"}
	]`)
	out, ok, err := ParseLeadOutcome(data)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, out.Checkpoint.Options[0].Action)
	assert.Nil(t, out.Checkpoint.Options[1].Action)
}

// TestCheckpointOptionAction_RoundTripsThroughMetadata pins that the
// action serializes into checkpoint task_message metadata (so the
// persisted options array carries the action for the resume-path
// resolver to read back).
func TestCheckpointOptionAction_RoundTripsThroughMetadata(t *testing.T) {
	cp := &CheckpointPayload{
		Kind:     CheckpointKindDecision,
		Question: "What now?",
		Options: []CheckpointOption{
			{ID: "reroute", Label: "Re-run via planner", Action: &CheckpointOptionAction{Type: CheckpointActionRerouteWorkflow, Workflow: "research-planner"}},
			{ID: "plain", Label: "Just retry"},
		},
	}
	meta, err := SerializeCheckpointMetadata(cp)
	require.NoError(t, err)

	var back CheckpointPayload
	require.NoError(t, json.Unmarshal(meta, &back))
	require.Len(t, back.Options, 2)
	require.NotNil(t, back.Options[0].Action)
	assert.Equal(t, CheckpointActionRerouteWorkflow, back.Options[0].Action.Type)
	assert.Equal(t, "research-planner", back.Options[0].Action.Workflow)
	assert.Nil(t, back.Options[1].Action, "an option without an action stays omitempty in metadata")
}
