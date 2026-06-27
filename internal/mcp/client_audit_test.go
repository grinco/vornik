package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClient_SSE_ErrorStatusDoesNotLeakBodyToCaller guards the fix that
// callSSE no longer embeds the verbatim upstream response body in the error
// it returns. That error propagates to the external /mcp/tools/call HTTP
// response, and the body is an untrusted third-party MCP server's bytes
// (arbitrary size + content). The caller-facing error must carry only the
// status code; the body is logged, not returned.
// (Audit 2026-06-03: verbatim upstream-error passthrough to project caller.)
func TestClient_SSE_ErrorStatusDoesNotLeakBodyToCaller(t *testing.T) {
	const secret = "SECRET-UPSTREAM-DETAIL-tok_abc123XYZ"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Let the Connect handshake (initialize / tools/list) succeed so we
		// reach the tools/call path; only the actual tool call errors with
		// a body that must NOT leak.
		if strings.Contains(string(body), "tools/call") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"` + secret + `"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": []any{}},
		})
	}))
	defer srv.Close()

	c, err := Connect(context.Background(), ServerConfig{
		Name: "broker-fake", Transport: "sse", URL: srv.URL,
	}, zerolog.Nop())
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	_, callErr := c.CallTool(context.Background(), "place_order", json.RawMessage(`{}`))
	require.Error(t, callErr, "a 500 from the upstream MCP server must surface as an error")
	assert.Contains(t, callErr.Error(), "500", "caller-facing error should carry the status code")
	assert.NotContains(t, callErr.Error(), secret,
		"upstream response body must NOT leak into the caller-facing error")
}
