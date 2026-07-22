// Package urlguard implements the SSRF pre-gate and the STRICT write host
// matcher shared by the web-write dispatcher tool and the scraper.
//
// Two entry points:
//
//   - ValidateTargetURL: a hard gate on a target URL string. https-only, no
//     userinfo, no IP-literal host, no host that resolves into a blocked range
//     (loopback/private/link-local/CGNAT/unspecified/ULA/IPv4-mapped-IPv6), and
//     no well-known cloud metadata hostname. DNS-pin / per-redirect
//     re-validation happens at connection time in the scraper (Task 9); this is
//     the pre-gate plus the shared blocked-range logic.
//
//   - HostAllowed: the STRICT write matcher. Deliberately stricter than the
//     read-side host matching (a write is higher-stakes): exact host OR a
//     label-anchored "*.domain" wildcard ONLY. There is NO apex/www
//     equivalence (listing www.airline.com does NOT permit airline.com, and
//     vice-versa), and a bare "*" never matches. This intentionally does not
//     reuse any www-stripping read-side helper — that apex/www widening
//     over-scopes writes (see LLD ."Write authorization" I3).
package urlguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/idna"
)

// writeIDNA is the UTS-46 NON-transitional (IDNA2008) lookup profile, pinned
// once. Equivalent to the predefined idna.Lookup profile. Non-transitional so
// that e.g. German "ß" and final sigma map per the modern rules rather than the
// legacy transitional folding.
var writeIDNA = idna.New(idna.MapForLookup(), idna.BidiRule())

// blockedNets is the set of CIDR ranges a target host must not resolve into.
// Built once at package init.
var blockedNets = buildBlockedNets()

// metadataHosts are well-known cloud metadata endpoint hostnames that must be
// rejected by name (their IPs, e.g. 169.254.169.254 / 100.100.100.200, are also
// covered by the CIDR set, but the names must never be dialed either).
var metadataHosts = map[string]bool{
	"metadata.google.internal": true,
	"metadata.goog":            true,
	"metadata":                 true,
}

func buildBlockedNets() []*net.IPNet {
	cidrs := []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // RFC1918 private
		"172.16.0.0/12",  // RFC1918 private
		"192.168.0.0/16", // RFC1918 private
		"169.254.0.0/16", // link-local (incl. AWS/GCP metadata 169.254.169.254)
		"100.64.0.0/10",  // CGNAT (RFC6598; incl. Alibaba metadata 100.100.100.200)
		"0.0.0.0/8",      // "this" network / unspecified (IPv4)
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 unique-local (ULA)
		"fe80::/10",      // IPv6 link-local
		"::/128",         // IPv6 unspecified
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			// Static, compile-time-known list; a parse failure is a programming
			// error, so fail loudly rather than silently under-blocking.
			panic(fmt.Sprintf("urlguard: bad CIDR %q: %v", c, err))
		}
		nets = append(nets, n)
	}
	return nets
}

// isBlockedIP reports whether ip falls in any blocked range. IPv4-mapped IPv6
// addresses (::ffff:a.b.c.d) are normalized to their IPv4 form first so that
// e.g. ::ffff:127.0.0.1 is caught by 127.0.0.0/8.
func isBlockedIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ValidateTargetURL enforces https-only, no userinfo, no IP-literal host, no
// host resolving into a blocked range, and no metadata hostname. It returns the
// parsed URL on success.
//
// DNS handling: if the host does not resolve, the resolved-IP check is skipped
// (fail-open at the pre-gate) because the scraper performs authoritative
// DNS-pin / per-hop re-validation at connection time (Task 9). Positive
// resolution into a blocked range is always rejected here.
func ValidateTargetURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("urlguard: parse: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, fmt.Errorf("urlguard: scheme %q not allowed (https only)", u.Scheme)
	}
	// Reject userinfo (user:pass@host) outright — it is both a phishing vector
	// and a common host-confusion trick.
	if u.User != nil {
		return nil, errors.New("urlguard: userinfo not allowed in URL")
	}
	host := u.Hostname() // strips any :port and IPv6 brackets
	if host == "" {
		return nil, errors.New("urlguard: empty host")
	}
	// Guard against host-confusion: after url.Parse there must be no stray '@'
	// left in the authority component.
	if strings.Contains(u.Host, "@") {
		return nil, errors.New("urlguard: malformed host authority")
	}

	// Reject metadata hostnames by name (case-insensitive, trailing dot stripped).
	nameKey := strings.ToLower(strings.TrimSuffix(host, "."))
	if metadataHosts[nameKey] {
		return nil, fmt.Errorf("urlguard: metadata hostname %q not allowed", host)
	}

	// Reject IP-literal hosts entirely (public or not) — writes must target a
	// named host, and IP literals are a classic SSRF bypass.
	if ip := net.ParseIP(host); ip != nil {
		return nil, fmt.Errorf("urlguard: IP-literal host %q not allowed", host)
	}

	// Resolve and reject if any resolved address is in a blocked range.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, lerr := net.DefaultResolver.LookupIPAddr(ctx, host)
	if lerr == nil {
		for _, a := range addrs {
			if isBlockedIP(a.IP) {
				return nil, fmt.Errorf("urlguard: host %q resolves to blocked address %s", host, a.IP)
			}
		}
	}
	// lerr != nil (NXDOMAIN / offline): scraper re-validates at connect time.

	return u, nil
}

