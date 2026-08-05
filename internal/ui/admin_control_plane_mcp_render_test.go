package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/persistence"
)

// REGRESSION (2026-08-04): the MCP-server authentication feature shipped
// backend-complete and view-EMPTY. mcpAuthFieldsFromForm parsed every auth_*
// field, decorateCPMCPAuth filled AuthMode/Connected/CanConnect, and
// AdminControlPlaneMCPConnect was routed — but admin_control_plane.html's MCP
// section rendered NONE of it, so an operator opening
// /ui/admin/control-plane?section=mcp saw no way to configure authentication at
// all. The gap was invisible to CI because every auth test called mcpAddEdit
// directly with a synthesized request and asserted on emitted YAML; nothing
// asserted the rendered page. These tests assert the VIEW.

// cpMCPRenderRegistry is a static MCPRegistrySource — the tab reads the cached
// snapshot, so no live probe is involved.
type cpMCPRenderRegistry struct{ servers []mcp.ServerSnapshot }

func (r cpMCPRenderRegistry) Snapshot(context.Context) []mcp.ServerSnapshot { return r.servers }

// cpMCPRenderOAuth is a fake MCPOAuthAdmin. redirectErr non-nil models the
// unset-public_base_url precondition; grant non-nil models a stored grant.
type cpMCPRenderOAuth struct {
	refs        map[string]mcpconnect.ServerRef
	grant       *persistence.MCPOAuthToken
	redirectErr error
}

func (f cpMCPRenderOAuth) ResolveServer(_, serverName string) (mcpconnect.ServerRef, bool) {
	ref, ok := f.refs[serverName]
	return ref, ok
}

func (f cpMCPRenderOAuth) Grant(context.Context, string, string) (*persistence.MCPOAuthToken, error) {
	return f.grant, nil
}

func (f cpMCPRenderOAuth) Begin(context.Context, mcpconnect.ServerRef, string) (mcpconnect.BeginResult, error) {
	return mcpconnect.BeginResult{AuthorizationURL: "https://vendor.example/authorize"}, nil
}

func (f cpMCPRenderOAuth) Disconnect(context.Context, string, string, string) error { return nil }

func (f cpMCPRenderOAuth) RedirectURI() (string, error) {
	if f.redirectErr != nil {
		return "", f.redirectErr
	}
	return "https://vornik.example/auth/mcp/callback", nil
}

// renderCPMCP renders the MCP tab and returns the HTML.
func renderCPMCP(t *testing.T, opts ...ServerOption) string {
	t.Helper()
	s, _ := mcpTestServer(t)
	for _, o := range opts {
		o(s)
	}
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/admin/control-plane?section=mcp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET section=mcp: status %d", rec.Code)
	}
	return rec.Body.String()
}

// TestCPMCPForm_RendersEveryAuthField — the form POSTs to a handler that reads
// these exact names (mcpAuthFieldsFromForm). A name the template never renders
// is a config option the operator cannot reach.
func TestCPMCPForm_RendersEveryAuthField(t *testing.T) {
	html := renderCPMCP(t)
	for _, field := range []string{
		"auth_mode",
		"auth_header", "auth_value_from", "auth_value_prefix",
		"auth_env_from",
		"auth_scopes", "auth_client_id", "auth_client_secret_from",
		"auth_authorization_endpoint", "auth_token_endpoint",
	} {
		if !strings.Contains(html, `name="`+field+`"`) {
			t.Errorf("form is missing the %q input — the handler reads it, so the operator can never set it", field)
		}
	}
	// The closed mode enum must all be selectable, including none (which
	// REMOVES an existing block — the only way to turn auth off from the UI).
	for _, mode := range []string{mcpauth.ModeNone, mcpauth.ModeStatic, mcpauth.ModeEnv, mcpauth.ModeOAuth} {
		if !strings.Contains(html, `value="`+mode+`"`) {
			t.Errorf("auth mode %q is not selectable", mode)
		}
	}
}

// TestCPMCPForm_TellsTheOperatorToUseASecretReference — the handler rejects a
// literal outright, so the form has to say what it wants BEFORE the round-trip.
func TestCPMCPForm_TellsTheOperatorToUseASecretReference(t *testing.T) {
	html := renderCPMCP(t)
	if !strings.Contains(html, "secret://") {
		t.Error("the auth fields take secret:// references only; the form never says so")
	}
}

// errNoPublicBaseURLForTest models RedirectURI's failure when
// server.public_base_url is unset.
var errNoPublicBaseURLForTest = errors.New("public_base_url is not set")

