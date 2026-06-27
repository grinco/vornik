package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMCPEgress_AllowedToolsEnforcedDaemonSide is the Tier-1 egress
// characterization (https://docs.vornik.io): it proves the allowed_tools gate is
// enforced *inside the daemon's MCP client*, before any JSON-RPC write
// reaches the upstream server — not merely advertised to the agent.
//
// The earlier unit tests cover the pieces in isolation:
//   - TestClient_AllowedTools_FiltersCatalog drives a hand-rolled mirror of
//     the filter (applyAllowlistForTest), not the real Connect() path.
//   - TestCallTool_RejectsToolOutsideAllowlist calls CallTool on a Client
//     with NO transport, so its error could just as well come from a missing
//     connection as from the gate.
//
// This test closes that gap end-to-end against a live in-process MCP server
// (httptest) reached through the real Connect()/CallTool() code path, with a
// tools/call request counter so we can assert the wire was never touched for
// a denied tool. That is what makes the enforcement provably daemon-side.
func TestMCPEgress_AllowedToolsEnforcedDaemonSide(t *testing.T) {
	const (
		allowedTool   = "search_emails" // in the project allowlist
		forbiddenTool = "delete_email"  // advertised by server, NOT allowlisted
		hiddenTool    = "send_email"    // advertised by server, NOT allowlisted
	)

	var (
		mu            sync.Mutex
		toolCallNames []string // names the upstream actually received on tools/call
	)
	recordToolCall := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		toolCallNames = append(toolCallNames, name)
	}
	receivedToolCalls := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(toolCallNames))
		copy(out, toolCallNames)
		return out
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)

		switch req.Method {
		case "initialize":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": "2024-11-05"},
			})
		case "tools/list":
			// Advertise a superset; the allowlist must shrink this.
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []any{
					map[string]any{"name": allowedTool},
					map[string]any{"name": forbiddenTool},
					map[string]any{"name": hiddenTool},
				}},
			})
		case "tools/call":
			recordToolCall(req.Params.Name)
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "ok"}},
				},
			})
		default:
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		}
	}))
	defer srv.Close()

	c, err := Connect(context.Background(), ServerConfig{
		Name:         "gmail",
		Transport:    "sse",
		URL:          srv.URL,
		AllowedTools: []string{allowedTool},
	}, zerolog.Nop())
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	// (1) The exposed/listed catalog is filtered to the allowlist — even
	// though the server advertised three tools, only the allowlisted one is
	// visible to every downstream consumer (dispatcher, agent bridge, ...).
	require.Equal(t, []string{allowedTool}, toolNames(c.Tools()),
		"Connect() must filter the advertised catalog down to allowed_tools")

	// (2) A non-allowlisted call is rejected daemon-side BEFORE the wire.
	_, err = c.CallTool(context.Background(), forbiddenTool, json.RawMessage(`{}`))
	require.Error(t, err, "a tool outside allowed_tools must be rejected")
	assert.Contains(t, err.Error(), "not in allowed_tools")
	assert.Empty(t, receivedToolCalls(),
		"SECURITY: a denied tool call must never reach the upstream server")

	// (3) An allowlisted call is permitted and does reach the wire.
	res, err := c.CallTool(context.Background(), allowedTool, json.RawMessage(`{}`))
	require.NoError(t, err, "an allowlisted tool call must be permitted")
	assert.Equal(t, "ok", res.Text())
	assert.Equal(t, []string{allowedTool}, receivedToolCalls(),
		"only the allowlisted call should have reached the upstream")

	// (4) A tool the server advertised but that we filtered out is also
	// gated — proving the gate keys off the allowlist, not merely off the
	// advertised catalog.
	_, err = c.CallTool(context.Background(), hiddenTool, json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in allowed_tools")
	assert.Equal(t, []string{allowedTool}, receivedToolCalls(),
		"SECURITY: the filtered-out tool must not have reached the upstream")
}
