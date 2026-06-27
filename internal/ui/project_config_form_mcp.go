package ui

// MCP-servers section of the form-driven project config editor.
// The "Advanced YAML" footnote used to send operators to the raw
// YAML route just to subscribe their project to an MCP server +
// narrow the allowed tools. This file fills that gap: the form
// renders one row per daemon-known MCP server (plus any custom
// project-only servers already on disk) and saves the operator's
// picks back into the project YAML's mcp.servers block.
//
// Form contract (the form field names the template uses):
//
//   mcpSubscribe_<serverName>            "on" when checked
//   mcpAllowedToolsMode_<serverName>     "all" | "selected"
//   mcpAllowedTool_<serverName>_<tool>   "on" when ticked
//                                        (only honoured when
//                                        mode=selected)
//
//   mcpCustomCount                       integer
//   mcpCustom_<i>_name                   server identifier
//   mcpCustom_<i>_transport              "stdio" | "sse" | "http"
//   mcpCustom_<i>_url                    URL (sse / http) or empty
//   mcpCustom_<i>_allowedTools           textarea, one tool per line
//
// "Subscribed" semantics: presence of an mcp.servers entry with
// the daemon server's name. Absence = the project is invisible to
// that server (matches today's behaviour — projects opt in, not
// out). Within a subscribed entry, an empty allowed_tools slice
// means "no per-tool narrowing, inherit the daemon catalog" (also
// matches today's loader semantics).

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/registry"
)

// MCPRegistryTool is the daemon-known description of a single tool
// advertised by an MCP server. Mirrors the {name, description}
// shape returned by GET /api/v1/mcp/servers (slice 1 of the
// parallel agent's work).
type MCPRegistryTool struct {
	Name        string
	Description string
}

// MCPRegistryServer is the daemon-level view of one MCP server
// the registry knows about. The form's MCP section renders one
// checkbox row per element of this slice.
//
// Reachable / Error mirror the upstream endpoint's health fields
// so operators can see at-a-glance why a server's tool catalog is
// empty (auth failure, daemon couldn't dial, etc) without dropping
// to the daemon log.
type MCPRegistryServer struct {
	Name      string
	Transport string
	URL       string
	Tools     []MCPRegistryTool
	Reachable bool
	Error     string
}

// MCPFormRegistrySource yields the daemon-level MCP server registry.
// The parallel agent's slice 1 will wire an HTTP-client adapter
// (or a direct daemon-registry shim) that satisfies this interface;
// until then, the form degrades gracefully — nil source = empty
// state banner.
type MCPFormRegistrySource interface {
	Servers(ctx context.Context) ([]MCPRegistryServer, error)
}

// MCPRegistryRow is a single rendered registry-driven row in the
// form. Joins one MCPRegistryServer with the project's current
// subscribe state + per-tool allowlist.
type MCPRegistryRow struct {
	Server     MCPRegistryServer
	Subscribed bool
	// AllowAllTools is true when the project subscribes to this
	// server without per-tool narrowing (mcp.servers[i].allowed_tools
	// is empty). Renders as the "all" master checkbox checked.
	AllowAllTools bool
	// AllowedTools is the set of per-tool names the project
	// currently allows. Maps to the per-tool checkboxes ticked
	// when AllowAllTools is false.
	AllowedTools map[string]bool
}

// MCPCustomRow is a project-only MCP server entry (not in the
// daemon registry — typically a one-off URL or experimental
// stdio config). Rendered as an editable row below the registry-
// known rows. Operators add / remove these via the form.
type MCPCustomRow struct {
	Index        int
	Name         string
	Transport    string
	URL          string
	AllowedTools string // newline-separated for the textarea
}

