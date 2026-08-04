package mcp

import (
	"net/http"
	"sync/atomic"

	"vornik.io/vornik/internal/version"
)

// buildVersion is the daemon build version used in the outbound User-Agent.
// Package-level and set once at boot (SetVersion) rather than carried on
// ServerConfig on purpose: the value is a property of the process, not of a
// server, and every construction site that had to remember to copy it would
// eventually be the one that forgot. Empty until stamped, which yields the
// version.Default fallback.
var buildVersion atomic.Value // string

// SetVersion records the daemon's build version for the MCP client's
// User-Agent. Called once during startup; safe to call concurrently with
// requests, and safe never to call at all.
//
// Deliberately NOT folded into service.Container.SetVersion, which stamps the
// same string for the cluster heartbeat and problem reports: that runs after
// the container is constructed, whereas this is stamped as the first statement
// of service.Run so no MCP traffic — including a registry refresh kicked off
// during startup — can go out carrying the version.Default fallback. Two call
// sites, two different moments, on purpose.
func SetVersion(v string) { buildVersion.Store(v) }

// userAgent renders the header value for outbound MCP HTTP requests.
func userAgent() string {
	v, _ := buildVersion.Load().(string)
	return version.UserAgent(v)
}

// setBaseHeaders stamps the headers every MCP HTTP request carries regardless
// of transport or configuration. Today that is the User-Agent.
//
// Why Vornik identifies itself (design §5, F3): Go's default
// "Go-http-client/1.1" is anonymous and shares a reputation pool with every
// other Go program on the internet, six of eighteen surveyed MCP vendors
// already WAF-block a specific library UA, and a blocked UA presents as a 403
// with no challenge — which reads as an auth failure and is not one. We
// identify honestly rather than spoofing a browser, which would compete with
// the exact fingerprinting those WAFs perform.
//
// Called BEFORE applyConfigHeaders so an operator who deliberately pins a
// User-Agent in the per-server Headers map still wins.
func (c *Client) setBaseHeaders(httpReq *http.Request) {
	httpReq.Header.Set("User-Agent", userAgent())
}
