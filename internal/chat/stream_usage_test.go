package chat

import (
	"encoding/json"
	"testing"
)

// An OpenAI-compatible server omits usage from a streamed response unless
// stream_options.include_usage is set. Vornik set "stream": true and nothing
// else, so every streamed completion reported zero prompt tokens, zero
// completion tokens and zero cost — the parser had always read usage, it was
// just never sent any.
//
// Confirmed on the self-hosted vLLM, 2026-08-16: prefix caching is enabled and
// effective there (~2.9x on a repeated prefix), and usage reporting is gated
// behind exactly this flag.
func TestStreamRequest_AsksForUsage(t *testing.T) {
	// Mirrors the literal built in ProcessStreaming.
	type streamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	}
	type streamRequest struct {
		ChatRequest
		Stream        bool          `json:"stream"`
		StreamOptions streamOptions `json:"stream_options"`
	}
	req := streamRequest{
		ChatRequest:   ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}},
		Stream:        true,
		StreamOptions: streamOptions{IncludeUsage: true},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so, ok := got["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options absent from %s", b)
	}
	if so["include_usage"] != true {
		t.Errorf("include_usage = %v, want true — without it the server sends no usage at all", so["include_usage"])
	}
}
