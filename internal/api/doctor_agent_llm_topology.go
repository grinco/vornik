package api

import (
	"net"
	"net/url"
	"strings"
)

// isDaemonProxyEndpoint reports whether endpoint routes agent LLM calls back
// through the daemon's own /api/v1 chat-completions proxy (topology 2). The
// minted per-task sk-vornik-* key (executor.injectPerTaskKey) is only valid
// there; any other target 401s. Equivalence class per onboarding-hardening
// design F2b.
func isDaemonProxyEndpoint(endpoint, unixSocket, listenAddr string) bool {
	if endpoint == "" {
		return false
	}
	if strings.HasPrefix(endpoint, "unix://") {
		rest := strings.TrimPrefix(endpoint, "unix://")
		i := strings.Index(rest, ".sock")
		if i < 0 {
			return false
		}
		sock, path := rest[:i+len(".sock")], rest[i+len(".sock"):]
		return sock == unixSocket && strings.HasPrefix(path, "/api/v1")
	}
	u, err := url.Parse(endpoint)
	if err != nil || !strings.HasPrefix(u.Path, "/api/v1") {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || host == "host.containers.internal" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	// Same host as the daemon's own listen address.
	if la, _, err := net.SplitHostPort(listenAddr); err == nil && host == la {
		return true
	}
	return false
}

// checkAgentLLMTopology is the doctor check guarding the #1 fresh-install
// failure: "Invalid API key" on every task. The executor mints a per-task
// sk-vornik-* key (internal/executor/container.go) that ONLY the daemon's
// own /api/v1 proxy accepts. If runtime.agent_llm.endpoint is empty, the
// agent falls back to chat.endpoint (the real upstream, config.go
// ResolvedAgentLLM) and every job 401s against it. ERROR (not WARNING)
// because this is a guaranteed, total task-failure mode, not a degraded one.
func (h *DoctorHandlers) checkAgentLLMTopology() DoctorCheck {
	name := "agent_llm_topology"
	if isDaemonProxyEndpoint(h.agentLLMEndpoint, h.unixSocketPath, h.serverAddress) {
		return DoctorCheck{Name: name, Status: "OK", Message: "agent_llm.endpoint routes through the daemon /api/v1 proxy"}
	}
	target := h.agentLLMEndpoint
	if target == "" {
		target = "empty (agents reuse chat.endpoint, the upstream)"
	}
	return DoctorCheck{
		Name:   name,
		Status: "ERROR",
		Message: "agents mint per-task keys that only the daemon /api/v1 proxy accepts " +
			"(security: a prompt-injected agent can't replay an unscoped key), but " +
			"agent_llm.endpoint is " + target + " — every task will fail with 'Invalid API key'. " +
			"Set AGENT_LLM_ENDPOINT to the daemon proxy — http://host.containers.internal:8080/api/v1 " +
			"(or unix://<sock>/api/v1 for zero-egress/network=none deployments).",
	}
}
