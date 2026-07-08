package ui

import (
	"context"

	"vornik.io/vornik/internal/mcp"
)

// MCPRegistrySource is the read-only handle to the daemon-level MCP registry
// (cached snapshot + reachability). Consumed by the control-plane hub's MCP
// tab (buildCPMCP). Kept as its own interface so the ui package doesn't pull
// internal/api into its import graph.
//
// (Formerly declared in mcp.go alongside the standalone /ui/mcp discovery
// page; that page was retired in the 2026-07-08 nav dedupe — /ui/mcp now 302s
// to the hub MCP tab — but the registry handle lives on.)
type MCPRegistrySource interface {
	Snapshot(ctx context.Context) []mcp.ServerSnapshot
}
