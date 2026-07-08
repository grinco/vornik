package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// loadFromBytesForTest unmarshals raw YAML into the daemon's Config struct — the
// point is to prove the edited bytes parse into MCP.Servers (yaml `mcp.servers`)
// exactly as the running daemon would read them.
func loadFromBytesForTest(t *testing.T, b []byte) (*Config, error) {
	t.Helper()
	var c Config
	err := yaml.Unmarshal(b, &c)
	return &c, err
}

// TestAppendYAMLListItem_AppendsToExistingList is the 2026-07-08 MCP-hub fix:
// the daemon catalog is the LIST at mcp.servers, and adds must append a list
// item there (preserving comments + existing items), not write a mcp_servers
// map.
func TestAppendYAMLListItem_AppendsToExistingList(t *testing.T) {
	in := []byte("# top\nmcp:\n  servers:\n    - name: existing # keep\n      transport: sse\n      url: http://a\nother: 1\n")
	out, err := AppendYAMLListItem(in, "mcp.servers", []YAMLListField{
		{Key: "name", Value: "homeassistant"},
		{Key: "transport", Value: "streamable-http"},
		{Key: "url", Value: "http://ha:9583/x"},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	s := string(out)
	// Both servers present; comment + sibling key preserved.
	for _, want := range []string{"name: existing", "# keep", "name: homeassistant", "streamable-http", "http://ha:9583/x", "other: 1", "# top"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	// It parses and the config layer sees BOTH servers under mcp.servers.
	cfg, err := loadFromBytesForTest(t, out)
	if len(cfg.MCP.Servers) != 2 {
		t.Fatalf("want 2 mcp.servers, got %d (%+v) err=%v", len(cfg.MCP.Servers), cfg.MCP.Servers, err)
	}
}

// TestAppendYAMLListItem_CreatesMissingListAndParents builds mcp.servers from a
// config that has neither.
func TestAppendYAMLListItem_CreatesMissingListAndParents(t *testing.T) {
	in := []byte("server:\n  address: :8080\n")
	out, err := AppendYAMLListItem(in, "mcp.servers", []YAMLListField{
		{Key: "name", Value: "solo"},
		{Key: "transport", Value: "sse"},
		{Key: "url", Value: "http://solo"},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !strings.Contains(string(out), "name: solo") {
		t.Fatalf("missing appended item:\n%s", out)
	}
	cfg, err := loadFromBytesForTest(t, out)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(cfg.MCP.Servers) != 1 || cfg.MCP.Servers[0].Name != "solo" {
		t.Fatalf("want 1 server 'solo', got %+v", cfg.MCP.Servers)
	}
}

// TestRemoveYAMLListItemByField removes by name and reports absence.
func TestRemoveYAMLListItemByField(t *testing.T) {
	in := []byte("mcp:\n  servers:\n    - name: keep\n      transport: sse\n    - name: drop\n      transport: sse\n")
	out, removed, err := RemoveYAMLListItemByField(in, "mcp.servers", "name", "drop")
	if err != nil || !removed {
		t.Fatalf("remove: removed=%v err=%v", removed, err)
	}
	if strings.Contains(string(out), "name: drop") {
		t.Errorf("dropped item still present:\n%s", out)
	}
	if !strings.Contains(string(out), "name: keep") {
		t.Errorf("kept item vanished:\n%s", out)
	}
	// Absent name → removed=false, no error, content unchanged.
	_, removed2, err := RemoveYAMLListItemByField(in, "mcp.servers", "name", "ghost")
	if err != nil || removed2 {
		t.Fatalf("ghost remove: removed=%v err=%v", removed2, err)
	}
}
