package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clientCovSSEServer stands up an httptest server that answers the MCP
// JSON-RPC handshake (initialize / tools/list) over the SSE transport's
// POST-to-/message shape and lets the caller plug in a per-request hook
// to assert on headers or branch on the tool-call body.
func clientCovSSEServer(t *testing.T, hook func(w http.ResponseWriter, r *http.Request, body []byte) (handled bool)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if hook != nil && hook(w, r, body) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": []any{}},
		})
	}))
}

// TestCallTool_ThrottleBlocksWithoutRPC verifies the in-daemon per-tool
// token bucket short-circuits CallTool: a drained bucket returns a
// *ToolRateLimitError carrying the configured Retry-After WITHOUT ever
// dialling the upstream. The whole point of the ceiling is to absorb the
// misbehaviour ourselves, so the transport must never be touched.
func TestCallTool_ThrottleBlocksWithoutRPC(t *testing.T) {
	var hits int
	srv := clientCovSSEServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		hits++
		return false
	})
	defer srv.Close()

	c := &Client{
		config: ServerConfig{
			Name:      "broker",
			ProjectID: "ibkr",
			Transport: "sse",
			URL:       srv.URL,
			ToolRateLimits: map[string]ToolRateLimitSpec{
				"place_order": {RPS: 1, Burst: 1},
			},
		},
		logger:     zerolog.Nop(),
		httpClient: srv.Client(),
	}
	c.toolLimiter = NewToolRateLimiter(c.config.ToolRateLimits)
	require.NotNil(t, c.toolLimiter, "spec is enabled, limiter must exist")

	// First call drains the single-token burst against the upstream...
	_, err := c.CallTool(context.Background(), "place_order", json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Equal(t, 1, hits, "first call should reach the upstream")

	// ...the second is rejected by the bucket before any RPC.
	_, err = c.CallTool(context.Background(), "place_order", json.RawMessage(`{}`))
	var rl *ToolRateLimitError
	require.True(t, errors.As(err, &rl), "expected *ToolRateLimitError, got %v", err)
	assert.Equal(t, "broker", rl.Server)
	assert.Equal(t, "place_order", rl.Tool)
	assert.GreaterOrEqual(t, rl.RetryAfter, time.Second)
	assert.Equal(t, 1, hits, "throttled call must NOT touch the upstream")
}

// TestCallTool_RejectsToolOutsideAllowlist locks the allowlist gate: a
// model-hallucinated tool name is refused before the RPC even when the
// upstream would have accepted it.
func TestCallTool_RejectsToolOutsideAllowlist(t *testing.T) {
	c := &Client{
		config: ServerConfig{Name: "gmail", AllowedTools: []string{"read_email"}},
		logger: zerolog.Nop(),
	}
	c.allowedSet = map[string]struct{}{"read_email": {}}

	_, err := c.CallTool(context.Background(), "delete_email", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in allowed_tools")
}

// TestCallSSE_ParsesToolResult exercises the SSE happy path end to end:
// the JSON-RPC envelope is unmarshalled and the ToolResult text surfaces.
func TestCallSSE_ParsesToolResult(t *testing.T) {
	srv := clientCovSSEServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if !contains(body, "tools/call") {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 2,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "pong"}},
			},
		})
		return true
	})
	defer srv.Close()

	c, err := Connect(context.Background(), ServerConfig{Name: "s", Transport: "sse", URL: srv.URL}, zerolog.Nop())
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	res, err := c.CallTool(context.Background(), "ping", json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "pong", res.Text())
}

// TestCallSSE_429SurfacesRateLimitError verifies the upstream-429 path maps
// to a *ToolRateLimitError with the Retry-After header honoured.
func TestCallSSE_429SurfacesRateLimitError(t *testing.T) {
	srv := clientCovSSEServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if !contains(body, "tools/call") {
			return false
		}
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	})
	defer srv.Close()

	c, err := Connect(context.Background(), ServerConfig{Name: "broker", Transport: "sse", URL: srv.URL}, zerolog.Nop())
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	_, callErr := c.CallTool(context.Background(), "place_order", json.RawMessage(`{}`))
	var rl *ToolRateLimitError
	require.True(t, errors.As(callErr, &rl), "expected *ToolRateLimitError, got %v", callErr)
	assert.Equal(t, 5*time.Second, rl.RetryAfter)
}

