package integrations

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDialGuard_BlocksLoopback — a probe target on 127.0.0.1 (the address
// httptest.NewServer binds to) must be refused at connect time, not merely
// warned about. This is the core SSRF invariant (design §6): every prober
// dials through DialGuard, and DialGuard blocks loopback/private/link-local
// destinations unless explicitly allowed.
func TestDialGuard_BlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	guard := DialGuard{}
	client := guard.HTTPClient(2 * time.Second)

	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("expected the loopback dial to be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "dial guard") {
		t.Errorf("error = %q, want it to name the dial guard as the cause", err.Error())
	}
}

// TestDialGuard_BlocksHostnameResolvingToLoopback — a DNS-rebinding-shaped
// case: the hostname itself ("localhost") doesn't look private as a string,
// but resolves to 127.0.0.1. The guard must catch this via the *resolved* IP
// (net.Dialer.Control runs post-resolution), not a hostname string check.
func TestDialGuard_BlocksHostnameResolvingToLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, port, ok := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	if !ok {
		t.Fatalf("could not parse port out of test server URL %q", srv.URL)
	}

	guard := DialGuard{}
	client := guard.HTTPClient(2 * time.Second)

	_, err := client.Get("http://localhost:" + port)
	if err == nil {
		t.Fatal("expected the 'localhost' dial (resolves to 127.0.0.1) to be refused")
	}
	if !strings.Contains(err.Error(), "dial guard") {
		t.Errorf("error = %q, want it to name the dial guard as the cause", err.Error())
	}
}

// TestDialGuard_AllowedHosts_PermitsNamedHost — the opt-in allowlist lets a
// self-hosted operator permit a specific internal/loopback host explicitly.
func TestDialGuard_AllowedHosts_PermitsNamedHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	_, port, ok := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	if !ok {
		t.Fatalf("could not parse port out of test server URL %q", srv.URL)
	}

	guard := DialGuard{AllowedHosts: []string{"localhost"}}
	client := guard.HTTPClient(2 * time.Second)

	resp, err := client.Get("http://localhost:" + port)
	if err != nil {
		t.Fatalf("allowlisted host must be permitted, got error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want %d (request must have actually reached the test server)", resp.StatusCode, http.StatusTeapot)
	}
}

// TestDialGuard_AllowedHosts_DoesNotPermitOtherHosts — the allowlist is
// per-host, not a global bypass: a different loopback host not on the list
// is still refused.
func TestDialGuard_AllowedHosts_DoesNotPermitOtherHosts(t *testing.T) {
	guard := DialGuard{AllowedHosts: []string{"only-this-host.example"}}
	client := guard.HTTPClient(2 * time.Second)

	_, err := client.Get("http://127.0.0.1:1/")
	if err == nil {
		t.Fatal("expected a non-allowlisted loopback host to be refused")
	}
	if !strings.Contains(err.Error(), "dial guard") {
		t.Errorf("error = %q, want it to name the dial guard as the cause", err.Error())
	}
}

// TestIsBlockedIP_Classification unit-tests the resolved-IP classification
// rule directly: RFC1918 / loopback / link-local / unspecified are blocked;
// ordinary public addresses are not. This is the predicate DialGuard's
// Control callback applies to the *resolved* IP (see the connect-time tests
// above for the end-to-end behavior).
func TestIsBlockedIP_Classification(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},             // loopback
		{"::1", true},                   // loopback v6
		{"10.0.0.1", true},              // RFC1918
		{"172.16.0.5", true},            // RFC1918
		{"192.168.1.1", true},           // RFC1918
		{"169.254.1.1", true},           // link-local unicast
		{"224.0.0.1", true},             // link-local multicast
		{"0.0.0.0", true},               // unspecified
		{"93.184.216.34", false},        // public
		{"8.8.8.8", false},              // public
		{"2001:4860:4860::8888", false}, // public v6
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("could not parse IP %q", tc.ip)
		}
		got := isBlockedIP(ip)
		if got != tc.blocked {
			t.Errorf("isBlockedIP(%s) = %v, want %v", tc.ip, got, tc.blocked)
		}
	}
}

// TestDialGuard_ContextTimeout — HTTPClient bounds requests to the given
// timeout so a hung/slow candidate endpoint can't stall a probe forever.
func TestDialGuard_ContextTimeout(t *testing.T) {
	guard := DialGuard{}
	client := guard.HTTPClient(25 * time.Millisecond)
	if client.Timeout != 25*time.Millisecond {
		t.Errorf("client.Timeout = %v, want 25ms", client.Timeout)
	}
}

// TestDialGuard_Dialer_BlocksLoopback — the *net.Dialer returned by
// Dialer() is the seam email.IMAPDialConfig.Dialer expects; it must apply
// the same guard as DialContext/HTTPClient (they share this method), not a
// weaker one.
func TestDialGuard_Dialer_BlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	guard := DialGuard{}
	dialer := guard.Dialer("127.0.0.1")
	_, err := dialer.DialContext(context.Background(), "tcp", "127.0.0.1:"+port)
	if err == nil {
		t.Fatal("expected the guarded Dialer to refuse a loopback destination")
	}
}

// TestDialGuard_Dialer_AllowedHostBypassesGuard.
func TestDialGuard_Dialer_AllowedHostBypassesGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	guard := DialGuard{AllowedHosts: []string{"127.0.0.1"}}
	dialer := guard.Dialer("127.0.0.1")
	conn, err := dialer.DialContext(context.Background(), "tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("expected the allowlisted host's Dialer to connect, got: %v", err)
	}
	_ = conn.Close()
}

func TestDialGuard_DialContext_RespectsCancelledContext(t *testing.T) {
	guard := DialGuard{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := guard.DialContext(ctx, "tcp", "93.184.216.34:80")
	if err == nil {
		t.Fatal("expected a cancelled context to produce an error")
	}
}
