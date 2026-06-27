package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// encodeOKResponse writes a minimal valid chat completion so the client's
// happy path completes without erroring — tests that only care about the
// outbound request use it as the server reply.
func encodeOKResponse(w http.ResponseWriter) {
	resp := ChatResponse{
		ID:    "test",
		Model: "m",
		Choices: []struct {
			Index        int     `json:"index"`
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		}{{Index: 0, Message: Message{Role: "assistant", Content: "ok"}}},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// TestClient_WithExtraHeaders_SentOnCompletion verifies the static extra
// headers (OpenRouter's HTTP-Referer / X-Title attribution) ride every
// completion request alongside — not instead of — the auth header.
func TestClient_WithExtraHeaders_SentOnCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer or-key", r.Header.Get("Authorization"))
		assert.Equal(t, "vornik/2026.4.5", r.Header.Get("HTTP-Referer"))
		assert.Equal(t, "vornik", r.Header.Get("X-Title"))
		encodeOKResponse(w)
	}))
	defer server.Close()

	client := NewClient(server.URL, "or-key", "deepseek/deepseek-r1:free",
		WithExtraHeaders(map[string]string{
			"HTTP-Referer": "vornik/2026.4.5",
			"X-Title":      "vornik",
		}),
	)
	_, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	require.NoError(t, err)
}

// TestClient_WithExtraHeaders_SentOnModelsList verifies the same headers
// ride the /models discovery request — OpenRouter ranks attribution off
// any authenticated call, and Ping piggybacks on ListModels.
func TestClient_WithExtraHeaders_SentOnModelsList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/models", r.URL.Path)
		assert.Equal(t, "vornik/2026.4.5", r.Header.Get("HTTP-Referer"))
		assert.Equal(t, "vornik", r.Header.Get("X-Title"))
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek/deepseek-r1:free"}]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "or-key", "m",
		WithExtraHeaders(map[string]string{
			"HTTP-Referer": "vornik/2026.4.5",
			"X-Title":      "vornik",
		}),
	)
	_, err := client.ListModels(context.Background())
	require.NoError(t, err)
}

// TestClient_WithExtraHeaders_NilAndEmpty verifies that an unset / empty
// extra-headers map leaves the request shape exactly as before — only the
// auth header is present.
func TestClient_WithExtraHeaders_NilAndEmpty(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"nil", nil},
		{"empty", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer or-key", r.Header.Get("Authorization"))
				assert.Empty(t, r.Header.Get("HTTP-Referer"))
				assert.Empty(t, r.Header.Get("X-Title"))
				encodeOKResponse(w)
			}))
			defer server.Close()

			client := NewClient(server.URL, "or-key", "m", WithExtraHeaders(tc.headers))
			_, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
			require.NoError(t, err)
		})
	}
}

// TestClient_WithExtraHeaders_CannotOverrideAuth guards the invariant that
// extra headers never clobber the real Authorization header — a fat-
// fingered config can't silently disable auth.
func TestClient_WithExtraHeaders_CannotOverrideAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer real-key", r.Header.Get("Authorization"),
			"extra header named Authorization must not override the configured api key")
		encodeOKResponse(w)
	}))
	defer server.Close()

	client := NewClient(server.URL, "real-key", "m",
		WithExtraHeaders(map[string]string{"Authorization": "Bearer hijacked"}),
	)
	_, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	require.NoError(t, err)
}
