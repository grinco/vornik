package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/persistence"
)

type stubConnector struct {
	ref       mcpconnect.ServerRef
	refOK     bool
	begun     mcpconnect.BeginResult
	beginErr  error
	grant     *persistence.MCPOAuthToken
	discAct   string
	discCalls int
	redirect  string
}

func (s *stubConnector) ResolveServer(string, string) (mcpconnect.ServerRef, bool) {
	return s.ref, s.refOK
}

func (s *stubConnector) Begin(context.Context, mcpconnect.ServerRef, string) (mcpconnect.BeginResult, error) {
	return s.begun, s.beginErr
}

func (s *stubConnector) Disconnect(_ context.Context, _, _, actor string) error {
	s.discAct = actor
	s.discCalls++
	return nil
}

func (s *stubConnector) Grant(context.Context, string, string) (*persistence.MCPOAuthToken, error) {
	return s.grant, nil
}

func (s *stubConnector) RedirectURI() (string, error) { return s.redirect, nil }

// mcpOAuthServer builds a Server with auth DISABLED, which is the single-operator homelab shape
// and lets these tests assert on handler behaviour rather than on the gate. The gate has its own
// tests below.
func mcpOAuthServer(t *testing.T, conn MCPOAuthConnector) *Server {
	t.Helper()
	return NewServer(WithMCPOAuthConnector(conn))
}

func doJSON(t *testing.T, h http.HandlerFunc, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		r = httptest.NewRequest(method, target, strings.NewReader(string(raw)))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	// Auth OFF — the single-operator homelab shape, where every caller is
	// admin. IsAuthEnabledFromContext defaults to TRUE when the key is absent
	// (fail-safe), so it has to be stamped explicitly. The gate itself has its
	// own tests below.
	r = r.WithContext(context.WithValue(r.Context(), authEnabledKey, false))
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func TestMCPOAuthBegin_ReturnsTheAskAndTheRedirectURI(t *testing.T) {
	conn := &stubConnector{
		refOK:    true,
		ref:      mcpconnect.ServerRef{ProjectID: "p1", ServerName: "linear"},
		redirect: "https://v.example.com/auth/mcp/callback",
		begun: mcpconnect.BeginResult{
			AuthorizationURL: "https://auth.example/authorize?x=1",
			Resource:         "https://mcp.example/mcp",
			Scopes:           []string{"read:issues", "offline_access"},
			State:            "st-1",
		},
	}
	s := mcpOAuthServer(t, conn)

	w := doJSON(t, s.MCPOAuthBegin, http.MethodPost, "/api/v1/mcp/oauth/begin",
		map[string]string{"project_id": "p1", "server": "linear"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp mcpOAuthBeginResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "https://auth.example/authorize?x=1", resp.AuthorizationURL)
	assert.Equal(t, "https://mcp.example/mcp", resp.Resource)
	assert.Equal(t, []string{"read:issues", "offline_access"}, resp.Scopes)
	assert.Equal(t, "https://v.example.com/auth/mcp/callback", resp.RedirectURI)

	// The state must NOT leave the daemon: it is the CSRF binding for a flow
	// the daemon's own callback completes, and the CLI has no use for it.
	assert.NotContains(t, w.Body.String(), "st-1")
}

func TestMCPOAuthBegin_UnknownServerIs404(t *testing.T) {
	s := mcpOAuthServer(t, &stubConnector{refOK: false})
	w := doJSON(t, s.MCPOAuthBegin, http.MethodPost, "/api/v1/mcp/oauth/begin",
		map[string]string{"server": "nope"})
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "MCP_SERVER_NOT_FOUND")
}

// TestMCPOAuthBegin_ErrorsCarryActionableCodes — the CLI branches on these, and each one has a
// different operator action.
func TestMCPOAuthBegin_ErrorsCarryActionableCodes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		status   int
		code     string
		mentions string
	}{
		{"precondition", mcpconnect.ErrNoPublicBaseURL, http.StatusPreconditionFailed,
			"PUBLIC_BASE_URL_REQUIRED", "public_base_url"},
		{"not oauth mode", mcpconnect.ErrNotOAuth, http.StatusBadRequest, "NOT_OAUTH_MODE", "mode: oauth"},
		{"no dcr", mcpauth.ErrNoDCR, http.StatusBadRequest, "CLIENT_REGISTRATION_REQUIRED", "auth.client_id"},
		{"no discovery", mcpauth.ErrNoDiscovery, http.StatusBadRequest, "DISCOVERY_UNSUPPORTED", "manually"},
		{"waf refusal", mcpauth.ErrServerRefused, http.StatusBadGateway, "SERVER_REFUSED", "not an auth failure"},
		{"not protected", mcpauth.ErrNotProtected, http.StatusBadRequest, "SERVER_NOT_PROTECTED", "no auth block"},
		{"secret missing", mcpauth.ErrSecretUnresolved, http.StatusPreconditionFailed, "SECRET_UNRESOLVED", "secret store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mcpOAuthServer(t, &stubConnector{refOK: true, beginErr: tc.err})
			w := doJSON(t, s.MCPOAuthBegin, http.MethodPost, "/api/v1/mcp/oauth/begin",
				map[string]string{"server": "x"})
			assert.Equal(t, tc.status, w.Code)
			assert.Contains(t, w.Body.String(), tc.code)
			assert.Contains(t, w.Body.String(), tc.mentions)
		})
	}
}

