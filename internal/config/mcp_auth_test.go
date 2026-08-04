package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/mcpauth"
)

// These tests reuse minimalValidConfig (model_capabilities_test.go) so an MCP
// auth assertion cannot be satisfied by an unrelated required-field error.

func TestConfigValidate_MCPAuthMalformedBlockFailsAtLoad(t *testing.T) {
	c := minimalValidConfig()
	c.MCP.Servers = []MCPServerConfig{{
		Name:      "n8n",
		Transport: "streamable-http",
		URL:       "https://n8n.example.com/mcp/abc",
		Auth:      mcpauth.Auth{Mode: mcpauth.ModeStatic, ValueFrom: "PLACEHOLDER-not-a-secret-ref"},
	}}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected a secret-reference error")
	}
	if !strings.Contains(err.Error(), "mcp.servers[0].auth") {
		t.Errorf("error must locate the server: %v", err)
	}
	// The rejected literal must not be echoed into the boot log.
	if strings.Contains(err.Error(), "PLACEHOLDER-not-a-secret-ref") {
		t.Errorf("error echoed the credential: %v", err)
	}
}

// TestConfigValidate_MCPAuthDaemonScopeNeedsNoAllowlist — daemon-scope servers
// are admin-configured and have no project permissions.secrets to check
// against, so a valid reference must pass with no grant list in sight.
func TestConfigValidate_MCPAuthDaemonScopeNeedsNoAllowlist(t *testing.T) {
	c := minimalValidConfig()
	c.MCP.Servers = []MCPServerConfig{{
		Name:      "n8n",
		Transport: "streamable-http",
		URL:       "https://n8n.example.com/mcp/abc",
		Auth:      mcpauth.Auth{Mode: mcpauth.ModeStatic, ValueFrom: "secret://n8n_mcp_token"},
	}}
	if err := c.Validate(); err != nil {
		t.Errorf("daemon-scope auth must validate without an allowlist: %v", err)
	}
}

func TestConfigValidate_MCPAuthZeroValueIsUnchangedBehaviour(t *testing.T) {
	c := minimalValidConfig()
	c.MCP.Servers = []MCPServerConfig{{Name: "scraper", Transport: "sse", URL: "http://127.0.0.1:9000/sse"}}
	if err := c.Validate(); err != nil {
		t.Errorf("a server with no auth block must still validate: %v", err)
	}
}

// TestMCPServerConfig_AuthYAMLKeysMatchTheProjectSurface is the contract the
// design states as a goal: identical spelling in project YAML and config.yaml,
// so an operator moving a server between scopes copies the block verbatim.
func TestMCPServerConfig_AuthYAMLKeysMatchTheProjectSurface(t *testing.T) {
	const doc = `
name: reddit
transport: stdio
command: reddit-mcp
auth:
  mode: env
  env_from:
    REDDIT_CLIENT_ID: secret://reddit_client_id
`
	var got MCPServerConfig
	if err := yaml.Unmarshal([]byte(doc), &got); err != nil {
		t.Fatal(err)
	}
	if got.Auth.EffectiveMode() != mcpauth.ModeEnv {
		t.Fatalf("auth.mode = %q", got.Auth.Mode)
	}
	if got.Auth.EnvFrom["REDDIT_CLIENT_ID"] != "secret://reddit_client_id" {
		t.Errorf("env_from = %v", got.Auth.EnvFrom)
	}
}
