package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// pagedropTools are the four namespaced tools the assistant + janka projects
// must grant agents permission to CALL (project permissions.allowedTools).
var pagedropTools = []string{
	"mcp__pagedrop__pagedrop_publish_page",
	"mcp__pagedrop__pagedrop_publish_doc",
	"mcp__pagedrop__pagedrop_republish",
	"mcp__pagedrop__pagedrop_list",
}

// pagedropServerTools are the raw tool names that must be set as allowed_tools
// on each project's pagedrop mcp.servers entry. This is a SEPARATE gate from
// permissions.allowedTools: it narrows what the server EXPOSES at discovery.
// A name-only entry (no allowed_tools) inherits the daemon connection but NOT
// its allowed_tools (container_subsystems.go copies the project entry's
// AllowedTools), so it would surface all six upstream tools — including
// publish_deck/search — to the dispatcher even though calls are gated. This
// list guards that regression (see the design doc's Implementation Notes #3).
var pagedropServerTools = []string{
	"pagedrop_publish_page",
	"pagedrop_publish_doc",
	"pagedrop_republish",
	"pagedrop_list",
}

func writeProject(t *testing.T, dir, name, body string) {
	t.Helper()
	// LoadProjects reads project YAML files from a "projects" subdirectory
	// of the directory it is given, not from the directory itself.
	projectsDir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", projectsDir, err)
	}
	if err := os.WriteFile(filepath.Join(projectsDir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func hasAll(got, want []string) bool {
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func hasAny(got, want []string) bool {
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if set[w] {
			return true
		}
	}
	return false
}

// TestPageDropGrant_ScopedToTwoProjects mirrors the shipped config shape:
// assistant + janka grant the four pagedrop tools via permissions.allowedTools
// plus a name-only mcp.servers entry; a third project grants none.
func TestPageDropGrant_ScopedToTwoProjects(t *testing.T) {
	dir := t.TempDir()

	granted := `projectId: %s
swarmId: %s-swarm
defaultWorkflowId: adaptive
mcp:
  servers:
    - name: pagedrop
permissions:
  allowedTools:
    - "mcp__pagedrop__pagedrop_publish_page"
    - "mcp__pagedrop__pagedrop_publish_doc"
    - "mcp__pagedrop__pagedrop_republish"
    - "mcp__pagedrop__pagedrop_list"
`
	writeProject(t, dir, "assistant", "projectId: assistant\nswarmId: assistant-swarm\ndefaultWorkflowId: adaptive\nmcp:\n  servers:\n    - name: pagedrop\n      allowed_tools:\n        - pagedrop_publish_page\n        - pagedrop_publish_doc\n        - pagedrop_republish\n        - pagedrop_list\npermissions:\n  allowedTools:\n    - \"mcp__pagedrop__pagedrop_publish_page\"\n    - \"mcp__pagedrop__pagedrop_publish_doc\"\n    - \"mcp__pagedrop__pagedrop_republish\"\n    - \"mcp__pagedrop__pagedrop_list\"\n")
	writeProject(t, dir, "janka", "projectId: janka\nswarmId: janka-swarm\ndefaultWorkflowId: adaptive\nmcp:\n  servers:\n    - name: pagedrop\n      allowed_tools:\n        - pagedrop_publish_page\n        - pagedrop_publish_doc\n        - pagedrop_republish\n        - pagedrop_list\npermissions:\n  allowedTools:\n    - \"mcp__pagedrop__pagedrop_publish_page\"\n    - \"mcp__pagedrop__pagedrop_publish_doc\"\n    - \"mcp__pagedrop__pagedrop_republish\"\n    - \"mcp__pagedrop__pagedrop_list\"\n")
	writeProject(t, dir, "ibkr-trader", "projectId: ibkr-trader\nswarmId: ibkr-trader-swarm\ndefaultWorkflowId: adaptive\npermissions:\n  allowedTools:\n    - \"place_order\"\n")
	_ = granted

	projects, err := LoadProjects(dir)
	if err != nil {
		t.Fatalf("LoadProjects: %v", err)
	}

	for _, name := range []string{"assistant", "janka"} {
		p := projects[name]
		if p == nil {
			t.Fatalf("project %q not loaded", name)
		}
		if !hasAll(p.Permissions.AllowedTools, pagedropTools) {
			t.Errorf("%s: missing pagedrop tools; got %v", name, p.Permissions.AllowedTools)
		}
		var pagedropSrv *MCPServerConfig
		for i := range p.MCP.Servers {
			if p.MCP.Servers[i].Name == "pagedrop" {
				pagedropSrv = &p.MCP.Servers[i]
			}
		}
		if pagedropSrv == nil {
			t.Errorf("%s: missing pagedrop mcp.servers entry", name)
		} else if !hasAll(pagedropSrv.AllowedTools, pagedropServerTools) {
			// Guards the rollout regression (design Implementation Notes #3):
			// without per-server allowed_tools, discovery leaks publish_deck/search.
			t.Errorf("%s: pagedrop mcp.servers entry must set allowed_tools to narrow discovery to %v; got %v",
				name, pagedropServerTools, pagedropSrv.AllowedTools)
		}
	}

	if other := projects["ibkr-trader"]; other == nil {
		t.Fatal("ibkr-trader not loaded")
	} else if hasAny(other.Permissions.AllowedTools, pagedropTools) {
		t.Errorf("ibkr-trader must NOT grant pagedrop tools; got %v", other.Permissions.AllowedTools)
	}
}
