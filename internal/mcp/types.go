// Package mcp provides a Model Context Protocol (MCP) client for vornik.
// It supports stdio, SSE, and streamable-http transports and exposes
// discovered tools in OpenAI function-calling format for use by the
// dispatcher and agents.
package mcp

import "encoding/json"

// ServerConfig defines how to connect to an MCP server.
type ServerConfig struct {
	Name      string            `yaml:"name" json:"name"`
	Transport string            `yaml:"transport" json:"transport"` // "stdio", "sse", or "streamable-http"
	Command   string            `yaml:"command" json:"command"`     // for stdio
	Args      []string          `yaml:"args" json:"args"`           // for stdio
	Env       map[string]string `yaml:"env" json:"env"`             // for stdio (supports ${VAR} expansion)
	URL       string            `yaml:"url" json:"url"`             // for sse and streamable-http
	// AllowedTools, when non-empty, restricts the Client's exposed tool
	// set to only those whose names are listed. Empty means "all tools
	// the server advertises" (back-compatible default).
	AllowedTools []string `yaml:"allowed_tools" json:"allowed_tools,omitempty"`
	// Headers are attached to every SSE-transport HTTP request the
	// Client makes (initialize, tools/list, tools/call). Populated
	// programmatically by the daemon — NOT from project YAML — so
	// per-project context the server needs (X-Project-ID, the
	// project's per-call cap overlay in X-Project-Caps) lands on
	// every JSON-RPC envelope without the agent or the prompt
	// having to know about it. Stdio transport ignores Headers.
	Headers map[string]string `yaml:"-" json:"-"`
	// ToolRateLimits is the daemon-resolved per-tool token-bucket
	// configuration (rate-limit hardening sub-item 3). Populated by
	// the wiring layer from ProjectMCP.ToolRateLimits — the YAML
	// shape stays in registry, the Client stays decoupled from it.
	// Keys are bare tool names ("place_order") or "server.tool"
	// pairs ("broker.place_order") for disambiguation when two
	// servers expose the same tool name. Empty disables per-tool
	// throttling for this Client.
	ToolRateLimits map[string]ToolRateLimitSpec `yaml:"-" json:"-"`
	// ProjectID is the project this Client serves, used as the
	// `project` label on vornik_mcp_tool_rate_limited_total when
	// a per-tool bucket rejects a call. Empty disables the
	// counter increment (the throttle still fires; just no
	// labelled metric).
	ProjectID string `yaml:"-" json:"-"`
}

// TaskIDHeaderKey / ExecutionIDHeaderKey carry the originating
// agent's task / execution IDs through the daemon → broker MCP
// call chain so the broker can attribute each trading_orders
// row to the task that placed it.
//
// Flow: agent's mcp-bridge reads VORNIK_TASK_ID /
// VORNIK_EXECUTION_ID env vars stamped on its container, sets
// them as X-Task-ID / X-Execution-ID headers on its POST to
// the daemon. The daemon's CallMCPTool handler extracts them
// from r.Header and stashes them on ctx under these keys.
// Client.CallTool reads them back and forwards on the
// outbound MCP request. Broker mcpserver.handleMessage
// re-extracts and threads them into the SafetyEnvelope, which
// stamps them on every audit row.
//
// Defined on the mcp package (not internal/api) because the
// Client is the one that actually sets the outbound headers
// — keeps the convention next to the consumer.
type TaskIDHeaderKey struct{}
type ExecutionIDHeaderKey struct{}

// Tool is an MCP tool definition as returned by tools/list.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolResult is the response from tools/call.
type ToolResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ContentItem is one piece of content in a tool result.
type ContentItem struct {
	Type string `json:"type"` // "text", "image", "resource"
	Text string `json:"text,omitempty"`
}

// Text returns the concatenated text content of the result.
func (r *ToolResult) Text() string {
	var s string
	for _, c := range r.Content {
		if c.Type == "text" || c.Type == "" {
			s += c.Text
		}
	}
	return s
}

// --- JSON-RPC protocol types ---

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonRPCError) Error() string {
	return e.Message
}

type initializeParams struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    struct{}   `json:"capabilities"`
	ClientInfo      clientInfo `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolsListResult struct {
	Tools []Tool `json:"tools"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}