// TestCallSSE_429WithoutHeaderFloorsToOneSecond covers the retryAfter<=0
// fallback when the upstream 429 omits a usable Retry-After header.
func TestCallSSE_429WithoutHeaderFloorsToOneSecond(t *testing.T) {
	srv := clientCovSSEServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if !contains(body, "tools/call") {
			return false
		}
		w.WriteHeader(http.StatusTooManyRequests) // no Retry-After
		return true
	})
	defer srv.Close()

	c, err := Connect(context.Background(), ServerConfig{Name: "broker", Transport: "sse", URL: srv.URL}, zerolog.Nop())
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	_, callErr := c.CallTool(context.Background(), "place_order", json.RawMessage(`{}`))
	var rl *ToolRateLimitError
	require.True(t, errors.As(callErr, &rl))
	assert.Equal(t, time.Second, rl.RetryAfter, "missing header must floor to 1s")
}

// TestCallSSE_JSONRPCErrorObjectSurfaces verifies a JSON-RPC error object
// in an otherwise-200 SSE reply is returned to the caller verbatim.
func TestCallSSE_JSONRPCErrorObjectSurfaces(t *testing.T) {
	srv := clientCovSSEServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if !contains(body, "tools/call") {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 2,
			"error": map[string]any{"code": -32000, "message": "boom-from-server"},
		})
		return true
	})
	defer srv.Close()

	c, err := Connect(context.Background(), ServerConfig{Name: "s", Transport: "sse", URL: srv.URL}, zerolog.Nop())
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	_, callErr := c.CallTool(context.Background(), "ping", json.RawMessage(`{}`))
	require.Error(t, callErr)
	assert.Contains(t, callErr.Error(), "boom-from-server")
}

// TestCallSSE_ForwardsTaskAndExecutionHeaders pins the attribution path:
// X-Task-ID / X-Execution-ID stashed on ctx are forwarded on the outbound
// SSE request so the broker can tie each order to its originating task.
func TestCallSSE_ForwardsTaskAndExecutionHeaders(t *testing.T) {
	var (
		mu      sync.Mutex
		gotTask string
		gotExec string
		sawTool bool
	)
	srv := clientCovSSEServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if !contains(body, "tools/call") {
			return false
		}
		mu.Lock()
		gotTask = r.Header.Get("X-Task-ID")
		gotExec = r.Header.Get("X-Execution-ID")
		sawTool = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "result": map[string]any{"content": []any{}}})
		return true
	})
	defer srv.Close()

	c, err := Connect(context.Background(), ServerConfig{Name: "broker", Transport: "sse", URL: srv.URL}, zerolog.Nop())
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	ctx := context.WithValue(context.Background(), TaskIDHeaderKey{}, "task-42")
	ctx = context.WithValue(ctx, ExecutionIDHeaderKey{}, "exec-7")
	_, err = c.CallTool(ctx, "place_order", json.RawMessage(`{}`))
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.True(t, sawTool, "tool call never reached the server")
	assert.Equal(t, "task-42", gotTask)
	assert.Equal(t, "exec-7", gotExec)
}

// TestCallSSE_TransportError surfaces a dial failure (server closed) as an
// error rather than a panic / hang.
func TestCallSSE_TransportError(t *testing.T) {
	srv := clientCovSSEServer(t, nil)
	url := srv.URL
	srv.Close() // now unreachable

	c := &Client{
		config:     ServerConfig{Name: "s", Transport: "sse", URL: url},
		logger:     zerolog.Nop(),
		httpClient: &http.Client{Timeout: 500 * time.Millisecond},
	}
	_, err := c.callSSE(context.Background(), "tools/list", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sse request")
}

// TestNotify_StdioWritesNewlineDelimitedFrame asserts the stdio notify path
// marshals a JSON-RPC notification (no id) and writes a newline-terminated
// frame to stdin.
func TestNotify_StdioWritesNewlineDelimitedFrame(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()

	c := &Client{
		config: ServerConfig{Name: "s", Transport: "stdio"},
		logger: zerolog.Nop(),
		stdin:  pw,
	}

	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := pr.Read(buf)
		done <- buf[:n]
	}()

	require.NoError(t, c.notify("notifications/initialized", nil))

	select {
	case frame := <-done:
		assert.Equal(t, byte('\n'), frame[len(frame)-1], "frame must be newline-terminated")
		var raw map[string]any
		require.NoError(t, json.Unmarshal(frame[:len(frame)-1], &raw))
		assert.Equal(t, "notifications/initialized", raw["method"])
		assert.Equal(t, "2.0", raw["jsonrpc"])
	case <-time.After(time.Second):
		t.Fatal("notify never wrote to stdin")
	}
}

