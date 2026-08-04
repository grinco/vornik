package mcpconnect

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/persistence"
)

// --- fakes -----------------------------------------------------------------

type fakeTokens struct {
	mu   sync.Mutex
	rows map[string]*persistence.MCPOAuthToken
	// lockMu honours the WithRefreshLock CONTRACT ("only one refresh in
	// flight per grant"), which both real implementations provide — Postgres
	// via a transaction-scoped advisory lock, SQLite via an in-process
	// channel, both pinned by the repotest suite. A double that skipped it
	// would let a test pass while the production guarantee was untested.
	lockMu sync.Mutex
}

func newFakeTokens() *fakeTokens {
	return &fakeTokens{rows: map[string]*persistence.MCPOAuthToken{}}
}

func (f *fakeTokens) key(p, s string) string { return p + "\x00" + s }

func (f *fakeTokens) Get(_ context.Context, p, s string) (*persistence.MCPOAuthToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[f.key(p, s)], nil
}

func (f *fakeTokens) Upsert(_ context.Context, tok *persistence.MCPOAuthToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := *tok
	f.rows[f.key(tok.ProjectID, tok.ServerName)] = &copied
	return nil
}

func (f *fakeTokens) SwapRefreshToken(_ context.Context, used string, next *persistence.MCPOAuthToken) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.rows[f.key(next.ProjectID, next.ServerName)]
	if cur == nil || cur.RefreshToken != used {
		return false, nil
	}
	copied := *next
	copied.ConnectedBy = cur.ConnectedBy
	copied.ConnectedAt = cur.ConnectedAt
	f.rows[f.key(next.ProjectID, next.ServerName)] = &copied
	return true, nil
}

func (f *fakeTokens) MarkNeedsReconnect(_ context.Context, p, s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row := f.rows[f.key(p, s)]; row != nil {
		row.NeedsReconnect = true
	}
	return nil
}

func (f *fakeTokens) Delete(_ context.Context, p, s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, f.key(p, s))
	return nil
}

func (f *fakeTokens) ListForProject(_ context.Context, p string) ([]*persistence.MCPOAuthToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*persistence.MCPOAuthToken
	for _, row := range f.rows {
		if row.ProjectID == p {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeTokens) WithRefreshLock(ctx context.Context, _, _ string, fn func(context.Context) error) error {
	f.lockMu.Lock()
	defer f.lockMu.Unlock()
	return fn(ctx)
}

type fakeAudit struct {
	mu      sync.Mutex
	entries []*persistence.AdminAuditEntry
}

func (f *fakeAudit) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

type mapSecrets map[string]string

func (m mapSecrets) Get(name string) (string, bool) {
	v, ok := m[name]
	return v, ok && v != ""
}

// oauthServer is a stand-in vendor: PRM + AS metadata + token endpoint, with knobs for the
// shapes the survey found (DCR present or absent, scopes advertised).
type oauthServer struct {
	*httptest.Server
	withDCR   bool
	scopes    []string
	lastToken url.Values
}

func newOAuthServer(t *testing.T, withDCR bool, scopes ...string) *oauthServer {
	t.Helper()
	o := &oauthServer{withDCR: withDCR, scopes: scopes}
	o.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource":              o.URL + "/mcp",
				"authorization_servers": []string{o.URL},
				"scopes_supported":      o.scopes,
			})
		case r.URL.Path == "/.well-known/oauth-authorization-server":
			doc := map[string]any{
				"issuer":                                o.URL,
				"authorization_endpoint":                o.URL + "/authorize",
				"token_endpoint":                        o.URL + "/token",
				"token_endpoint_auth_methods_supported": []string{"none"},
			}
			if o.withDCR {
				doc["registration_endpoint"] = o.URL + "/register"
			}
			_ = json.NewEncoder(w).Encode(doc)
		case r.URL.Path == "/register":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "dcr-client"})
		case r.URL.Path == "/token":
			_ = r.ParseForm()
			o.lastToken = r.PostForm
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at-1",
				"refresh_token": "rt-1",
				"expires_in":    3600,
				"scope":         strings.Join(o.scopes, " "),
			})
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(o.Close)
	return o
}

