// Coverage rollup for the small per-file helpers that are easy to
// hit in isolation but were missed by package-level sweeps:
//   - priority_queue: QueuedProvider.CompleteWithTools / Stream
//   - stream.go: readLimited
//   - codex_subscription_convert: buildRequestBody (incl. tool-schema +
//     tool-call round-trip + image-block rejection)
//
// Each test below pins behaviour rather than asking for tool output —
// these are pure-Go helpers with no network dependency.

package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// --- QueuedProvider Complete variants -----------------------------------

// recordingProvider counts each method invocation and what message
// content / tool name it saw, so QueuedProvider tests can verify the
// inner provider was called with the right args.
type recordingProvider struct {
	namedStubProvider
	completeHits       int
	completeWithHits   int
	completeStreamHits int
	lastToolNames      []string
	streamCallback     bool
}

func (r *recordingProvider) Complete(_ context.Context, msgs []Message) (*ChatResponse, error) {
	r.completeHits++
	return &ChatResponse{ID: "complete", Model: msgs[0].Content}, nil
}

func (r *recordingProvider) CompleteWithTools(_ context.Context, _ []Message, tools []Tool) (*ChatResponse, error) {
	r.completeWithHits++
	for _, t := range tools {
		r.lastToolNames = append(r.lastToolNames, t.Function.Name)
	}
	return &ChatResponse{ID: "with-tools"}, nil
}

func (r *recordingProvider) CompleteWithToolsStream(_ context.Context, _ []Message, _ []Tool, onText StreamCallback) (*ChatResponse, error) {
	r.completeStreamHits++
	if onText != nil {
		r.streamCallback = true
		onText("chunk-1")
	}
	return &ChatResponse{ID: "stream"}, nil
}

func TestQueuedProvider_CompleteWithTools_DispatchesThroughInner(t *testing.T) {
	inner := &recordingProvider{namedStubProvider: namedStubProvider{name: "x"}}
	q := NewQueuedProvider(inner, 1)
	tools := []Tool{
		{Type: "function", Function: ToolFunction{Name: "search"}},
		{Type: "function", Function: ToolFunction{Name: "create"}},
	}
	resp, err := q.CompleteWithTools(context.Background(),
		[]Message{{Role: "user", Content: "find me X"}}, tools)
	if err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	if resp.ID != "with-tools" {
		t.Errorf("ID: got %q, want with-tools", resp.ID)
	}
	if inner.completeWithHits != 1 {
		t.Errorf("inner.completeWithHits = %d, want 1", inner.completeWithHits)
	}
	if len(inner.lastToolNames) != 2 || inner.lastToolNames[0] != "search" {
		t.Errorf("tool names lost in transit: %v", inner.lastToolNames)
	}
}

func TestQueuedProvider_CompleteWithToolsStream_PreservesCallback(t *testing.T) {
	inner := &recordingProvider{namedStubProvider: namedStubProvider{name: "x"}}
	q := NewQueuedProvider(inner, 1)
	var got string
	resp, err := q.CompleteWithToolsStream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil,
		func(s string) { got += s })
	if err != nil {
		t.Fatalf("CompleteWithToolsStream: %v", err)
	}
	if resp.ID != "stream" {
		t.Errorf("ID: got %q, want stream", resp.ID)
	}
	if !inner.streamCallback {
		t.Error("inner provider did not see the stream callback")
	}
	if got != "chunk-1" {
		t.Errorf("stream output: got %q, want chunk-1", got)
	}
}

// --- stream.readLimited -------------------------------------------------

type fixedReader struct {
	data []byte
	off  int
	err  error
}

func (r *fixedReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func TestReadLimited_Truncates(t *testing.T) {
	r := &fixedReader{data: []byte(strings.Repeat("a", 200))}
	got, _ := readLimited(r, 32)
	if len(got) != 32 {
		t.Errorf("len = %d, want 32", len(got))
	}
}

func TestReadLimited_PassesError(t *testing.T) {
	wantErr := errors.New("eof")
	r := &fixedReader{data: []byte("hi"), err: wantErr}
	// First call returns "hi" + nil, but we don't iterate; readLimited
	// just calls Read once.
	got, _ := readLimited(r, 32)
	if string(got) != "hi" {
		t.Errorf("got %q, want hi", got)
	}
}

// --- codex_subscription_convert.buildRequestBody ------------------------

func TestCodexBuildRequestBody_SimpleUserTurn(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4-mini")
	body, err := c.buildRequestBody(
		[]Message{
			{Role: "system", Content: "be concise"},
			{Role: "user", Content: "hello"},
		}, nil)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if out["instructions"] != "be concise" {
		t.Errorf("instructions: got %v, want 'be concise'", out["instructions"])
	}
	input, _ := out["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input items: got %d, want 1", len(input))
	}
}

