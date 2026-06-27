package executor

import (
	"encoding/json"
	"os"
	"testing"

	"vornik.io/vornik/internal/registry"
)

func TestShouldWriteMCPConfigDisabledWhenDaemonProxyConfigured(t *testing.T) {
	if shouldWriteMCPConfig(map[string]string{"VORNIK_API_URL": "http://127.0.0.1:8080"}) {
		t.Fatal("expected daemon-proxy mode to skip mcp.json")
	}
	if !shouldWriteMCPConfig(nil) {
		t.Fatal("expected local fallback mode to write mcp.json")
	}
}

func TestBuildMCPConfigDoesNotExpandVornikSecrets(t *testing.T) {
	t.Setenv("VORNIK_API_KEY", "daemon-secret")
	t.Setenv("MCP_PROJECT_TOKEN", "project-token")

	data, err := buildMCPConfig([]registry.MCPServerConfig{
		{
			Name:      "test",
			Transport: "stdio",
			Command:   "server",
			Env: map[string]string{
				"LEAK":  "${VORNIK_API_KEY}",
				"TOKEN": "${MCP_PROJECT_TOKEN}",
			},
		},
	})
	if err != nil {
		t.Fatalf("buildMCPConfig() error = %v", err)
	}

	var decoded []struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to decode mcp config: %v", err)
	}
	if got := decoded[0].Env["LEAK"]; got != "" {
		t.Fatalf("expected VORNIK_ expansion to be blocked, got %q", got)
	}
	if got := decoded[0].Env["TOKEN"]; got != "project-token" {
		t.Fatalf("expected project token expansion, got %q", got)
	}

	if string(data) == "" || os.Getenv("VORNIK_API_KEY") == "" {
		t.Fatal("test setup unexpectedly empty")
	}
}
