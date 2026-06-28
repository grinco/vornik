package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testTimeout = 5 * time.Second

func TestPingCompletion_InvokesCompleteWithMaxTokensOne(t *testing.T) {
	var recordedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recordedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"."},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "test-model", WithTimeout(testTimeout))
	if err := c.PingCompletion(context.Background()); err != nil {
		t.Fatalf("PingCompletion returned error on 200: %v", err)
	}

	var req map[string]any
	if err := json.Unmarshal([]byte(recordedBody), &req); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if req["model"] != "test-model" {
		t.Errorf("ping model = %v, want test-model", req["model"])
	}
	if mt, _ := req["max_tokens"].(float64); mt != 1 {
		t.Errorf("ping max_tokens = %v, want 1 (minimal cost)", req["max_tokens"])
	}
	if req["stream"] != nil {
		t.Errorf("ping must not request streaming, got stream=%v", req["stream"])
	}
}

func TestPingCompletion_ReturnsErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "bad-key", "test-model", WithTimeout(testTimeout))
	err := c.PingCompletion(context.Background())
	if err == nil {
		t.Fatal("PingCompletion should return error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("PingCompletion error should mention status 401, got: %v", err)
	}
}
