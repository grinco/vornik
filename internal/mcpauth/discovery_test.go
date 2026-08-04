package mcpauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The design's §10 asks for table-driven discovery tests over the 18 surveyed vendors. These
// reproduce the response SHAPES that differ — which is what the survey actually found (§2.1's
// last three columns are identical for 16 of 18) — rather than checking in 18 literal bodies
// that would add bulk without adding a code path. Each case names the finding it covers.

// prmBody is a minimal RFC 9728 protected-resource-metadata document.
func prmBody(resource, asURL string, scopes ...string) string {
	b, _ := json.Marshal(map[string]any{
		"resource":              resource,
		"authorization_servers": []string{asURL},
		"scopes_supported":      scopes,
	})
	return string(b)
}

// asBody is a minimal RFC 8414 authorization-server-metadata document.
func asBody(issuer string, withDCR bool) string {
	doc := map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/authorize",
		"token_endpoint":                        issuer + "/token",
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post"},
	}
	if withDCR {
		doc["registration_endpoint"] = issuer + "/register"
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// asServer serves RFC 8414 metadata at both well-known paths.
func asServer(t *testing.T, withDCR bool) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server", "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(asBody(srv.URL, withDCR)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDiscover_ChallengeCarriesResourceMetadata is the happy path 16 of 18 vendors take.
func TestDiscover_ChallengeCarriesResourceMetadata(t *testing.T) {
	as := asServer(t, true)

	var mcp *httptest.Server
	mcp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource/v1/mcp" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(prmBody(mcp.URL+"/v1/mcp", as.URL, "read:jira-work")))
			return
		}
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="mcp", resource_metadata="`+mcp.URL+`/.well-known/oauth-protected-resource/v1/mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer mcp.Close()

	md, err := Discover(context.Background(), mcp.Client(), mcp.URL+"/v1/mcp")
	require.NoError(t, err)
	assert.Equal(t, mcp.URL+"/v1/mcp", md.Resource)
	assert.Equal(t, as.URL+"/authorize", md.AuthorizationEndpoint)
	assert.Equal(t, as.URL+"/token", md.TokenEndpoint)
	assert.Equal(t, as.URL+"/register", md.RegistrationEndpoint)
	assert.Equal(t, []string{"read:jira-work"}, md.ScopesSupported)
	assert.True(t, md.SupportsDCR())
}

// TestDiscover_F2_ChallengeOmitsResourceMetadata — Stripe and Zapier 401 without the
// resource_metadata parameter yet serve a valid PRM at the RFC 9728 well-known path. Trusting
// the challenge alone would declare them undiscoverable.
func TestDiscover_F2_ChallengeOmitsResourceMetadata(t *testing.T) {
	as := asServer(t, true)

	var mcp *httptest.Server
	mcp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource/mcp" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(prmBody(mcp.URL+"/mcp", as.URL)))
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`) // no resource_metadata
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer mcp.Close()

	md, err := Discover(context.Background(), mcp.Client(), mcp.URL+"/mcp")
	require.NoError(t, err)
	assert.Equal(t, mcp.URL+"/mcp", md.Resource)
	assert.Equal(t, as.URL+"/token", md.TokenEndpoint)
}

// TestDiscover_F2_FallsBackToHostRoot — the path-scoped well-known probe misses; the host-root
// one is the last automatic hop before declaring failure.
func TestDiscover_F2_FallsBackToHostRoot(t *testing.T) {
	as := asServer(t, false)

	var mcp *httptest.Server
	mcp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(prmBody(mcp.URL+"/deep/path/mcp", as.URL)))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/.well-known/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer mcp.Close()

	md, err := Discover(context.Background(), mcp.Client(), mcp.URL+"/deep/path/mcp")
	require.NoError(t, err)
	assert.Equal(t, mcp.URL+"/deep/path/mcp", md.Resource)
	assert.False(t, md.SupportsDCR(), "this AS advertises no registration_endpoint")
}

// TestDiscover_F4_NoPRMAnywhere — Intercom 401s with realm="OAuth" and serves no PRM at any
// well-known path. The error must be the one that tells an operator to supply endpoints
// manually, not a generic failure.
func TestDiscover_F4_NoPRMAnywhere(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/.well-known/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="OAuth"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer mcp.Close()

	_, err := Discover(context.Background(), mcp.Client(), mcp.URL+"/mcp")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoDiscovery), "want ErrNoDiscovery, got %v", err)
	assert.Contains(t, err.Error(), "manually")
}

// TestDiscover_F3_WAFBlockIsNotAnAuthFailure — a blocked User-Agent presents as 403 with no
// challenge, which reads as a permissions bug and is not one. The whole diagnostic value of F3
// is that these two are told apart.
func TestDiscover_F3_WAFBlockIsNotAnAuthFailure(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden) // no WWW-Authenticate
	}))
	defer mcp.Close()

	_, err := Discover(context.Background(), mcp.Client(), mcp.URL+"/mcp")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrServerRefused), "want ErrServerRefused, got %v", err)
	// The two outcomes an operator must not confuse: this is neither a
	// discovery gap (supply endpoints manually) nor a credential problem
	// (fix scopes), and the message has to say which it is.
	assert.False(t, errors.Is(err, ErrNoDiscovery))
	assert.Contains(t, err.Error(), "not an auth failure")
	assert.Contains(t, err.Error(), "403")
}

// TestDiscover_PublicServerNeedsNoAuth — Cloudflare's docs server answers 200. Discovery must
// say "this server is not protected" rather than inventing an endpoint.
func TestDiscover_PublicServerNeedsNoAuth(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer mcp.Close()

	_, err := Discover(context.Background(), mcp.Client(), mcp.URL+"/mcp")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotProtected), "want ErrNotProtected, got %v", err)
}

// TestDiscover_SendsTheVornikUserAgent — F3's methodology note: probe with the UA the
// production client will actually send, or the survey measures the wrong thing.
func TestDiscover_SendsTheVornikUserAgent(t *testing.T) {
	var seen []string
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusForbidden)
	}))
	defer mcp.Close()

	_, _ = Discover(context.Background(), mcp.Client(), mcp.URL+"/mcp")
	require.NotEmpty(t, seen)
	for _, ua := range seen {
		assert.True(t, strings.HasPrefix(ua, "Vornik/"), "User-Agent = %q", ua)
	}
}

func TestParseChallenge(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantMeta string
		wantOK   bool
	}{
		{`Bearer realm="mcp", resource_metadata="https://x/.well-known/oauth-protected-resource"`,
			"https://x/.well-known/oauth-protected-resource", true},
		{`Bearer resource_metadata="https://x/prm", error="invalid_token"`, "https://x/prm", true},
		// Unquoted parameter values are legal per RFC 7235's token production.
		{`Bearer resource_metadata=https://x/prm`, "https://x/prm", true},
		{`Bearer realm="OAuth"`, "", true},
		{`Basic realm="x"`, "", false},
		{"", "", false},
	} {
		meta, ok := parseChallenge(tc.in)
		assert.Equal(t, tc.wantOK, ok, "parseChallenge(%q) ok", tc.in)
		assert.Equal(t, tc.wantMeta, meta, "parseChallenge(%q) metadata", tc.in)
	}
}

// TestWellKnownProbePaths pins the RFC 9728 path construction, including the insertion point:
// the well-known segment goes after the HOST, with the resource path appended — not appended to
// the resource path itself.
func TestWellKnownProbePaths(t *testing.T) {
	got := wellKnownProbeURLs("https://mcp.example.com/v1/mcp/authv2")
	assert.Equal(t, []string{
		"https://mcp.example.com/.well-known/oauth-protected-resource/v1/mcp/authv2",
		"https://mcp.example.com/.well-known/oauth-protected-resource",
	}, got)

	// A root-path resource must not produce a doubled slash.
	assert.Equal(t, []string{
		"https://mcp.example.com/.well-known/oauth-protected-resource",
	}, wellKnownProbeURLs("https://mcp.example.com/"))
}
