package api

import (
	"encoding/json"
	"net/http"
	"time"

	"vornik.io/vornik/internal/mcp"
)

// mcpServerToolJSON is the per-tool element on the daemon-level
// discovery response. Slimmer than chat.Tool (which carries the
// OpenAI function-calling wrapper used by agents) — operators only
// need name + description to decide if a tool is wired up. The
// shape is stable for the parallel project-form agent to consume.
type mcpServerToolJSON struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// mcpServerJSON is one server row in the discovery response. Mirrors
// mcp.ServerSnapshot but uses JSON-shaped fields the API contract
// commits to (tools: null when unreachable, no internal types).
type mcpServerJSON struct {
	Name          string              `json:"name"`
	Transport     string              `json:"transport"`
	URL           string              `json:"url,omitempty"`
	Command       string              `json:"command,omitempty"`
	Reachable     bool                `json:"reachable"`
	Tools         []mcpServerToolJSON `json:"tools"`
	Error         string              `json:"error,omitempty"`
	LastCheckedAt time.Time           `json:"last_checked_at"`
}

// mcpServersResponse is the wire shape of GET /api/v1/mcp/servers.
type mcpServersResponse struct {
	Servers []mcpServerJSON `json:"servers"`
}

// ListMCPServers handles GET /api/v1/mcp/servers — the daemon-level
// MCP discovery surface. Read-only, no project scoping. Returns the
// cached server catalog with reachability flags; never blocks on a
// slow MCP server (the registry refreshes asynchronously).
//
// IMPORTANT: this endpoint is purely informational. Listing a server
// here does NOT grant any project access to its tools — that still
// requires an explicit mcp.servers entry on the project's YAML.
// The split is deliberate: operators should be able to inventory
// what's installed without auto-extending capabilities to every
// project on the daemon.
func (s *Server) ListMCPServers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.mcpRegistry == nil {
		respondJSON(w, http.StatusOK, mcpServersResponse{Servers: []mcpServerJSON{}})
		return
	}

	snap := s.mcpRegistry.Snapshot(r.Context())
	out := mcpServersResponse{Servers: make([]mcpServerJSON, 0, len(snap))}
	for _, srv := range snap {
		out.Servers = append(out.Servers, toJSON(srv))
	}
	respondJSON(w, http.StatusOK, out)
}

// toJSON projects an mcp.ServerSnapshot into the wire shape. Kept
// in a dedicated helper so the JSON contract is testable without
// constructing a Server.
func toJSON(s mcp.ServerSnapshot) mcpServerJSON {
	out := mcpServerJSON{
		Name:          s.Name,
		Transport:     s.Transport,
		URL:           s.URL,
		Command:       s.Command,
		Reachable:     s.Reachable,
		Error:         s.Error,
		LastCheckedAt: s.LastCheckedAt,
	}
	if s.Reachable {
		// Empty tool list is a legitimate state (server connected,
		// advertises no tools today). Distinguished from
		// reachable=false on the wire by Reachable + an empty
		// array (vs reachable=false where Tools is nil).
		tools := make([]mcpServerToolJSON, 0, len(s.Tools))
		for _, t := range s.Tools {
			tools = append(tools, mcpServerToolJSON{
				Name:        t.Name,
				Description: t.Description,
			})
		}
		out.Tools = tools
	}
	return out
}

// Ensure JSON encoder is the canonical respondJSON path. Compile-time
// guard against accidental drift.
var _ = json.Marshal