// TestMCPOAuthBegin_UnexpectedErrorDoesNotLeakTheVendorMessage — an OAuth error body can echo
// request parameters, which on a token request means the client secret.
func TestMCPOAuthBegin_UnexpectedErrorDoesNotLeakTheVendorMessage(t *testing.T) {
	s := mcpOAuthServer(t, &stubConnector{
		refOK:    true,
		beginErr: errors.New("token endpoint said SUPER-SECRET-VALUE"),
	})
	w := doJSON(t, s.MCPOAuthBegin, http.MethodPost, "/api/v1/mcp/oauth/begin",
		map[string]string{"server": "x"})
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.NotContains(t, w.Body.String(), "SUPER-SECRET-VALUE")
	assert.Contains(t, w.Body.String(), "MCP_OAUTH_FAILED")
}

// TestMCPOAuthStatus_NeverReturnsTheToken — the CLI is a verifier of the recorded grant, never a
// holder of it (§7.2a N1).
func TestMCPOAuthStatus_NeverReturnsTheToken(t *testing.T) {
	exp := time.Now().Add(time.Hour).UTC()
	s := mcpOAuthServer(t, &stubConnector{grant: &persistence.MCPOAuthToken{
		ProjectID: "p1", ServerName: "linear",
		Resource:    "https://mcp.example/mcp",
		AccessToken: "at-SECRET", RefreshToken: "rt-SECRET",
		Scopes: "read:issues offline_access", ConnectedBy: "alice",
		ConnectedAt: time.Now().UTC(), ExpiresAt: &exp,
	}})

	w := doJSON(t, s.MCPOAuthStatus, http.MethodGet,
		"/api/v1/mcp/oauth/status?project_id=p1&server=linear", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	body := w.Body.String()
	assert.NotContains(t, body, "at-SECRET")
	assert.NotContains(t, body, "rt-SECRET")

	var resp mcpOAuthStatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Connected)
	assert.Equal(t, "https://mcp.example/mcp", resp.Resource)
	assert.Equal(t, []string{"read:issues", "offline_access"}, resp.Scopes)
	assert.Equal(t, "alice", resp.ConnectedBy)
	assert.False(t, resp.NeedsReconnect)
	assert.NotEmpty(t, resp.ExpiresAt)
}

func TestMCPOAuthStatus_NotConnected(t *testing.T) {
	s := mcpOAuthServer(t, &stubConnector{grant: nil})
	w := doJSON(t, s.MCPOAuthStatus, http.MethodGet, "/api/v1/mcp/oauth/status?server=x", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var resp mcpOAuthStatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.Connected)
}

func TestMCPOAuthStatus_SurfacesNeedsReconnect(t *testing.T) {
	s := mcpOAuthServer(t, &stubConnector{grant: &persistence.MCPOAuthToken{
		ServerName: "x", NeedsReconnect: true, ConnectedAt: time.Now().UTC(),
	}})
	w := doJSON(t, s.MCPOAuthStatus, http.MethodGet, "/api/v1/mcp/oauth/status?server=x", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var resp mcpOAuthStatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Connected)
	assert.True(t, resp.NeedsReconnect, "a grant needing re-consent must be distinguishable from a healthy one")
}