// TestNotify_SSEIsNoOp documents that the SSE transport treats notify as a
// no-op (it has no separate notification channel).
func TestNotify_SSEIsNoOp(t *testing.T) {
	c := &Client{config: ServerConfig{Name: "s", Transport: "sse"}, logger: zerolog.Nop()}
	require.NoError(t, c.notify("notifications/initialized", nil))
}

// TestCall_UnsupportedTransport hits the default arm of call().
func TestCall_UnsupportedTransport(t *testing.T) {
	c := &Client{config: ServerConfig{Name: "s", Transport: "smoke-signal"}, logger: zerolog.Nop()}
	_, err := c.call(context.Background(), "tools/list", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported transport")
}

// TestValidateLauncher_Branches covers the allowlist, absolute-path, and
// rejection arms of validateLauncher.
func TestValidateLauncher_Branches(t *testing.T) {
	require.NoError(t, validateLauncher("npx"))
	require.NoError(t, validateLauncher("uvx"))
	require.NoError(t, validateLauncher("python3"))

	require.Error(t, validateLauncher(""))
	require.Error(t, validateLauncher("./relative/path"))
	require.Error(t, validateLauncher("totally-unknown-launcher"))
	require.Error(t, validateLauncher("/opt/somewhere/evil"), "non-standard absolute path rejected")

	// An absolute path that resolves under an allowed system dir passes.
	for _, p := range []string{"/bin/sh", "/usr/bin/env"} {
		if _, statErr := os.Stat(p); statErr == nil {
			require.NoError(t, validateLauncher(p), "%s should be allowed", p)
			break
		}
	}
}

// TestLogWriter_ForwardsTrimmed checks the stderr-forwarding writer reports
// the full byte count and ignores whitespace-only writes.
func TestLogWriter_ForwardsTrimmed(t *testing.T) {
	w := &logWriter{logger: zerolog.Nop(), server: "s"}
	n, err := w.Write([]byte("  hello from server  "))
	require.NoError(t, err)
	assert.Equal(t, len("  hello from server  "), n, "must report the full input length")

	n, err = w.Write([]byte("   \n\t "))
	require.NoError(t, err)
	assert.Equal(t, len("   \n\t "), n)
}

// TestExpandSafe_RefusesVornikSecrets verifies the env expander blanks any
// VORNIK_-prefixed variable so daemon secrets never leak into subprocess
// args/env, while ordinary variables expand normally.
func TestExpandSafe_RefusesVornikSecrets(t *testing.T) {
	t.Setenv("VORNIK_DB_PASSWORD", "super-secret")
	t.Setenv("CLIENTCOV_PLAIN", "visible")

	assert.Equal(t, "", expandSafe("${VORNIK_DB_PASSWORD}"), "VORNIK_ secret must blank out")
	assert.Equal(t, "visible", expandSafe("${CLIENTCOV_PLAIN}"))
	assert.Equal(t, "", expandSafe("$CLIENTCOV_UNSET"), "unknown var expands empty")
}

// TestBaseMCPEnv_OnlyAllowlistedKeys confirms baseMCPEnv passes through only
// allowlisted variables and never a VORNIK_ secret.
func TestBaseMCPEnv_OnlyAllowlistedKeys(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("VORNIK_ADMIN_KEY", "leak-me")

	env := baseMCPEnv()
	var sawPath bool
	for _, kv := range env {
		assert.NotContains(t, kv, "VORNIK_", "no VORNIK_ var may pass into the subprocess env")
		if filepath.Clean(kv) == "PATH=/usr/bin:/bin" {
			sawPath = true
		}
	}
	assert.True(t, sawPath, "allowlisted PATH must pass through")
}

// contains is a tiny []byte substring helper for the SSE-server hooks.
func contains(b []byte, sub string) bool {
	return strings.Contains(string(b), sub)
}
