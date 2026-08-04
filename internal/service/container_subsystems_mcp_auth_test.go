package service

import (
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcpauth"
)

// The wiring layer is where an `auth:` block becomes credential material. These
// tests cover step 2 of mcp-server-authentication-design.md end to end from the
// operator's YAML: mode static -> a header, mode env -> subprocess env, mode
// oauth -> registered but unauthenticated (the flow ships in steps 3-5), and an
// unresolvable secret -> the server is withheld rather than silently
// unauthenticated.

func containerWithProjectMCP(t *testing.T, projectMCPYAML string, daemonServers ...config.MCPServerConfig) *Container {
	t.Helper()
	return &Container{
		Logger:   zerolog.Nop(),
		Registry: writeMCPInheritFixture(t, projectMCPYAML),
		Config:   &config.Config{MCP: config.MCPConfig{Servers: daemonServers}},
	}
}

func TestMcpDesiredServers_StaticAuthBecomesAHeader(t *testing.T) {
	t.Setenv("N8N_MCP_TOKEN", "tok-123")

	c := containerWithProjectMCP(t, `permissions:
  secrets: ["N8N_MCP_TOKEN"]
mcp:
  servers:
    - name: "n8n"
      transport: "streamable-http"
      url: "https://n8n.example.com/mcp/abc"
      auth:
        mode: static
        value_from: "secret://N8N_MCP_TOKEN"
        value_prefix: "Bearer "
`)

	servers := c.mcpDesiredServers()["test-project"]
	if len(servers) != 1 {
		t.Fatalf("want exactly one server, got %d", len(servers))
	}
	if got := servers[0].AuthHeaders["Authorization"]; got != "Bearer tok-123" {
		t.Errorf("AuthHeaders[Authorization] = %q, want %q", got, "Bearer tok-123")
	}
	// The credential must not land in the plain Headers map, which IS
	// serialized in operator-facing surfaces.
	if _, ok := servers[0].Headers["Authorization"]; ok {
		t.Error("a resolved credential must live in AuthHeaders, not Headers")
	}
}

func TestMcpDesiredServers_EnvAuthBecomesSubprocessEnv(t *testing.T) {
	t.Setenv("REDDIT_ID", "id-1")
	t.Setenv("REDDIT_SECRET", "sec-1")

	c := containerWithProjectMCP(t, `permissions:
  secrets: ["REDDIT_ID", "REDDIT_SECRET"]
mcp:
  servers:
    - name: "reddit"
      transport: "stdio"
      command: "reddit-mcp"
      auth:
        mode: env
        env_from:
          REDDIT_CLIENT_ID: "secret://REDDIT_ID"
          REDDIT_CLIENT_SECRET: "secret://REDDIT_SECRET"
`)

	servers := c.mcpDesiredServers()["test-project"]
	if len(servers) != 1 {
		t.Fatalf("want exactly one server, got %d", len(servers))
	}
	if got := servers[0].AuthEnv["REDDIT_CLIENT_SECRET"]; got != "sec-1" {
		t.Errorf("AuthEnv[REDDIT_CLIENT_SECRET] = %q, want %q", got, "sec-1")
	}
	if len(servers[0].AuthHeaders) != 0 {
		t.Error("a stdio credential must not become an HTTP header")
	}
}

// TestMcpDesiredServers_UnresolvableSecretWithholdsTheServer — fail closed. A
// server registered without the credential it declares would 401 on every tool
// call, which reads to an operator as a vendor permissions problem. Withholding
// it, with an ERROR naming the secret, points at the actual cause.
func TestMcpDesiredServers_UnresolvableSecretWithholdsTheServer(t *testing.T) {
	c := containerWithProjectMCP(t, `permissions:
  secrets: ["N8N_MCP_TOKEN"]
mcp:
  servers:
    - name: "n8n"
      transport: "streamable-http"
      url: "https://n8n.example.com/mcp/abc"
      auth:
        mode: static
        value_from: "secret://N8N_MCP_TOKEN"
`)

	if servers := c.mcpDesiredServers()["test-project"]; len(servers) != 0 {
		t.Errorf("a server whose credential cannot be resolved must be withheld, got %+v", servers)
	}
}

// TestMcpDesiredServers_OAuthRegistersUnauthenticatedForNow — the config
// surface lands before the flow, so an operator can write and validate the block
// today. The server stays usable (its unauthenticated tools still work, and its
// authenticated ones fail at the vendor with a real 401) instead of vanishing.
func TestMcpDesiredServers_OAuthRegistersUnauthenticatedForNow(t *testing.T) {
	c := containerWithProjectMCP(t, `mcp:
  servers:
    - name: "atlassian"
      transport: "streamable-http"
      url: "https://mcp.atlassian.com/v1/mcp/authv2"
      auth:
        mode: oauth
        scopes: ["read:jira-work"]
`)

	servers := c.mcpDesiredServers()["test-project"]
	if len(servers) != 1 {
		t.Fatalf("an oauth server must still be registered, got %d", len(servers))
	}
	if len(servers[0].AuthHeaders) != 0 {
		t.Errorf("no OAuth token exists yet; AuthHeaders = %v", servers[0].AuthHeaders)
	}
}

