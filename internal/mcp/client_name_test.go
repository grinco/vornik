package mcp

import "testing"

// TestClient_Name covers the trivial Name() getter without needing
// a live MCP subprocess. The struct literal sidesteps Connect's
// transport bootstrap entirely.
func TestClient_Name(t *testing.T) {
	c := &Client{config: ServerConfig{Name: "my-server"}}
	if got := c.Name(); got != "my-server" {
		t.Errorf("Name() = %q, want %q", got, "my-server")
	}
}

func TestClient_Name_EmptyConfig(t *testing.T) {
	c := &Client{}
	if got := c.Name(); got != "" {
		t.Errorf("Name() on zero Client = %q, want \"\"", got)
	}
}