// populateMCPSection joins the daemon registry against the
// project's current mcp.servers block and builds the data the
// template iterates over. Called on every form render (initial
// GET + post-save refresh).
//
// Defensive against a nil source / nil project: leaves the rows
// empty and surfaces a banner via MCPRegistryUnavailable so the
// operator knows why the section is empty.
func populateMCPSection(data *ProjectConfigFormData, source MCPFormRegistrySource, proj *registry.Project) {
	var projServers []registry.MCPServerConfig
	if proj != nil {
		projServers = proj.MCP.Servers
	}

	// Build a lookup of project servers by name so the registry
	// join can find the current subscribe state in O(1).
	byName := make(map[string]registry.MCPServerConfig, len(projServers))
	for _, s := range projServers {
		byName[s.Name] = s
	}

	// Try the daemon registry. A nil source or error degrades to
	// the banner; the project's own custom rows still render.
	var registryServers []MCPRegistryServer
	if source != nil {
		ctx, cancel := context.WithTimeout(context.Background(), mcpRegistryFetchTimeout)
		defer cancel()
		srv, err := source.Servers(ctx)
		if err != nil {
			data.MCPRegistryUnavailable = true
			data.MCPRegistryError = err.Error()
		} else {
			registryServers = srv
		}
	} else {
		data.MCPRegistryUnavailable = true
		data.MCPRegistryError = "no daemon-level MCP registry configured"
	}

	registryByName := make(map[string]bool, len(registryServers))
	for _, s := range registryServers {
		registryByName[s.Name] = true
		row := MCPRegistryRow{Server: s, AllowedTools: map[string]bool{}}
		if proj, ok := byName[s.Name]; ok {
			row.Subscribed = true
			if len(proj.AllowedTools) == 0 {
				row.AllowAllTools = true
			} else {
				for _, t := range proj.AllowedTools {
					row.AllowedTools[t] = true
				}
			}
		} else {
			// Default for a freshly-subscribed server: inherit
			// the full catalog (AllowAllTools=true). The template
			// shows the master checkbox ticked.
			row.AllowAllTools = true
		}
		data.MCPRegistryRows = append(data.MCPRegistryRows, row)
	}

	// Custom rows: any project-only server NOT in the daemon
	// registry. Rendered as editable name+transport+url+tools.
	for i, s := range projServers {
		if registryByName[s.Name] {
			continue
		}
		data.MCPCustomRows = append(data.MCPCustomRows, MCPCustomRow{
			Index:        i,
			Name:         s.Name,
			Transport:    s.Transport,
			URL:          s.URL,
			AllowedTools: strings.Join(s.AllowedTools, "\n"),
		})
	}
}

// mcpRegistryFetchTimeout caps the registry probe so a wedged
// daemon-level MCP endpoint doesn't stall the form render. Form
// rendering is a foreground operator action — better to fail fast
// and show the banner than to spin.
const mcpRegistryFetchTimeout = 3 * time.Second

// overlayMCPSection mirrors overlayFormValuesOntoData but for the
// MCP section. Separated so the failure-fallback render reflects
// the operator's MCP picks alongside their other field edits.
//
// Important: the *daemon registry rows* keep their canonical
// Server.Name/Transport/URL — those come from the registry, not
// the form. Only subscribe state + per-tool selection round-trip
// through the POST body.
func overlayMCPSection(data *ProjectConfigFormData, r *http.Request) {
	// Registry-driven rows: walk the existing data.MCPRegistryRows
	// (populated from the form data source at render time) and
	// update each row's subscribe / allowed-tools state from the
	// posted form values.
	for i := range data.MCPRegistryRows {
		row := &data.MCPRegistryRows[i]
		name := row.Server.Name
		row.Subscribed = parseFormBool(r.FormValue("mcpSubscribe_" + name))
		mode := strings.TrimSpace(r.FormValue("mcpAllowedToolsMode_" + name))
		row.AllowAllTools = mode != "selected"
		row.AllowedTools = map[string]bool{}
		if row.Subscribed && !row.AllowAllTools {
			for _, tool := range row.Server.Tools {
				if parseFormBool(r.FormValue("mcpAllowedTool_" + name + "_" + tool.Name)) {
					row.AllowedTools[tool.Name] = true
				}
			}
		}
	}

	// Custom rows: clobber + rebuild from posted fields. The form
	// renders a hidden mcpCustomCount; we walk i from 0..count-1
	// and skip any row whose name is blank (lets operators delete
	// a row by clearing its name input — simpler than a separate
	// remove control).
	data.MCPCustomRows = nil
	// Clamp the loop count so a malformed POST can't trigger an
	// unbounded loop. 256 custom servers per project is plenty —
	// more than that and the operator has bigger problems.
	count := parseFormUint(r.FormValue("mcpCustomCount"), 256)
	for i := 0; i < count; i++ {
		name := strings.TrimSpace(r.FormValue(fmt.Sprintf("mcpCustom_%d_name", i)))
		if name == "" {
			continue
		}
		data.MCPCustomRows = append(data.MCPCustomRows, MCPCustomRow{
			Index:        i,
			Name:         name,
			Transport:    strings.TrimSpace(r.FormValue(fmt.Sprintf("mcpCustom_%d_transport", i))),
			URL:          strings.TrimSpace(r.FormValue(fmt.Sprintf("mcpCustom_%d_url", i))),
			AllowedTools: r.FormValue(fmt.Sprintf("mcpCustom_%d_allowedTools", i)),
		})
	}
}

