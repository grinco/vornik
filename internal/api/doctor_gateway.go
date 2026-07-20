package api

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"time"
)

// isLoopbackHost reports whether host (a bare URL hostname, no port) is a
// loopback destination: the literal "localhost" or any IP in the loopback
// range (127.0.0.0/8, ::1). Kept local to internal/api to avoid importing
// internal/integrations or apigateway/gateway (which would form an import
// cycle); it is defense-in-depth over the config.Validate loopback rule.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// checkGatewayHealthy pings the local API gateway. SKIPPED when unconfigured so
// a daemon without the gateway isn't marked unhealthy (design §9 doctor
// "gateway reachable"). Any HTTP response counts as reachable; a request-build
// error or transport error is ERROR.
func (h *DoctorHandlers) checkGatewayHealthy(ctx context.Context, _ bool) DoctorCheck {
	const name = "gateway_healthy"
	if h == nil || h.gatewayURL == "" {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "API gateway not configured; skipping"}
	}
	// review B1: never GET a non-loopback host, even with the short-timeout
	// client below. The gateway is a loopback-only local sidecar; a
	// non-loopback gatewayURL is misconfiguration, and probing it would make
	// the doctor itself perform an SSRF. Reject on the parsed host BEFORE
	// dialing (config.Validate enforces the same rule at startup; this is
	// defense-in-depth).
	if u, err := url.Parse(h.gatewayURL); err != nil || !isLoopbackHost(u.Hostname()) {
		return DoctorCheck{Name: name, Status: "ERROR", Message: "gateway address must be loopback"}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// Hit the gateway base URL and treat any HTTP response as "reachable".
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.gatewayURL, nil)
	if err != nil {
		return DoctorCheck{Name: name, Status: "ERROR", Message: "gateway URL invalid: " + err.Error()}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return DoctorCheck{Name: name, Status: "ERROR", Message: "gateway unreachable: " + err.Error()}
	}
	_ = resp.Body.Close()
	return DoctorCheck{Name: name, Status: "OK", Message: "gateway reachable"}
}

// SetGatewayURL wires the gateway endpoint; empty leaves the check SKIPPED.
func (h *DoctorHandlers) SetGatewayURL(u string) {
	if h != nil {
		h.gatewayURL = u
	}
}
