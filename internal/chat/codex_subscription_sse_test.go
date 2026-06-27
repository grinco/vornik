package chat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCodexResponsesSSE_TextDelta(t *testing.T) {
	// Test basic text streaming with delta events
	sseInput := `event: response.output_text.delta
data: {"delta":"Hello"}

event: response.output_text.delta
data: {"delta":" world"}

event: response.completed
data: {"response":{"id":"resp-123","usage":{"input_tokens":10,"output_tokens":5}}}

`
	var capturedText string
	onText := func(text string) {
		capturedText = text
	}

	resp, err := parseCodexResponsesSSE(strings.NewReader(sseInput), onText)
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, "Hello world", resp.Choices[0].Message.Content)
	assert.Equal(t, "resp-123", resp.ID)
	assert.Equal(t, 10, resp.Usage.PromptTokens)
	assert.Equal(t, 5, resp.Usage.CompletionTokens)
	assert.Equal(t, "stop", resp.Choices[0].FinishReason)
	assert.Equal(t, "Hello world", capturedText)
}

func TestParseCodexResponsesSSE_ToolCall(t *testing.T) {
	// Test function_call flow with arguments delta
	sseInput := `event: response.output_item.added
data: {"item":{"type":"function_call","call_id":"call-abc","name":"get_weather"}}

event: response.function_call_arguments.delta
data: {"delta":"{\""}

event: response.function_call_arguments.delta
data: {"delta":"location\":\"Prague\"}"}

event: response.function_call_arguments.done
data: {}

event: response.completed
data: {"response":{"id":"resp-456","usage":{"input_tokens":20,"output_tokens":15}}}

`
	resp, err := parseCodexResponsesSSE(strings.NewReader(sseInput), nil)
	require.NoError(t, err)
	require.NotNil(t, resp)

	require.Len(t, resp.Choices[0].Message.ToolCalls, 1)
	toolCall := resp.Choices[0].Message.ToolCalls[0]
	assert.Equal(t, "call-abc", toolCall.ID)
	assert.Equal(t, "function", toolCall.Type)
	assert.Equal(t, "get_weather", toolCall.Function.Name)
	assert.Equal(t, "{\"location\":\"Prague\"}", toolCall.Function.Arguments)
	assert.Equal(t, "tool_calls", resp.Choices[0].FinishReason)
}

func TestParseCodexResponsesSSE_Error(t *testing.T) {
	// Test response.error event returns error
	sseInput := `event: response.error
data: {"error":{"code":"rate_limited","message":"Too many requests"}}

`
	_, err := parseCodexResponsesSSE(strings.NewReader(sseInput), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate_limited")
	assert.Contains(t, err.Error(), "Too many requests")
}

func TestParseCodexResponsesSSE_DoneMarker(t *testing.T) {
	// Test [DONE] marker is handled gracefully
	sseInput := `event: response.output_text.delta
data: {"delta":"Hi"}

data: [DONE]

`
	resp, err := parseCodexResponsesSSE(strings.NewReader(sseInput), nil)
	require.NoError(t, err)
	assert.Equal(t, "Hi", resp.Choices[0].Message.Content)
}

func TestParseCodexResponsesSSE_DataOnlyEnvelopeAndUnterminatedFrame(t *testing.T) {
	// No event: header, event type comes from JSON payload type field.
	// Also no trailing blank line so the final frame is flushed at EOF.
	sseInput := `data: {"type":"response.output_text.delta","delta":"hi"}`

	resp, err := parseCodexResponsesSSE(strings.NewReader(sseInput), nil)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "hi", resp.Choices[0].Message.Content)
}

func TestParseCodexResponsesSSE_ResilientToCommentsUnknownFieldsAndBadJSON(t *testing.T) {
	sseInput := `
: heartbeat
id: 123

event: response.output_text.delta
data: {"delta":"ok"}

event: response.output_text.delta
data: not-json

event: response.output_text.delta
data: {"delta":""}

`

	resp, err := parseCodexResponsesSSE(strings.NewReader(sseInput), nil)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "ok", resp.Choices[0].Message.Content)
}

func TestParseCodexResponsesSSE_CompletedUsageFallbackAndIDFirstWriteWins(t *testing.T) {
	sseInput := `event: response.completed
data: {"response":{"id":"resp-first","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":0}}}

event: response.completed
data: {"response":{"id":"resp-second","usage":{"input_tokens":9,"output_tokens":8,"total_tokens":17}}}

`

	resp, err := parseCodexResponsesSSE(strings.NewReader(sseInput), nil)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// ID should only be set once (first non-empty ID wins).
	assert.Equal(t, "resp-first", resp.ID)
	// Latest usage event overwrites counts; explicit total is preserved.
	assert.Equal(t, 9, resp.Usage.PromptTokens)
	assert.Equal(t, 8, resp.Usage.CompletionTokens)
	assert.Equal(t, 17, resp.Usage.TotalTokens)
}

func TestParseCodexResponsesSSE_FunctionCallDoneWithoutCallIDDoesNotAppend(t *testing.T) {
	sseInput := `event: response.output_item.added
data: {"item":{"type":"function_call","name":"tool_without_id"}}

event: response.function_call_arguments.delta
data: {"delta":"{}"}

event: response.function_call_arguments.done
data: {}

`

	resp, err := parseCodexResponsesSSE(strings.NewReader(sseInput), nil)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Empty(t, resp.Choices[0].Message.ToolCalls)
	assert.Equal(t, "stop", resp.Choices[0].FinishReason)
}

func TestParseCodexResponsesSSE_DataPrefixLeadingSpaceIsTrimmed(t *testing.T) {
	sseInput := `event: response.output_text.delta
data: {"delta":"space-trimmed"}

`

	resp, err := parseCodexResponsesSSE(strings.NewReader(sseInput), nil)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "space-trimmed", resp.Choices[0].Message.Content)
}

func TestParseCodexResponsesSSE_OutputTextDoneAndCompletedWithInvalidResponsePayload(t *testing.T) {
	sseInput := `event: response.output_text.delta
data: {"delta":"ok"}

event: response.output_text.done
data: {}

event: response.completed
data: {"response":"not-an-object"}

`

	resp, err := parseCodexResponsesSSE(strings.NewReader(sseInput), nil)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "ok", resp.Choices[0].Message.Content)
	// Invalid response payload should be ignored without setting usage/id.
	assert.Equal(t, "", resp.ID)
	assert.Equal(t, 0, resp.Usage.PromptTokens)
	assert.Equal(t, 0, resp.Usage.CompletionTokens)
	assert.Equal(t, 0, resp.Usage.TotalTokens)
}

type failingReader struct {
	chunks [][]byte
	err    error
	idx    int
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.idx < len(r.chunks) {
		n := copy(p, r.chunks[r.idx])
		r.idx++
		return n, nil
	}
	return 0, r.err
}

func TestParseCodexResponsesSSE_ScannerReadError(t *testing.T) {
	r := &failingReader{
		chunks: [][]byte{[]byte("event: response.output_text.delta\ndata: {\"delta\":\"x\"}\n")},
		err:    assert.AnError,
	}

	resp, err := parseCodexResponsesSSE(r, nil)
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "sse read")
	assert.Contains(t, err.Error(), assert.AnError.Error())
}

func TestChooseFinishReason(t *testing.T) {
	tests := []struct {
		name         string
		hasToolCalls bool
		want         string
	}{
		{"no tool calls", false, "stop"},
		{"has tool calls", true, "tool_calls"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chooseFinishReason(tt.hasToolCalls)
			assert.Equal(t, tt.want, got)
		})
	}
}
