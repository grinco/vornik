package report

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/secrets"
)

// The public body must NOT leak identifiers — email, home paths (with the
// username + project dir), LAN IPs, the machine hostname, or a secret token —
// whether they appear in a doctor Message or the user's --summary.
func TestAnonymizeBody_NoLeaks(t *testing.T) {
	in := BodyInput{
		Version: "2026.7.4-60", Edition: "ce", OS: "linux", Arch: "amd64",
		Hostname: "AI-PC", DaemonUp: false,
		Symptom: "failed connecting to swarm at 192.0.2.10 or 2001:db8::1234, see /var/home/vadim/projects/vornik-marketing/logs; ping vadim@vornik.io",
		Checks: []Check{
			{Name: "database", Status: "fail", Message: "dial 10.88.0.4:5432 refused for /var/home/vadim/x on ai-pc"},
			{Name: "config", Status: "fail", Message: "token=sk-abcdef0123456789abcdef leaked in config"},
			// BARE key shapes (no `key=` context, below the 40-char entropy
			// floor) — these slipped past secrets.Redact until reKeyShapes was
			// added (found live 2026-07-25). Public post must not carry them.
			{Name: "journal", Status: "warn", Message: "saw sk-bareABCDEF0123456789 and AKIABAREKEY01234567 and ghp_bareGithubToken0123456789ABCDEFxx in logs"},
		},
	}
	body, err := AnonymizeBody(in)
	if err != nil {
		t.Fatalf("AnonymizeBody: %v", err)
	}
	for _, leak := range []string{
		"vadim@vornik.io", "/var/home/vadim", "192.0.2.10", "10.88.0.4", "AI-PC",
		"2001:db8::1234", "ai-pc",
		"sk-abcdef0123456789abcdef", "vadim",
		// review-20260725-7530 #1/#2: the home-path TAIL (project dir) must not
		// survive; the whole path collapses to <path>.
		"vornik-marketing", "projects", "/x",
		// bare key shapes (no assignment context) — reKeyShapes net.
		"sk-bareABCDEF0123456789", "AKIABAREKEY01234567", "ghp_bareGithubToken0123456789ABCDEFxx",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("public body LEAKS %q:\n%s", leak, body)
		}
	}
	// Safe/controlled fields survive.
	for _, keep := range []string{"2026.7.4-60", "ce", "linux/amd64", "database", "config", "down"} {
		if !strings.Contains(body, keep) {
			t.Errorf("body missing expected %q:\n%s", keep, body)
		}
	}
}

// Fail-closed: if the secret detector can't be built, no body is emitted and the
// error is the STATIC message (no wrapped offending value).
func TestAnonymizeBody_FailClosed(t *testing.T) {
	orig := newDetector
	newDetector = func() (*secrets.MultiDetector, error) { return nil, errors.New("boom-secret-VALUE") }
	defer func() { newDetector = orig }()

	body, err := AnonymizeBody(BodyInput{Version: "1", Symptom: "boom-secret-VALUE"})
	if err == nil {
		t.Fatal("expected fail-closed error")
	}
	if body != "" {
		t.Errorf("fail-closed must emit no body, got %q", body)
	}
	if strings.Contains(err.Error(), "boom-secret-VALUE") {
		t.Errorf("error must be static, must not leak the value: %v", err)
	}
}

func TestIssueURL_Basic(t *testing.T) {
	u := IssueURL("Install failure: podman", "body **markdown** here")
	if !strings.HasPrefix(u, "https://github.com/grinco/vornik/issues/new?") {
		t.Fatalf("wrong repo/base: %s", u)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("unparseable URL: %v", err)
	}
	q := parsed.Query()
	if q.Get("labels") != "bug" {
		t.Errorf("labels = %q, want bug", q.Get("labels"))
	}
	if q.Get("title") != "Install failure: podman" {
		t.Errorf("title round-trip failed: %q", q.Get("title"))
	}
	if q.Get("body") != "body **markdown** here" {
		t.Errorf("body round-trip failed: %q", q.Get("body"))
	}
}