// TestMcpDesiredServers_NoAuthBlockIsUnchanged is the back-compat guard: every
// server configured before this feature existed must be wired exactly as before.
func TestMcpDesiredServers_NoAuthBlockIsUnchanged(t *testing.T) {
	c := containerWithProjectMCP(t, `mcp:
  servers:
    - name: "scraper"
      transport: "sse"
      url: "http://127.0.0.1:9000/sse"
`)

	servers := c.mcpDesiredServers()["test-project"]
	if len(servers) != 1 {
		t.Fatalf("want exactly one server, got %d", len(servers))
	}
	if servers[0].AuthHeaders != nil || servers[0].AuthEnv != nil {
		t.Errorf("a server with no auth block must carry no auth material: %+v", servers[0])
	}
}

// TestMcpDesiredServers_NameOnlyEntryInheritsDaemonAuth — a project entry that
// supplies only a name means "use that daemon server", so it inherits the
// daemon block's credentials along with its connection fields. Daemon-scope
// servers are reachable from every project by design (§9), which is why the
// project's own permissions.secrets does not gate an inherited block.
func TestMcpDesiredServers_NameOnlyEntryInheritsDaemonAuth(t *testing.T) {
	t.Setenv("DAEMON_N8N_TOKEN", "daemon-tok")

	c := containerWithProjectMCP(t,
		"mcp:\n  servers:\n    - name: \"n8n\"\n",
		config.MCPServerConfig{
			Name:      "n8n",
			Transport: "streamable-http",
			URL:       "https://n8n.example.com/mcp/abc",
			Auth: mcpauth.Auth{
				Mode:      mcpauth.ModeStatic,
				ValueFrom: "secret://DAEMON_N8N_TOKEN",
			},
		})

	servers := c.mcpDesiredServers()["test-project"]
	if len(servers) != 1 {
		t.Fatalf("want exactly one server, got %d", len(servers))
	}
	if got := servers[0].AuthHeaders["Authorization"]; got != "daemon-tok" {
		t.Errorf("AuthHeaders[Authorization] = %q, want the inherited daemon credential", got)
	}
}

// TestMcpDesiredServers_AmbiguousAuthOnBothSidesWithholdsTheServer —
// review-20260804-350e finding 1. A name-only entry that ALSO declares auth,
// against a daemon server that declares auth, is ambiguous: picking either side
// would make the load-time grant check on the project's block meaningless while
// leaving it able to fail the whole project. Refuse instead of guessing.
func TestMcpDesiredServers_AmbiguousAuthOnBothSidesWithholdsTheServer(t *testing.T) {
	t.Setenv("PROJECT_TOKEN", "ptok")
	t.Setenv("DAEMON_TOKEN", "dtok")

	c := containerWithProjectMCP(t, `permissions:
  secrets: ["PROJECT_TOKEN"]
mcp:
  servers:
    - name: "n8n"
      auth:
        mode: static
        value_from: "secret://PROJECT_TOKEN"
`,
		config.MCPServerConfig{
			Name:      "n8n",
			Transport: "streamable-http",
			URL:       "https://n8n.example.com/mcp/abc",
			Auth:      mcpauth.Auth{Mode: mcpauth.ModeStatic, ValueFrom: "secret://DAEMON_TOKEN"},
		})

	if servers := c.mcpDesiredServers()["test-project"]; len(servers) != 0 {
		t.Errorf("an ambiguous auth declaration must withhold the server, got %+v", servers)
	}
}

// TestMcpDesiredServers_ProjectAuthWinsWhenTheDaemonDeclaresNone — the
// non-ambiguous half: a name-only entry owning its credential still inherits the
// daemon's CONNECTION fields, and its own allowlist gates the secret.
func TestMcpDesiredServers_ProjectAuthWinsWhenTheDaemonDeclaresNone(t *testing.T) {
	t.Setenv("PROJECT_TOKEN", "ptok")

	c := containerWithProjectMCP(t, `permissions:
  secrets: ["PROJECT_TOKEN"]
mcp:
  servers:
    - name: "n8n"
      auth:
        mode: static
        value_from: "secret://PROJECT_TOKEN"
`,
		config.MCPServerConfig{
			Name:      "n8n",
			Transport: "streamable-http",
			URL:       "https://n8n.example.com/mcp/abc",
		})

	servers := c.mcpDesiredServers()["test-project"]
	if len(servers) != 1 {
		t.Fatalf("want exactly one server, got %d", len(servers))
	}
	if got := servers[0].AuthHeaders["Authorization"]; got != "ptok" {
		t.Errorf("AuthHeaders[Authorization] = %q, want the project's own credential", got)
	}
	if servers[0].Transport != "streamable-http" {
		t.Errorf("Transport = %q, want the inherited daemon transport", servers[0].Transport)
	}
}

// TestDaemonMCPServerConfigs_ResolvesAuth — the daemon-level discovery catalog
// probes servers too, so an authenticated one must carry its header there as
// well or the catalog reports it unreachable.
func TestDaemonMCPServerConfigs_ResolvesAuth(t *testing.T) {
	t.Setenv("DAEMON_TOKEN", "dtok")

	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{MCP: config.MCPConfig{Servers: []config.MCPServerConfig{{
			Name:      "n8n",
			Transport: "streamable-http",
			URL:       "https://n8n.example.com/mcp/abc",
			Auth: mcpauth.Auth{
				Mode:        mcpauth.ModeStatic,
				ValueFrom:   "secret://DAEMON_TOKEN",
				ValuePrefix: "Bearer ",
			},
		}}}},
	}

	servers := c.daemonMCPServerConfigs()
	if len(servers) != 1 {
		t.Fatalf("want exactly one server, got %d", len(servers))
	}
	if got := servers[0].AuthHeaders["Authorization"]; got != "Bearer dtok" {
		t.Errorf("AuthHeaders[Authorization] = %q", got)
	}
}
