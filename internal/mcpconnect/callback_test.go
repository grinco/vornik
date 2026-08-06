package mcpconnect

import (
	"context"
	"github.com/rs/zerolog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/persistence"
)

func callbackGet(t *testing.T, h http.Handler, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, CallbackPath+"?"+q.Encode(), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCallback_HappyPathNeverRendersTheToken(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:issues")
	tokens := newFakeTokens()
	c := newConnector(t, tokens, &fakeAudit{}, "https://v.example.com")

	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "linear", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "alice")
	require.NoError(t, err)

	w := callbackGet(t, c.CallbackHandler(), url.Values{
		"state": {begun.State}, "code": {"the-code"},
	})
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()

	assert.Contains(t, body, "Connected")
	assert.Contains(t, body, "linear")
	assert.Contains(t, body, "project p1")
	assert.Contains(t, body, "read:issues")
	// The whole point: the page is reached in a browser and may be
	// screenshotted, bookmarked or shoulder-surfed.
	assert.NotContains(t, body, "at-1")
	assert.NotContains(t, body, "rt-1")
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))

	stored, err := tokens.Get(context.Background(), "p1", "linear")
	require.NoError(t, err)
	require.NotNil(t, stored)
}

// TestCallback_DaemonScopeSaysSo — the operator should see the blast radius on the page that
// confirms the grant, not only in an audit row.
func TestCallback_DaemonScopeSaysSo(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:x")
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")
	begun, err := c.Begin(context.Background(), ServerRef{
		ServerName: "shared", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "op")
	require.NoError(t, err)

	w := callbackGet(t, c.CallbackHandler(), url.Values{"state": {begun.State}, "code": {"c"}})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "every project on this daemon")
}

// TestCallback_VendorRefusalIsNotReportedAsOurFailure — a human declining consent is not a
// deployment problem, and telling them to check logs would be wrong.
func TestCallback_VendorRefusalIsNotReportedAsOurFailure(t *testing.T) {
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")
	w := callbackGet(t, c.CallbackHandler(), url.Values{
		"error":             {"access_denied"},
		"error_description": {"The user denied the request"},
		"state":             {"whatever"},
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "was not granted")
	assert.Contains(t, body, "access_denied")
	assert.Contains(t, body, "Nothing was changed")
}

func TestCallback_ExpiredOrForgedStateIsRefused(t *testing.T) {
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")
	w := callbackGet(t, c.CallbackHandler(), url.Values{"state": {"nope"}, "code": {"c"}})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "expired")
}

func TestCallback_DirectVisitIsExplained(t *testing.T) {
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")
	w := callbackGet(t, c.CallbackHandler(), url.Values{})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "not meant to be opened directly")
}

// TestCallback_VendorTextIsEscaped — the error parameters come from an untrusted redirect, and
// this page renders them.
func TestCallback_VendorTextIsEscaped(t *testing.T) {
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")
	w := callbackGet(t, c.CallbackHandler(), url.Values{
		"error":             {`<script>alert(1)</script>`},
		"error_description": {`<img src=x onerror=alert(1)>`},
	})
	body := w.Body.String()
	assert.NotContains(t, body, "<script>")
	assert.NotContains(t, body, "<img src=x")
	assert.Contains(t, body, "&lt;script&gt;")
}

// TestCallback_ServerNameIsEscaped — the server name comes from config, which the control plane
// can write; it must not be able to inject markup into this page.
func TestCallback_ServerNameIsEscaped(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:x")
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")
	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: `<script>x</script>`, URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "op")
	require.NoError(t, err)

	w := callbackGet(t, c.CallbackHandler(), url.Values{"state": {begun.State}, "code": {"c"}})
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "<script>x</script>")
}

// TestCallback_ExchangeFailureKeepsTheVendorBodyOffThePage — an OAuth error body can echo request
// parameters, which on a token request means the client secret.
func TestCallback_ExchangeFailureKeepsTheVendorBodyOffThePage(t *testing.T) {
	var vendor *httptest.Server
	vendor = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_, _ = w.Write([]byte(`{"issuer":"` + vendor.URL + `","authorization_endpoint":"` + vendor.URL + `/authorize","token_endpoint":"` + vendor.URL + `/token","registration_endpoint":"` + vendor.URL + `/register"}`))
		case "/register":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"client_id":"cid"}`))
		case "/token":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"echo SUPER-SECRET"}`))
		default:
			if len(r.URL.Path) > 12 && r.URL.Path[:13] == "/.well-known/" {
				_, _ = w.Write([]byte(`{"resource":"` + vendor.URL + `/mcp","authorization_servers":["` + vendor.URL + `"]}`))
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="x"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer vendor.Close()

	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")
	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "s", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "op")
	require.NoError(t, err)

	w := callbackGet(t, c.CallbackHandler(), url.Values{"state": {begun.State}, "code": {"c"}})
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.NotContains(t, w.Body.String(), "SUPER-SECRET")
}

// TestCallback_NoGrantIsPersistedOnFailure — a failed exchange must not leave a half-written row.
func TestCallback_NoGrantIsPersistedOnFailure(t *testing.T) {
	tokens := newFakeTokens()
	c := newConnector(t, tokens, &fakeAudit{}, "https://v.example.com")
	w := callbackGet(t, c.CallbackHandler(), url.Values{"state": {"unknown"}, "code": {"c"}})
	require.Equal(t, http.StatusBadRequest, w.Code)

	rows, err := tokens.ListForProject(context.Background(), "p1")
	require.NoError(t, err)
	assert.Empty(t, rows)
	var _ *persistence.MCPOAuthToken // documents the row type under test
}

// TestConnector_OnGrantedFiresOnConnectAndDisconnect pins the hook that makes a
// consent take effect.
//
// Storing a token is not the same as using one: the access token is injected
// when an MCP client is wired, at boot and on config reload. Before this hook a
// completed consent changed nothing — the callback page said "Connected" while
// the tool surface kept sending unauthenticated requests and the control-plane
// badge kept reporting that authentication was required. Reported 2026-08-05
// against the atlassian server, where consent at 22:21:26 did nothing and a
// manual `vornikctl config reload` three minutes later is what connected it.
//
// Disconnect is covered too, and for the mirror-image reason: without the
// re-wire the client keeps its injected Authorization header and goes on using a
// grant the operator just revoked.
func TestConnector_OnGrantedFiresOnConnectAndDisconnect(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	c := &Connector{
		Logger: zerolog.Nop(),
		OnGranted: func(projectID, serverName string) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, projectID+"/"+serverName)
		},
	}

	c.notifyGranted("", "atlassian")
	c.notifyGranted("janka", "slack")

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0] != "/atlassian" || calls[1] != "janka/slack" {
		t.Fatalf("hook calls = %v, want the daemon-scope and project-scope pair", calls)
	}
}

// TestConnector_OnGrantedNilIsSafe — the hook is optional, and the CLI path
// wires no container.
func TestConnector_OnGrantedNilIsSafe(_ *testing.T) {
	c := &Connector{Logger: zerolog.Nop()}
	c.notifyGranted("", "atlassian") // must not panic
}

// TestConnector_OnGrantedPanicDoesNotEscape — this runs on the operator's
// callback request, after a consent that already succeeded and was already
// persisted. A wiring bug in the hook must not turn that into a failed page or
// a dead daemon.
func TestConnector_OnGrantedPanicDoesNotEscape(_ *testing.T) {
	c := &Connector{
		Logger:    zerolog.Nop(),
		OnGranted: func(string, string) { panic("wiring bug") },
	}
	c.notifyGranted("", "atlassian") // must not propagate
}
