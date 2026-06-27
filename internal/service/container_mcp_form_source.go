package service

import (
	"context"

	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/ui"
)

// mcpFormRegistryAdapter adapts a *mcp.Registry (slice 1's daemon-level
// discovery cache) to ui.MCPFormRegistrySource (slice 2's contract for
// the project config form's MCP servers section). The two interfaces
// were written independently and the type shapes don't line up — the
// adapter does the per-server flatten so the form can render without
// pulling internal/mcp into the ui package's import graph.
//
// Empty registry → returns nil slice, nil error. The form treats that
// as "no servers configured" and renders its empty-state banner.
type mcpFormRegistryAdapter struct {
	registry *mcp.Registry
}

// Servers satisfies ui.MCPFormRegistrySource. Reads the registry's
// cached snapshot — same data the /ui/mcp page and /api/v1/mcp/servers
// endpoint expose, so all three surfaces stay coherent.
func (a *mcpFormRegistryAdapter) Servers(ctx context.Context) ([]ui.MCPRegistryServer, error) {
	if a == nil || a.registry == nil {
		return nil, nil
	}
	snap := a.registry.Snapshot(ctx)
	out := make([]ui.MCPRegistryServer, 0, len(snap))
	for _, s := range snap {
		tools := make([]ui.MCPRegistryTool, 0, len(s.Tools))
		for _, t := range s.Tools {
			tools = append(tools, ui.MCPRegistryTool{
				Name:        t.Name,
				Description: t.Description,
			})
		}
		out = append(out, ui.MCPRegistryServer{
			Name:      s.Name,
			Transport: s.Transport,
			URL:       s.URL,
			Tools:     tools,
			Reachable: s.Reachable,
			Error:     s.Error,
		})
	}
	return out, nil
}