func newConnector(t *testing.T, tokens persistence.MCPOAuthTokenRepository, audit AuditSink, base string) *Connector {
	t.Helper()
	return &Connector{
		Tokens:  tokens,
		Secrets: mapSecrets{},
		Audit:   audit,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		BaseURL: func() string { return base },
		Logger:  zerolog.Nop(),
	}
}

// --- §7.1 the redirect-URI precondition ------------------------------------

func TestRedirectURI_PreconditionFailsBeforeConsent(t *testing.T) {
	for _, tc := range []struct{ name, base, wantErr string }{
		{"unset", "", "public_base_url"},
		{"plain http on a LAN address", "http://192.0.2.10:8080", "neither https nor loopback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newConnector(t, newFakeTokens(), nil, tc.base)
			_, err := c.RedirectURI()
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrNoPublicBaseURL))
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestRedirectURI_AcceptsHTTPSAndLoopback(t *testing.T) {
	c := newConnector(t, newFakeTokens(), nil, "https://vornik.example.com/")
	got, err := c.RedirectURI()
	require.NoError(t, err)
	assert.Equal(t, "https://vornik.example.com/auth/mcp/callback", got,
		"the trailing slash must not double up")

	c.BaseURL = func() string { return "http://localhost:8080" }
	got, err = c.RedirectURI()
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:8080/auth/mcp/callback", got)
}

// TestBegin_FailsPreconditionBeforeReachingTheVendor — failing after consent wastes the
// operator's time and strands an authorization code, so the check comes first.
func TestBegin_FailsPreconditionBeforeReachingTheVendor(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer srv.Close()

	c := newConnector(t, newFakeTokens(), nil, "")
	_, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "s", URL: srv.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "op")
	require.ErrorIs(t, err, ErrNoPublicBaseURL)
	assert.False(t, reached, "the vendor must not be contacted when the precondition fails")
}

func TestBegin_RefusesNonOAuthModes(t *testing.T) {
	c := newConnector(t, newFakeTokens(), nil, "https://v.example.com")
	_, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "s", URL: "https://x/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeStatic, ValueFrom: "secret://t"},
	}, "op")
	require.ErrorIs(t, err, ErrNotOAuth)
}

// --- the happy path, end to end --------------------------------------------

func TestBeginComplete_PersistsGrantAndRecordsConsent(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:issues", "write:issues", "offline_access")
	tokens := newFakeTokens()
	audit := &fakeAudit{}
	c := newConnector(t, tokens, audit, "https://vornik.example.com")

	ref := ServerRef{
		ProjectID: "p1", ServerName: "linear", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth, Scopes: []string{"read:issues", "offline_access"}},
	}
	begun, err := c.Begin(context.Background(), ref, "alice@example.com")
	require.NoError(t, err)
	assert.Equal(t, vendor.URL+"/mcp", begun.Resource)
	assert.Equal(t, []string{"read:issues", "offline_access"}, begun.Scopes)

	u, err := url.Parse(begun.AuthorizationURL)
	require.NoError(t, err)
	assert.Equal(t, "dcr-client", u.Query().Get("client_id"), "DCR should have registered a client")
	assert.Equal(t, begun.State, u.Query().Get("state"))
	assert.Equal(t, vendor.URL+"/mcp", u.Query().Get("resource"))

	// Nothing is persisted until the callback: a half-finished consent must
	// leave no grant behind.
	pre, err := tokens.Get(context.Background(), "p1", "linear")
	require.NoError(t, err)
	assert.Nil(t, pre)

	tok, err := c.Complete(context.Background(), begun.State, "the-code")
	require.NoError(t, err)
	assert.Equal(t, "at-1", tok.AccessToken)
	assert.Equal(t, "rt-1", tok.RefreshToken)
	assert.Equal(t, "alice@example.com", tok.ConnectedBy)
	assert.Equal(t, vendor.URL+"/mcp", tok.Resource)
	require.NotNil(t, tok.ExpiresAt)

	stored, err := tokens.Get(context.Background(), "p1", "linear")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "at-1", stored.AccessToken)

	// The §7.2 record: no token, no diff, and it names who consented to what.
	require.Len(t, audit.entries, 1)
	e := audit.entries[0]
	assert.Equal(t, "mcp.oauth.connect", e.Action)
	assert.Equal(t, "p1/linear", e.Target)
	assert.Equal(t, "alice@example.com", e.Principal)
	assert.NotContains(t, e.After, "at-1")
	assert.NotContains(t, e.After, "rt-1")
	var rec GrantRecord
	require.NoError(t, json.Unmarshal([]byte(e.After), &rec))
	assert.Equal(t, vendor.URL+"/mcp", rec.Resource)
	assert.False(t, rec.DaemonScope)
}

