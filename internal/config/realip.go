package config

// ResolvedRealIP is the effective trusted-proxy client-IP policy after
// applying the backward-compat fallback from the deprecated
// api.rate_limit.per_ip.trusted_proxies key. The service container turns
// this into an internal/httpx/realip.Config and, when DeprecatedFallback
// is true, emits a one-time startup WARNING pointing at the new key.
type ResolvedRealIP struct {
	Enabled        bool
	TrustedProxies []string
	Header         string
	// DeprecatedFallback reports that TrustedProxies was sourced from the
	// deprecated api.rate_limit.per_ip.trusted_proxies key because
	// server.real_ip.trusted_proxies was empty.
	DeprecatedFallback bool
}

// ResolveRealIP computes the effective real-IP policy for the daemon.
//
// Precedence for the trust list:
//  1. server.real_ip.trusted_proxies when non-empty (the canonical key).
//  2. otherwise the deprecated api.rate_limit.per_ip.trusted_proxies,
//     with DeprecatedFallback set so the container can warn.
//
// Enabled and Header always come from server.real_ip. When the new block
// is unconfigured but the deprecated trust list exists, the fallback is
// treated as enabled so behaviour is preserved across the rename.
func (c *Config) ResolveRealIP() ResolvedRealIP {
	out := ResolvedRealIP{
		Enabled:        c.Server.RealIP.Enabled,
		TrustedProxies: c.Server.RealIP.TrustedProxies,
		Header:         c.Server.RealIP.Header,
	}
	if len(out.TrustedProxies) == 0 {
		if dep := c.API.RateLimit.PerIP.TrustedProxies; len(dep) > 0 {
			out.TrustedProxies = dep
			out.DeprecatedFallback = true
			// The deprecated key never had an enabled toggle — its mere
			// presence meant "honour the header". Preserve that so the
			// rename doesn't silently disable trusted-proxy handling.
			out.Enabled = true
		}
	}
	return out
}

// RealIPConfigured reports whether trusted-proxy resolution will honour a
// forwarding header at all (enabled AND a non-empty effective trust list).
// Used to drive the "auth enabled but real_ip unconfigured" startup
// warning.
func (c *Config) RealIPConfigured() bool {
	r := c.ResolveRealIP()
	return r.Enabled && len(r.TrustedProxies) > 0
}
