package service

// Regression: template-bundles-v2 final review — name-only subscription
// entries (tool-assistant template) were silently skipped with
// `unsupported transport ""`. The tool-assistant project.yaml.tmpl only
// ever renders `mcp.servers` entries as `- name: "<server>"`, relying on
// the daemon to fill in the actual connection details for a server it
// already knows about at the daemon level (config.yaml's mcp.servers
// block). mcpDesiredServers copied project entries verbatim, so those
// clients never got a Transport and mcp.Client.Connect's default case
// rejected them outright. See mcpDesiredServers in
// container_subsystems.go for the fix (transport inheritance).

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
)

// writeMCPInheritFixture builds a minimal registry layout (project +
// swarm + workflow, satisfying cross-reference validation) with a
// single project whose mcp.servers block is supplied by the caller.
func writeMCPInheritFixture(t *testing.T, projectMCPYAML string) *registry.Registry {
	t.Helper()
	tmpDir := t.TempDir()
	for _, subdir := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(tmpDir, subdir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", subdir, err)
		}
	}

	swarmYAML := "---\nswarmId: \"test-swarm\"\nroles:\n  - name: \"coder\"\n    runtime:\n      image: \"test:latest\"\n---\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "swarms", "test.md"), []byte(swarmYAML), 0o644); err != nil {
		t.Fatalf("write swarm: %v", err)
	}

	workflowYAML := "---\nworkflowId: \"test-workflow\"\nentrypoint: \"step1\"\nsteps:\n  step1:\n    type: \"agent\"\n    prompt: \"do work\"\n    role: \"coder\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "workflows", "test.md"), []byte(workflowYAML), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	projectYAML := "projectId: \"test-project\"\nswarmId: \"test-swarm\"\ndefaultWorkflowId: \"test-workflow\"\n" + projectMCPYAML
	if err := os.WriteFile(filepath.Join(tmpDir, "projects", "test.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project: %v", err)
	}

	reg := registry.New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return reg
}

// TestMcpDesiredServers_NameOnlyEntryInheritsDaemonTransport covers case
// (a): a project entry that supplies only a name (the shape the
// tool-assistant template renders) must inherit the matching
// daemon-level server's connection fields, while keeping the project's
// own allowed_tools narrowing.
func TestMcpDesiredServers_NameOnlyEntryInheritsDaemonTransport(t *testing.T) {
	reg := writeMCPInheritFixture(t, "mcp:\n  servers:\n    - name: \"server1\"\n      allowed_tools: [\"read_file\"]\n")

	c := &Container{
		Logger:   zerolog.Nop(),
		Registry: reg,
		Config: &config.Config{
			MCP: config.MCPConfig{
				Servers: []config.MCPServerConfig{
					{
						Name:      "server1",
						Transport: "stdio",
						Command:   "server1-bin",
						Args:      []string{"--flag"},
						Env:       map[string]string{"FOO": "bar"},
					},
				},
			},
		},
	}

	desired := c.mcpDesiredServers()
	servers, ok := desired["test-project"]
	if !ok || len(servers) != 1 {
		t.Fatalf("desired[test-project] = %v, want exactly one server", servers)
	}
	got := servers[0]
	if got.Transport != "stdio" {
		t.Errorf("Transport = %q, want inherited %q", got.Transport, "stdio")
	}
	if got.Command != "server1-bin" {
		t.Errorf("Command = %q, want inherited %q", got.Command, "server1-bin")
	}
	if !reflect.DeepEqual(got.Args, []string{"--flag"}) {
		t.Errorf("Args = %v, want inherited [--flag]", got.Args)
	}
	if !reflect.DeepEqual(got.Env, map[string]string{"FOO": "bar"}) {
		t.Errorf("Env = %v, want inherited map[FOO:bar]", got.Env)
	}
	got.Args[0] = "--mutated"
	got.Env["FOO"] = "mutated"
	daemon := c.Config.MCP.Servers[0]
	if !reflect.DeepEqual(daemon.Args, []string{"--flag"}) {
		t.Errorf("inherited Args aliased daemon config: %v", daemon.Args)
	}
	if !reflect.DeepEqual(daemon.Env, map[string]string{"FOO": "bar"}) {
		t.Errorf("inherited Env aliased daemon config: %v", daemon.Env)
	}
	if !reflect.DeepEqual(got.AllowedTools, []string{"read_file"}) {
		t.Errorf("AllowedTools = %v, want project's own [read_file] preserved", got.AllowedTools)
	}
}

// TestMcpDesiredServers_OwnTransportUntouched covers case (b): a
// project entry that already carries its own transport must NOT be
// overwritten by a same-named daemon-level server's fields.
func TestMcpDesiredServers_OwnTransportUntouched(t *testing.T) {
	reg := writeMCPInheritFixture(t, "mcp:\n  servers:\n    - name: \"server1\"\n      transport: \"sse\"\n      url: \"https://project-own.example/mcp\"\n")

	c := &Container{
		Logger:   zerolog.Nop(),
		Registry: reg,
		Config: &config.Config{
			MCP: config.MCPConfig{
				Servers: []config.MCPServerConfig{
					{
						Name:      "server1",
						Transport: "stdio",
						Command:   "server1-bin",
					},
				},
			},
		},
	}

	desired := c.mcpDesiredServers()
	got := desired["test-project"][0]
	if got.Transport != "sse" {
		t.Errorf("Transport = %q, want project's own %q untouched", got.Transport, "sse")
	}
	if got.URL != "https://project-own.example/mcp" {
		t.Errorf("URL = %q, want project's own untouched", got.URL)
	}
	if got.Command != "" {
		t.Errorf("Command = %q, want empty (no bleed-through from daemon config)", got.Command)
	}
}

// TestMcpDesiredServers_NameOnlyNoDaemonMatch_PassesThroughUnchanged
// covers case (c): a name-only entry with no matching daemon-level
// server keeps today's log-and-skip path — mcpDesiredServers must not
// panic or synthesize a transport out of nothing.
func TestMcpDesiredServers_NameOnlyNoDaemonMatch_PassesThroughUnchanged(t *testing.T) {
	reg := writeMCPInheritFixture(t, "mcp:\n  servers:\n    - name: \"unknown-server\"\n")

	c := &Container{
		Logger:   zerolog.Nop(),
		Registry: reg,
		Config:   &config.Config{},
	}

	desired := c.mcpDesiredServers()
	got := desired["test-project"][0]
	if got.Transport != "" {
		t.Errorf("Transport = %q, want empty (no daemon match to inherit from)", got.Transport)
	}
	if got.Name != "unknown-server" {
		t.Errorf("Name = %q, want unknown-server", got.Name)
	}
}
