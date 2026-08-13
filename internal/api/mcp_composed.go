// Composed MCPExecutor — merges the external mcp.Manager catalog
// with the daemon's built-in document_* tools so worker agents see
// both in a single /mcp/tools response.
//
// The composition lives in the api package (not mcp) because
// document tools are an api-layer concern: they reach into
// persistence repos + the extractor package. Keeping mcp.Manager
// focused on subprocess-backed servers preserves the package
// boundary.
package api

import (
	"context"
	"strings"

	"vornik.io/vornik/internal/a2a/consult"
	"vornik.io/vornik/internal/chat"
)

// ComposedMCPExecutor implements MCPExecutor by stacking daemon-side synthetic
// providers (document_* tools, A2A consult tools) on top of the external
// mcp.Manager. Any field may be nil — Tools/Execute degrade gracefully.
type ComposedMCPExecutor struct {
	External MCPExecutor // typically *mcp.Manager
	Builtin  *DocumentToolProvider
	Consult  *consult.Provider // A2A domain-expert consult tools (mcp__consult__<peer>)
	// Grants serves mcp__vornik__grant_step_tools — the lead narrowing which tools a
	// step is advertised (registry design §10.1). Nil leaves the ceiling as the only
	// narrowing, which is the pre-feature behaviour.
	Grants *ToolGrantProvider
}

// Tools returns the union of external MCP server tools and the built-in
// document_* + consult_* surfaces.
func (c *ComposedMCPExecutor) Tools(projectID string) []chat.Tool {
	var out []chat.Tool
	if c.External != nil {
		out = c.External.Tools(projectID)
	}
	if c.Builtin != nil {
		out = append(out, c.Builtin.Tools(projectID)...)
	}
	if c.Consult != nil {
		out = append(out, c.Consult.Tools(projectID)...)
	}
	if c.Grants != nil {
		out = append(out, c.Grants.Tools(projectID)...)
	}
	return out
}

// Execute routes by the server segment of the qualified name. The builtin
// document + consult server names are reserved — an external server registered
// under those names is shadowed (defensive against a project mis-configuring an
// MCP server with the same name).
func (c *ComposedMCPExecutor) Execute(ctx context.Context, projectID, qualifiedName, argsJSON string) (string, error) {
	if c.Builtin != nil && c.Builtin.Owns(qualifiedName) {
		return c.Builtin.Execute(ctx, projectID, qualifiedName, argsJSON)
	}
	if c.Consult != nil && c.Consult.Owns(qualifiedName) {
		return c.Consult.Execute(ctx, projectID, qualifiedName, argsJSON)
	}
	if c.Grants != nil && c.Grants.Owns(qualifiedName) {
		return c.Grants.Execute(ctx, projectID, qualifiedName, argsJSON)
	}
	if c.External == nil {
		return "", errBuiltinNoExternal(qualifiedName)
	}
	return c.External.Execute(ctx, projectID, qualifiedName, argsJSON)
}

func errBuiltinNoExternal(qualifiedName string) error {
	// Distinct error so callers (mcp-bridge → daemon → curl logs) can
	// tell "we don't have any MCP at all" from "tool not found in
	// the external set".
	return &noExternalError{qualifiedName: qualifiedName}
}

type noExternalError struct{ qualifiedName string }

func (e *noExternalError) Error() string {
	return "no MCP executor wired and tool " + e.qualifiedName + " is not a built-in"
}

// HasBuiltinPrefix reports whether the qualified name targets the
// built-in document tools surface. Exposed for tests + diagnostics.
func HasBuiltinPrefix(qualifiedName string) bool {
	return strings.HasPrefix(qualifiedName, "mcp__"+builtinMCPServer+"__")
}
