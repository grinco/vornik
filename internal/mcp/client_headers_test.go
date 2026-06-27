package mcp

import (
	"net/http"
	"testing"

	"github.com/rs/zerolog"
)

// TestApplyConfigHeaders_ReservedNotOverridable is the security
// regression for MCP header collision: a per-server YAML `headers` map
// must NOT be able to override protocol-owned headers (session id,
// content negotiation). Before the guard, the config loop ran after the
// protocol headers and silently won — letting a misconfigured/malicious
// config hijack Mcp-Session-Id.
func TestApplyConfigHeaders_ReservedNotOverridable(t *testing.T) {
	c := &Client{
		logger: zerolog.Nop(),
		config: ServerConfig{
			Name: "evil",
			Headers: map[string]string{
				"Mcp-Session-Id":       "attacker-session",
				"content-type":         "text/evil",    // lower-case must still be caught
				"ACCEPT":               "text/evil",    // upper-case must still be caught
				"MCP-Protocol-Version": "0.0.0",        //
				"Authorization":        "Bearer legit", // legitimate header survives
				"X-Project-ID":         "proj-1",       // legitimate header survives
			},
		},
	}

	req, err := http.NewRequest(http.MethodPost, "http://example/message", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the protocol-owned headers the way setStreamableHeaders does.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2024-11-05")
	req.Header.Set("Mcp-Session-Id", "real-session")

	c.applyConfigHeaders(req)

	// Reserved headers must retain their protocol values.
	if got := req.Header.Get("Mcp-Session-Id"); got != "real-session" {
		t.Errorf("Mcp-Session-Id = %q; config must not override it", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q; config must not override it", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json, text/event-stream" {
		t.Errorf("Accept = %q; config must not override it", got)
	}
	if got := req.Header.Get("MCP-Protocol-Version"); got != "2024-11-05" {
		t.Errorf("MCP-Protocol-Version = %q; config must not override it", got)
	}
	// Legitimate, non-reserved headers must still be applied.
	if got := req.Header.Get("Authorization"); got != "Bearer legit" {
		t.Errorf("Authorization = %q; non-reserved config header should apply", got)
	}
	if got := req.Header.Get("X-Project-ID"); got != "proj-1" {
		t.Errorf("X-Project-ID = %q; non-reserved config header should apply", got)
	}
}

// TestApplyConfigHeaders_NilMapNoOp ensures a nil/empty Headers map is a
// no-op and doesn't disturb the seeded protocol headers.
func TestApplyConfigHeaders_NilMapNoOp(t *testing.T) {
	c := &Client{logger: zerolog.Nop(), config: ServerConfig{Name: "s"}}
	req, _ := http.NewRequest(http.MethodPost, "http://example/message", nil)
	req.Header.Set("Mcp-Session-Id", "real-session")
	c.applyConfigHeaders(req)
	if got := req.Header.Get("Mcp-Session-Id"); got != "real-session" {
		t.Errorf("nil Headers map must be a no-op; got %q", got)
	}
}
