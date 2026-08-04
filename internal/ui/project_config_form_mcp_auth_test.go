package ui

import (
	"testing"

	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/registry"
)

// The project-config form rebuilds the WHOLE mcp.servers list from form state
// on every save, and it renders no auth controls (those arrive with step 6 of
// mcp-server-authentication-design.md). Without preservation, an operator who
// ticks an unrelated checkbox silently deletes the auth block and
// unauthenticates the server — the exact silent-config-loss class the
// name-only-subscription bug caused in production 2026-07-29.

func TestPopulateMCPSection_CapturesAuthForPreservation(t *testing.T) {
	proj := &registry.Project{
		ID: "p1",
		MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{{
			Name:      "n8n",
			Transport: "streamable-http",
			URL:       "https://n8n.example.com/mcp/abc",
			Auth: mcpauth.Auth{
				Mode:        mcpauth.ModeStatic,
				ValueFrom:   "secret://N8N_TOKEN",
				ValuePrefix: "Bearer ",
			},
		}}},
	}
	data := &ProjectConfigFormData{}
	populateMCPSection(data, nil, proj)

	preserved, ok := data.MCPPreservedFields["n8n"]
	if !ok {
		t.Fatalf("no preserved fields captured; got %v", data.MCPPreservedFields)
	}
	auth, ok := preserved["auth"].(map[string]any)
	if !ok {
		t.Fatalf("preserved auth is %T, want map[string]any", preserved["auth"])
	}
	if auth["mode"] != string(mcpauth.ModeStatic) {
		t.Errorf("auth.mode = %v", auth["mode"])
	}
	if auth["value_from"] != "secret://N8N_TOKEN" {
		t.Errorf("auth.value_from = %v", auth["value_from"])
	}
	if auth["value_prefix"] != "Bearer " {
		t.Errorf("auth.value_prefix = %v (the trailing space is load-bearing)", auth["value_prefix"])
	}
}

func TestPopulateMCPSection_NoAuthCapturesNothing(t *testing.T) {
	proj := &registry.Project{
		ID:  "p1",
		MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{{Name: "scraper", Transport: "sse", URL: "http://x/sse"}}},
	}
	data := &ProjectConfigFormData{}
	populateMCPSection(data, nil, proj)

	if len(data.MCPPreservedFields) != 0 {
		t.Errorf("a server with no auth block must contribute nothing to preserve: %v", data.MCPPreservedFields)
	}
}

// TestBuildMCPServersValue_PreservesAuthOnCustomRow — a project-only server the
// form renders as an editable custom row.
func TestBuildMCPServersValue_PreservesAuthOnCustomRow(t *testing.T) {
	data := &ProjectConfigFormData{
		MCPCustomRows: []MCPCustomRow{{
			Index: 0, Name: "n8n", Transport: "streamable-http", URL: "https://n8n.example.com/mcp/abc",
		}},
		MCPPreservedFields: map[string]map[string]any{
			"n8n": {"auth": map[string]any{"mode": "static", "value_from": "secret://N8N_TOKEN"}},
		},
	}
	value, empty := buildMCPServersValue(data)
	if empty || len(value) != 1 {
		t.Fatalf("want one entry, got %v (empty=%v)", value, empty)
	}
	auth, ok := value[0]["auth"].(map[string]any)
	if !ok {
		t.Fatalf("auth block was dropped on save: %v", value[0])
	}
	if auth["value_from"] != "secret://N8N_TOKEN" {
		t.Errorf("auth.value_from = %v", auth["value_from"])
	}
	// The writer needs the key in its ordering hint or it lands in an
	// unpredictable position on rewrite.
	order, _ := value[0]["_order"].([]string)
	var found bool
	for _, k := range order {
		if k == "auth" {
			found = true
		}
	}
	if !found {
		t.Errorf("_order = %v, want it to include auth", order)
	}
}

// TestBuildMCPServersValue_PreservesAuthOnSubscribedRegistryRow — the name-only
// subscription shape. The auth block lives on the project entry, so it must
// survive even though the entry deliberately carries no connection fields.
func TestBuildMCPServersValue_PreservesAuthOnSubscribedRegistryRow(t *testing.T) {
	data := &ProjectConfigFormData{
		MCPRegistryRows: []MCPRegistryRow{{
			Server:        MCPRegistryServer{Name: "n8n", Transport: "streamable-http"},
			Subscribed:    true,
			AllowAllTools: true,
		}},
		MCPPreservedFields: map[string]map[string]any{
			"n8n": {"auth": map[string]any{"mode": "static", "value_from": "secret://N8N_TOKEN"}},
		},
	}
	value, _ := buildMCPServersValue(data)
	if len(value) != 1 {
		t.Fatalf("want one entry, got %v", value)
	}
	if _, ok := value[0]["auth"]; !ok {
		t.Errorf("auth block was dropped on save: %v", value[0])
	}
	// Preservation must not resurrect the connection fields a name-only
	// subscription deliberately omits (the 2026-07-29 production break).
	if _, ok := value[0]["transport"]; ok {
		t.Errorf("name-only subscription must not gain a transport: %v", value[0])
	}
}

// TestBuildMCPServersValue_PreservationIsScopedToTheServerItCameFrom guards
// against a preserved block bleeding onto a different server.
func TestBuildMCPServersValue_PreservationIsScopedToTheServerItCameFrom(t *testing.T) {
	data := &ProjectConfigFormData{
		MCPCustomRows: []MCPCustomRow{{Index: 0, Name: "other", Transport: "sse", URL: "http://x/sse"}},
		MCPPreservedFields: map[string]map[string]any{
			"n8n": {"auth": map[string]any{"mode": "static"}},
		},
	}
	value, _ := buildMCPServersValue(data)
	if len(value) != 1 {
		t.Fatalf("want one entry, got %v", value)
	}
	if _, ok := value[0]["auth"]; ok {
		t.Errorf("another server's auth block leaked onto %v", value[0])
	}
}

// TestBuildMCPServersValue_PreservationCannotBeUsedToInjectManagedFields — the
// preserved map comes from the project's on-disk YAML rather than the POST
// body, but it must still not be able to override a field the form owns.
func TestBuildMCPServersValue_PreservationCannotOverrideManagedFields(t *testing.T) {
	data := &ProjectConfigFormData{
		MCPCustomRows: []MCPCustomRow{{Index: 0, Name: "n8n", Transport: "sse", URL: "http://real/sse"}},
		MCPPreservedFields: map[string]map[string]any{
			"n8n": {
				"auth": map[string]any{"mode": "static"},
				"url":  "http://attacker/sse",
				"name": "someone-else",
			},
		},
	}
	value, _ := buildMCPServersValue(data)
	if len(value) != 1 {
		t.Fatalf("want one entry, got %v", value)
	}
	if value[0]["url"] != "http://real/sse" {
		t.Errorf("url = %v, want the form's value", value[0]["url"])
	}
	if value[0]["name"] != "n8n" {
		t.Errorf("name = %v, want the form's value", value[0]["name"])
	}
}
