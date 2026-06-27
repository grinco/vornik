package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStreamToolCalls_NonZeroIndexAssembled — operator-observed via
// Home Assistant's Ollama-Conversation integration talking to vornik
// → Vertex (google/gemma-4-26b-a4b-it-maas). Multi-turn requests
// (prompt_eval_count ≈ 2800) returned `done_reason: "tool_calls"`
// but `tool_calls: None` in the parsed Ollama response, so HA never
// executed the call.
//
// Root cause: the SSE assembler aggregated tool_call deltas into
// map[int]*ToolCall keyed by the delta's `index` field, then built
// the final slice with `for i := 0; i < len(map); i++ { map[i] }`.
// That loop assumes keys are dense starting at 0. When Vertex
// streamed the first tool_call with `"index": 1` (its emitter
// continues numbering across the conversation, not within the
// response), the map had one entry at key 1; the loop only checked
// key 0 and produced an empty slice.
//
// Regression guard: a single tool_call delta with index=1 must
// survive into the assembled response. Same shape, idx=2, also
// verified to cover gap-from-zero cases.
func TestStreamToolCalls_NonZeroIndexAssembled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// One tool_call streamed across two deltas (name first,
		// arguments next), index=1 in both. Matches the Vertex/Gemini
		// emission pattern the HA failure trace shows.
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":1,"id":"call_abc","type":"function","function":{"name":"current_time","arguments":""}}]}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := &Client{
		endpoint:   srv.URL,
		model:      "test-model",
		httpClient: &http.Client{},
	}
	resp, err := c.CompleteWithToolsStream(context.Background(),
		[]Message{{Role: "user", Content: "what time is it?"}}, nil, nil)
	if err != nil {
		t.Fatalf("streaming failed: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatalf("no choices in response")
	}
	got := resp.Choices[0].Message.ToolCalls
	if len(got) != 1 {
		t.Fatalf("assembled tool calls = %d, want 1 (lost by index-0-only loop)", len(got))
	}
	if got[0].Function.Name != "current_time" {
		t.Errorf("tool name = %q, want %q", got[0].Function.Name, "current_time")
	}
	if got[0].Function.Arguments != "{}" {
		t.Errorf("tool arguments = %q, want %q", got[0].Function.Arguments, "{}")
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want %q", resp.Choices[0].FinishReason, "tool_calls")
	}
}

func TestStreamToolCalls_ExtraContentPreserved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_sig","type":"function","extra_content":{"google":{"thought_signature":"sig"}},"function":{"name":"create_task","arguments":"{}"}}]}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := &Client{
		endpoint:   srv.URL,
		model:      "google/gemini-3-pro",
		httpClient: &http.Client{},
	}
	resp, err := c.CompleteWithToolsStream(context.Background(),
		[]Message{{Role: "user", Content: "create"}}, nil, nil)
	if err != nil {
		t.Fatalf("streaming failed: %v", err)
	}
	got := resp.Choices[0].Message.ToolCalls
	if len(got) != 1 {
		t.Fatalf("assembled tool calls = %d, want 1", len(got))
	}
	if string(got[0].ExtraContent) != `{"google":{"thought_signature":"sig"}}` {
		t.Fatalf("extra_content = %s", got[0].ExtraContent)
	}
}

// TestStreamToolCalls_SparseIndicesAllPreserved — two tool calls
// with indices 0 and 2 (gap at 1). The old loop checked 0..len-1
// = 0..1, would find idx=0 but not idx=2 (and find no entry at
// idx=1). Verify both make it through and order matches the
// numerical index.
func TestStreamToolCalls_SparseIndicesAllPreserved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"c0","type":"function","function":{"name":"tool_a","arguments":"{}"}}]}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"c2","type":"function","function":{"name":"tool_c","arguments":"{}"}}]}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := &Client{
		endpoint:   srv.URL,
		model:      "test-model",
		httpClient: &http.Client{},
	}
	resp, err := c.CompleteWithToolsStream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("streaming failed: %v", err)
	}
	got := resp.Choices[0].Message.ToolCalls
	if len(got) != 2 {
		t.Fatalf("assembled tool calls = %d, want 2", len(got))
	}
	if got[0].Function.Name != "tool_a" || got[1].Function.Name != "tool_c" {
		t.Errorf("tool order = [%s, %s], want [tool_a, tool_c]",
			got[0].Function.Name, got[1].Function.Name)
	}
}
