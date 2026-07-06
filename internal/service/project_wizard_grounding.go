package service

import (
	"context"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/projectwizard"
)

// wizardMCPGroundingAdapter adapts a *mcp.Registry (slice 1's
// daemon-level discovery cache) to projectwizard.MCPGroundingSource so
// the wizard's system-prompt grounding block reflects the same live
// server + tool inventory the /ui/mcp page and the project config
// form's MCP section (mcpFormRegistryAdapter, container_mcp_form_source.go)
// already surface. Every configured server is included regardless of
// Reachable — same behaviour as mcpFormRegistryAdapter — so a
// temporarily-down server still grounds the LLM on the tool
// vocabulary it exposed on its last successful refresh rather than
// vanishing from the prompt.
type wizardMCPGroundingAdapter struct {
	registry *mcp.Registry
}

// Servers satisfies projectwizard.MCPGroundingSource.
func (a *wizardMCPGroundingAdapter) Servers(ctx context.Context) []projectwizard.GroundingServer {
	if a == nil || a.registry == nil {
		return nil
	}
	return snapshotsToGroundingServers(a.registry.Snapshot(ctx))
}

// snapshotsToGroundingServers is the pure ServerSnapshot →
// GroundingServer mapping, split out from wizardMCPGroundingAdapter so
// the seam is unit-testable against hand-built snapshots without a
// live *mcp.Registry (whose catalog only updates via a real
// tools/list round trip through spawnRefresh/refreshOne).
func snapshotsToGroundingServers(snap []mcp.ServerSnapshot) []projectwizard.GroundingServer {
	out := make([]projectwizard.GroundingServer, 0, len(snap))
	for _, s := range snap {
		tools := make([]string, 0, len(s.Tools))
		for _, t := range s.Tools {
			tools = append(tools, t.Name)
		}
		out = append(out, projectwizard.GroundingServer{Name: s.Name, Tools: tools})
	}
	return out
}

// wizardKnownMCPServers builds the projectwizard.Wizard.KnownMCP
// accessor: the map[string]bool known-set the commit-time compose
// engine's mcp_server applier uses to reject a server name the LLM
// hallucinated (see wizard.go's composeFromEnvelope and
// ComposeDeps.KnownMCP in compose.go). Keyed by every server name
// CONFIGURED on the daemon regardless of live Reachable — compose
// validates that the operator declared the server, not that it is
// currently up. Returns nil when registry is nil so callers can
// leave Wizard.KnownMCP unset (grounding/compose then treats the
// known-set as empty, matching a no-MCP deployment).
func wizardKnownMCPServers(registry *mcp.Registry) func(ctx context.Context) map[string]bool {
	if registry == nil {
		return nil
	}
	return func(ctx context.Context) map[string]bool {
		return snapshotsToKnownMCPSet(registry.Snapshot(ctx))
	}
}

// snapshotsToKnownMCPSet is the pure ServerSnapshot → known-set
// mapping, split out for the same testability reason as
// snapshotsToGroundingServers above.
func snapshotsToKnownMCPSet(snap []mcp.ServerSnapshot) map[string]bool {
	out := make(map[string]bool, len(snap))
	for _, s := range snap {
		out[s.Name] = true
	}
	return out
}

// wizardModelLister adapts a chat.Provider to projectwizard.ModelLister
// via the same templateModelIDs flatten the template gallery's
// optionsFrom(models) resolver and templateModelIDs' other callers
// use. ModelLister has no error return; a discovery failure (provider
// doesn't implement model listing, upstream error) degrades to an
// empty list — the wizard's grounding then renders "default model
// only" rather than failing the whole turn over a catalog fetch.
type wizardModelLister struct {
	provider chat.Provider
}

// Models satisfies projectwizard.ModelLister.
func (m wizardModelLister) Models(ctx context.Context) []string {
	if m.provider == nil {
		return nil
	}
	ids, err := templateModelIDs(ctx, m.provider)
	if err != nil {
		return nil
	}
	return ids
}
