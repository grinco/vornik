package version

import (
	"strings"
	"testing"
)

// TestUserAgent_Shape pins the outbound identity string. It is the value
// vendors rate-limit, allowlist and support-triage by, so its shape is a
// contract with the outside world rather than an implementation detail
// (MCP server authentication design §5, F3).
func TestUserAgent_Shape(t *testing.T) {
	got := UserAgent("2026.7.7")
	want := "Vornik/2026.7.7 (+https://vornik.io)"
	if got != want {
		t.Errorf("UserAgent = %q, want %q", got, want)
	}
}

// TestUserAgent_EmptyVersionFallsBackToDefault keeps an unstamped build from
// emitting "Vornik/ (+...)", which reads as a malformed client to a WAF.
func TestUserAgent_EmptyVersionFallsBackToDefault(t *testing.T) {
	got := UserAgent("")
	if !strings.Contains(got, Default) {
		t.Errorf("UserAgent(%q) = %q; want the %s fallback", "", got, Default)
	}
}

// TestUserAgent_NeverLooksLikeALibraryOrBrowser is the F3 regression: the
// point of a Vornik-specific UA is that it is neither an anonymous library
// default (shared reputation pool, the thing WAFs sweep up) nor a browser
// spoof (dishonest, and fragile against the same fingerprinting).
func TestUserAgent_NeverLooksLikeALibraryOrBrowser(t *testing.T) {
	got := UserAgent("2026.7.7")
	for _, bad := range []string{"Go-http-client", "python", "urllib", "Mozilla", "curl"} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(bad)) {
			t.Errorf("UserAgent = %q; must not impersonate or default to %q", got, bad)
		}
	}
}

// TestUserAgent_SanitizesVersion keeps an injected build string from breaking
// the header. A version arrives from an ldflag and is not attacker-controlled
// today, but a stray newline would corrupt every outbound request, so the
// helper is total rather than trusting its input.
func TestUserAgent_SanitizesVersion(t *testing.T) {
	got := UserAgent("2026.7.7\r\nX-Evil: 1")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("UserAgent = %q; must not carry CR/LF", got)
	}
	if strings.Contains(got, "X-Evil") {
		t.Errorf("UserAgent = %q; must not carry a smuggled header", got)
	}
}
