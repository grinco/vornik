package api

import "testing"

func TestIsDaemonProxyEndpoint(t *testing.T) {
	const sock = "/data/vornik.sock"
	cases := []struct {
		ep   string
		want bool
	}{
		{"", false}, // empty -> upstream reuse
		{"unix:///data/vornik.sock/api/v1", true},             // network=none default
		{"http://127.0.0.1:8080/api/v1", true},                // loopback proxy
		{"http://localhost:8080/api/v1", true},                // loopback proxy
		{"http://host.containers.internal:8080/api/v1", true}, // networked role
		{"https://api.z.ai/v1", false},                        // real upstream
		{"http://127.0.0.1:8080/v1", false},                   // loopback but not /api/v1 path
	}
	for _, c := range cases {
		if got := isDaemonProxyEndpoint(c.ep, sock, "127.0.0.1:8080"); got != c.want {
			t.Errorf("isDaemonProxyEndpoint(%q)=%v want %v", c.ep, got, c.want)
		}
	}
}

// Regression: fresh-install "Invalid API key" (2026-07-25) — empty endpoint
// routes the minted per-task key to the upstream. See design F2b.
func TestCheckAgentLLMTopology(t *testing.T) {
	h := &DoctorHandlers{unixSocketPath: "/data/vornik.sock", serverAddress: "127.0.0.1:8080"}
	h.agentLLMEndpoint = ""
	if got := h.checkAgentLLMTopology(); got.Status != "ERROR" {
		t.Fatalf("empty endpoint -> ERROR, got %q (%s)", got.Status, got.Message)
	}
	h.agentLLMEndpoint = "unix:///data/vornik.sock/api/v1"
	if got := h.checkAgentLLMTopology(); got.Status != "OK" {
		t.Fatalf("proxy endpoint -> OK, got %q (%s)", got.Status, got.Message)
	}
	h.agentLLMEndpoint = "https://api.z.ai/v1"
	if got := h.checkAgentLLMTopology(); got.Status != "ERROR" {
		t.Fatalf("upstream endpoint -> ERROR, got %q", got.Status)
	}
}
