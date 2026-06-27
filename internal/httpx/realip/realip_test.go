package realip

import (
	"net/http"
	"testing"
)

func req(remoteAddr string, headers map[string]string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func mustConfig(t *testing.T, enabled bool, proxies []string, header string) Config {
	t.Helper()
	c, err := NewConfig(enabled, proxies, header)
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	return c
}

func TestResolveClientIP_TrustedSourceValidHeader(t *testing.T) {
	c := mustConfig(t, true, []string{"10.0.0.5/32"}, "CF-Connecting-IP")
	r := req("10.0.0.5:54321", map[string]string{"CF-Connecting-IP": "203.0.113.7"})
	if got := c.ResolveClientIP(r); got != "203.0.113.7" {
		t.Fatalf("trusted source + valid header: want 203.0.113.7, got %q", got)
	}
}

// TestResolveClientIP_SpoofRegression is the security regression for the
// Cloudflare tunnel real-IP spoof: leftmost-XFF was attacker-controllable.
// An UNtrusted RemoteAddr that forges CF-Connecting-IP / X-Forwarded-For
// must NOT influence the resolved client IP — we return RemoteAddr's host.
// Pre-fix the leftmost-XFF code path would have returned the forged value,
// letting an attacker trip another customer's per-IP lockout or evade the
// rate-limit by rotating the spoofed value.
func TestResolveClientIP_SpoofRegression(t *testing.T) {
	c := mustConfig(t, true, []string{"10.0.0.5/32"}, "CF-Connecting-IP")
	r := req("198.51.100.9:40000", map[string]string{
		"CF-Connecting-IP": "203.0.113.7",      // forged
		"X-Forwarded-For":  "1.2.3.4, 5.6.7.8", // forged leftmost
	})
	if got := c.ResolveClientIP(r); got != "198.51.100.9" {
		t.Fatalf("spoof regression: untrusted source must key on RemoteAddr, want 198.51.100.9, got %q", got)
	}
}

func TestResolveClientIP_TrustedMissingHeader(t *testing.T) {
	c := mustConfig(t, true, []string{"10.0.0.5/32"}, "CF-Connecting-IP")
	r := req("10.0.0.5:54321", nil)
	if got := c.ResolveClientIP(r); got != "10.0.0.5" {
		t.Fatalf("trusted + missing header: want 10.0.0.5, got %q", got)
	}
}

func TestResolveClientIP_TrustedGarbageHeader(t *testing.T) {
	c := mustConfig(t, true, []string{"10.0.0.5/32"}, "CF-Connecting-IP")
	r := req("10.0.0.5:54321", map[string]string{"CF-Connecting-IP": "not-an-ip"})
	if got := c.ResolveClientIP(r); got != "10.0.0.5" {
		t.Fatalf("trusted + garbage header: want 10.0.0.5, got %q", got)
	}
}

func TestResolveClientIP_IPv6(t *testing.T) {
	c := mustConfig(t, true, []string{"2001:db8::/32"}, "CF-Connecting-IP")
	r := req("[2001:db8::1]:54321", map[string]string{"CF-Connecting-IP": "2001:db8:abcd::beef"})
	if got := c.ResolveClientIP(r); got != "2001:db8:abcd::beef" {
		t.Fatalf("ipv6 trusted + header: want 2001:db8:abcd::beef, got %q", got)
	}
}

func TestResolveClientIP_BareIPTrustedProxy(t *testing.T) {
	c := mustConfig(t, true, []string{"10.0.0.5"}, "CF-Connecting-IP")
	r := req("10.0.0.5:1", map[string]string{"CF-Connecting-IP": "203.0.113.7"})
	if got := c.ResolveClientIP(r); got != "203.0.113.7" {
		t.Fatalf("bare-IP trusted: want 203.0.113.7, got %q", got)
	}
	// A neighbour in the same /24 must NOT be trusted by a /32.
	r2 := req("10.0.0.6:1", map[string]string{"CF-Connecting-IP": "203.0.113.7"})
	if got := c.ResolveClientIP(r2); got != "10.0.0.6" {
		t.Fatalf("bare-IP is /32: 10.0.0.6 must not be trusted, got %q", got)
	}
}

func TestResolveClientIP_CIDRTrustedProxy(t *testing.T) {
	c := mustConfig(t, true, []string{"10.0.0.0/24"}, "CF-Connecting-IP")
	r := req("10.0.0.99:1", map[string]string{"CF-Connecting-IP": "203.0.113.7"})
	if got := c.ResolveClientIP(r); got != "203.0.113.7" {
		t.Fatalf("cidr trusted: want 203.0.113.7, got %q", got)
	}
}

func TestResolveClientIP_Disabled(t *testing.T) {
	c := mustConfig(t, false, []string{"10.0.0.5/32"}, "CF-Connecting-IP")
	r := req("10.0.0.5:54321", map[string]string{"CF-Connecting-IP": "203.0.113.7"})
	if got := c.ResolveClientIP(r); got != "10.0.0.5" {
		t.Fatalf("disabled: want RemoteAddr 10.0.0.5, got %q", got)
	}
}

func TestResolveClientIP_RemoteAddrNoPort(t *testing.T) {
	c := mustConfig(t, true, []string{"10.0.0.5/32"}, "CF-Connecting-IP")
	r := req("10.0.0.5", map[string]string{"CF-Connecting-IP": "203.0.113.7"})
	if got := c.ResolveClientIP(r); got != "203.0.113.7" {
		t.Fatalf("no-port RemoteAddr trusted: want 203.0.113.7, got %q", got)
	}
}

func TestNewConfig_DefaultHeader(t *testing.T) {
	c, err := NewConfig(true, []string{"10.0.0.5/32"}, "")
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	if c.Header != "CF-Connecting-IP" {
		t.Fatalf("default header: want CF-Connecting-IP, got %q", c.Header)
	}
}

func TestNewConfig_BadCIDRRejected(t *testing.T) {
	if _, err := NewConfig(true, []string{"not-a-cidr"}, ""); err == nil {
		t.Fatal("NewConfig: expected error for bad CIDR, got nil")
	}
}

func TestNewConfig_CustomHeader(t *testing.T) {
	c := mustConfig(t, true, []string{"10.0.0.5/32"}, "True-Client-IP")
	r := req("10.0.0.5:1", map[string]string{"True-Client-IP": "203.0.113.7"})
	if got := c.ResolveClientIP(r); got != "203.0.113.7" {
		t.Fatalf("custom header: want 203.0.113.7, got %q", got)
	}
}
