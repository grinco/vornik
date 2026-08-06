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
	"vornik.io/vornik/internal/mcpauth"
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

// TestMcpCredentialScope_InheritedGrantResolvesAtDaemonScope pins the
// resolution rule the MCP-auth design states in §9 and the code did not follow
// until 2026-08-05.
//
// "Daemon-scope servers are available to all projects" is a property; the
// mechanism is which project_id the GRANT is resolved under. A project that
// subscribes by name only inherits the daemon server's connection fields AND
// its credential, so the token must be resolved at "" — the scope the grant was
// actually stored at. Resolving it under the subscribing project's id finds
// nothing, the server registers unauthenticated (§8 registers rather than
// withholds), initialize 401s, and the project holds the server with ZERO tools
// while the daemon-scope registry reports it healthy with a full tool list.
//
// That is exactly what was observed: atlassian green on the MCP tab with 16
// tools, absent from the dispatcher's palette, and
// `vornikctl mcp oauth-status atlassian -p easeit-companion` correctly saying
// it was not connected for that project.
func TestMcpCredentialScope_InheritedGrantResolvesAtDaemonScope(t *testing.T) {
	if got := mcpCredentialScope("easeit-companion", true); got != "" {
		t.Errorf("inherited credential resolved at %q, want \"\" (daemon scope) — "+
			"one grant is shared by every project that does not override it", got)
	}
	// A project that owns its credential keeps its own scope, so two projects
	// with their own auth blocks stay isolated.
	if got := mcpCredentialScope("easeit-companion", false); got != "easeit-companion" {
		t.Errorf("own credential resolved at %q, want the project id", got)
	}
	if got := mcpCredentialScope("", true); got != "" {
		t.Errorf("daemon wiring resolved at %q, want \"\"", got)
	}
}

// TestMcpServerRef_InheritedEntryResolvesTheDaemonGrantRow closes the chain the
// scope rule opens. mcpCredentialScope decides WHICH project_id is used; this
// pins what that id then resolves to — the ref whose (ProjectID, ServerName)
// pair is the literal key of the token row that Tokens.Get reads and, on
// refresh, WithRefreshLock/refreshLocked write back to.
//
// Both halves matter. ProjectID "" means every inheriting project reads and
// refreshes the ONE daemon grant instead of each minting its own row. And the
// URL must come from the daemon entry, because the connector re-checks that a
// token was issued for the resource it is about to be presented to
// (assertResourceMatches) — a ref carrying the project's empty URL would fail
// that check even with the right row.
func TestMcpServerRef_InheritedEntryResolvesTheDaemonGrantRow(t *testing.T) {
	reg := writeMCPInheritFixture(t, "mcp:\n  servers:\n    - name: \"server1\"\n")
	c := &Container{
		Logger:   zerolog.Nop(),
		Registry: reg,
		Config: &config.Config{
			MCP: config.MCPConfig{
				Servers: []config.MCPServerConfig{{
					Name:      "server1",
					Transport: "streamable-http",
					URL:       "https://vendor.example.com/mcp",
					Auth:      mcpauth.Auth{Mode: mcpauth.ModeOAuth},
				}},
			},
		},
	}

	ref, ok := c.mcpServerRef(mcpCredentialScope("test-project", true), "server1")
	if !ok {
		t.Fatal("the daemon-scope server must be resolvable at daemon scope")
	}
	if ref.ProjectID != "" {
		t.Errorf("ref.ProjectID = %q, want \"\" — this is half the token row key, "+
			"so a non-empty value reads a row the consent never wrote", ref.ProjectID)
	}
	if ref.ServerName != "server1" {
		t.Errorf("ref.ServerName = %q, want server1", ref.ServerName)
	}
	if ref.URL != "https://vendor.example.com/mcp" {
		t.Errorf("ref.URL = %q, want the daemon entry's URL — the connector "+
			"validates the grant's audience against this", ref.URL)
	}
	if ref.Auth.Mode != mcpauth.ModeOAuth {
		t.Errorf("ref.Auth.Mode = %q, want oauth", ref.Auth.Mode)
	}
}

// TestMcpServerRef_ProjectScopedLookupOfAnInheritedServerRedirectsToDaemonScope
// covers the resolver as reached by `mcp connect/disconnect/oauth-status -p X`,
// which pass the project's OWN id — unlike the wiring, which already applies
// mcpCredentialScope before calling.
//
// The resolver must apply the same rule, because it is what those three surfaces
// use to decide which grant row they act on. Before this, `mcp connect -p X
// <server>` wrote a project-scope grant that the wiring — resolving at daemon
// scope — never read: consent completed, the operator was told it succeeded, and
// the server still had no tools.
func TestMcpServerRef_ProjectScopedLookupOfAnInheritedServerRedirectsToDaemonScope(t *testing.T) {
	reg := writeMCPInheritFixture(t, "mcp:\n  servers:\n    - name: \"server1\"\n")
	c := &Container{
		Logger:   zerolog.Nop(),
		Registry: reg,
		Config: &config.Config{
			MCP: config.MCPConfig{
				Servers: []config.MCPServerConfig{{
					Name:      "server1",
					Transport: "streamable-http",
					URL:       "https://vendor.example.com/mcp",
					Auth:      mcpauth.Auth{Mode: mcpauth.ModeOAuth},
				}},
			},
		},
	}

	// Asked for at the PROJECT's id, the way the CLI asks.
	ref, ok := c.mcpServerRef("test-project", "server1")
	if !ok {
		t.Fatal("the project's name-only subscription must resolve")
	}
	if ref.ProjectID != "" {
		t.Errorf("ref.ProjectID = %q, want \"\" — a consent given here must land on "+
			"the row the wiring reads, or it is written and never used", ref.ProjectID)
	}
	if ref.InheritedFrom != "test-project" {
		t.Errorf("ref.InheritedFrom = %q, want test-project — the asking project must stay "+
			"reportable so a surface can say which grant is doing the work", ref.InheritedFrom)
	}
}

// A project that declares its OWN auth block owns its credential, so the
// redirect must not fire — otherwise every project would collapse onto the
// daemon's grant and the override the design allows would be unreachable.
func TestMcpServerRef_ProjectWithItsOwnAuthKeepsItsOwnScope(t *testing.T) {
	reg := writeMCPInheritFixture(t, `mcp:
  servers:
    - name: "server1"
      auth:
        mode: oauth
`)
	c := &Container{
		Logger:   zerolog.Nop(),
		Registry: reg,
		Config: &config.Config{
			MCP: config.MCPConfig{
				Servers: []config.MCPServerConfig{{
					Name:      "server1",
					Transport: "streamable-http",
					URL:       "https://vendor.example.com/mcp",
					Auth:      mcpauth.Auth{Mode: mcpauth.ModeOAuth},
				}},
			},
		},
	}

	ref, ok := c.mcpServerRef("test-project", "server1")
	if !ok {
		t.Fatal("the project entry must resolve")
	}
	if ref.ProjectID != "test-project" {
		t.Errorf("ref.ProjectID = %q, want test-project — a project declaring its own "+
			"auth block must keep its own credential scope", ref.ProjectID)
	}
	if ref.InheritedFrom != "" {
		t.Errorf("ref.InheritedFrom = %q, want empty — nothing was inherited", ref.InheritedFrom)
	}
}