func TestCodexBuildRequestBody_WithToolsAndEffortLevel(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4",
		WithCodexSubscriptionEffortLevel("high"))
	tools := []Tool{
		{Type: "function", Function: ToolFunction{
			Name:        "search",
			Description: "Search the web",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		}},
		// Tool with empty parameters — should be defaulted to {} schema.
		{Type: "function", Function: ToolFunction{Name: "ping"}},
	}
	body, err := c.buildRequestBody(
		[]Message{{Role: "user", Content: "go"}}, tools)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if out["tool_choice"] != "auto" {
		t.Errorf("tool_choice: got %v, want auto", out["tool_choice"])
	}
	reasoning, ok := out["reasoning"].(map[string]any)
	if !ok {
		t.Fatal("reasoning block missing despite effort level")
	}
	if reasoning["effort"] != "high" {
		t.Errorf("reasoning.effort: got %v, want high", reasoning["effort"])
	}
	conv, _ := out["tools"].([]any)
	if len(conv) != 2 {
		t.Errorf("tools count: got %d, want 2", len(conv))
	}
}

func TestCodexBuildRequestBody_RejectsImageBlock(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4")
	msgs := []Message{
		{Role: "user", Blocks: []ContentBlock{
			{Type: "image_url", ImageURL: &ImageURLContent{URL: "https://example.com/x.png"}},
		}},
	}
	_, err := c.buildRequestBody(msgs, nil)
	if err == nil {
		t.Error("expected error for image_url block, got nil")
	}
}

func TestCodexBuildRequestBody_AssistantToolCallRoundTrip(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4")
	msgs := []Message{
		{Role: "user", Content: "search"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID:   "tool-1",
			Type: "function",
			Function: FunctionCall{
				Name:      "search",
				Arguments: `{"q":"foo"}`,
			},
		}}},
		{Role: "tool", ToolCallID: "tool-1", Content: `{"hits":[]}`},
	}
	body, err := c.buildRequestBody(msgs, nil)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	input, _ := out["input"].([]any)
	// We expect 3 input items: user message, function_call, function_call_output.
	if len(input) != 3 {
		t.Errorf("input items: got %d, want 3", len(input))
	}
	// Last item carries the tool result.
	last, _ := input[2].(map[string]any)
	if last["type"] != "function_call_output" {
		t.Errorf("last input type: got %v, want function_call_output", last["type"])
	}
	if last["call_id"] != "tool-1" {
		t.Errorf("call_id: got %v, want tool-1", last["call_id"])
	}
}

func TestCodexBuildRequestBody_EmptyArgumentsDefaulted(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4")
	msgs := []Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID:   "tc-x",
			Type: "function",
			Function: FunctionCall{
				Name:      "ping",
				Arguments: "",
			},
		}}},
	}
	body, err := c.buildRequestBody(msgs, nil)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	if !strings.Contains(string(body), `"arguments":"{}"`) {
		t.Errorf("expected empty arguments defaulted to '{}'; body=%s", string(body))
	}
}

func TestCodexBuildRequestBody_UnknownRoleRejected(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4")
	_, err := c.buildRequestBody(
		[]Message{{Role: "unknown", Content: "x"}}, nil)
	if err == nil {
		t.Error("unknown role: expected error, got nil")
	}
}

func TestCodexBuildRequestBody_JoinInstructions_MultipleSystems(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4")
	body, err := c.buildRequestBody(
		[]Message{
			{Role: "system", Content: "a"},
			{Role: "system", Content: "b"},
			{Role: "user", Content: "hi"},
		}, nil)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	if !strings.Contains(string(body), `"instructions":"a\n\nb"`) {
		t.Errorf("instructions not joined; body=%s", string(body))
	}
}
