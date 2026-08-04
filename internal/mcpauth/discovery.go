package mcpauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"vornik.io/vornik/internal/version"
)

// Discovery of an MCP server's authorization requirements: the RFC 9728 protected-resource
// metadata → RFC 8414 authorization-server metadata chain, plus the fallbacks the live survey
// (design §2) found necessary.
//
// Plain net/http against documented JSON shapes rather than a library: the whole of the value
// here is in the FALLBACKS (F2's well-known probe, F4's absent PRM, F3's 403-without-challenge),
// and a library that abstracts the challenge away would hide exactly those.

var (
	// ErrNoDiscovery reports that the server publishes no protected-resource metadata at any
	// well-known path (F4 — Intercom). Endpoints must be supplied manually.
	ErrNoDiscovery = errors.New("mcp server does not support OAuth discovery")

	// ErrServerRefused reports a refusal that is NOT an authorization failure: a 403 with no
	// WWW-Authenticate challenge, which is how a vendor WAF presents (F3). Distinguished
	// because it reads as a permissions bug and is not one — retrying with a token will
	// never help, and the fix is at the vendor's edge.
	ErrServerRefused = errors.New("mcp server refused the connection (not an auth failure)")

	// ErrNotProtected reports that the server answered the unauthenticated probe normally,
	// so it needs no credentials at all (Cloudflare's docs server).
	ErrNotProtected = errors.New("mcp server is not access-protected")
)

// Metadata is the resolved authorization configuration for one MCP server.
type Metadata struct {
	// Resource is the RFC 8707 canonical resource URI, taken from the PRM rather than
	// assumed to equal the server URL. F5: it is the audience the token is issued for, and
	// two products sharing an authorization server have distinct ones.
	Resource string

	AuthorizationEndpoint string
	TokenEndpoint         string
	// RegistrationEndpoint is empty when the authorization server offers no dynamic client
	// registration (F1 — Slack, GitHub, Box), in which case a client_id must be configured.
	RegistrationEndpoint string

	// ScopesSupported is the PRM's advertised set, used when config names no scopes.
	ScopesSupported []string

	// TokenEndpointAuthMethods is the AS's advertised set. Slack accepts only
	// client_secret_post, i.e. a confidential client — a public-client PKCE-only
	// implementation cannot talk to it (F1).
	TokenEndpointAuthMethods []string
}

// SupportsDCR reports whether the authorization server accepts dynamic client registration.
func (m Metadata) SupportsDCR() bool { return m.RegistrationEndpoint != "" }

// RequiresClientSecret reports whether the authorization server refuses public clients — it
// advertises auth methods and "none" is not among them.
func (m Metadata) RequiresClientSecret() bool {
	if len(m.TokenEndpointAuthMethods) == 0 {
		return false
	}
	for _, m := range m.TokenEndpointAuthMethods {
		if m == "none" {
			return false
		}
	}
	return true
}

// prmDocument is the RFC 9728 protected-resource metadata shape.
type prmDocument struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// asDocument is the RFC 8414 authorization-server metadata shape.
type asDocument struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
}

// Discover resolves how to authenticate to the MCP server at serverURL.
//
// The chain, in order, is the one the survey proved necessary:
//  1. POST an unauthenticated `initialize` and read the 401's WWW-Authenticate challenge.
//  2. If the challenge names `resource_metadata`, fetch that.
//  3. Otherwise probe the RFC 9728 well-known paths — path-scoped, then host root (F2).
//  4. Fetch the authorization server's RFC 8414 metadata, trying the OAuth well-known path
//     then the OpenID Connect one, since some servers publish only the latter.
//
// A 403 with no challenge is ErrServerRefused (F3), a normal answer is ErrNotProtected, and
// exhausting the chain is ErrNoDiscovery — three distinct outcomes because the operator's next
// action differs for each.
func Discover(ctx context.Context, client *http.Client, serverURL string) (Metadata, error) {
	if client == nil {
		client = http.DefaultClient
	}

	challenge, status, err := probeUnauthenticated(ctx, client, serverURL)
	if err != nil {
		return Metadata{}, err
	}
	switch {
	case status == http.StatusForbidden && challenge == "":
		return Metadata{}, fmt.Errorf("%w: the server answered 403 with no WWW-Authenticate challenge, which is how a vendor WAF presents rather than how an authorization failure does", ErrServerRefused)
	case status < 400:
		return Metadata{}, fmt.Errorf("%w: it answered an unauthenticated request normally, so no auth block is needed", ErrNotProtected)
	}

	var prmURLs []string
	if meta, ok := parseChallenge(challenge); ok && meta != "" {
		prmURLs = append(prmURLs, meta)
	}
	// F2: the challenge cannot be trusted to carry resource_metadata, so the well-known
	// probes are always appended rather than used only when the challenge is absent.
	prmURLs = append(prmURLs, wellKnownProbeURLs(serverURL)...)

	var prm *prmDocument
	for _, u := range prmURLs {
		doc, err := fetchJSON[prmDocument](ctx, client, u)
		if err != nil || doc == nil || len(doc.AuthorizationServers) == 0 {
			continue
		}
		prm = doc
		break
	}
	if prm == nil {
		return Metadata{}, fmt.Errorf("%w: no protected-resource metadata at the challenge URL or any well-known path — supply authorization_endpoint and token_endpoint manually", ErrNoDiscovery)
	}

	resource := prm.Resource
	if resource == "" {
		// The spec requires it; a server that omits it gets the server URL, which is
		// what the resource indicator would have been anyway.
		resource = serverURL
	}

	as, err := fetchASMetadata(ctx, client, prm.AuthorizationServers)
	if err != nil {
		return Metadata{}, err
	}
	if as.AuthorizationEndpoint == "" || as.TokenEndpoint == "" {
		return Metadata{}, fmt.Errorf("%w: the authorization server's metadata names no authorization_endpoint/token_endpoint pair", ErrNoDiscovery)
	}

	scopes := prm.ScopesSupported
	if len(scopes) == 0 {
		scopes = as.ScopesSupported
	}
	return Metadata{
		Resource:                 resource,
		AuthorizationEndpoint:    as.AuthorizationEndpoint,
		TokenEndpoint:            as.TokenEndpoint,
		RegistrationEndpoint:     as.RegistrationEndpoint,
		ScopesSupported:          scopes,
		TokenEndpointAuthMethods: as.TokenEndpointAuthMethodsSupported,
	}, nil
}

