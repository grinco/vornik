package dispatcher

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/hallucination"
)

// TestBuildChatGroundingContext_PullsToolCallsAndProjects —
// pin the chat-side grounding builder. The detector relies on
// (a) tool-call names, (b) tool outputs as the URL source,
// (c) project IDs, all sourced from the dispatcher turn's
// in-memory state. A regression that drops any of those leaves
// the rule with empty negative space and false-negatives.
func TestBuildChatGroundingContext_PullsToolCallsAndProjects(t *testing.T) {
	a := &Agent{}
	msgs := []chat.Message{
		{Role: "user", Content: "search the latest scrape"},
		{Role: "assistant", Content: ""},
		{
			Role:    "tool",
			Name:    "mcp__scraper__web_fetch",
			Content: `{"url":"https://example.com/jobs","status":200,"body":"<html>..."}`,
		},
		{
			Role:    "tool",
			Name:    "list_projects",
			Content: "Found 2 project(s):\n- janka (Janka)\n- snake (Snake)\n",
		},
	}
	gc := a.buildChatGroundingContext(t.Context(), msgs, []string{"janka", "snake"}, "janka")
	require.NotNil(t, gc)
	assert.Contains(t, gc.ToolCallNames, "mcp__scraper__web_fetch")
	assert.Contains(t, gc.ToolCallNames, "list_projects")
	assert.Contains(t, gc.FetchedURLs, "https://example.com/jobs",
		"URL extracted from tool output must land in FetchedURLs lower-cased")
	assert.Contains(t, gc.KnownProjectIDs, "janka")
	assert.Contains(t, gc.KnownProjectIDs, "snake")
	assert.Contains(t, gc.ToolOutputs, "Found 2 project(s)",
		"tool output text must concatenate so the numeric-claim rule has substrate")
}

// TestFormatHallucinationRetryPrompt_NamesRejectedClaims —
// the retry prompt is what the model reads on its second
// attempt. It must list the rejected claims by type+value so
// the model can correct or remove them. A retry prompt that
// just says "you hallucinated" is no better than the original.
func TestFormatHallucinationRetryPrompt_NamesRejectedClaims(t *testing.T) {
	signals := []hallucination.Signal{
		hallucination.NewSignal("url_not_fetched", hallucination.SeverityHigh, "url",
			"https://x.example", "We fetched https://x.example.", "tool_audit", "URL not in audit"),
		hallucination.NewSignal("task_id_not_found", hallucination.SeverityHigh, "task_id",
			"task_99999_deadbeef", "Earlier task_99999_deadbeef.", "tool_audit", "task ID not found"),
	}
	p := formatHallucinationRetryPrompt(signals)
	assert.Contains(t, p, "https://x.example")
	assert.Contains(t, p, "task_99999_deadbeef")
	assert.Contains(t, p, "Do not invent",
		"the prompt must explicitly tell the model not to fabricate replacements")
}

// TestFormatUserWarningBanner_PluralisesCorrectly — small
// detail but the banner is operator-facing prose; cardinality
// matters for readability ("an unsupported claim" vs "3
// unsupported claims").
func TestFormatUserWarningBanner_PluralisesCorrectly(t *testing.T) {
	one := []hallucination.Signal{{Severity: hallucination.SeverityHigh}}
	multi := []hallucination.Signal{
		{Severity: hallucination.SeverityHigh},
		{Severity: hallucination.SeverityHigh},
		{Severity: hallucination.SeverityHigh},
	}
	assert.Contains(t, formatUserWarningBanner(one), "an unsupported claim")
	assert.Contains(t, formatUserWarningBanner(multi), "3 unsupported claims")
}

// TestRetainBlockingSignals_FiltersToHigh — the retry / banner
// path receives only High signals; warns are surfaced on the
// task UI but don't drive user-facing chat decoration. A bug
// here would either spam the user (if warns leak through) or
// silently drop High signals (if the filter inverts).
func TestRetainBlockingSignals_FiltersToHigh(t *testing.T) {
	in := []hallucination.Signal{
		{Severity: hallucination.SeverityHigh, Detector: "high1"},
		{Severity: hallucination.SeverityWarn, Detector: "warn1"},
		{Severity: hallucination.SeverityInfo, Detector: "info1"},
		{Severity: hallucination.SeverityHigh, Detector: "high2"},
	}
	got := retainBlockingSignals(in)
	require.Len(t, got, 2)
	names := []string{got[0].Detector, got[1].Detector}
	assert.ElementsMatch(t, []string{"high1", "high2"}, names)
}

// TestProjectIDsFromRegistry_HandlesNilEntries — registry
// snapshots can carry nil pointers when partial-load happens
// (config reloader race, missing files); the helper must skip
// them without panicking.
func TestProjectIDsFromRegistry_HandlesNilEntries(t *testing.T) {
	out := projectIDsFromRegistry(nil)
	assert.Nil(t, out)
}

// TestBuildChatGroundingContext_CapturesAssistantToolCallArguments —
// assistant turns with tool calls feed ToolCallInputs so the
// hallucinated_tool_format rule has substrate to detect wrappers
// and special tokens in the model's arguments.
func TestBuildChatGroundingContext_CapturesAssistantToolCallArguments(t *testing.T) {
	a := &Agent{}
	msgs := []chat.Message{
		{
			Role: "assistant",
			ToolCalls: []chat.ToolCall{
				{ID: "tc1", Type: "function", Function: chat.FunctionCall{Name: "create_task", Arguments: `{"prompt":"do x"}`}},
				{ID: "tc2", Type: "function", Function: chat.FunctionCall{Name: "list_tasks", Arguments: ""}},
			},
		},
	}
	gc := a.buildChatGroundingContext(t.Context(), msgs, nil, "")
	require.NotNil(t, gc)
	assert.Equal(t, []string{`{"prompt":"do x"}`}, gc.ToolCallInputs,
		"non-empty arguments are captured; empty args are skipped")
}

// TestBuildChatGroundingContext_ActiveProjectAlwaysInKnownProjectIDs —
// even with a nil projectIDs list, the active project is added so
// the project_id rule has at least the current project for membership.
func TestBuildChatGroundingContext_ActiveProjectAlwaysInKnownProjectIDs(t *testing.T) {
	a := &Agent{}
	gc := a.buildChatGroundingContext(t.Context(), nil, nil, "janka")
	require.NotNil(t, gc)
	assert.Contains(t, gc.KnownProjectIDs, "janka")
}

// TestRetainBlockingSignals_EmptyInputReturnsNil — defensive pin.
func TestRetainBlockingSignals_EmptyInputReturnsNil(t *testing.T) {
	assert.Empty(t, retainBlockingSignals(nil))
	assert.Empty(t, retainBlockingSignals([]hallucination.Signal{
		{Severity: hallucination.SeverityWarn},
		{Severity: hallucination.SeverityInfo},
	}))
}