// TestComplete_StateIsOneShot — a leaked callback URL must not be replayable.
func TestComplete_StateIsOneShot(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:x")
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")

	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "s", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "op")
	require.NoError(t, err)

	_, err = c.Complete(context.Background(), begun.State, "code")
	require.NoError(t, err)

	_, err = c.Complete(context.Background(), begun.State, "code")
	require.ErrorIs(t, err, ErrUnknownState)
}

// TestComplete_FailedExchangeStillConsumesTheState — the code is single-use at the vendor too, so
// leaving the state resumable would only invite a confusing second failure.
func TestComplete_FailedExchangeStillConsumesTheState(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:x")
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")
	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "s", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "op")
	require.NoError(t, err)

	_, err = c.Complete(context.Background(), begun.State, "")
	require.Error(t, err)
	_, err = c.Complete(context.Background(), begun.State, "code")
	require.ErrorIs(t, err, ErrUnknownState)
}

func TestComplete_UnknownStateIsRefused(t *testing.T) {
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")
	_, err := c.Complete(context.Background(), "forged-state", "code")
	require.ErrorIs(t, err, ErrUnknownState)
}

// TestBegin_NoDCRAndNoClientIDIsANamedError — F1: Slack, GitHub and Box advertise no
// registration endpoint, and the operator's next action is to configure a client.
func TestBegin_NoDCRAndNoClientIDIsANamedError(t *testing.T) {
	vendor := newOAuthServer(t, false, "read:x")
	c := newConnector(t, newFakeTokens(), nil, "https://v.example.com")

	_, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "slack", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "op")
	require.Error(t, err)
	assert.True(t, errors.Is(err, mcpauth.ErrNoDCR), "want ErrNoDCR, got %v", err)
	assert.Contains(t, err.Error(), "auth.client_id")
}

// TestBegin_ConfidentialClientResolvesItsSecret — the F1 shape that DCR cannot serve.
func TestBegin_ConfidentialClientResolvesItsSecret(t *testing.T) {
	vendor := newOAuthServer(t, false, "read:x")
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")
	c.Secrets = mapSecrets{"SLACK_MCP_SECRET": "shh"}

	ref := ServerRef{
		ProjectID: "p1", ServerName: "slack", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{
			Mode: mcpauth.ModeOAuth, ClientID: "1234.5678",
			ClientSecretFrom: "secret://SLACK_MCP_SECRET",
		},
		GrantedSecrets: []string{"SLACK_MCP_SECRET"},
	}
	begun, err := c.Begin(context.Background(), ref, "op")
	require.NoError(t, err)

	_, err = c.Complete(context.Background(), begun.State, "code")
	require.NoError(t, err)
	assert.Equal(t, "shh", vendor.lastToken.Get("client_secret"),
		"a confidential client must authenticate with client_secret_post")
}

// TestBegin_ConfidentialSecretMustBeGranted — permissions.secrets gates the OAuth path too, not
// just mode: static.
func TestBegin_ConfidentialSecretMustBeGranted(t *testing.T) {
	vendor := newOAuthServer(t, false, "read:x")
	c := newConnector(t, newFakeTokens(), nil, "https://v.example.com")
	c.Secrets = mapSecrets{"SLACK_MCP_SECRET": "shh"}

	_, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "slack", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{
			Mode: mcpauth.ModeOAuth, ClientID: "cid",
			ClientSecretFrom: "secret://SLACK_MCP_SECRET",
		},
		GrantedSecrets: []string{"SOMETHING_ELSE"},
	}, "op")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permissions.secrets")
}