// HostAllowed is the STRICT write matcher: exact host OR a label-anchored
// "*.domain" wildcard only.
//
//   - Bare "*" never matches.
//   - NO apex/www equivalence.
//   - Trailing FQDN dot is stripped; any :port is stripped (match on hostname).
//   - Both host and allow entries are normalized via the pinned non-transitional
//     IDNA (UTS-46) lookup profile to their ASCII (punycode) form before
//     comparison, so a Unicode allow entry and its punycode are equivalent.
//   - A host whose Unicode form is NOT single-script (a TR39 mixed-script /
//     confusable / homograph host) is rejected, UNLESS its exact punycode form
//     is explicitly present in `allow` — an operator override for deliberately
//     internationalized targets.
//
// Confusable detection (documented per task): rather than shipping the full
// TR39 confusables table, this inspects the ORIGINAL Unicode runes of the host
// and rejects any host whose runes span more than one Unicode script, ignoring
// script-neutral runes (Common/Inherited — digits, hyphen, dot). Script
// membership is resolved against the standard-library unicode.Scripts tables.
// This is sufficient to reject Latin/Cyrillic (and any other cross-script)
// homographs such as "аirline.com".
func HostAllowed(host string, allow []string) (bool, error) {
	h := strings.TrimSuffix(strings.TrimSpace(host), ".")
	if h == "" {
		return false, errors.New("urlguard: empty host")
	}
	// Strip any :port. SplitHostPort errors when there is no port, in which
	// case we keep the host as-is.
	if hp, _, err := net.SplitHostPort(h); err == nil {
		h = hp
	}

	asciiHost, err := writeIDNA.ToASCII(h)
	if err != nil {
		return false, fmt.Errorf("urlguard: invalid host %q: %w", host, err)
	}
	asciiHost = strings.ToLower(strings.TrimSuffix(asciiHost, "."))

	confusable := !isSingleScript(h)

	if confusable {
		// Only an exact punycode override in the allow list can permit a
		// mixed-script host. Wildcards do not apply.
		for _, a := range allow {
			na, ok := normalizeAllowExact(a)
			if ok && na == asciiHost {
				return true, nil
			}
		}
		return false, nil
	}

	for _, a := range allow {
		a = strings.TrimSpace(a)
		if a == "" || a == "*" {
			// Bare "*" (and empty) never match.
			continue
		}
		if strings.HasPrefix(a, "*.") {
			suffix, ok := normalizeAllowExact(a[len("*."):])
			if !ok || suffix == "" {
				continue
			}
			// Label-anchored: host must be a strict subdomain of suffix.
			if strings.HasSuffix(asciiHost, "."+suffix) && len(asciiHost) > len(suffix)+1 {
				return true, nil
			}
			continue
		}
		na, ok := normalizeAllowExact(a)
		if ok && na == asciiHost {
			return true, nil
		}
	}
	return false, nil
}

// normalizeAllowExact converts a non-wildcard allow entry (or a wildcard
// suffix) to its lowercased ASCII/punycode form. Returns ok=false if it cannot
// be normalized (invalid IDN), so the caller can skip it.
func normalizeAllowExact(a string) (string, bool) {
	a = strings.TrimSuffix(strings.TrimSpace(a), ".")
	if a == "" {
		return "", false
	}
	ascii, err := writeIDNA.ToASCII(a)
	if err != nil {
		return "", false
	}
	return strings.ToLower(strings.TrimSuffix(ascii, ".")), true
}

// isSingleScript reports whether every script-bearing rune in s belongs to the
// same Unicode script. Script-neutral runes (Common/Inherited: ASCII digits,
// '-', '.', etc.) are ignored. A pure-ASCII host is trivially single-script.
func isSingleScript(s string) bool {
	var seen string
	for _, r := range s {
		if r < 0x80 {
			// ASCII letters are Latin; ASCII digits/punctuation are Common.
			// None of these can, on their own, create a mixed-script host that
			// the pure-ASCII fast path wouldn't already accept, but a Latin
			// letter still pins the script to "Latin" so a later non-Latin
			// rune is detected as mixed.
			if unicode.IsLetter(r) {
				if seen != "" && seen != "Latin" {
					return false
				}
				seen = "Latin"
			}
			continue
		}
		if unicode.Is(unicode.Common, r) || unicode.Is(unicode.Inherited, r) {
			continue
		}
		name := scriptOf(r)
		if name == "" {
			// Unknown/unassigned script-bearing rune: treat conservatively as
			// its own distinct "script" so it cannot silently co-exist.
			name = "Unknown"
		}
		if seen != "" && seen != name {
			return false
		}
		seen = name
	}
	return true
}

// scriptOf returns the Unicode script name for r, or "" if none matches
// (excluding the neutral Common/Inherited scripts, handled by the caller).
func scriptOf(r rune) string {
	for name, table := range unicode.Scripts {
		if name == "Common" || name == "Inherited" {
			continue
		}
		if unicode.Is(table, r) {
			return name
		}
	}
	return ""
}
