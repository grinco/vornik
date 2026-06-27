package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChatClientAssistant_HappyPath — spins up a fake OpenAI-
// compatible /v1/chat/completions endpoint, sends a request,
// asserts the adapter returns the response text. Covers the
// "real LLM path" without requiring a real LLM.
func TestChatClientAssistant_HappyPath(t *testing.T) {
	var capturedBody string
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "fake-1",
			"object": "chat.completion",
			"model":  "fake-model",
			"choices": []map[string]any{
				{
					"index":         0,
					"finish_reason": "stop",
					"message": map[string]any{
						"role":    "assistant",
						"content": "Drafted by the fake server.",
					},
				},
			},
		})
	}))
	defer srv.Close()

	adapter := NewChatClientAssistant(srv.URL, "test-key", 5*time.Second)
	got, err := adapter.Complete(context.Background(), "fake-model", "be helpful", "draft a prompt")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Drafted by the fake server.", got.Text)

	// The chat.Client sends an OpenAI-style request — confirm
	// our user + system messages reached the gateway and the
	// API key was carried via Authorization: Bearer.
	assert.Contains(t, capturedBody, "be helpful")
	assert.Contains(t, capturedBody, "draft a prompt")
	assert.Contains(t, capturedBody, `"model":"fake-model"`)
	assert.Equal(t, "Bearer test-key", capturedAuth)
}

// TestChatClientAssistant_EmptyEndpoint — defensive: an unset
// endpoint at construction (operator forgot to wire the daemon)
// surfaces with a clear error before any HTTP machinery fires.
func TestChatClientAssistant_EmptyEndpoint(t *testing.T) {
	adapter := NewChatClientAssistant("", "k", 0)
	_, err := adapter.Complete(context.Background(), "m", "s", "u")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint is empty")
}

// TestChatClientAssistant_EmptyModel — similarly clear when
// the handler's model resolution returned an empty string
// (e.g. no project model + no daemon default configured).
func TestChatClientAssistant_EmptyModel(t *testing.T) {
	adapter := NewChatClientAssistant("http://x", "k", 0)
	_, err := adapter.Complete(context.Background(), "", "s", "u")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model is empty")
}

// TestChatClientAssistant_NoChoices — provider returns 200
// with an empty choices array (rare, but observed on some
// rate-limit edge cases). Adapter surfaces the issue rather
// than returning an empty suggestion.
func TestChatClientAssistant_NoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "fake-2",
			"object":  "chat.completion",
			"model":   "fake-model",
			"choices": []map[string]any{},
		})
	}))
	defer srv.Close()
	adapter := NewChatClientAssistant(srv.URL, "k", 5*time.Second)
	_, err := adapter.Complete(context.Background(), "fake-model", "s", "u")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "no choices"), "err = %v", err)
}

// TestChatClientAssistant_PropagatesHTTPError — upstream 5xx
// surfaces as an error so the handler can return 502 with the
// detail.
func TestChatClientAssistant_PropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	adapter := NewChatClientAssistant(srv.URL, "k", 5*time.Second)
	_, err := adapter.Complete(context.Background(), "fake-model", "s", "u")
	require.Error(t, err)
}

// TestChatClientAssistant_PropagatesCacheTokens — OpenAI-compat
// gateways that front Bedrock/Anthropic emit cache_creation_tokens
// + cache_read_tokens on prompt-prefix cache hits (Phase A of the
// LLM-caching rollout). The adapter must thread them through so
// the `_authoring` source on /ui/spend shows the same hit-ratio +
// savings columns as the dispatcher / executor sources.
func TestChatClientAssistant_PropagatesCacheTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "fake-cache",
			"object": "chat.completion",
			"model":  "fake-model",
			"choices": []map[string]any{
				{
					"index":         0,
					"finish_reason": "stop",
					"message": map[string]any{
						"role":    "assistant",
						"content": "cached",
					},
				},
			},
			"usage": map[string]any{
				"prompt_tokens":         500,
				"completion_tokens":     60,
				"cache_creation_tokens": 1200,
				"cache_read_tokens":     4800,
			},
		})
	}))
	defer srv.Close()
	adapter := NewChatClientAssistant(srv.URL, "k", 5*time.Second)
	got, err := adapter.Complete(context.Background(), "fake-model", "s", "u")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1200, got.CacheCreationTokens)
	assert.Equal(t, 4800, got.CacheReadTokens)
}
