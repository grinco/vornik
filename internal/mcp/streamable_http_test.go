package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// itoa keeps the SSE string literals in these tests readable.
func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func TestCallStreamableHTTP_JSONReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/json, text/event-stream", r.Header.Get("Accept"))
		assert.Equal(t, "2024-11-05", r.Header.Get("MCP-Protocol-Version"))
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Mcp-Session-Id", "S1")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"ok": true}})
	}))
	defer srv.Close()

	c := &Client{
		config:     ServerConfig{Name: "ha", Transport: "streamable-http", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer tok"}},
		logger:     zerolog.Nop(),
		httpClient: srv.Client(),
	}
	raw, err := c.callStreamableHTTP(context.Background(), "tools/list", map[string]any{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true}`, string(raw))
	assert.Equal(t, "S1", c.sessionID.Load().(string))
}

func TestCallStreamableHTTP_429IsToolRateLimitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := &Client{
		config:     ServerConfig{Name: "ha", Transport: "streamable-http", URL: srv.URL},
		logger:     zerolog.Nop(),
		httpClient: srv.Client(),
	}
	_, err := c.callStreamableHTTP(context.Background(), "tools/call", map[string]any{})
	var rl *ToolRateLimitError
	require.True(t, errors.As(err, &rl), "expected *ToolRateLimitError, got %v", err)
	assert.Equal(t, "ha", rl.Server)
}

func TestCallStreamableHTTP_500IsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "upstream boom")
	}))
	defer srv.Close()

	c := &Client{
		config:     ServerConfig{Name: "ha", Transport: "streamable-http", URL: srv.URL},
		logger:     zerolog.Nop(),
		httpClient: srv.Client(),
	}
	_, err := c.callStreamableHTTP(context.Background(), "tools/list", map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "streamable-http server returned 500")
	// The untrusted upstream body must NOT leak into the returned error.
	assert.NotContains(t, err.Error(), "upstream boom")
}

func TestCallStreamableHTTP_JSONErrorObjectSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": -32601, "message": "method not found"},
		})
	}))
	defer srv.Close()

	c := &Client{
		config:     ServerConfig{Name: "ha", Transport: "streamable-http", URL: srv.URL},
		logger:     zerolog.Nop(),
		httpClient: srv.Client(),
	}
	_, err := c.callStreamableHTTP(context.Background(), "tools/call", map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "method not found")
}

func TestNotify_StreamableHTTPPostsNotificationWithoutID(t *testing.T) {
	var gotMethod string
	var hasID bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// setStreamableHeaders must run on the notify path too.
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "2024-11-05", r.Header.Get("MCP-Protocol-Version"))
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		var raw map[string]any
		_ = json.Unmarshal(body, &raw)
		gotMethod, _ = raw["method"].(string)
		_, hasID = raw["id"]
		w.WriteHeader(http.StatusAccepted) // 202, no body
	}))
	defer srv.Close()

	c := &Client{
		config:     ServerConfig{Name: "ha", Transport: "streamable-http", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer tok"}},
		logger:     zerolog.Nop(),
		httpClient: srv.Client(),
	}
	require.NoError(t, c.notify("notifications/initialized", nil))
	assert.Equal(t, "notifications/initialized", gotMethod)
	assert.False(t, hasID, "a JSON-RPC notification must have no id field")
}

func TestNotify_StreamableHTTPNetworkErrorIsSwallowed(t *testing.T) {
	// Fire-and-forget: a transport failure must NOT fail (would otherwise
	// fail Connect via initialize). Point at a closed server.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // now unreachable

	c := &Client{
		config:     ServerConfig{Name: "ha", Transport: "streamable-http", URL: url},
		logger:     zerolog.Nop(),
		httpClient: &http.Client{},
	}
	assert.NoError(t, c.notify("notifications/initialized", nil))
}

func TestConnect_StreamableHTTPRequiresURL(t *testing.T) {
	_, err := Connect(context.Background(),
		ServerConfig{Name: "ha", Transport: "streamable-http"}, zerolog.Nop())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "url is required")
}

func TestConnect_UnknownTransportListsStreamableHTTP(t *testing.T) {
	_, err := Connect(context.Background(),
		ServerConfig{Name: "x", Transport: "carrier-pigeon"}, zerolog.Nop())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "use stdio, sse, or streamable-http")
}

func TestCallStreamableHTTP_SSEReplyAndSessionEcho(t *testing.T) {
	var sawSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawSession = r.Header.Get("Mcp-Session-Id")
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"x\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"id\":"+itoa(req.ID)+",\"result\":{\"v\":42}}\n\n")
	}))
	defer srv.Close()

	c := &Client{
		config:     ServerConfig{Name: "ha", Transport: "streamable-http", URL: srv.URL},
		logger:     zerolog.Nop(),
		httpClient: srv.Client(),
	}
	c.sessionID.Store("S9")
	raw, err := c.callStreamableHTTP(context.Background(), "tools/call", map[string]any{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"v":42}`, string(raw))
	assert.Equal(t, "S9", sawSession)
}

func TestStreamableHTTP_EndToEnd(t *testing.T) {
	var (
		mu         sync.Mutex
		gotInitzd  bool
		echoedSess []string // session id seen on non-initialize requests
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		defer mu.Unlock()
		writeJSON := func(result any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-xyz")
			writeJSON(map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}})
		case "notifications/initialized":
			gotInitzd = true
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			echoedSess = append(echoedSess, r.Header.Get("Mcp-Session-Id"))
			writeJSON(map[string]any{"tools": []map[string]any{
				{"name": "GetLiveContext", "description": "HA state", "inputSchema": map[string]any{"type": "object"}},
			}})
		case "tools/call":
			echoedSess = append(echoedSess, r.Header.Get("Mcp-Session-Id"))
			// The Bearer header (from cfg.Headers) and the call args must
			// survive the full Connect path, not just a bare callStreamableHTTP.
			assert.Equal(t, "Bearer ha-token", r.Header.Get("Authorization"))
			assert.Contains(t, string(body), "GetLiveContext")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"id\":"+itoa(req.ID)+
				",\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"living room: 21C\"}]}}\n\n")
		}
	}))
	defer srv.Close()

	cfg := ServerConfig{
		Name:      "home-assistant",
		Transport: "streamable-http",
		URL:       srv.URL,
		Headers:   map[string]string{"Authorization": "Bearer ha-token"},
	}
	c, err := Connect(context.Background(), cfg, zerolog.Nop())
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	assert.Equal(t, []string{"GetLiveContext"}, toolNames(c.Tools()))

	res, err := c.CallTool(context.Background(), "GetLiveContext", json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.Contains(t, res.Text(), "living room: 21C")

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, gotInitzd, "server never received notifications/initialized")
	require.NotEmpty(t, echoedSess)
	for _, s := range echoedSess {
		assert.Equal(t, "sess-xyz", s, "client must echo the initialize-assigned session id")
	}
}