// probeUnauthenticated POSTs an `initialize` with no token and returns the challenge header and
// status. The body matters only in that it must be a plausible JSON-RPC request — several
// vendors reject a malformed one before the auth layer, which would make every server look
// undiscoverable.
func probeUnauthenticated(ctx context.Context, client *http.Client, serverURL string) (challenge string, status int, err error) {
	const body = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"vornik","version":"1"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, strings.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("mcpauth: build discovery probe: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2024-11-05")
	setUserAgent(req)

	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("mcpauth: discovery probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	return resp.Header.Get("WWW-Authenticate"), resp.StatusCode, nil
}

// parseChallenge reports whether the header carries a Bearer challenge, and the
// resource_metadata URL when present. A Bearer challenge with no resource_metadata returns
// ("", true) — that is F2's case, and it is materially different from "not a Bearer challenge".
func parseChallenge(header string) (resourceMetadata string, isBearer bool) {
	h := strings.TrimSpace(header)
	if h == "" || !strings.HasPrefix(strings.ToLower(h), "bearer") {
		return "", false
	}
	rest := strings.TrimSpace(h[len("bearer"):])
	for _, part := range splitChallengeParams(rest) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), "resource_metadata") {
			return strings.Trim(strings.TrimSpace(v), `"`), true
		}
	}
	return "", true
}

// splitChallengeParams splits on commas that are not inside a quoted string.
func splitChallengeParams(s string) []string {
	var (
		out    []string
		cur    strings.Builder
		inQuot bool
	)
	for _, r := range s {
		switch {
		case r == '"':
			inQuot = !inQuot
			cur.WriteRune(r)
		case r == ',' && !inQuot:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, strings.TrimSpace(cur.String()))
	}
	return out
}

// wellKnownProbeURLs builds the RFC 9728 candidates for a resource URL: the well-known segment
// goes after the HOST with the resource path appended, then the bare host-root form. Appending
// the well-known segment to the resource path instead is the easy mistake, and it silently
// misses every vendor that uses the spec layout.
func wellKnownProbeURLs(serverURL string) []string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil
	}
	base := u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource"
	path := strings.TrimSuffix(u.Path, "/")
	if path == "" || path == "/" {
		return []string{base}
	}
	return []string{base + path, base}
}

// fetchASMetadata tries each advertised authorization server, and for each one the OAuth
// well-known path before the OpenID Connect one — some servers publish only the latter.
func fetchASMetadata(ctx context.Context, client *http.Client, servers []string) (*asDocument, error) {
	var lastErr error
	for _, issuer := range servers {
		issuer = strings.TrimSuffix(issuer, "/")
		for _, suffix := range []string{
			"/.well-known/oauth-authorization-server",
			"/.well-known/openid-configuration",
		} {
			doc, err := fetchJSON[asDocument](ctx, client, issuer+suffix)
			if err != nil {
				lastErr = err
				continue
			}
			if doc != nil && doc.TokenEndpoint != "" {
				return doc, nil
			}
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("%w: could not read authorization-server metadata: %v", ErrNoDiscovery, lastErr)
	}
	return nil, fmt.Errorf("%w: the protected-resource metadata names no reachable authorization server", ErrNoDiscovery)
}

// fetchJSON GETs a JSON document. Bounded read: these are small metadata documents, and an
// unbounded read from an untrusted host is an easy denial of service.
func fetchJSON[T any](ctx context.Context, client *http.Client, rawURL string) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	setUserAgent(req)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, err
	}
	var doc T
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("GET %s: %w", rawURL, err)
	}
	return &doc, nil
}

// setUserAgent stamps Vornik's identity on every outbound discovery / OAuth request. F3's
// methodology note is the reason this is not left to Go's default: the survey was first run with
// a library UA that six vendors WAF-block, which produced eight spurious 403s and one false
// general conclusion. Probe with the UA the production client actually sends.
func setUserAgent(req *http.Request) {
	req.Header.Set("User-Agent", version.UserAgent(buildVersion()))
}
