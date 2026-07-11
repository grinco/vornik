package integrations

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// DialGuard is the shared SSRF guard every Prober dials through (design
// §6, security-critical). Probes accept user-supplied endpoints — an MCP
// server URL, a self-hosted IMAP/SMTP host, a GitHub Enterprise base URL —
// and since project-scoped (non-admin) users can trigger a probe, an
// unguarded dial is a dial-out-anywhere primitive. DialGuard blocks RFC1918,
// loopback, and link-local destinations at connect time.
//
// The check runs on the *resolved* IP via net.Dialer.Control, which fires
// after DNS resolution but before the connect(2) syscall — not a hostname
// string check — so a hostname that resolves to a private address (DNS
// rebinding, or simply "localhost") is caught regardless of what the
// hostname string looks like.
//
// AllowedHosts is an opt-in escape hatch (empty = block all private/
// loopback/link-local destinations) for self-hosted operators who need to
// probe a specific internal host (e.g. "imap.internal.example.com",
// "mcp.lan"). It matches the ORIGINAL hostname from the dial address, not
// the resolved IP — an allowlisted host bypasses the IP check entirely,
// by design (that's the point: the operator is vouching for that specific
// host being safe to reach even though it's internal).
type DialGuard struct {
	AllowedHosts []string
}

// HTTPClient returns an *http.Client whose transport dials every connection
// through DialContext, bounded by timeout. Every prober constructor takes a
// client built this way — there is no un-guarded dial path.
func (g DialGuard) HTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = g.DialContext
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

// DialContext is the guarded dial function used as http.Transport.
// DialContext. It resolves addr, checks the resolved IP (or the allowlist)
// via net.Dialer.Control, and either completes the connection or refuses
// it before any bytes are exchanged. Has the exact shape SMTPDialFunc
// (internal/email) expects, so a guard's method value can be passed
// straight through for the SMTP probe leg too.
func (g DialGuard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// addr without a port (shouldn't normally happen via http.Transport,
		// but DialContext is also called directly by the SMTP prober) —
		// fall back to treating the whole string as the host.
		host = addr
	}
	return g.Dialer(host).DialContext(ctx, network, addr)
}

// Dialer returns a *net.Dialer pre-configured to guard connections to
// host: allowlisted hosts get a plain dialer (opt-in bypass, by design);
// everything else gets a Control callback that inspects the *resolved* IP
// on every candidate address and refuses private/loopback/link-local ones.
//
// This is the seam email.IMAPDialConfig.Dialer expects — go-imap's
// imapclient.DialTLS takes a *net.Dialer via its Options (there is no
// DialFunc/net.Conn injection point on DialTLS itself), so the guard has
// to be expressible as a concrete *net.Dialer, not just a DialContext
// closure. DialContext (above) is built on top of this same method so the
// HTTP and IMAP paths share one guarding implementation.
func (g DialGuard) Dialer(host string) *net.Dialer {
	if g.hostAllowed(host) {
		// Allowlisted: skip the resolved-IP check entirely, per the type's
		// documented contract.
		return &net.Dialer{}
	}
	return &net.Dialer{
		Control: func(_, address string, _ syscall.RawConn) error {
			ipHost, _, splitErr := net.SplitHostPort(address)
			if splitErr != nil {
				ipHost = address
			}
			ip := net.ParseIP(ipHost)
			if ip == nil {
				return fmt.Errorf("dial guard: could not parse resolved address %q", address)
			}
			if isBlockedIP(ip) {
				return fmt.Errorf("dial guard: refusing to dial private/loopback/link-local address %s (host %s)", ip, host)
			}
			return nil
		},
	}
}

func (g DialGuard) hostAllowed(host string) bool {
	for _, h := range g.AllowedHosts {
		if h == host {
			return true
		}
	}
	return false
}

// isBlockedIP reports whether ip falls in a range DialGuard refuses by
// default: RFC1918 private, loopback, link-local (unicast or multicast),
// or unspecified (0.0.0.0 / ::).
func isBlockedIP(ip net.IP) bool {
	return ip.IsPrivate() ||
		ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}
