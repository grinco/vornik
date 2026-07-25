package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/apigateway"
)

// review (Task 3): keep both compile-time capability assertions live in the
// test package too, alongside the ones in client.go — a regression here
// would otherwise only surface as a mysterious type-assertion failure deep
// in the dispatcher's list_apis path.
var (
	_ apigateway.Client         = (*Client)(nil)
	_ apigateway.ProviderLister = (*Client)(nil)
)

// TestListProviders_SortedWithExamplesIntact covers design §5.2: the
// concrete gateway Client satisfies the optional apigateway.ProviderLister
// capability via Registry.Describe(), which sorts by name and carries
// Examples through untouched.
func TestListProviders_SortedWithExamplesIntact(t *testing.T) {
	reg := apigateway.Registry{
		"zeta":  {BasePath: "/zeta", Description: "Zeta API"},
		"alpha": {BasePath: "/alpha", Description: "Alpha API", Examples: []string{"GET /alpha/ping"}},
	}
	c, err := New("http://127.0.0.1:8010", "tok", reg, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var pl apigateway.ProviderLister = c
	got := pl.ListProviders()
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Errorf("order = [%q, %q], want alpha before zeta", got[0].Name, got[1].Name)
	}
	if len(got[0].Examples) != 1 || got[0].Examples[0] != "GET /alpha/ping" {
		t.Errorf("alpha Examples = %v, want [GET /alpha/ping]", got[0].Examples)
	}
	if len(got[1].Examples) != 0 {
		t.Errorf("zeta Examples = %v, want empty", got[1].Examples)
	}
}

func newClientForServer(t *testing.T, srv *httptest.Server, reg apigateway.Registry, token string) *Client {
	t.Helper()
	// httptest listens on 127.0.0.1; DialGuard must allow that host explicitly
	// (it blocks loopback by default) — this mirrors the real gateway being local.
	c, err := New(srv.URL, token, reg, 5*time.Second)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return c
}

