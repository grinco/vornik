package ratelimit

import (
	"net/http"
	"time"

	"vornik.io/vornik/internal/httpx/realip"
)

// PerIPLimiter is the unauthenticated data-plane backstop —
// sub-item 2 of the rate-limit hardening track. It sits in front
// of AuthMiddleware so a flood from a single client cannot even
// reach the auth path. The bucket primitive is the same one
// per-API-key + per-MCP-tool limits use (keybucket.go); the
// distinct wrapper keeps the call-site self-documenting AND
// lets the IP-extraction logic — which is HTTP-shaped — live
// off the generic primitive.
//
// Client-IP derivation is NO LONGER done here. As of the
// Cloudflare-tunnel real-IP work, the trusted-proxy resolution
// happens ONCE in the outermost realip.Middleware and the vetted
// IP is read from the request context. This limiter therefore
// reads realip.ClientIPFromContext, falling back to RemoteAddr's
// host only when the context is unset (non-middleware / test
// paths). The old leftmost-X-Forwarded-For path — which let an
// attacker forge a victim's IP to trip the per-IP lockout or
// rotate the value to evade the limit — is gone.
// see LLD § https://docs.vornik.io
//
// Concurrency: same lock structure as APIKeyLimiter. Per-IP
// bucket allocation is lazy; idle IPs are GC'd on demand by the
// caller (the AuthMiddleware doesn't currently call Forget, but
// the long-tail cost is bounded by the burst size × number of
// distinct attacker IPs — under SaaS scale that's fine, the
// memory cost is ~64 bytes per IP).
type PerIPLimiter struct {
	inner *APIKeyLimiter
}

// NewPerIPLimiter constructs an empty limiter. Trusted-proxy
// resolution is centralised in internal/httpx/realip and no
// longer configured here.
func NewPerIPLimiter() *PerIPLimiter {
	return &PerIPLimiter{inner: NewAPIKeyLimiter()}
}

// Allow consumes one token from the bucket for the IP derived
// from r. Returns the standard KeyDecision shape so callers can
// reuse the per-key 429 emission path. rps≤0 or burst≤0
// short-circuits to an un-blocked decision — the daemon's
// auth_middleware nil-checks the config block before calling
// here, so this just preserves the contract.
func (l *PerIPLimiter) Allow(r *http.Request, rps, burst int, now time.Time) (KeyDecision, string) {
	if l == nil || r == nil || rps <= 0 || burst <= 0 {
		return KeyDecision{}, ""
	}
	ip := l.ClientIP(r)
	if ip == "" {
		return KeyDecision{}, ""
	}
	return l.inner.Allow(ip, rps, burst, now), ip
}

// SnapshotFor exposes the current bucket state for a single IP
// (read-only; does not consume). Useful for tests and the
// /metrics endpoint.
func (l *PerIPLimiter) SnapshotFor(ip string) (KeyBucketSnapshot, bool) {
	if l == nil {
		return KeyBucketSnapshot{}, false
	}
	return l.inner.SnapshotFor(ip)
}

// ClientIP returns the effective client IP for r. It reads the
// value the outermost realip.Middleware resolved and stored in
// the request context; if the context is unset (a non-middleware
// path or a unit test) it falls back to RemoteAddr's host. It
// NEVER reads X-Forwarded-For — that resolution is centralised in
// internal/httpx/realip and gated on the trusted-proxy list.
func (l *PerIPLimiter) ClientIP(r *http.Request) string {
	if l == nil || r == nil {
		return ""
	}
	if ip := realip.ClientIPFromContext(r.Context()); ip != "" {
		return ip
	}
	return realip.RemoteHost(r)
}