func TestMCPOAuthDisconnect_RecordsAnActor(t *testing.T) {
	conn := &stubConnector{}
	s := mcpOAuthServer(t, conn)
	w := doJSON(t, s.MCPOAuthDisconnect, http.MethodPost, "/api/v1/mcp/oauth/disconnect",
		map[string]string{"project_id": "p1", "server": "linear"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, conn.discCalls)
	assert.NotEmpty(t, conn.discAct, "the grant record needs an actor")
}

func TestMCPOAuth_MethodAndBodyValidation(t *testing.T) {
	s := mcpOAuthServer(t, &stubConnector{refOK: true})

	assert.Equal(t, http.StatusMethodNotAllowed,
		doJSON(t, s.MCPOAuthBegin, http.MethodGet, "/api/v1/mcp/oauth/begin", nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed,
		doJSON(t, s.MCPOAuthStatus, http.MethodPost, "/api/v1/mcp/oauth/status?server=x", nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed,
		doJSON(t, s.MCPOAuthDisconnect, http.MethodGet, "/api/v1/mcp/oauth/disconnect", nil).Code)

	assert.Equal(t, http.StatusBadRequest,
		doJSON(t, s.MCPOAuthBegin, http.MethodPost, "/api/v1/mcp/oauth/begin", map[string]string{}).Code)
	assert.Equal(t, http.StatusBadRequest,
		doJSON(t, s.MCPOAuthStatus, http.MethodGet, "/api/v1/mcp/oauth/status", nil).Code)
}

// TestMCPOAuth_UnwiredDaemonSaysSo — a deployment with no token store should say that rather than
// 404, which would read as "wrong URL".
func TestMCPOAuth_UnwiredDaemonSaysSo(t *testing.T) {
	s := NewServer()
	w := doJSON(t, s.MCPOAuthBegin, http.MethodPost, "/api/v1/mcp/oauth/begin",
		map[string]string{"server": "x"})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "MCP_OAUTH_UNAVAILABLE")
}

// TestRequireAdminClassGate_IsNotEditionGated is the whole point of the separate gate: MCP
// authentication is a Community feature, and requireAdminGate would answer 501
// EDITION_UNSUPPORTED wherever the admin SURFACE is absent (i.e. on CE).
func TestRequireAdminClassGate_IsNotEditionGated(t *testing.T) {
	s := NewServer(
		WithMCPOAuthConnector(&stubConnector{refOK: false}),
		// Admin ENABLED with an allowlisted key, but adminSurfacePresent
		// stays false because WithAdminSurfacePresent is never called — the
		// Community-Edition shape, where requireAdminGate answers 501.
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"admin-key"}}),
	)
	// adminSurfacePresent is false here — the CE shape. The admin-class gate
	// must still let an authenticated admin through, so the request reaches
	// the handler and fails on its own merits (404, not 501).
	r := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/oauth/begin",
		strings.NewReader(`{"server":"x"}`))
	r = r.WithContext(context.WithValue(
		context.WithValue(r.Context(), apiKeyKey, "admin-key"), authEnabledKey, true))
	w := httptest.NewRecorder()
	s.MCPOAuthBegin(w, r)
	assert.NotEqual(t, http.StatusNotImplemented, w.Code,
		"MCP OAuth must not be edition-gated (design §7.2 CE note)")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestRequireAdminClassGate_RejectsNonAdminKeys keeps the gate a real gate.
func TestRequireAdminClassGate_RejectsNonAdminKeys(t *testing.T) {
	s := NewServer(
		WithMCPOAuthConnector(&stubConnector{refOK: true}),
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"admin-key"}}),
	)

	// Auth enabled + a non-admin key => 403.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/oauth/begin", strings.NewReader(`{"server":"x"}`))
	r = r.WithContext(context.WithValue(
		context.WithValue(r.Context(), apiKeyKey, "ordinary-key"), authEnabledKey, true))
	w := httptest.NewRecorder()
	s.MCPOAuthBegin(w, r)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	// Auth enabled + no key at all => 401.
	r = httptest.NewRequest(http.MethodPost, "/api/v1/mcp/oauth/begin", strings.NewReader(`{"server":"x"}`))
	r = r.WithContext(context.WithValue(r.Context(), authEnabledKey, true))
	w = httptest.NewRecorder()
	s.MCPOAuthBegin(w, r)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Auth enabled + the admin key => through to the handler.
	r = httptest.NewRequest(http.MethodPost, "/api/v1/mcp/oauth/begin", strings.NewReader(`{"server":"x"}`))
	r = r.WithContext(context.WithValue(
		context.WithValue(r.Context(), apiKeyKey, "admin-key"), authEnabledKey, true))
	w = httptest.NewRecorder()
	s.MCPOAuthBegin(w, r)
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// TestMCPOAuthActor_NeverCarriesTheKey — the actor string lands in an audit row.
func TestMCPOAuthActor_NeverCarriesTheKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	r = r.WithContext(context.WithValue(r.Context(), apiKeyKey, "vk_live_supersecretkeyvalue"))
	actor := mcpOAuthActor(r)
	assert.NotContains(t, actor, "supersecret")
	assert.Contains(t, actor, "api-key:")
}
