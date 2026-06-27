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

// managerCovSSEClient connects a real (in-memory) SSE-transport Client to a
// test server so Manager.Execute can drive CallTool end to end without a
// subprocess. toolReply is invoked for each tools/call request and returns
// the JSON-RPC `result` object the server should send back.
func managerCovSSEClient(t *testing.T, name string, toolReply func(args string) map[string]any) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "tools/call") {
			var req struct {
				Params struct {
					Arguments json.RawMessage `json:"arguments"`
				} `json:"params"`
			}
			_ = json.Unmarshal(body, &req)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 99, "result": toolReply(string(req.Params.Arguments)),
			})
			return
		}
		// initialize / tools/list handshake.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"tools": []map[string]any{{"name": "do_thing"}}},
		})
	}))
	t.Cleanup(srv.Close)

	c, err := Connect(context.Background(), ServerConfig{Name: name, Transport: "sse", URL: srv.URL}, zerolog.Nop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestManagerExecute_HappyPathReturnsText drives the success branch of
// Manager.Execute: the qualified name is parsed, routed to the right
// client, and the ToolResult text is returned unwrapped.
func TestManagerExecute_HappyPathReturnsText(t *testing.T) {
	client := managerCovSSEClient(t, "calc", func(args string) map[string]any {
		assert.JSONEq(t, `{"x":1}`, args, "Execute must forward the args JSON verbatim")
		return map[string]any{"content": []map[string]any{{"type": "text", "text": "result-42"}}}
	})

	mgr := NewManager(zerolog.Nop())
	mgr.mu.Lock()
	mgr.clients["proj"] = map[string]*Client{"calc": client}
	mgr.mu.Unlock()

	out, err := mgr.Execute(context.Background(), "proj", "mcp__calc__do_thing", `{"x":1}`)
	require.NoError(t, err)
	assert.Equal(t, "result-42", out)
}

// TestManagerExecute_IsErrorResultIsPrefixed covers the result.IsError
// branch: a tool that returns isError:true is surfaced to the caller as a
// non-error string prefixed "MCP error:", NOT as a Go error (the agent
// sees it as tool output to reason about).
func TestManagerExecute_IsErrorResultIsPrefixed(t *testing.T) {
	client := managerCovSSEClient(t, "calc", func(string) map[string]any {
		return map[string]any{
			"isError": true,
			"content": []map[string]any{{"type": "text", "text": "divide by zero"}},
		}
	})

	mgr := NewManager(zerolog.Nop())
	mgr.mu.Lock()
	mgr.clients["proj"] = map[string]*Client{"calc": client}
	mgr.mu.Unlock()

	out, err := mgr.Execute(context.Background(), "proj", "mcp__calc__do_thing", `{}`)
	require.NoError(t, err, "an isError tool result is output, not a transport failure")
	assert.Equal(t, "MCP error: divide by zero", out)
}

// TestManagerExecute_CallToolErrorIsWrapped covers the err != nil branch:
// a JSON-RPC error from the server is wrapped with the qualified tool name
// so logs/traces identify the failing call.
func TestManagerExecute_CallToolErrorIsWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "tools/call") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 99,
				"error": map[string]any{"code": -32000, "message": "upstream refused"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": []any{}}})
	}))
	defer srv.Close()

	client, err := Connect(context.Background(), ServerConfig{Name: "calc", Transport: "sse", URL: srv.URL}, zerolog.Nop())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	mgr := NewManager(zerolog.Nop())
	mgr.mu.Lock()
	mgr.clients["proj"] = map[string]*Client{"calc": client}
	mgr.mu.Unlock()

	_, execErr := mgr.Execute(context.Background(), "proj", "mcp__calc__do_thing", `{}`)
	require.Error(t, execErr)
	assert.Contains(t, execErr.Error(), "mcp__calc__do_thing failed")
	assert.Contains(t, execErr.Error(), "upstream refused")
}

// TestManagerTools_DefaultsEmptySchema confirms Tools() substitutes a
// minimal object schema when a tool advertises no inputSchema — some
// providers reject a function definition with an empty parameters block.
func TestManagerTools_DefaultsEmptySchema(t *testing.T) {
	client := newFakeClient(ServerConfig{Name: "srv"}, []Tool{
		{Name: "no_schema"}, // empty InputSchema
		{Name: "has_schema", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{}}}`)},
	})
	mgr := NewManager(zerolog.Nop())
	mgr.mu.Lock()
	mgr.clients["proj"] = map[string]*Client{"srv": client}
	mgr.mu.Unlock()

	tools := mgr.Tools("proj")
	require.Len(t, tools, 2)

	byName := map[string]json.RawMessage{}
	for _, tl := range tools {
		byName[tl.Function.Name] = tl.Function.Parameters
	}
	assert.JSONEq(t, `{"type":"object","properties":{}}`, string(byName["mcp__srv__no_schema"]),
		"empty schema must default to an object schema")
	assert.JSONEq(t, `{"type":"object","properties":{"q":{}}}`, string(byName["mcp__srv__has_schema"]),
		"non-empty schema is passed through untouched")
}
