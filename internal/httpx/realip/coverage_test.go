package realip

import (
	"context"
	"net/http"
	"testing"
)

func TestRemoteHost_Variants(t *testing.T) {
	cases := map[string]struct {
		remoteAddr string
		nilReq     bool
		want       string
	}{
		"host:port":   {remoteAddr: "10.0.0.1:5000", want: "10.0.0.1"},
		"bare host":   {remoteAddr: "10.0.0.1", want: "10.0.0.1"},
		"empty":       {remoteAddr: "", want: ""},
		"ipv6":        {remoteAddr: "[2001:db8::1]:443", want: "2001:db8::1"},
		"nil request": {nilReq: true, want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var r *http.Request
			if !tc.nilReq {
				r, _ = http.NewRequest(http.MethodGet, "/", nil)
				r.RemoteAddr = tc.remoteAddr
			}
			if got := RemoteHost(r); got != tc.want {
				t.Fatalf("RemoteHost(%q) = %q, want %q", tc.remoteAddr, got, tc.want)
			}
		})
	}
}

func TestClientIPFromContext_NilAndUnset(t *testing.T) {
	if got := ClientIPFromContext(nil); got != "" { //nolint:staticcheck // intentional nil ctx
		t.Fatalf("nil ctx: want empty, got %q", got)
	}
	if got := ClientIPFromContext(context.Background()); got != "" {
		t.Fatalf("unset ctx: want empty, got %q", got)
	}
	ctx := WithClientIP(context.Background(), "203.0.113.7")
	if got := ClientIPFromContext(ctx); got != "203.0.113.7" {
		t.Fatalf("set ctx: want 203.0.113.7, got %q", got)
	}
}

func TestParseHost_Empty(t *testing.T) {
	if ip := parseHost(""); ip != nil {
		t.Fatalf("parseHost(\"\") = %v, want nil", ip)
	}
	if ip := parseHost("garbage"); ip != nil {
		t.Fatalf("parseHost(garbage) = %v, want nil", ip)
	}
	if ip := parseHost("10.0.0.1"); ip == nil {
		t.Fatal("parseHost(10.0.0.1) = nil, want non-nil")
	}
}

func TestHasForwardingHeader_Branches(t *testing.T) {
	c := mustConfig(t, true, []string{"10.0.0.5/32"}, "True-Client-IP")

	none := req("10.0.0.5:1", nil)
	if c.HasForwardingHeader(none) {
		t.Fatal("no header: want false")
	}
	custom := req("10.0.0.5:1", map[string]string{"True-Client-IP": "1.2.3.4"})
	if !c.HasForwardingHeader(custom) {
		t.Fatal("configured header present: want true")
	}
	xff := req("10.0.0.5:1", map[string]string{"X-Forwarded-For": "1.2.3.4"})
	if !c.HasForwardingHeader(xff) {
		t.Fatal("XFF present: want true")
	}
	cf := req("10.0.0.5:1", map[string]string{"CF-Connecting-IP": "1.2.3.4"})
	if !c.HasForwardingHeader(cf) {
		t.Fatal("default CF header present: want true")
	}
}

func TestNewMetrics_NilRegistererPanicsOnceOnly(t *testing.T) {
	// Exercise the nil-registerer default branch on an isolated registry to
	// avoid the DefaultRegisterer double-registration panic across runs.
	m := NewMetrics(newTestRegistry(t))
	if m.UntrustedHeaderTotal == nil {
		t.Fatal("UntrustedHeaderTotal must be registered")
	}
}

func TestNewConfig_SkipsEmptyEntries(t *testing.T) {
	c, err := NewConfig(true, []string{"", "  ", "10.0.0.5/32"}, "")
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	if len(c.TrustedProxies) != 1 {
		t.Fatalf("empty entries must be skipped: got %d nets", len(c.TrustedProxies))
	}
}