// TestBegin_ReusesAStoredDCRClient — §7.2a: one client per (deployment, server). Re-registering
// per connect would accumulate garbage clients at the authorization server.
func TestBegin_ReusesAStoredDCRClient(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:x")
	tokens := newFakeTokens()
	require.NoError(t, tokens.Upsert(context.Background(), &persistence.MCPOAuthToken{
		ProjectID: "p1", ServerName: "linear", ClientID: "already-registered",
		AccessToken: "old", Resource: vendor.URL + "/mcp",
	}))
	c := newConnector(t, tokens, &fakeAudit{}, "https://v.example.com")

	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "linear", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "op")
	require.NoError(t, err)
	u, _ := url.Parse(begun.AuthorizationURL)
	assert.Equal(t, "already-registered", u.Query().Get("client_id"))
}

// TestBegin_ManualEndpointsSkipDiscovery — F4: Intercom publishes no PRM anywhere.
func TestBegin_ManualEndpointsSkipDiscovery(t *testing.T) {
	var probed bool
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probed = true
		w.WriteHeader(http.StatusNotFound)
	}))
	defer vendor.Close()

	c := newConnector(t, newFakeTokens(), nil, "https://v.example.com")
	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "intercom", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{
			Mode:                  mcpauth.ModeOAuth,
			ClientID:              "cid",
			AuthorizationEndpoint: "https://app.intercom.com/oauth",
			TokenEndpoint:         "https://api.intercom.io/auth/eagle/token",
		},
	}, "op")
	require.NoError(t, err)
	assert.False(t, probed, "configured endpoints must skip the discovery probe entirely")
	assert.True(t, strings.HasPrefix(begun.AuthorizationURL, "https://app.intercom.com/oauth?"))
}

// TestBegin_DaemonScopeDefaultsToReadOnlyScopes — §12.2, hardened from "warning only": a
// daemon-scope token is reachable from every project, so write scopes must be named explicitly
// in config rather than inherited from the PRM's advertised set.
func TestBegin_DaemonScopeDefaultsToReadOnlyScopes(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:issues", "write:issues", "admin", "offline_access")
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")

	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "", ServerName: "linear", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "op")
	require.NoError(t, err)
	assert.Equal(t, []string{"read:issues", "offline_access"}, begun.Scopes,
		"an unrecognised or write scope must be DROPPED for a daemon-scope grant, not assumed harmless")

	// A project-scoped server keeps the ordinary default: everything advertised.
	begun, err = c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "linear", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "op")
	require.NoError(t, err)
	assert.Equal(t, []string{"read:issues", "write:issues", "admin", "offline_access"}, begun.Scopes)
}

// TestBegin_DaemonScopeHonoursExplicitScopes — the escape hatch: naming write scopes in config
// is exactly how an operator opts into them.
func TestBegin_DaemonScopeHonoursExplicitScopes(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:issues", "write:issues")
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")

	begun, err := c.Begin(context.Background(), ServerRef{
		ServerName: "linear", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth, Scopes: []string{"write:issues"}},
	}, "op")
	require.NoError(t, err)
	assert.Equal(t, []string{"write:issues"}, begun.Scopes)
}

