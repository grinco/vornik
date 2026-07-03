// Package netguard provides an SSRF-hardened HTTP client for in-process
// fetches of caller-influenced URLs. It resolves the target host before
// dialing and refuses loopback / RFC1918 / link-local / multicast / cloud-
// metadata addresses, and re-validates every redirect hop so a public URL
// cannot bounce the daemon into its own internal network.
//
// The logic mirrors internal/memory.URLLivenessChecker's private-address
// guard, which is bound to that subsystem's Repository and cannot be reused
// across packages. This package is the shared home for that primitive; the
// liveness checker is a candidate to fold onto it in a later pass.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// IsPrivateIP reports whether ip is one an SSRF guard must refuse: the
// unspecified address, loopback, RFC1918 private ranges, link-local (incl.
// the 169.254.0.0/16 cloud-metadata range), or any multicast address.
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsUnspecified() ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast()
}

// IsLocalHostname reports whether host is "localhost" or a "*.localhost" name.
func IsLocalHostname(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

// publicIPsForHost resolves host and returns its addresses, erroring if the
// host is a local name or ANY resolved address is private/link-local/loopback.
// An IP-literal host is checked directly without a DNS lookup.
func publicIPsForHost(ctx context.Context, host string) ([]net.IP, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, fmt.Errorf("URL host is empty")
	}
	if IsLocalHostname(host) {
		return nil, fmt.Errorf("local hostname %q is not allowed", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if IsPrivateIP(ip) {
			return nil, fmt.Errorf("private IP %q is not allowed", host)
		}
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("host %q resolved no addresses", host)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if IsPrivateIP(addr.IP) {
			return nil, fmt.Errorf("host %q resolves to private IP %q", host, addr.IP.String())
		}
		ips = append(ips, addr.IP)
	}
	return ips, nil
}

// ValidateURL rejects a target that would let a caller-influenced URL trigger
// SSRF: only http(s) with a public-routable host, no userinfo.
func ValidateURL(ctx context.Context, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
	}
	if parsed.User != nil {
		return fmt.Errorf("URL userinfo is not allowed")
	}
	_, err = publicIPsForHost(ctx, parsed.Hostname())
	return err
}

// dialContext resolves the host, refuses private addresses, and dials only a
// public IP — so the guard applies to the address actually connected to, not
// just the URL string.
func dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := publicIPsForHost(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{}
	var lastErr error
	for _, ip := range ips {
		conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("host %q resolved no addresses", host)
}

// NewGuardedClient returns an *http.Client with the given overall timeout whose
// DialContext refuses private/link-local/loopback targets and whose
// CheckRedirect re-validates each hop, so a public URL cannot redirect into the
// internal network (cloud metadata, admin ports, RFC1918).
func NewGuardedClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: dialContext,
		},
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return ValidateURL(req.Context(), req.URL.String())
		},
	}
}