func TestIssueURL_Truncates(t *testing.T) {
	huge := strings.Repeat("x", 20000)
	u := IssueURL("t", huge)
	if len(u) > urlMaxBytes {
		t.Errorf("URL not truncated under cap: len=%d", len(u))
	}
	parsed, _ := url.Parse(u)
	if !strings.Contains(parsed.Query().Get("body"), "truncated") {
		t.Errorf("truncated URL must carry the attach-note: %q", parsed.Query().Get("body"))
	}
}

// TestRedactIPv6Token pins both halves of the IPv6 redaction contract:
// real addresses are redacted (A19), and ordinary text containing "::" is left
// intact. The original substring regex failed the second half because "::",
// "d::" and "e::f" all parse as valid IPv6, so a fragment grabbed from the
// middle of a word was redacted — `core::fmt::Debug` became `cor<ip>mt<ip>ug`
// in PUBLIC issue bodies (audit 2026-07-25 follow-up).
func TestRedactIPv6Token(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		// --- must redact -------------------------------------------------
		{"loopback", "::1", "<ip>"},
		{"link-local with zone", "fe80::1%eth0", "<ip>%eth0"},
		{"compressed global", "2001:db8::1", "<ip>"},
		{"full form", "2001:0db8:0000:0000:0000:0000:0000:0001", "<ip>"},
		{"multicast", "ff02::1", "<ip>"},
		{"bracketed with port", "[2001:db8::1]:8080", "[<ip>]:8080"},
		{"trailing sentence period", "2001:db8::1.", "<ip>."},
		// --- must NOT be touched -----------------------------------------
		{"rust path", "core::fmt::Debug", "core::fmt::Debug"},
		{"cpp symbol", "std::vector", "std::vector"},
		{"postgres cast", "::text", "::text"},
		{"bare scope operator", "::", "::"},
		{"timestamp", "16:52:10Z", "16:52:10Z"},
		{"ipv4 left to reIPv4", "192.168.1.1", "192.168.1.1"},
		{"plain word", "database", "database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactIPv6Token(tc.in); got != tc.want {
				t.Errorf("redactIPv6Token(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestScrubberIPv6InContext runs the whole scrubber over realistic doctor
// message text: the address must go, the surrounding diagnostics must survive
// verbatim so the public issue is still actionable.
func TestScrubberIPv6InContext(t *testing.T) {
	scrub, err := scrubber("build-host-01")
	if err != nil {
		t.Fatalf("scrubber: %v", err)
	}
	for _, tc := range []struct {
		name        string
		in          string
		wantAbsent  string
		wantPresent []string
	}{
		{
			name:        "address redacted, prose intact",
			in:          "dial tcp [2001:db8:dead:beef::5]:5432: connection refused",
			wantAbsent:  "2001:db8",
			wantPresent: []string{"<ip>", "5432", "connection refused"},
		},
		{
			name:        "postgres cast survives",
			in:          `pq: operator does not exist: text = integer in "created_at"::text`,
			wantAbsent:  "<ip>",
			wantPresent: []string{"::text", "operator does not exist"},
		},
		{
			name:        "go/rust style path survives",
			in:          "panic in core::fmt::Debug while formatting the report",
			wantAbsent:  "<ip>",
			wantPresent: []string{"core::fmt::Debug"},
		},
		{
			name:        "case-insensitive hostname still redacted (A21)",
			in:          "connecting to BUILD-Host-01 failed",
			wantAbsent:  "BUILD-Host-01",
			wantPresent: []string{"<host>"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scrub(tc.in)
			if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
				t.Errorf("scrub(%q) = %q, must not contain %q", tc.in, got, tc.wantAbsent)
			}
			for _, want := range tc.wantPresent {
				if !strings.Contains(got, want) {
					t.Errorf("scrub(%q) = %q, want it to retain %q", tc.in, got, want)
				}
			}
		})
	}
}
