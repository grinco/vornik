// Package gateway holds the concrete DialGuard-pinned Client implementation
// for the API gateway. It lives apart from package apigateway (the interface,
// DTOs, registry, and sentinels) because it imports integrations.DialGuard,
// which transitively depends on internal/dispatcher — keeping the concrete
// client here lets dispatcher import the integrations-free apigateway package
// for the query_api tool without forming an import cycle. See
// https://docs.vornik.io
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/integrations"
)

// Client is the concrete apigateway.Client. Its http.Client is built ONCE via
// DialGuard pinned to the gateway host — there is no other client-construction
// path, so a crafted provider/path cannot redirect egress (design §5, C2).
type Client struct {
	base  *url.URL
	token string
	reg   apigateway.Registry
	httpc *http.Client
}

// Compile-time capability checks (Task 3): Client stays a Call-only
// apigateway.Client (no breaking change for existing fakes) while also
// satisfying the optional apigateway.ProviderLister discovery capability.
var (
	_ apigateway.Client         = (*Client)(nil)
	_ apigateway.ProviderLister = (*Client)(nil)
)

// New builds the pinned client. baseURL is the local gateway URL.
func New(baseURL, token string, reg apigateway.Registry, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("gateway base url: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("gateway base url has no host: %q", baseURL)
	}
	// DialGuard blocks loopback/private by default; the gateway is local, so the
	// gateway host is the single explicit allowlist entry (design §5, C2).
	guard := integrations.DialGuard{AllowedHosts: []string{u.Hostname()}}
	hc := guard.HTTPClient(timeout)
	// Refuse cross-host redirects: a 3xx Location to another host is never
	// followed (design §5 / §3.2 redirect-echo defense).
	base := *u
	hc.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if req.URL.Hostname() != base.Hostname() {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &Client{base: u, token: token, reg: reg, httpc: hc}, nil
}

// scrub removes the internal token from any string surfaced to the caller.
func (c *Client) scrub(s string) string {
	if len(c.token) >= 4 {
		s = strings.ReplaceAll(s, c.token, "[redacted]")
	}
	return s
}

// Call validates the provider + method policy BEFORE any network call, then
// issues the request to the pinned gateway with the internal apikey header.
// Gateway 401 → ErrGatewayAuth; 404/405 → ErrUpstreamMethod (design §3.2, §5).
func (c *Client) Call(ctx context.Context, req apigateway.Request) (apigateway.Response, error) {
	prov, ok := c.reg.Lookup(req.Provider)
	if !ok {
		return apigateway.Response{}, apigateway.ErrUnknownProvider
	}
	if err := apigateway.MethodAllowed(prov, req.Method); err != nil {
		return apigateway.Response{}, err
	}

	// review F2: reject any ".." path segment before composing the URL — a
	// traversal attempt must never reach the network. The daemon registry is
	// the conservative pre-filter here; the gateway route allowlist stays
	// authoritative (design §5, C2). Segment inspection (not just a substring
	// check) so a literal ".." dir escapes while filenames like "..foo" pass.
	for _, seg := range strings.Split(req.Path, "/") {
		if seg == ".." {
			return apigateway.Response{}, apigateway.ErrInvalidPath
		}
	}

	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + prov.BasePath + "/" + strings.TrimLeft(req.Path, "/")
	if len(req.Query) > 0 {
		q := u.Query()
		for k, v := range req.Query {
			q.Set(k, fmt.Sprintf("%v", v))
		}
		u.RawQuery = q.Encode()
	}

	var bodyReader io.Reader
	if len(req.Body) > 0 {
		b, err := json.Marshal(req.Body)
		if err != nil {
			return apigateway.Response{}, fmt.Errorf("encode body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	httpReq, err := http.NewRequestWithContext(ctx, strings.ToUpper(req.Method), u.String(), bodyReader)
	if err != nil {
		return apigateway.Response{}, apigateway.ErrGatewayRequest
	}
	httpReq.Header.Set("apikey", c.token) // internal daemon↔gateway key-auth
	if bodyReader != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpc.Do(httpReq)
	if err != nil {
		return apigateway.Response{}, apigateway.ErrGatewayRequest
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return apigateway.Response{}, apigateway.ErrGatewayAuth
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return apigateway.Response{}, apigateway.ErrUpstreamMethod
	}
	return apigateway.Response{Status: resp.StatusCode, Body: c.scrub(string(body))}, nil
}

// ListProviders implements the optional apigateway.ProviderLister capability
// (design §5.2): the list_apis dispatcher tool type-asserts the client to
// this interface to discover the registered, non-secret provider catalog.
func (c *Client) ListProviders() []apigateway.ProviderInfo {
	return c.reg.Describe()
}
