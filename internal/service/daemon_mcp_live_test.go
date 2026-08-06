package service

import (
	"testing"

	"vornik.io/vornik/internal/config"
)

// TestDaemonMCPServers_ReflectsAReload is the regression test for the
// 2026-08-05 operator report: "the mcp config was applied, but not reloaded —
// it requires restart".
//
// c.Config is deliberately never swapped on reload (applyHotConfig says why:
// too many goroutines hold that pointer). Every reader of the daemon-level MCP
// catalog read c.Config.MCP.Servers directly, so all of them — the tool
// manager, the discovery registry that backs the MCP tab, and the connect path
// `vornikctl mcp connect` resolves through — were pinned to the boot value.
// A server added through the hub applied, reloaded successfully, and then did
// not exist as far as any of them were concerned, while the proposal that made
// the change said "applies live".
func TestDaemonMCPServers_ReflectsAReload(t *testing.T) {
	c := &Container{Config: &config.Config{}}
	c.Config.MCP.Servers = []config.MCPServerConfig{
		{Name: "existing", Transport: "sse", URL: "http://existing"},
	}

	// Before any reload: the boot config is the answer.
	if got := c.daemonMCPServers(); len(got) != 1 || got[0].Name != "existing" {
		t.Fatalf("boot catalog = %+v, want just existing", got)
	}

	// A reload publishes the freshly-parsed catalog.
	c.publishDaemonMCPServers([]config.MCPServerConfig{
		{Name: "existing", Transport: "sse", URL: "http://existing"},
		{Name: "atlassian", Transport: "streamable-http", URL: "https://mcp.atlassian.com/v1/mcp/authv2"},
	})

	got := c.daemonMCPServers()
	if len(got) != 2 {
		t.Fatalf("after reload = %d servers, want 2 — a reload-added server must be visible without a restart", len(got))
	}
	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	if !names["atlassian"] {
		t.Error("the added server is missing from the live catalog")
	}
	// c.Config must be left alone — that is the invariant the live holder exists
	// to respect, not one it may quietly break.
	if len(c.Config.MCP.Servers) != 1 {
		t.Errorf("c.Config was mutated (%d servers); the live holder must not swap it",
			len(c.Config.MCP.Servers))
	}
}

// TestDaemonMCPServers_RemovalIsLiveToo — a removed server must disappear from
// the live catalog, otherwise "remove server" through the hub is the same bug
// in the other direction.
func TestDaemonMCPServers_RemovalIsLiveToo(t *testing.T) {
	c := &Container{Config: &config.Config{}}
	c.Config.MCP.Servers = []config.MCPServerConfig{
		{Name: "a", Transport: "sse", URL: "http://a"},
		{Name: "b", Transport: "sse", URL: "http://b"},
	}
	c.publishDaemonMCPServers([]config.MCPServerConfig{{Name: "a", Transport: "sse", URL: "http://a"}})

	got := c.daemonMCPServers()
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("after removal = %+v, want just a", got)
	}
}

// TestDaemonMCPServers_NilConfigIsSafe — the accessor is called from wiring
// that can run before a config exists.
func TestDaemonMCPServers_NilConfigIsSafe(t *testing.T) {
	c := &Container{}
	if got := c.daemonMCPServers(); got != nil {
		t.Errorf("nil config must yield nil, got %+v", got)
	}
}

// TestPublishDaemonMCPServers_Snapshots — the publisher must copy, so a caller
// that reuses its slice cannot mutate the live catalog from under readers.
func TestPublishDaemonMCPServers_Snapshots(t *testing.T) {
	c := &Container{Config: &config.Config{}}
	src := []config.MCPServerConfig{{Name: "a", Transport: "sse", URL: "http://a"}}
	c.publishDaemonMCPServers(src)
	src[0].Name = "mutated"

	if got := c.daemonMCPServers(); got[0].Name != "a" {
		t.Errorf("live catalog aliased the caller's slice: got %q", got[0].Name)
	}
}

// TestPublicOrigin_ReflectsAReload pins the second half of the same
// c.Config-is-pinned-to-boot bug.
//
// The MCP connector's BaseURL carried a comment stating it read LIVE so that
// "setting public_base_url then reloading config must be enough — an operator
// should not have to restart the daemon to make Connect work." It read
// c.Config.Server.PublicBaseURL, which is never swapped on reload, so the
// documented intent was defeated: Connect kept building its redirect URI from
// whatever the origin was at boot. A redirect URI that does not match what the
// vendor has registered fails AFTER the operator consents, which is the worst
// place to find out.
func TestPublicOrigin_ReflectsAReload(t *testing.T) {
	c := &Container{Config: &config.Config{}}
	c.Config.Server.PublicBaseURL = "https://boot.example.com"

	if got := c.publicOrigin(); got != "https://boot.example.com" {
		t.Fatalf("boot origin = %q", got)
	}

	c.publishPublicOrigin("https://reloaded.example.com")
	if got := c.publicOrigin(); got != "https://reloaded.example.com" {
		t.Errorf("after reload = %q, want the published origin", got)
	}
	// c.Config must not be mutated — that invariant is why the live holder exists.
	if c.Config.Server.PublicBaseURL != "https://boot.example.com" {
		t.Errorf("c.Config was mutated: %q", c.Config.Server.PublicBaseURL)
	}
}

// TestPublicOrigin_HonoursTheAuthFallback — the connector previously read
// Server.PublicBaseURL directly, skipping the auth.external_base_url fallback
// that Config.PublicOrigin() honours. A deployment that set only the auth key
// therefore had a working login and a Connect button that could not build a
// redirect URI.
func TestPublicOrigin_HonoursTheAuthFallback(t *testing.T) {
	c := &Container{Config: &config.Config{}}
	c.Config.Auth.ExternalBaseURL = "https://only-auth-key.example.com"
	if got := c.publicOrigin(); got != "https://only-auth-key.example.com" {
		t.Errorf("origin = %q, want the auth fallback", got)
	}
}

// TestPublicOrigin_NilConfigIsSafe — called from wiring that can run before a
// config exists.
func TestPublicOrigin_NilConfigIsSafe(t *testing.T) {
	c := &Container{}
	if got := c.publicOrigin(); got != "" {
		t.Errorf("nil config must yield empty, got %q", got)
	}
}