// buildMCPServersValue assembles the []map[string]any patch value
// for mcp.servers from the joined registry + custom rows. Output
// shape matches registry.MCPServerConfig (name, transport, url,
// allowed_tools) so the loader unmarshals it cleanly.
//
// Returns (nil, true) when the operator subscribed to zero servers
// AND has no custom rows — caller deletes the entire mcp block in
// that case (avoids `mcp: {servers: []}` litter).
func buildMCPServersValue(data *ProjectConfigFormData) (value []map[string]any, empty bool) {
	for _, row := range data.MCPRegistryRows {
		if !row.Subscribed {
			continue
		}
		entry := map[string]any{
			"name":      row.Server.Name,
			"transport": row.Server.Transport,
			"_order":    []string{"name", "transport", "url", "allowed_tools"},
		}
		if row.Server.URL != "" {
			entry["url"] = row.Server.URL
		}
		var allowed []string
		if !row.AllowAllTools {
			for _, tool := range row.Server.Tools {
				if row.AllowedTools[tool.Name] {
					allowed = append(allowed, tool.Name)
				}
			}
		}
		if len(allowed) > 0 {
			entry["allowed_tools"] = allowed
		}
		value = append(value, entry)
	}
	for _, row := range data.MCPCustomRows {
		entry := map[string]any{
			"name":   row.Name,
			"_order": []string{"name", "transport", "url", "allowed_tools"},
		}
		if row.Transport != "" {
			entry["transport"] = row.Transport
		}
		if row.URL != "" {
			entry["url"] = row.URL
		}
		if tools := splitChipList(row.AllowedTools); len(tools) > 0 {
			entry["allowed_tools"] = tools
		}
		value = append(value, entry)
	}
	return value, len(value) == 0
}

// buildMCPPatches emits the yamlPatch list that drives mcp.servers
// for one save. Returned as a small []yamlPatch so the main
// buildFormPatches can splice them in without growing too long
// for human review.
func buildMCPPatches(data *ProjectConfigFormData) []yamlPatch {
	value, empty := buildMCPServersValue(data)
	if empty {
		// Single delete patch — clears the entire mcp block when
		// no servers are subscribed and no customs remain. Keeps
		// the YAML tidy across operator toggles.
		return []yamlPatch{
			{Path: []string{"mcp", "servers"}, Value: []map[string]any{}, RemoveIfEmpty: true},
		}
	}
	return []yamlPatch{
		{Path: []string{"mcp", "servers"}, Value: value},
	}
}

// parseFormUint is a defensive helper for the hidden custom-row
// count input. Unlike parseFormInt, it clamps to a sane upper bound
// so a malformed request can't trigger a hostile loop count.
func parseFormUint(raw string, max int) int {
	t := strings.TrimSpace(raw)
	if t == "" {
		return 0
	}
	n, err := strconv.Atoi(t)
	if err != nil || n < 0 {
		return 0
	}
	if n > max {
		return max
	}
	return n
}
