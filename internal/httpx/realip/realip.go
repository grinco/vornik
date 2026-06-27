// Package realip centralises trusted-proxy client-IP resolution for the
// vornik HTTP surface. It exists because the daemon's UI/API is moving
// behind a Cloudflare Zero Trust tunnel (cloudflared on a SEPARATE host,
// dialling the daemon over a LAN address). Before this package there were
// six independent client-IP derivations scattered across internal/api,
// internal/ui, internal/ratelimit, internal/auth/loginflow and the access
// log — most read X-Forwarded-For's LEFTMOST entry. Cloudflare APPENDS the
// real client to any client-supplied XFF, so the leftmost hop is
// attacker-controlled: a caller could forge another customer's IP to trip
// the per-IP brute-force lockout, or rotate the spoofed value to evade the
// rate-limit. This package resolves the real client ONCE, from a trusted
// header (default CF-Connecting-IP, which Cloudflare sets and clients
// cannot append to), and stashes it in the request context so every
// downstream consumer reads the same vetted value.
//
// Design constraints: this is a LEAF package — it imports only net/http +
// stdlib so it can be wired from internal/service without an import cycle
// through internal/api, internal/ui or internal/config.
//
// see LLD § https://docs.vornik.io
package realip

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// DefaultHeader is the canonical trusted client-IP header. Cloudflare sets
// CF-Connecting-IP to a SINGLE value (the real client) and clients cannot
// append to it — unlike X-Forwarded-For, whose leftmost entry is
// client-controlled. No Enterprise plan is required for this header.
const DefaultHeader = "CF-Connecting-IP"

// Config is the resolved, validated real-IP policy. Build it via NewConfig.
type Config struct {
	// Enabled gates header honouring. When false, ResolveClientIP always
	// returns RemoteAddr's host — the fail-safe default so that turning on
	// the tunnel REQUIRES explicitly configuring real_ip.
	Enabled bool
	// TrustedProxies is the set of source networks (the cloudflared host)
	// whose trusted header we honour. Empty ⇒ nothing is trusted.
	TrustedProxies []*net.IPNet
	// Header is the trusted client-IP header name, defaulting to
	// DefaultHeader when constructed empty.
	Header string
}

// NewConfig parses trustedProxies (CIDRs or bare IPs; a bare IP becomes a
// /32 or /128 host route) and defaults header to DefaultHeader when empty.
// A malformed entry is rejected with an error rather than silently dropped
// — a typo in the trust list is a security-relevant misconfiguration the
// operator must see at load time, not discover via a spoofed lockout.
func NewConfig(enabled bool, trustedProxies []string, header string) (Config, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		header = DefaultHeader
	}
	cidrs := make([]*net.IPNet, 0, len(trustedProxies))
	for _, raw := range trustedProxies {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "/") {
			ip := net.ParseIP(raw)
			if ip == nil {
				return Config{}, fmt.Errorf("realip: invalid trusted proxy %q: not an IP or CIDR", raw)
			}
			if ip.To4() != nil {
				raw += "/32"
			} else {
				raw += "/128"
			}
		}
		_, cidr, err := net.ParseCIDR(raw)
		if err != nil {
			return Config{}, fmt.Errorf("realip: invalid trusted proxy %q: %w", raw, err)
		}
		cidrs = append(cidrs, cidr)
	}
	return Config{Enabled: enabled, TrustedProxies: cidrs, Header: header}, nil
}

// ResolveClientIP returns the effective client IP for r:
//
//  1. host = the IP parsed from r.RemoteAddr (port-tolerant).
//  2. if disabled OR host is not in TrustedProxies → return host. NEVER
//     read any forwarding header from an untrusted peer.
//  3. host trusted → read the configured header; if it parses as an IP
//     return it, else (missing/garbage) fall back to host.
//
// It NEVER reads X-Forwarded-For's leftmost entry — that was the spoofable
// path this package replaces.
func (c Config) ResolveClientIP(r *http.Request) string {
	host := remoteHost(r)
	if !c.Enabled {
		return host
	}
	peer := net.ParseIP(host)
	if peer == nil || !c.trusts(peer) {
		return host
	}
	if hv := strings.TrimSpace(r.Header.Get(c.Header)); hv != "" {
		if ip := net.ParseIP(hv); ip != nil {
			return ip.String()
		}
	}
	return host
}

// HasForwardingHeader reports whether r carries any client-IP forwarding
// header — the configured header, X-Forwarded-For, or CF-Connecting-IP.
// The middleware uses it to detect a spoof ATTEMPT from an untrusted peer.
func (c Config) HasForwardingHeader(r *http.Request) bool {
	if r.Header.Get(c.Header) != "" {
		return true
	}
	if r.Header.Get("X-Forwarded-For") != "" {
		return true
	}
	return r.Header.Get(DefaultHeader) != ""
}

// parseHost parses a bare host string to a net.IP, returning nil when it
// is not a valid IP.
func parseHost(host string) net.IP {
	if host == "" {
		return nil
	}
	return net.ParseIP(host)
}

func (c Config) trusts(ip net.IP) bool {
	for _, cidr := range c.TrustedProxies {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// remoteHost strips the port from r.RemoteAddr, tolerating an addr that
// already has no port. Returns "" only for an empty RemoteAddr.
func remoteHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	addr := r.RemoteAddr
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// ----- context plumbing -----

type ctxKey struct{}

// WithClientIP returns a copy of ctx carrying the resolved client IP.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, ctxKey{}, ip)
}

// ClientIPFromContext returns the resolved client IP stored by the
// middleware, or "" when unset (non-middleware/test paths). Callers fall
// back to their own RemoteAddr strip in that case.
func ClientIPFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// RemoteHost is the exported RemoteAddr-host fallback the six consumers use
// when the context value is empty. Keeps the strip logic in one place.
func RemoteHost(r *http.Request) string {
	return remoteHost(r)
}
