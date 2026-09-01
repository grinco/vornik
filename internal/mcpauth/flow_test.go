package mcpauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPKCE_S256IsCorrect(t *testing.T) {
	p, err := NewPKCE()
	require.NoError(t, err)
	assert.NotEmpty(t, p.Verifier)

	// The challenge must be BASE64URL(SHA256(verifier)) with no padding — an
	// off-by-one here fails at the vendor with an opaque invalid_grant.
	sum := sha256.Sum256([]byte(p.Verifier))
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), p.Challenge)
	assert.NotContains(t, p.Challenge, "=")

	other, err := NewPKCE()
	require.NoError(t, err)
	assert.NotEqual(t, p.Verifier, other.Verifier, "each attempt needs a fresh proof key")
}

func TestNewState_IsUnguessableAndUnique(t *testing.T) {
	a, err := NewState()
	require.NoError(t, err)
	b, err := NewState()
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
	assert.GreaterOrEqual(t, len(a), 32)
}

// TestAuthorizationURL_CarriesResourceAndPKCE — F5: `resource` is load-bearing, not ceremonial.
func TestAuthorizationURL_CarriesResourceAndPKCE(t *testing.T) {
	md := Metadata{
		AuthorizationEndpoint: "https://auth.atlassian.com/authorize?audience=api",
		Resource:              "https://mcp.atlassian.com/v1/mcp/authv2",
	}
	pkce := PKCE{Verifier: "v", Challenge: "c"}
	raw, err := AuthorizationURL(md, ClientCredentials{ID: "cid"},
		"https://vornik.example.com/ui/mcp/oauth/callback",
		[]string{"read:jira-work", "offline_access"}, "state-1", pkce)
	require.NoError(t, err)

	u, err := url.Parse(raw)
	require.NoError(t, err)
	q := u.Query()
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "cid", q.Get("client_id"))
	assert.Equal(t, "https://vornik.example.com/ui/mcp/oauth/callback", q.Get("redirect_uri"))
	assert.Equal(t, "state-1", q.Get("state"))
	assert.Equal(t, "c", q.Get("code_challenge"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.Equal(t, "https://mcp.atlassian.com/v1/mcp/authv2", q.Get("resource"))
	assert.Equal(t, "read:jira-work offline_access", q.Get("scope"))
	// A query already on the endpoint must survive — Atlassian's carries audience=api.
	assert.Equal(t, "api", q.Get("audience"))
}

// tokenServer captures the form of the last token request and replies with a token.
func tokenServer(t *testing.T, reply map[string]any, status int) (*httptest.Server, *url.Values) {
	t.Helper()
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		got = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if reply != nil {
			_ = writeJSON(w, reply)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// TestExchangeCode_SendsResourceAndVerifier — the token half of F5, plus PKCE completion.
func TestExchangeCode_SendsResourceAndVerifier(t *testing.T) {
	srv, form := tokenServer(t, map[string]any{
		"access_token":  "at-1",
		"refresh_token": "rt-1",
		"expires_in":    3600,
		"scope":         "read:jira-work",
		"token_type":    "Bearer",
	}, http.StatusOK)

	md := Metadata{TokenEndpoint: srv.URL, Resource: "https://res/mcp"}
	tr, err := ExchangeCode(context.Background(), srv.Client(), md,
		ClientCredentials{ID: "cid"}, "https://cb", "the-code", PKCE{Verifier: "ver-1"})
	require.NoError(t, err)

	assert.Equal(t, "at-1", tr.AccessToken)
	assert.Equal(t, "rt-1", tr.RefreshToken)
	assert.Equal(t, "read:jira-work", tr.Scopes)
	require.NotNil(t, tr.ExpiresAt)

	assert.Equal(t, "authorization_code", form.Get("grant_type"))
	assert.Equal(t, "the-code", form.Get("code"))
	assert.Equal(t, "ver-1", form.Get("code_verifier"))
	assert.Equal(t, "https://res/mcp", form.Get("resource"), "F5: resource must ride the TOKEN request too")
	// A public client must not send an empty client_secret — some servers
	// reject the parameter's mere presence.
	assert.False(t, form.Has("client_secret"))
}

// TestExchangeCode_ConfidentialClientUsesSecretPost — F1: Slack's AS accepts only
// client_secret_post, so a public-client-only implementation cannot talk to it.
func TestExchangeCode_ConfidentialClientUsesSecretPost(t *testing.T) {
	srv, form := tokenServer(t, map[string]any{"access_token": "at", "token_type": "Bearer"}, http.StatusOK)

	_, err := ExchangeCode(context.Background(), srv.Client(),
		Metadata{TokenEndpoint: srv.URL, Resource: "https://res"},
		ClientCredentials{ID: "cid", Secret: "shh"}, "https://cb", "code", PKCE{Verifier: "v"})
	require.NoError(t, err)
	assert.Equal(t, "shh", form.Get("client_secret"))
}

func TestRefresh_SendsResourceAndRefreshToken(t *testing.T) {
	srv, form := tokenServer(t, map[string]any{
		"access_token":  "at-2",
		"refresh_token": "rt-2",
		"expires_in":    60,
	}, http.StatusOK)

	tr, err := Refresh(context.Background(), srv.Client(),
		Metadata{TokenEndpoint: srv.URL, Resource: "https://res"},
		ClientCredentials{ID: "cid"}, "rt-1")
	require.NoError(t, err)
	assert.Equal(t, "at-2", tr.AccessToken)
	assert.Equal(t, "rt-2", tr.RefreshToken)
	assert.Equal(t, "refresh_token", form.Get("grant_type"))
	assert.Equal(t, "rt-1", form.Get("refresh_token"))
	assert.Equal(t, "https://res", form.Get("resource"))
}

// Authorization codes, refresh tokens and confidential-client secrets are
// request-body credentials. net/http follows 307/308 redirects by replaying
// that body, so the OAuth client must reject redirects rather than forward a
// grant to an origin selected by the token endpoint.
func TestExchangeCode_DoesNotForwardCredentialsAcrossRedirect(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		_ = r.ParseForm()
		if r.PostForm.Get("code") == "secret-code" || r.PostForm.Get("client_secret") == "secret-client" {
			t.Error("redirect target received OAuth credentials")
		}
		_ = writeJSON(w, map[string]any{"access_token": "stolen"})
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	_, err := ExchangeCode(context.Background(), source.Client(),
		Metadata{TokenEndpoint: source.URL},
		ClientCredentials{ID: "cid", Secret: "secret-client"},
		"https://cb", "secret-code", PKCE{Verifier: "verifier"})
	require.Error(t, err)
	assert.Zero(t, targetHits.Load(), "OAuth credentials must never follow a redirect")
}

// TestRefresh_InvalidGrantIsDistinguished — the one error where retrying is guaranteed useless.
func TestRefresh_InvalidGrantIsDistinguished(t *testing.T) {
	srv, _ := tokenServer(t, map[string]any{
		"error":             "invalid_grant",
		"error_description": "refresh token revoked",
	}, http.StatusBadRequest)

	_, err := Refresh(context.Background(), srv.Client(),
		Metadata{TokenEndpoint: srv.URL}, ClientCredentials{ID: "cid"}, "rt-dead")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidGrant), "want ErrInvalidGrant, got %v", err)
}

// TestPostToken_ErrorNeverEchoesTheRawBody — some authorization servers echo request parameters
// in an error, which on a token request means echoing the client secret or refresh token. The
// summary is bounded and structured for exactly that reason.
func TestPostToken_ErrorNeverEchoesTheRawBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"bad param","echoed_client_secret":"SUPER-SECRET"}`))
	}))
	defer srv.Close()

	_, err := ExchangeCode(context.Background(), srv.Client(),
		Metadata{TokenEndpoint: srv.URL}, ClientCredentials{ID: "cid", Secret: "SUPER-SECRET"},
		"https://cb", "code", PKCE{Verifier: "v"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SUPER-SECRET")
	assert.Contains(t, err.Error(), "invalid_request")
}

// TestRegister_NoDCRIsANamedError — F1: three of the largest surveyed vendors advertise no
// registration endpoint, and the operator's next action is to enter a client_id.
func TestRegister_NoDCRIsANamedError(t *testing.T) {
	_, err := Register(context.Background(), nil, Metadata{}, "https://cb", nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoDCR), "want ErrNoDCR, got %v", err)
	assert.Contains(t, err.Error(), "client_id")
}

func TestRegister_RequestShapeAndConfidentialClient(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, readJSON(r, &body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = writeJSON(w, map[string]any{"client_id": "new-cid", "client_secret": "new-secret"})
	}))
	defer srv.Close()

	md := Metadata{
		RegistrationEndpoint:     srv.URL,
		TokenEndpointAuthMethods: []string{"client_secret_post"}, // no "none" => confidential
	}
	creds, err := Register(context.Background(), srv.Client(), md,
		"https://vornik.example.com/ui/mcp/oauth/callback", []string{"read"})
	require.NoError(t, err)
	assert.Equal(t, "new-cid", creds.ID)
	assert.Equal(t, "new-secret", creds.Secret)
	assert.True(t, creds.Confidential())

	assert.Equal(t, "client_secret_post", body["token_endpoint_auth_method"],
		"an AS that does not advertise `none` must be registered as a confidential client")
	// The registration PINS the redirect URI, which is why one client is
	// shared per (deployment, server) — §7.2a.
	assert.Equal(t, []any{"https://vornik.example.com/ui/mcp/oauth/callback"}, body["redirect_uris"])
	assert.Equal(t, []any{"authorization_code", "refresh_token"}, body["grant_types"])
}

func TestRegister_PublicClientWhenNoneIsAdvertised(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, readJSON(r, &body))
		w.WriteHeader(http.StatusCreated)
		_ = writeJSON(w, map[string]any{"client_id": "pub-cid"})
	}))
	defer srv.Close()

	creds, err := Register(context.Background(), srv.Client(), Metadata{
		RegistrationEndpoint:     srv.URL,
		TokenEndpointAuthMethods: []string{"none", "client_secret_post"},
	}, "https://cb", nil)
	require.NoError(t, err)
	assert.False(t, creds.Confidential())
	assert.Equal(t, "none", body["token_endpoint_auth_method"])
}

func TestAuthorizationURL_RejectsUnsafeDiscoveredEndpoint(t *testing.T) {
	_, err := AuthorizationURL(Metadata{
		AuthorizationEndpoint: "http://authorization.example/authorize",
	}, ClientCredentials{ID: "cid"}, "https://cb", nil, "state", PKCE{Challenge: "challenge"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https")
}

func TestMetadata_RequiresClientSecret(t *testing.T) {
	assert.False(t, Metadata{}.RequiresClientSecret(),
		"a server that advertises nothing must not be assumed confidential")
	assert.False(t, Metadata{TokenEndpointAuthMethods: []string{"none"}}.RequiresClientSecret())
	assert.True(t, Metadata{TokenEndpointAuthMethods: []string{"client_secret_post"}}.RequiresClientSecret())
}
