package urlguard

import (
	"net"
	"testing"

	"golang.org/x/net/idna"
)

func TestIsBlockedIPRejectsEveryNonPublicRange(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"192.0.0.1",       // IETF protocol assignments
		"192.0.2.1",       // TEST-NET-1
		"198.18.0.1",      // benchmarking
		"198.51.100.1",    // TEST-NET-2
		"203.0.113.1",     // TEST-NET-3
		"224.0.0.1",       // multicast
		"240.0.0.1",       // reserved
		"255.255.255.255", // limited broadcast
		"2001:db8::1",     // IPv6 documentation
		"ff02::1",         // IPv6 multicast
		"192.88.99.1",     // 6to4 relay anycast
		// Transition prefixes embedding a blocked IPv4 destination. Without
		// these, a NAT64/6to4 gateway reaches the v4 target the rules above
		// forbid (audit 2026-07-25 follow-up to A01).
		"64:ff9b::a9fe:a9fe",   // NAT64-encoded 169.254.169.254 (metadata)
		"64:ff9b::7f00:1",      // NAT64-encoded 127.0.0.1
		"64:ff9b:1::a9fe:a9fe", // local-use NAT64 (RFC8215)
		"2002:7f00:1::",        // 6to4-wrapped 127.0.0.1
		"2001:0:0:0::1",        // Teredo
		"100::1",               // discard-only
		"fec0::1",              // deprecated site-local
	} {
		if ip := net.ParseIP(raw); ip == nil || !isBlockedIP(ip) {
			t.Errorf("isBlockedIP(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{
		"93.184.216.34",      // public IPv4 unicast
		"2606:2800:220:1::1", // public IPv6 unicast
		"2001:db9::1",        // adjacent to the documentation prefix, still public
	} {
		if ip := net.ParseIP(raw); ip == nil || isBlockedIP(ip) {
			t.Errorf("public unicast %q must remain allowed", raw)
		}
	}
}

func TestValidateTargetURL(t *testing.T) {
	bad := []string{
		"http://airline.com/claim",                // not https
		"https://user:pass@airline.com/claim",     // userinfo
		"https://169.254.169.254/",                // metadata IP (link-local literal)
		"https://10.0.0.1/", "https://127.0.0.1/", // private/loopback literal
		"https://100.100.100.200/",          // CGNAT / Alibaba metadata literal
		"https://metadata.google.internal/", // metadata hostname
		"file:///etc/passwd",                // scheme
	}
	for _, u := range bad {
		if _, err := ValidateTargetURL(u); err == nil {
			t.Errorf("expected reject: %s", u)
		}
	}
	if _, err := ValidateTargetURL("https://claims.airline.com/x"); err != nil {
		t.Errorf("expected accept: %v", err)
	}
}

func TestHostAllowed(t *testing.T) {
	// STRICT write matcher: exact only (NO apex/www); subdomains only via "*.".
	exact := map[string]bool{
		"airline.com":        true,
		"airline.com.":       true, // trailing dot normalized
		"www.airline.com":    false,
		"claims.airline.com": false,
		"notairline.com":     false,
	}
	for h, want := range exact {
		if got, _ := HostAllowed(h, []string{"airline.com"}); got != want {
			t.Errorf("HostAllowed(%q, [airline.com])=%v want %v", h, got, want)
		}
	}
	// "*.airline.com" matches subdomains but not the apex.
	wild := map[string]bool{"claims.airline.com": true, "airline.com": false}
	for h, want := range wild {
		if got, _ := HostAllowed(h, []string{"*.airline.com"}); got != want {
			t.Errorf("HostAllowed(%q, [*.airline.com])=%v want %v", h, got, want)
		}
	}
	// bare "*" is rejected (never matches); IDN homograph (Cyrillic а) must not match ASCII.
	if ok, _ := HostAllowed("anything.com", []string{"*"}); ok {
		t.Error("bare * must not match")
	}
	if ok, _ := HostAllowed("аirline.com", []string{"airline.com"}); ok {
		t.Error("cyrillic homograph must not match")
	}
}

// TestHostAllowedMixedScript exercises the mixed-script/confusable rejection
// beyond the single Cyrillic case: a Cyrillic "о" spliced into a Latin label
// must never satisfy an ASCII allow entry, nor a matching wildcard.
func TestHostAllowedMixedScript(t *testing.T) {
	homograph := "gоogle.com" // Cyrillic U+043E 'о'
	if ok, _ := HostAllowed(homograph, []string{"google.com"}); ok {
		t.Errorf("mixed-script %q must not match exact allow", homograph)
	}
	if ok, _ := HostAllowed(homograph, []string{"*.com"}); ok {
		t.Errorf("mixed-script %q must not match wildcard allow", homograph)
	}
	// A single-script non-Latin host is not confusable and may match its own
	// exact (punycode-normalized) allow entry.
	if ok, err := HostAllowed("例え.jp", []string{"例え.jp"}); err != nil || !ok {
		t.Errorf("single-script IDN should match its own allow entry: ok=%v err=%v", ok, err)
	}
}

// TestHostAllowedPunycodeOverride verifies the operator override: a
// mixed-script host is permitted iff its EXACT punycode form is present in the
// allow list.
func TestHostAllowedPunycodeOverride(t *testing.T) {
	homograph := "аirline.com" // Cyrillic 'а' + Latin
	puny, err := idna.Lookup.ToASCII(homograph)
	if err != nil {
		t.Fatalf("idna ToASCII(%q): %v", homograph, err)
	}
	if puny == homograph {
		t.Fatalf("expected a punycode xn-- form, got %q", puny)
	}
	if ok, err := HostAllowed(homograph, []string{puny}); err != nil || !ok {
		t.Errorf("mixed-script host must be allowed with exact punycode override: ok=%v err=%v", ok, err)
	}
	// The override is exact-only: a wildcard does not rescue a mixed-script host.
	if ok, _ := HostAllowed(homograph, []string{"*.com"}); ok {
		t.Errorf("mixed-script host must not be allowed via wildcard even with punycode elsewhere")
	}
}