func TestCall_SendsApikeyAndReturnsBody(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("apikey")
		if r.URL.Path != "/maps/geocode/json" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"OK"}`))
	}))
	defer srv.Close()
	reg := apigateway.Registry{"maps": {BasePath: "/maps"}}
	c := newClientForServer(t, srv, reg, "tok123")

	resp, err := c.Call(context.Background(), apigateway.Request{Provider: "maps", Method: "GET", Path: "/geocode/json"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Status != 200 || !strings.Contains(resp.Body, `"status":"OK"`) {
		t.Errorf("resp = %+v", resp)
	}
	if gotKey != "tok123" {
		t.Errorf("apikey header = %q, want tok123", gotKey)
	}
}

func TestCall_UnknownProviderNoNetwork(t *testing.T) {
	c := newClientForServer(t, httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("must not call gateway for unknown provider")
	})), apigateway.Registry{}, "t")
	_, err := c.Call(context.Background(), apigateway.Request{Provider: "ghost", Method: "GET"})
	if !errors.Is(err, apigateway.ErrUnknownProvider) {
		t.Errorf("err = %v, want ErrUnknownProvider", err)
	}
}

func TestCall_WriteOnReadOnlyRefusedNoNetwork(t *testing.T) {
	c := newClientForServer(t, httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("must not call gateway for policy-refused method")
	})), apigateway.Registry{"maps": {BasePath: "/maps"}}, "t")
	_, err := c.Call(context.Background(), apigateway.Request{Provider: "maps", Method: "POST"})
	if !errors.Is(err, apigateway.ErrMethodNotAllowed) {
		t.Errorf("err = %v, want ErrMethodNotAllowed", err)
	}
}

func TestCall_GatewayAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newClientForServer(t, srv, apigateway.Registry{"maps": {BasePath: "/maps"}}, "wrong")
	_, err := c.Call(context.Background(), apigateway.Request{Provider: "maps", Method: "GET"})
	if !errors.Is(err, apigateway.ErrGatewayAuth) {
		t.Errorf("err = %v, want ErrGatewayAuth on 401", err)
	}
}

func TestCall_MethodNotAllowedUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer srv.Close()
	// writes on so the daemon lets it through; gateway 405 is the boundary.
	reg := apigateway.Registry{"maps": {BasePath: "/maps", AllowedMethods: []string{"POST"}, WritesEnabled: true}}
	c := newClientForServer(t, srv, reg, "t")
	_, err := c.Call(context.Background(), apigateway.Request{Provider: "maps", Method: "POST"})
	if !errors.Is(err, apigateway.ErrUpstreamMethod) {
		t.Errorf("err = %v, want ErrUpstreamMethod on 405", err)
	}
}

// review F2: a path containing a ".." segment is a traversal attempt; the
// daemon-side pre-filter must reject it before any network call (design §5, C2).
func TestCall_PathTraversalRefusedNoNetwork(t *testing.T) {
	for _, p := range []string{"/../admin", "foo/../../bar", "/maps/../../secret"} {
		c := newClientForServer(t, httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("must not call gateway for traversal path %q", p)
		})), apigateway.Registry{"maps": {BasePath: "/maps"}}, "t")
		_, err := c.Call(context.Background(), apigateway.Request{Provider: "maps", Method: "GET", Path: p})
		if err == nil {
			t.Errorf("path %q: expected a traversal-rejection error, got nil", p)
		}
		if !errors.Is(err, apigateway.ErrInvalidPath) {
			t.Errorf("path %q: err = %v, want ErrInvalidPath", p, err)
		}
	}
}

func TestNew_InvalidBaseURL(t *testing.T) {
	// A control character makes url.Parse fail outright (the url.Parse branch).
	_, err := New("http://exa\x7fmple.com", "tok", apigateway.Registry{}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "gateway base url") {
		t.Fatalf("err = %v, want a url.Parse error", err)
	}
}

func TestNew_EmptyHost(t *testing.T) {
	// A scheme-less/host-less string parses but has an empty Host.
	_, err := New("no-scheme/only-path", "tok", apigateway.Registry{}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "no host") {
		t.Fatalf("err = %v, want an empty-host error", err)
	}
}

func TestCall_WriteBodyReachesUpstream(t *testing.T) {
	var gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotCT = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	reg := apigateway.Registry{"maps": {BasePath: "/maps", AllowedMethods: []string{"POST"}, WritesEnabled: true}}
	c := newClientForServer(t, srv, reg, "tok123")

	resp, err := c.Call(context.Background(), apigateway.Request{
		Provider: "maps", Method: "POST", Path: "/places",
		Body: map[string]any{"name": "cafe"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200", resp.Status)
	}
	if !strings.Contains(gotBody, `"name":"cafe"`) {
		t.Errorf("upstream body = %q, want JSON-encoded {name:cafe}", gotBody)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
}

func TestCall_Non200StatusReturnedNotError(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream boom"))
	}))
	defer srv.Close()
	reg := apigateway.Registry{"maps": {BasePath: "/maps"}}
	// short token (<4) exercises the scrub no-op branch.
	c := newClientForServer(t, srv, reg, "ab")

	resp, err := c.Call(context.Background(), apigateway.Request{
		Provider: "maps", Method: "GET", Path: "/geocode",
		Query: map[string]any{"q": "berlin"},
	})
	if err != nil {
		t.Fatalf("Call returned error for 500, want Response: %v", err)
	}
	if resp.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.Status)
	}
	if !strings.Contains(resp.Body, "upstream boom") {
		t.Errorf("body = %q, want passthrough", resp.Body)
	}
	if gotQuery != "berlin" {
		t.Errorf("upstream query q = %q, want berlin", gotQuery)
	}
}

func TestCall_ScrubsTokenInResponseBody(t *testing.T) {
	const token = "supersecret-tok"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Upstream/gateway accidentally echoes the internal key into the body.
		_, _ = w.Write([]byte(`{"echoed_apikey":"` + token + `"}`))
	}))
	defer srv.Close()
	reg := apigateway.Registry{"maps": {BasePath: "/maps"}}
	c := newClientForServer(t, srv, reg, token)

	resp, err := c.Call(context.Background(), apigateway.Request{Provider: "maps", Method: "GET"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.Contains(resp.Body, token) {
		t.Errorf("body still leaks token: %q", resp.Body)
	}
	if !strings.Contains(resp.Body, "[redacted]") {
		t.Errorf("body = %q, want [redacted]", resp.Body)
	}
}

func TestCall_CrossHostRedirectNotFollowed(t *testing.T) {
	// The redirect gate keys on Hostname() (port-agnostic), so the refusal only
	// triggers for a genuinely different host. If the client followed it, it
	// would dial evil.example.com; instead the returned response is the 3xx
	// itself (CheckRedirect → http.ErrUseLastResponse).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://evil.example.com/exfil")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte("redirecting"))
	}))
	defer srv.Close()
	reg := apigateway.Registry{"maps": {BasePath: "/maps"}}
	c := newClientForServer(t, srv, reg, "tok123")

	resp, err := c.Call(context.Background(), apigateway.Request{Provider: "maps", Method: "GET"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Status != http.StatusFound {
		t.Errorf("status = %d, want 302 (redirect returned, not followed)", resp.Status)
	}
	if !strings.Contains(resp.Body, "redirecting") {
		t.Errorf("body = %q, want the 3xx body, not the redirected target", resp.Body)
	}
}

func TestCall_InvalidMethodScrubbed(t *testing.T) {
	// An invalid HTTP method token makes http.NewRequestWithContext fail, which
	// drives the scrubErr path. WritesEnabled + AllowedMethods let it past policy.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("must not reach gateway when request construction fails")
	}))
	defer srv.Close()
	reg := apigateway.Registry{"maps": {BasePath: "/maps", AllowedMethods: []string{"BAD METHOD"}, WritesEnabled: true}}
	c := newClientForServer(t, srv, reg, "tok123")

	_, err := c.Call(context.Background(), apigateway.Request{Provider: "maps", Method: "BAD METHOD"})
	if !errors.Is(err, apigateway.ErrGatewayRequest) {
		t.Fatalf("err = %v, want ErrGatewayRequest", err)
	}
	if strings.Contains(err.Error(), "BAD METHOD") {
		t.Fatalf("request-construction error leaked attacker-controlled input: %v", err)
	}
}
