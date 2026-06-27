// Code in this file renders the daemon-level MCP discovery page at
// /ui/mcp. Separate from the per-project MCP tools view (which is
// reached from a project's detail page) because the daemon-level
// inventory is operator-scoped — it answers "what MCP servers does
// the daemon know about" without requiring drill-down per project.
//
// The page is deliberately read-only. We do NOT expose a "wire this
// to project X" button: granting a daemon-level server to a project
// has to be an explicit YAML edit the operator commits, which keeps
// the auto-discovery ≠ auto-grant boundary the rollout brief calls
// out.

package ui

import (
	"context"
	"net/http"
	"time"

	"vornik.io/vornik/internal/mcp"
)

// MCPRegistrySource is the read-only handle to the daemon-level
// MCP registry. Same shape as the API package's MCPRegistrySource;
// kept here as a separate interface so the ui package doesn't pull
// internal/api into its import graph.
type MCPRegistrySource interface {
	Snapshot(ctx context.Context) []mcp.ServerSnapshot
}

// MCPServerRow is one rendered row on /ui/mcp. Flattens
// mcp.ServerSnapshot into a shape the template can consume
// directly without inline-helper plumbing.
type MCPServerRow struct {
	Name          string
	Transport     string
	URL           string
	Command       string
	Reachable     bool
	Error         string
	ToolCount     int
	Tools         []MCPServerToolRow
	LastCheckedAt time.Time
}

// MCPServerToolRow is one tool entry rendered under a server's
// expanded tool list.
type MCPServerToolRow struct {
	Name        string
	Description string
}

// MCPIndexData backs mcp_index.html.
type MCPIndexData struct {
	Title       string
	CurrentPage string
	Servers     []MCPServerRow
	// Configured reflects whether the daemon has an mcp.servers
	// block at all. Used by the template to differentiate "no
	// daemon config" (operator hint to add the block) from "block
	// present but every server unreachable" (operator hint to
	// debug the wiring).
	Configured bool
}

// MCPIndex renders the daemon-level MCP discovery page.
func (s *Server) MCPIndex(w http.ResponseWriter, r *http.Request) {
	data := MCPIndexData{
		Title:       "MCP Servers",
		CurrentPage: "mcp",
	}

	if s.mcpRegistry != nil {
		// Snapshot is non-blocking — slow MCP servers don't slow
		// down the page render. The registry triggers an async
		// refresh in the background for any stale entry.
		snap := s.mcpRegistry.Snapshot(r.Context())
		data.Configured = len(snap) > 0
		data.Servers = make([]MCPServerRow, 0, len(snap))
		for _, srv := range snap {
			row := MCPServerRow{
				Name:          srv.Name,
				Transport:     srv.Transport,
				URL:           srv.URL,
				Command:       srv.Command,
				Reachable:     srv.Reachable,
				Error:         srv.Error,
				ToolCount:     len(srv.Tools),
				LastCheckedAt: srv.LastCheckedAt,
			}
			if srv.Reachable {
				row.Tools = make([]MCPServerToolRow, 0, len(srv.Tools))
				for _, t := range srv.Tools {
					row.Tools = append(row.Tools, MCPServerToolRow{
						Name:        t.Name,
						Description: t.Description,
					})
				}
			}
			data.Servers = append(data.Servers, row)
		}
	}

	s.render(w, "mcp_index.html", data)
}
