package registry

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/mcpauth"
)

// projectWithMCPAuth builds a minimally-valid project carrying one MCP server.
func projectWithMCPAuth(transport string, auth mcpauth.Auth, grantedSecrets []string) *Project {
	return &Project{
		ID:                "p1",
		SwarmID:           "s1",
		DefaultWorkflowID: "w1",
		Permissions:       ProjectPermissions{Secrets: grantedSecrets},
		MCP: ProjectMCP{Servers: []MCPServerConfig{{
			Name:      "n8n",
			Transport: transport,
			URL:       "https://n8n.example.com/mcp/abc",
			Auth:      auth,
		}}},
	}
}

// TestProjectValidate_MCPAuthMalformedBlockFailsAtLoad — the design requires
// failing loud at config load, not at the first tool call.
func TestProjectValidate_MCPAuthMalformedBlockFailsAtLoad(t *testing.T) {
	p := projectWithMCPAuth("streamable-http", mcpauth.Auth{Mode: "bearer"}, nil)
	err := p.Validate("p1.yaml")
	if err == nil {
		t.Fatal("expected an unknown-mode error")
	}
	if !strings.Contains(err.Error(), "mcp.servers[0].auth") {
		t.Errorf("error must locate the offending server: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown mode") {
		t.Errorf("error must name the rule: %v", err)
	}
}

// TestProjectValidate_MCPAuthSecretMustBeGranted is the first enforcement of
// permissions.secrets anywhere in the tree: an auth block cannot reach a secret
// the project was never granted.
func TestProjectValidate_MCPAuthSecretMustBeGranted(t *testing.T) {
	auth := mcpauth.Auth{Mode: mcpauth.ModeStatic, ValueFrom: "secret://n8n_token"}

	ungranted := projectWithMCPAuth("streamable-http", auth, []string{"other_secret"})
	err := ungranted.Validate("p1.yaml")
	if err == nil {
		t.Fatal("expected a permissions.secrets error")
	}
	if !strings.Contains(err.Error(), "n8n_token") || !strings.Contains(err.Error(), "permissions.secrets") {
		t.Errorf("error must name the secret and the allowlist: %v", err)
	}

	granted := projectWithMCPAuth("streamable-http", auth, []string{"n8n_token"})
	if err := granted.Validate("p1.yaml"); err != nil {
		t.Errorf("granted secret must validate: %v", err)
	}
}

// TestProjectValidate_MCPAuthZeroValueIsUnchangedBehaviour guards every
// existing deployment: no auth block means no new validation.
func TestProjectValidate_MCPAuthZeroValueIsUnchangedBehaviour(t *testing.T) {
	p := projectWithMCPAuth("stdio", mcpauth.Auth{}, nil)
	p.MCP.Servers[0].Command = "some-mcp"
	if err := p.Validate("p1.yaml"); err != nil {
		t.Errorf("a server with no auth block must still validate: %v", err)
	}
}

// TestProjectValidate_MCPAuthTransportMismatch pins the pairing rule at the
// project layer (mode env needs a subprocess).
func TestProjectValidate_MCPAuthTransportMismatch(t *testing.T) {
	p := projectWithMCPAuth("streamable-http",
		mcpauth.Auth{Mode: mcpauth.ModeEnv, EnvFrom: map[string]string{"TOKEN": "secret://t"}},
		[]string{"t"})
	err := p.Validate("p1.yaml")
	if err == nil || !strings.Contains(err.Error(), "stdio") {
		t.Fatalf("expected a transport-pairing error, got %v", err)
	}
}

// TestMCPServerConfig_AuthRoundTripsFromYAML pins the operator-facing spelling.
// The YAML keys are a contract with the docs and with the daemon config, which
// must accept the identical shape.
func TestMCPServerConfig_AuthRoundTripsFromYAML(t *testing.T) {
	const doc = `
name: n8n
transport: streamable-http
url: https://n8n.example.com/mcp/abc
auth:
  mode: static
  header: Authorization
  value_from: secret://n8n_mcp_token
  value_prefix: "Bearer "
`
	var got MCPServerConfig
	if err := yaml.Unmarshal([]byte(doc), &got); err != nil {
		t.Fatal(err)
	}
	if got.Auth.Mode != mcpauth.ModeStatic {
		t.Errorf("auth.mode = %q", got.Auth.Mode)
	}
	if got.Auth.ValueFrom != "secret://n8n_mcp_token" {
		t.Errorf("auth.value_from = %q", got.Auth.ValueFrom)
	}
	if got.Auth.ValuePrefix != "Bearer " {
		t.Errorf("auth.value_prefix = %q (the trailing space is load-bearing)", got.Auth.ValuePrefix)
	}
	if got.Auth.Header != "Authorization" {
		t.Errorf("auth.header = %q", got.Auth.Header)
	}
}

// TestMCPServerConfig_EnvAuthRoundTripsFromYAML covers the Plane 2 shape.
func TestMCPServerConfig_EnvAuthRoundTripsFromYAML(t *testing.T) {
	const doc = `
name: reddit
transport: stdio
command: reddit-mcp
auth:
  mode: env
  env_from:
    REDDIT_CLIENT_ID: secret://reddit_client_id
    REDDIT_CLIENT_SECRET: secret://reddit_client_secret
`
	var got MCPServerConfig
	if err := yaml.Unmarshal([]byte(doc), &got); err != nil {
		t.Fatal(err)
	}
	if got.Auth.EffectiveMode() != mcpauth.ModeEnv {
		t.Fatalf("auth.mode = %q", got.Auth.Mode)
	}
	if got.Auth.EnvFrom["REDDIT_CLIENT_SECRET"] != "secret://reddit_client_secret" {
		t.Errorf("env_from = %v", got.Auth.EnvFrom)
	}
}

// TestMCPServerConfig_AuthDoesNotSerializeIntoConfigWhenUnset keeps the
// zero value invisible: a project file round-tripped by the control plane must
// not sprout an empty `auth: {}` block.
func TestMCPServerConfig_AuthDoesNotSerializeIntoConfigWhenUnset(t *testing.T) {
	out, err := yaml.Marshal(MCPServerConfig{Name: "s", Transport: "stdio", Command: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "auth:") {
		t.Errorf("unset auth must be omitted; got:\n%s", out)
	}
}
