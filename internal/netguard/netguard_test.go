package netguard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsPrivateIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":       true,  // loopback
		"::1":             true,  // loopback v6
		"10.0.0.1":        true,  // RFC1918
		"192.168.1.1":     true,  // RFC1918
		"172.16.5.4":      true,  // RFC1918
		"169.254.169.254": true,  // link-local (cloud metadata)
		"0.0.0.0":         true,  // unspecified
		"224.0.0.1":       true,  // multicast
		"8.8.8.8":         false, // public
		"1.1.1.1":         false, // public
	}
	for s, want := range cases {
		if got := IsPrivateIP(net.ParseIP(s)); got != want {
			t.Errorf("IsPrivateIP(%s) = %v, want %v", s, got, want)
		}
	}
	if !IsPrivateIP(nil) {
		t.Error("IsPrivateIP(nil) must be true (fail closed)")
	}
}

func TestIsLocalHostname(t *testing.T) {
	cases := map[string]bool{
		"localhost":     true,
		"LocalHost.":    true,
		"foo.localhost": true,
		"example.com":   false,
		"":              false,
	}
	for h, want := range cases {
		if got := IsLocalHostname(h); got != want {
			t.Errorf("IsLocalHostname(%q) = %v, want %v", h, got, want)
		}
	}
}

// ValidateURL is exercised with IP-literal hosts so the test needs no DNS.
func TestValidateURL(t *testing.T) {
	ctx := context.Background()
	bad := []string{
		"http://127.0.0.1/x",
		"http://10.1.2.3/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/x",
		"ftp://8.8.8.8/x",            // scheme
		"http://user:pass@8.8.8.8/x", // userinfo
		"http://localhost/x",         // local name
	}
	for _, u := range bad {
		if err := ValidateURL(ctx, u); err == nil {
			t.Errorf("ValidateURL(%q) = nil, want error", u)
		}
	}
	if err := ValidateURL(ctx, "http://8.8.8.8/x"); err != nil {
		t.Errorf("ValidateURL(public IP) = %v, want nil", err)
	}
}

// The guarded client must refuse to reach a loopback httptest server — the
// canonical SSRF attempt (agent points an image URL at an internal address).
func TestNewGuardedClient_BlocksLoopback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewGuardedClient(2 * time.Second)
	resp, err := client.Get(ts.URL) // ts.URL is http://127.0.0.1:PORT
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("guarded client reached loopback %s — SSRF guard failed", ts.URL)
	}
}
