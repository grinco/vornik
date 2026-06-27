package chat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestClaudeSubscription_CompleteWithToolsAndStream drives the
// CompleteWithTools + CompleteWithToolsStream entry points so the
// per-method coverage isn't zero. Same SSE shape as the existing
// TestClient_SetsAllExpectedHeaders.
func TestClaudeSubscription_CompleteWithToolsAndStream(t *testing.T) {
	credsDir := t.TempDir()
	credsPath := filepath.Join(credsDir, ".credentials.json")
	claudeJSON := filepath.Join(credsDir, ".claude.json")
	if err := os.WriteFile(credsPath, []byte(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"r","expiresAt":99999999999999}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeJSON, []byte(`{"userID":"u","oauthAccount":{"accountUuid":"a"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"x","model":"m","usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n"+
			"event: content_block_start\n"+
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n"+
			"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`+"\n\n"+
			"event: content_block_stop\n"+
			`data: {"type":"content_block_stop","index":0}`+"\n\n"+
			"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`+"\n\n"+
			"event: message_stop\n"+
			`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)

	c := NewClaudeSubscriptionClient("claude-sonnet-4-6",
		WithClaudeSubscriptionAuthPath(credsPath),
	)
	c.account = &claudeAccountResolver{path: claudeJSON}

	// CompleteWithTools — calls c.call under the hood; success.
	resp, err := c.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	if resp == nil {
		t.Fatal("response is nil")
	}

	// CompleteWithToolsStream — same call path, with an onText callback.
	var streamed string
	resp, err = c.CompleteWithToolsStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
		func(s string) { streamed += s })
	if err != nil {
		t.Fatalf("CompleteWithToolsStream: %v", err)
	}
	if resp == nil {
		t.Fatal("stream response is nil")
	}
	if streamed != "ok" {
		t.Errorf("streamed text = %q, want 'ok'", streamed)
	}
}

// TestClaudeSubscription_Call_EmptyModel covers the model-required guard.
func TestClaudeSubscription_Call_EmptyModel(t *testing.T) {
	c := NewClaudeSubscriptionClient("")
	c.model = "" // explicit, even though ctor sets a default
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Error("empty model should error")
	}
}