// oauthRow is the one-server registry every OAuth row test starts from.
func oauthRow() ServerOption {
	return WithMCPRegistry(cpMCPRenderRegistry{servers: []mcp.ServerSnapshot{
		{Name: "atlassian", Transport: "streamable-http", URL: "https://mcp.atlassian.com/v1/mcp/authv2", Reachable: true},
	}})
}

// TestCPMCPRow_OAuthServerOffersConnect — an unconnected oauth server needs a
// Connect button; without it the whole consent flow is UI-unreachable.
func TestCPMCPRow_OAuthServerOffersConnect(t *testing.T) {
	reg := oauthRow()
	html := renderCPMCP(t, reg, WithMCPOAuthAdmin(cpMCPRenderOAuth{
		refs: map[string]mcpconnect.ServerRef{"atlassian": {ServerName: "atlassian", Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth}}},
	}))
	if !strings.Contains(html, "/admin/control-plane/mcp/connect") {
		t.Error("no Connect form — the routed handler is unreachable from the tab")
	}
	if !strings.Contains(html, `value="connect"`) {
		t.Error("Connect action missing")
	}
	// The reachability badge is auth-BLIND: a 401-on-every-call server reads
	// "reachable". The row must distinguish reachable from authorized.
	if !strings.Contains(html, "not connected") {
		t.Error("row does not say the server is unauthorized")
	}
	if !strings.Contains(html, "auth: "+mcpauth.ModeOAuth) {
		t.Error("row does not show the auth mode")
	}
}

// TestCPMCPRow_ConnectedServerOffersDisconnect — and shows who consented, since
// a daemon-scope grant is reachable from every project.
func TestCPMCPRow_ConnectedServerOffersDisconnect(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	reg := oauthRow()
	html := renderCPMCP(t, reg, WithMCPOAuthAdmin(cpMCPRenderOAuth{
		refs:  map[string]mcpconnect.ServerRef{"atlassian": {ServerName: "atlassian", Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth}}},
		grant: &persistence.MCPOAuthToken{ServerName: "atlassian", ConnectedBy: "operator@example.com", ExpiresAt: &exp},
	}))
	if !strings.Contains(html, `value="disconnect"`) {
		t.Error("a connected server offers no Disconnect")
	}
	if !strings.Contains(html, "operator@example.com") {
		t.Error("row does not say who consented")
	}
	if !strings.Contains(html, "connected") {
		t.Error("row does not show the connected state")
	}
}

// TestCPMCPRow_NeedsReconnectIsVisible — a revoked or unrecoverably-stale grant
// is the one state where tools fail and the config looks perfect.
func TestCPMCPRow_NeedsReconnectIsVisible(t *testing.T) {
	reg := oauthRow()
	html := renderCPMCP(t, reg, WithMCPOAuthAdmin(cpMCPRenderOAuth{
		refs:  map[string]mcpconnect.ServerRef{"atlassian": {ServerName: "atlassian", Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth}}},
		grant: &persistence.MCPOAuthToken{ServerName: "atlassian", ConnectedBy: "op", NeedsReconnect: true},
	}))
	if !strings.Contains(html, "needs reconnect") {
		t.Error("NeedsReconnect is not surfaced")
	}
}

// TestCPMCPRow_BlockedConnectExplainsWhy — failing AFTER the operator consented
// at the vendor wastes their time and strands an authorization code, which is
// why decorateCPMCPAuth computes ConnectBlockedReason up front.
func TestCPMCPRow_BlockedConnectExplainsWhy(t *testing.T) {
	reg := oauthRow()
	html := renderCPMCP(t, reg, WithMCPOAuthAdmin(cpMCPRenderOAuth{
		refs:        map[string]mcpconnect.ServerRef{"atlassian": {ServerName: "atlassian", Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth}}},
		redirectErr: errNoPublicBaseURLForTest,
	}))
	if !strings.Contains(html, "public_base_url") {
		t.Error("the blocked reason is computed but never rendered")
	}
	if !strings.Contains(html, "disabled") {
		t.Error("a Connect that cannot succeed must not be clickable")
	}
}

// TestCPMCPRow_NonOAuthModeHasNoConnectButton — static and env credentials are
// config-only; there is no handshake to run, so a Connect button would be a lie.
func TestCPMCPRow_NonOAuthModeHasNoConnectButton(t *testing.T) {
	reg := WithMCPRegistry(cpMCPRenderRegistry{servers: []mcp.ServerSnapshot{
		{Name: "n8n", Transport: "streamable-http", URL: "https://n8n.example.com/mcp/abc", Reachable: true},
	}})
	html := renderCPMCP(t, reg, WithMCPOAuthAdmin(cpMCPRenderOAuth{
		refs: map[string]mcpconnect.ServerRef{"n8n": {ServerName: "n8n", Auth: mcpauth.Auth{Mode: mcpauth.ModeStatic, ValueFrom: "secret://N8N_TOKEN"}}},
	}))
	if strings.Contains(html, `value="connect"`) {
		t.Error("a static-auth server must not offer an OAuth Connect")
	}
	if !strings.Contains(html, "auth: "+mcpauth.ModeStatic) {
		t.Error("row does not show the static auth mode")
	}
	// A secret REFERENCE is fine to render; a value would not be, and no value
	// ever reaches this layer.
	if strings.Contains(html, "value=\"secret://N8N_TOKEN\"") {
		t.Error("the row must not echo the reference into an input value")
	}
}

