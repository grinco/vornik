package realip

import "net/http"

// Middleware resolves the client IP once per request and stores it in the
// request context (read downstream via ClientIPFromContext). It is meant to
// be the OUTERMOST wrapper on both the API and UI handler chains, so the
// vetted IP is available before auth, rate-limit, CSRF and audit code runs.
//
// onUntrustedHeader, when non-nil, fires when the request carries a
// client-IP forwarding header but the immediate peer is NOT trusted — i.e.
// a spoof attempt or a proxy missing from trusted_proxies. Wire it to
// Metrics.UntrustedHeaderTotal.Inc. The header is ignored regardless; the
// callback is purely observational.
func Middleware(c Config, onUntrustedHeader func()) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := c.ResolveClientIP(r)
			if onUntrustedHeader != nil && c.Enabled && c.HasForwardingHeader(r) && !c.peerTrusted(r) {
				onUntrustedHeader()
			}
			next.ServeHTTP(w, r.WithContext(WithClientIP(r.Context(), ip)))
		})
	}
}

// peerTrusted reports whether r's immediate peer (RemoteAddr host) is in
// the trusted set. Used by the middleware to distinguish "header from a
// trusted proxy" (expected) from "header from an untrusted peer" (spoof
// attempt / misconfig).
func (c Config) peerTrusted(r *http.Request) bool {
	peer := parseHost(remoteHost(r))
	return peer != nil && c.trusts(peer)
}
