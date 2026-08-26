// Package mcp provides a Model Context Protocol (MCP) client for vornik.
// It supports stdio, SSE, and streamable-http transports and exposes
// discovered tools in OpenAI function-calling format for use by the
// dispatcher and agents.
package mcp

import (
	"context"
	"encoding/json"
	"net/http"
)

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
	// RequireDeclaredTools makes the mutating-tool gate REFUSE registration
	// when this server advertises a mutating tool that allowed_tools does not
	// name (Workspace LLD §10.2). Opt-in per server, and default false, for a
	// deliberate reason: several existing deployments run expose-all servers
	// whose tool sets are legitimately mutating (a page publisher, a home
	// -automation bridge), and a retroactive global rule would refuse to
	// register integrations that work today. Strictness is declared where it is
	// wanted rather than imposed where it is not.
	RequireDeclaredTools bool `yaml:"require_declared_tools" json:"require_declared_tools,omitempty"`
	// TimeoutSeconds overrides the per-request HTTP timeout for SSE /
	// streamable-http transports (initialize, tools/list, tools/call). 0
	// = the 30s default. Raise it for servers whose tools legitimately
	// run long — e.g. the scraper's web_fetch against slow / anti-bot
	// target sites, which was failing at the 30s default (context
	// deadline exceeded). Stdio transport ignores this.
	TimeoutSeconds int `yaml:"timeout_seconds" json:"timeout_seconds,omitempty"`
	// Headers are attached to every SSE-transport HTTP request the
	// Client makes (initialize, tools/list, tools/call). Populated
	// programmatically by the daemon — NOT from project YAML — so
	// per-project context the server needs (X-Project-ID, the
	// project's per-call cap overlay in X-Project-Caps) lands on
	// every JSON-RPC envelope without the agent or the prompt
	// having to know about it. Stdio transport ignores Headers.
	Headers map[string]string `yaml:"-" json:"-"`
	// AuthHeaders carries credentials resolved from the server's `auth:`
	// block (mode static today, oauth once the flow ships). Kept separate
	// from Headers so the composition order is explicit: auth is applied
	// LAST and deterministically wins over an operator-set header of the
	// same name, logging once at Warn when it overwrites one.
	//
	// Never serialized (yaml/json "-") because unlike Headers — whose
	// contents are the daemon's own X-Project-* metadata — this map holds
	// real credential material, and ServerConfig is JSON-encoded into the
	// MCP discovery API. Populated by the wiring layer; empty for every
	// server with no auth block.
	AuthHeaders map[string]string `yaml:"-" json:"-"`
	// AuthHeaderProvider, when non-nil, is consulted on EVERY request and its
	// result is applied INSTEAD of AuthHeaders.
	//
	// It exists because a credential derived from an OAuth GRANT expires,
	// while one derived from CONFIG does not. AuthHeaders is resolved by the
	// wiring layer, and the wiring layer runs only at boot, on config reload,
	// and on consent — so a bearer frozen there is valid for the vendor's
	// access-token lifetime and dead for however long the daemon runs after
	// that. Measured on the production deployment 2026-08-25: an 8-hour
	// Atlassian token, a 58h41m gap between rewires, and ~51 hours during
	// which every call 401'd while every status surface reported the grant
	// healthy. See
	// https://docs.vornik.io §3.1.
	//
	// Must be cheap on the common path — this is on every tool call. The
	// implementation (mcpconnect.Connector.AccessToken behind a token cache)
	// does no I/O until the token is within its refresh skew of expiry.
	//
	// A nil map with a nil error means "no credential", which is NOT an error:
	// an oauth-mode server the operator has never connected registers and
	// calls unauthenticated, so its tools stay visible and the operator can
	// see what needs connecting (auth design §8).
	//
	// A non-nil error is FATAL to the call. Degrading to an unauthenticated
	// request is exactly the silent failure this design removes.
	//
	// Never serialized, for the same reason as AuthHeaders.
	AuthHeaderProvider func(ctx context.Context) (map[string]string, error) `yaml:"-" json:"-"`
	// AuthInvalidator, when non-nil, discards whatever credential
	// AuthHeaderProvider last returned, so the next call re-resolves it from
	// the authoritative store instead of a cache.
	//
	// Called on exactly one condition: the vendor answered 401/403 to a
	// credential the daemon believed was valid. That belief comes from the
	// grant's stored expires_at, which is the VENDOR's advertised lifetime and
	// is not binding on the vendor — a grant revoked in the Atlassian console,
	// or reset by a scope change, dies before its stated expiry with nothing
	// to tell us. The 401 is the only authoritative signal, so it is the
	// trigger.
	//
	// Exactly one retry follows, never a loop (auth design §8). A server with
	// no invalidator has nothing to refresh — a static credential that 401s is
	// a config error, not a stale token — and is not retried at all.
	AuthInvalidator func(ctx context.Context) `yaml:"-" json:"-"`
	// AuthEnv carries credentials resolved from an `auth: {mode: env}`
	// block into a stdio subprocess's environment — the Plane 2 case where
	// the MCP server holds its OWN upstream app credentials (YouTube,
	// Reddit, Instagram wrappers) and Vornik's job is only to inject them
	// safely. Merged AFTER Env and, unlike Env, NOT ${VAR}-expanded: a
	// resolved secret is a literal, and expanding one would mangle any
	// credential containing '$'. Never serialized, for the same reason as
	// AuthHeaders. Ignored on non-stdio transports.
	AuthEnv map[string]string `yaml:"-" json:"-"`
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
	// HTTPClient overrides the client Connect uses for sse / streamable-http
	// transports. Nil (default, and every existing caller) preserves prior
	// behavior — Connect builds one internally from TimeoutSeconds. Populated
	// by the integrations probe layer (internal/integrations) so a candidate
	// MCP server URL a user pastes in is dialled through the SSRF-guarded
	// DialGuard client rather than an unguarded default client — Connect has
	// no other seam for that. Ignored by the stdio transport.
	HTTPClient *http.Client `yaml:"-" json:"-"`
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
	// Annotations carries the server's own read-only / destructive hints when
	// it supplies them. Authoritative over the name heuristic in
	// IsMutating — the server knows its semantics better than a verb table.
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
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