func TestComplete_DaemonScopeRecordIsMarked(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:x")
	audit := &fakeAudit{}
	c := newConnector(t, newFakeTokens(), audit, "https://v.example.com")

	begun, err := c.Begin(context.Background(), ServerRef{
		ServerName: "shared", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "op")
	require.NoError(t, err)
	_, err = c.Complete(context.Background(), begun.State, "code")
	require.NoError(t, err)

	require.Len(t, audit.entries, 1)
	assert.Equal(t, "daemon/shared", audit.entries[0].Target)
	var rec GrantRecord
	require.NoError(t, json.Unmarshal([]byte(audit.entries[0].After), &rec))
	assert.True(t, rec.DaemonScope, "the record must make the blast radius explicit")
}

func TestDisconnect_DeletesAndRecords(t *testing.T) {
	tokens := newFakeTokens()
	require.NoError(t, tokens.Upsert(context.Background(), &persistence.MCPOAuthToken{
		ProjectID: "p1", ServerName: "linear", AccessToken: "at", Resource: "https://r",
		Scopes: "read:x", ConnectedBy: "alice",
	}))
	audit := &fakeAudit{}
	c := newConnector(t, tokens, audit, "https://v.example.com")

	require.NoError(t, c.Disconnect(context.Background(), "p1", "linear", "bob"))

	got, err := tokens.Get(context.Background(), "p1", "linear")
	require.NoError(t, err)
	assert.Nil(t, got)

	require.Len(t, audit.entries, 1)
	assert.Equal(t, "mcp.oauth.disconnect", audit.entries[0].Action)
	assert.Equal(t, "bob", audit.entries[0].Principal)
	assert.NotContains(t, audit.entries[0].After, "at")
}

// TestPendingExpiry — an authorization left unfinished past the TTL must not stay replayable.
func TestPendingExpiry(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:x")
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")

	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "s", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "op")
	require.NoError(t, err)

	c.mu.Lock()
	c.pending[begun.State].CreatedAt = time.Now().Add(-pendingTTL - time.Minute)
	c.mu.Unlock()

	_, err = c.Complete(context.Background(), begun.State, "code")
	require.ErrorIs(t, err, ErrUnknownState)
}

func TestReadOnlyScopes(t *testing.T) {
	got := readOnlyScopes([]string{
		"read:jira-work", "write:jira-work", "offline_access", "openid",
		"issues.read", "boards:read", "drive.readonly", "manage:everything", "",
	})
	assert.Equal(t, []string{
		"read:jira-work", "offline_access", "openid", "issues.read", "boards:read", "drive.readonly",
	}, got)
}

// TestRedirectURI_ReadsTheBaseURLLive — the connector is built once at boot, so a captured string
// would mean an operator who sets public_base_url and reloads config still gets
// PUBLIC_BASE_URL_REQUIRED until they restart the daemon. Found while deploying for a live test.
func TestRedirectURI_ReadsTheBaseURLLive(t *testing.T) {
	live := ""
	c := &Connector{BaseURL: func() string { return live }}

	_, err := c.RedirectURI()
	require.ErrorIs(t, err, ErrNoPublicBaseURL)

	live = "https://swarms.example.com"
	got, err := c.RedirectURI()
	require.NoError(t, err)
	assert.Equal(t, "https://swarms.example.com/auth/mcp/callback", got)

	// …and a later change is picked up too, so the callback cannot keep
	// pointing at a stale origin.
	live = "https://moved.example.com"
	got, err = c.RedirectURI()
	require.NoError(t, err)
	assert.Equal(t, "https://moved.example.com/auth/mcp/callback", got)
}

// TestRedirectURI_NilBaseURLFailsThePrecondition — a connector built without the getter must fail
// closed rather than panic.
func TestRedirectURI_NilBaseURLFailsThePrecondition(t *testing.T) {
	_, err := (&Connector{}).RedirectURI()
	require.ErrorIs(t, err, ErrNoPublicBaseURL)
}

// TestCallbackPath_MatchesTheSurfaceStandard pins the path itself, not just that a constant is
// used: a browser-reached surface belongs under /auth/<purpose>/, never under /ui (a redirect
// target is not a console page) and never under /api/v1 (that is the machine surface — API-key
// auth, JSON in and out). Changing it is not free once a vendor has registered it, so it should
// take a deliberate test edit.
// see LLD § https://docs.vornik.io §4
func TestCallbackPath_MatchesTheSurfaceStandard(t *testing.T) {
	assert.Equal(t, "/auth/mcp/callback", CallbackPath)
	assert.True(t, strings.HasPrefix(CallbackPath, "/auth/"))
	assert.NotContains(t, CallbackPath, "/ui/")
	assert.NotContains(t, CallbackPath, "/api/")
}