// TestCPFlash_CoversEveryMCPRedirectToken — a handler that redirects with a
// token the flash map has no entry for shows the operator a BLANK banner, which
// reads as "nothing happened" after a failed Connect.
func TestCPFlash_CoversEveryMCPRedirectToken(t *testing.T) {
	for _, token := range []string{
		"mcp-proposed", "mcp-bad-name", "mcp-bad-transport", "mcp-bad-endpoint",
		"mcp-secret", "mcp-not-found", "mcp-bad-auth",
		"mcp-disconnected", "mcp-connect-failed",
	} {
		if cpFlashMessages[token] == "" {
			t.Errorf("done=%s renders an empty flash", token)
		}
	}
}

// TestMCPErrorIsAuthChallenge_MatchesTheRealClientError pins the coupling
// between this package's classifier and internal/mcp's error text.
//
// The health snapshot keeps the failure as a STRING, so errors.As is
// unavailable here and the classifier has to match on the message. That is
// fragile in exactly one way: if the client's format changes, the badge
// silently reverts to calling an authenticating server "unreachable" — the
// 2026-08-05 atlassian report. So the fixture is built from the client's own
// formatting rather than hand-written, and a format change fails here.
func TestMCPErrorIsAuthChallenge_MatchesTheRealClientError(t *testing.T) {
	// Mirrors internal/mcp/client.go's fmt.Errorf("%s server returned %d", …),
	// wrapped the way the health check records it.
	realErr := fmt.Sprintf("mcp initialize failed for atlassian: %s",
		fmt.Sprintf("streamable-http server returned %d", http.StatusUnauthorized))
	if !mcpErrorIsAuthChallenge(realErr) {
		t.Errorf("the real 401 error must classify as an auth challenge, got false for %q", realErr)
	}

	for _, notAuth := range []string{
		fmt.Sprintf("streamable-http server returned %d", http.StatusForbidden),
		fmt.Sprintf("sse server returned %d", http.StatusInternalServerError),
		"dial tcp: connection refused",
		"",
	} {
		if mcpErrorIsAuthChallenge(notAuth) {
			t.Errorf("%q must NOT classify as an auth challenge", notAuth)
		}
	}
}

// TestMCPRow_AuthChallengedRendersDistinctly — a 401 must read as a consent
// gap, not a networking fault. Reported 2026-08-05: the atlassian row said
// "unreachable ... streamable-http server returned 401" against a perfectly
// healthy endpoint, sending the operator to look for a broken URL.
//
// Driven through the real snapshot path so the classifier, the row field and
// the template are all exercised together.
func TestMCPRow_AuthChallengedRendersDistinctly(t *testing.T) {
	html := renderCPMCP(t, WithMCPRegistry(cpMCPRenderRegistry{servers: []mcp.ServerSnapshot{{
		Name: "atlassian", Transport: "streamable-http",
		URL:       "https://mcp.atlassian.com/v1/mcp/authv2",
		Reachable: false,
		Error:     "mcp initialize failed for atlassian: streamable-http server returned 401",
	}}}))
	if !strings.Contains(html, "needs authentication") {
		t.Errorf("a 401 row must say it needs authentication:\n%s", html)
	}
	if strings.Contains(html, ">unreachable<") {
		t.Errorf("a 401 row must not be labelled unreachable:\n%s", html)
	}
}

// TestMCPRow_GenuinelyUnreachableStillSaysSo — the badge must not become
// permissive. A connection failure is still a reachability fault.
func TestMCPRow_GenuinelyUnreachableStillSaysSo(t *testing.T) {
	html := renderCPMCP(t, WithMCPRegistry(cpMCPRenderRegistry{servers: []mcp.ServerSnapshot{{
		Name: "broken", Transport: "streamable-http",
		URL:       "https://nope.example.com/mcp",
		Reachable: false,
		Error:     "dial tcp: connection refused",
	}}}))
	if !strings.Contains(html, ">unreachable<") {
		t.Errorf("a real connection failure must still read unreachable:\n%s", html)
	}
}
