package podman

import (
	"os"
	"regexp"
	"testing"
)

// Regression: fresh-install "Invalid API key" (2026-07-25). injectPerTaskKey
// (executor/container.go:1943) always overwrites VORNIK_LLM_API_KEY with a
// minted per-task key that ONLY the daemon /api/v1 proxy accepts; an empty
// AGENT_LLM_ENDPOINT reuses chat.endpoint (config.go:2372) and sends the
// minted key to the upstream, which 401s. See onboarding-hardening-design F2a.
func TestEnvExampleDefaultsAgentLLMEndpointToDaemonProxy(t *testing.T) {
	data, err := os.ReadFile("vornik.env.example")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	re := regexp.MustCompile(`(?m)^AGENT_LLM_ENDPOINT=(.+)$`)
	m := re.FindSubmatch(data)
	if m == nil || len(m[1]) == 0 {
		t.Fatalf("AGENT_LLM_ENDPOINT must be set to a daemon-proxy value, not empty")
	}
	val := string(m[1])
	// Must be the daemon proxy, TCP form: dev-swarm roles ship network:egress
	// and no server.unix_socket is set for this topology, so
	// http://host.containers.internal:8080/api/v1 (the daemon's own 0.0.0.0
	// bind, rewritten for the agent side — service/container.go:695) is the
	// reachable default, always ending in /api/v1.
	if !regexp.MustCompile(`^https?://[^ ]+/api/v1$`).MatchString(val) {
		t.Fatalf("AGENT_LLM_ENDPOINT should be the daemon proxy (http(s)://.../api/v1), got %q", val)
	}
}
