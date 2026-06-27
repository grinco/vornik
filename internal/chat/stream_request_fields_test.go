package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamRequest_CarriesMaxTokensAndResponseFormat — the streaming
// path's request literal pre-fix copied only Model / Messages / Tools /
// Options. Roles configured with chat.max_tokens got uncapped output;
// roles with responseFormat: json_object had structured-output
// enforcement silently bypassed. Pin the contract: the request body
// posted to the upstream endpoint MUST contain both fields when set.
func TestStreamRequest_CarriesMaxTokensAndResponseFormat(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		// Send a minimal SSE response so the parser doesn't error.
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := &Client{
		endpoint:   srv.URL,
		model:      "test-model",
		maxTokens:  4096,
		httpClient: &http.Client{},
	}
	ctx := WithRequestResponseFormatStruct(context.Background(),
		&ResponseFormat{Type: "json_object"})

	_, _ = c.CompleteWithToolsStream(ctx, []Message{{Role: "user", Content: "hi"}}, nil, nil)

	if captured == nil {
		t.Fatal("upstream never received a request body")
	}
	if got, _ := captured["max_tokens"].(float64); int(got) != 4096 {
		t.Errorf("max_tokens: got %v, want 4096 (carried from c.maxTokens)", captured["max_tokens"])
	}
	rf, ok := captured["response_format"].(map[string]any)
	if !ok {
		t.Errorf("response_format missing; got %v", captured["response_format"])
	} else if rf["type"] != "json_object" {
		t.Errorf("response_format.type = %v, want json_object", rf["type"])
	}
}

// TestStreamScanner_BufferLargeEnoughForToolCallDeltas — pre-fix
// the SSE scanner used the default 64KB cap and silently truncated
// large tool-call argument deltas. Verify the buffer override
// accepts a single 100KB SSE line without hitting bufio.ErrTooLong.
// We assert by feeding a synthetic 100KB SSE line and confirming
// the scanner reads it whole rather than returning io.EOF early.
func TestStreamScanner_BufferLargeEnoughForToolCallDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 100KB of valid (if pointless) JSON-shape content embedded in
		// a single SSE data: line. The default 64KB scanner cap would
		// truncate this; the 16MB override must accept it.
		bigArg := strings.Repeat("x", 100*1024)
		evt := `data: {"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + bigArg + `"}}]}` + "\n\n"
		_, _ = w.Write([]byte(evt))
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
		t.Fatalf("streaming failed on 100KB SSE line: %v", err)
	}
	// The 100KB content was embedded in the delta; if the scanner
	// truncated, we'd have lost most of it. Verify the response
	// content has the expected length.
	if got := len(resp.Choices[0].Message.Content); got < 100*1024 {
		t.Errorf("content length = %d, want >= 100KB (scanner truncated?)", got)
	}
}

// shut up unused linter
var _ = bytes.NewReader
