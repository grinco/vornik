package mcpconnect

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/persistence"
)

// refreshVendor serves AS metadata and a token endpoint, counting refreshes and letting a test
// force an invalid_grant.
type refreshVendor struct {
	*httptest.Server
	mu           sync.Mutex
	refreshes    int
	invalidGrant bool
	lastForm     map[string]string
}

func newRefreshVendor(t *testing.T) *refreshVendor {
	t.Helper()
	v := &refreshVendor{}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 v.URL,
				"authorization_endpoint": v.URL + "/authorize",
				"token_endpoint":         v.URL + "/token",
				"registration_endpoint":  v.URL + "/register",
			})
		case "/token":
			_ = r.ParseForm()
			v.mu.Lock()
			v.refreshes++
			v.lastForm = map[string]string{}
			for k := range r.PostForm {
				v.lastForm[k] = r.PostForm.Get(k)
			}
			invalid := v.invalidGrant
			n := v.refreshes
			v.mu.Unlock()
			if invalid {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at-refreshed",
				"refresh_token": "rt-rotated",
				"expires_in":    3600,
				"scope":         "read:issues",
			})
			_ = n
		default:
			if strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource") {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"resource":              v.URL + "/mcp",
					"authorization_servers": []string{v.URL},
				})
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(v.Close)
	return v
}

func (v *refreshVendor) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.refreshes
}

func storedGrant(vendorURL string, expiresIn time.Duration, refresh string) *persistence.MCPOAuthToken {
	exp := time.Now().Add(expiresIn).UTC()
	return &persistence.MCPOAuthToken{
		ProjectID: "p1", ServerName: "linear",
		Resource: vendorURL + "/mcp", ClientID: "cid",
		AccessToken: "at-stored", RefreshToken: refresh,
		ExpiresAt: &exp, Scopes: "read:issues",
		ConnectedBy: "alice", ConnectedAt: time.Now().Add(-time.Hour).UTC(),
	}
}

func injectRef(vendorURL string) ServerRef {
	return ServerRef{
		ProjectID: "p1", ServerName: "linear", URL: vendorURL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}
}

func TestAccessToken_ValidTokenIsUsedWithoutRefreshing(t *testing.T) {
	v := newRefreshVendor(t)
	tokens := newFakeTokens()
	require.NoError(t, tokens.Upsert(context.Background(), storedGrant(v.URL, time.Hour, "rt-1")))
	c := newConnector(t, tokens, &fakeAudit{}, "https://x.example.com")

	got, err := c.AccessToken(context.Background(), injectRef(v.URL))
	require.NoError(t, err)
	assert.Equal(t, "at-stored", got)
	assert.Equal(t, 0, v.count(), "a valid token must not trigger a refresh")
}

// TestAccessToken_RefreshesBeforeExpiryNotAfter — a call that starts valid must not finish 401.
func TestAccessToken_RefreshesBeforeExpiryNotAfter(t *testing.T) {
	v := newRefreshVendor(t)
	tokens := newFakeTokens()
	// Inside the skew window: still technically valid, but not for long
	// enough to survive a request.
	require.NoError(t, tokens.Upsert(context.Background(), storedGrant(v.URL, refreshSkew/2, "rt-1")))
	c := newConnector(t, tokens, &fakeAudit{}, "https://x.example.com")

	got, err := c.AccessToken(context.Background(), injectRef(v.URL))
	require.NoError(t, err)
	assert.Equal(t, "at-refreshed", got)
	assert.Equal(t, 1, v.count())

	stored, err := tokens.Get(context.Background(), "p1", "linear")
	require.NoError(t, err)
	assert.Equal(t, "at-refreshed", stored.AccessToken)
	assert.Equal(t, "rt-rotated", stored.RefreshToken, "the rotated refresh token must be persisted")
	// A refresh is not a new consent.
	assert.Equal(t, "alice", stored.ConnectedBy)

	// F5: the refresh must name the resource the token was ISSUED for.
	v.mu.Lock()
	form := v.lastForm
	v.mu.Unlock()
	assert.Equal(t, v.URL+"/mcp", form["resource"])
	assert.Equal(t, "refresh_token", form["grant_type"])
	assert.Equal(t, "rt-1", form["refresh_token"])
}

func TestAccessToken_NoGrantIsNotAnError(t *testing.T) {
	v := newRefreshVendor(t)
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://x.example.com")
	got, err := c.AccessToken(context.Background(), injectRef(v.URL))
	require.NoError(t, err)
	assert.Empty(t, got, "the caller decides whether a missing grant is fatal")
}

// TestAccessToken_ExpiredWithNoRefreshTokenNeedsReconnect — §6: that combination IS
// needs_reconnect by definition.
func TestAccessToken_ExpiredWithNoRefreshTokenNeedsReconnect(t *testing.T) {
	v := newRefreshVendor(t)
	tokens := newFakeTokens()
	require.NoError(t, tokens.Upsert(context.Background(), storedGrant(v.URL, -time.Minute, "")))
	c := newConnector(t, tokens, &fakeAudit{}, "https://x.example.com")

	_, err := c.AccessToken(context.Background(), injectRef(v.URL))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNeedsReconnect), "got %v", err)

	stored, err := tokens.Get(context.Background(), "p1", "linear")
	require.NoError(t, err)
	assert.True(t, stored.NeedsReconnect, "the state must be recorded, not just returned")
}

// TestAccessToken_InvalidGrantIsTerminal — §8's "never loop": the grant is gone at the vendor, so
// flag it and fail non-retryably rather than retrying every tool call forever.
func TestAccessToken_InvalidGrantIsTerminal(t *testing.T) {
	v := newRefreshVendor(t)
	v.invalidGrant = true
	tokens := newFakeTokens()
	require.NoError(t, tokens.Upsert(context.Background(), storedGrant(v.URL, -time.Minute, "rt-dead")))
	c := newConnector(t, tokens, &fakeAudit{}, "https://x.example.com")

	_, err := c.AccessToken(context.Background(), injectRef(v.URL))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNeedsReconnect), "got %v", err)

	stored, err := tokens.Get(context.Background(), "p1", "linear")
	require.NoError(t, err)
	assert.True(t, stored.NeedsReconnect)

	// And a second call does NOT hit the vendor again.
	before := v.count()
	_, err = c.AccessToken(context.Background(), injectRef(v.URL))
	require.ErrorIs(t, err, ErrNeedsReconnect)
	assert.Equal(t, before, v.count(), "a needs_reconnect grant must not keep burning refresh attempts")
}

// TestAccessToken_TransientRefreshFailureLeavesTheGrantAlone — a 503 at the token endpoint is not
// a revoked grant, so flagging needs_reconnect would send an operator to re-consent for nothing.
func TestAccessToken_TransientRefreshFailureLeavesTheGrantAlone(t *testing.T) {
	var vendor *httptest.Server
	vendor = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_, _ = w.Write([]byte(`{"issuer":"` + vendor.URL + `","authorization_endpoint":"` + vendor.URL + `/authorize","token_endpoint":"` + vendor.URL + `/token"}`))
		case "/token":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			if strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource") {
				_, _ = w.Write([]byte(`{"resource":"` + vendor.URL + `/mcp","authorization_servers":["` + vendor.URL + `"]}`))
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="x"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer vendor.Close()

	tokens := newFakeTokens()
	require.NoError(t, tokens.Upsert(context.Background(), storedGrant(vendor.URL, -time.Minute, "rt-1")))
	c := newConnector(t, tokens, &fakeAudit{}, "https://x.example.com")

	_, err := c.AccessToken(context.Background(), injectRef(vendor.URL))
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNeedsReconnect), "a transient failure must not read as a revoked grant")

	stored, err := tokens.Get(context.Background(), "p1", "linear")
	require.NoError(t, err)
	assert.False(t, stored.NeedsReconnect)
	assert.Equal(t, "rt-1", stored.RefreshToken, "the refresh token must survive a failed attempt")
}

// TestAccessToken_ConcurrentCallsRefreshOnce — the lock's purpose: two daemons (here, two
// goroutines) must not both burn a refresh, because some authorization servers treat a replayed
// single-use refresh token as an attack and revoke the whole grant.
func TestAccessToken_ConcurrentCallsRefreshOnce(t *testing.T) {
	v := newRefreshVendor(t)
	tokens := newFakeTokens()
	require.NoError(t, tokens.Upsert(context.Background(), storedGrant(v.URL, -time.Minute, "rt-1")))
	c := newConnector(t, tokens, &fakeAudit{}, "https://x.example.com")

	var wg sync.WaitGroup
	results := make([]string, 6)
	errs := make([]error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.AccessToken(context.Background(), injectRef(v.URL))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "call %d", i)
		assert.Equal(t, "at-refreshed", results[i], "every caller must get the live token")
	}
	assert.Equal(t, 1, v.count(), "the double-check under the lock must collapse this to one refresh")
}

// TestAccessToken_RefusesAGrantIssuedForAnotherOrigin — the confused-deputy guard, enforced at USE:
// an operator who repoints a server's url must not have the old grant silently presented to the new
// vendor.
func TestAccessToken_RefusesAGrantIssuedForAnotherOrigin(t *testing.T) {
	v := newRefreshVendor(t)
	tokens := newFakeTokens()
	require.NoError(t, tokens.Upsert(context.Background(), storedGrant(v.URL, time.Hour, "rt-1")))
	c := newConnector(t, tokens, &fakeAudit{}, "https://x.example.com")

	ref := injectRef(v.URL)
	ref.URL = "https://someone-else.example.com/mcp"
	_, err := c.AccessToken(context.Background(), ref)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNeedsReconnect), "got %v", err)
	assert.Contains(t, err.Error(), "different origin")
}

// TestAccessToken_PathDifferencesAreNotAnOriginChange — the RFC 8707 resource is often the server
// URL with a normalised path, so comparing paths would refuse every legitimate grant.
func TestAccessToken_PathDifferencesAreNotAnOriginChange(t *testing.T) {
	v := newRefreshVendor(t)
	tokens := newFakeTokens()
	tok := storedGrant(v.URL, time.Hour, "rt-1")
	tok.Resource = v.URL // no /mcp suffix
	require.NoError(t, tokens.Upsert(context.Background(), tok))
	c := newConnector(t, tokens, &fakeAudit{}, "https://x.example.com")

	got, err := c.AccessToken(context.Background(), injectRef(v.URL))
	require.NoError(t, err)
	assert.Equal(t, "at-stored", got)
}

// A host can expose multiple independently-authorized MCP resources. A grant
// for /service-a must not be attached after config repoints the same server
// name to sibling /service-b merely because scheme+host still match.
func TestAccessToken_RefusesSiblingResourceOnSameOrigin(t *testing.T) {
	v := newRefreshVendor(t)
	tokens := newFakeTokens()
	tok := storedGrant(v.URL, time.Hour, "rt-1")
	tok.Resource = v.URL + "/service-a"
	require.NoError(t, tokens.Upsert(context.Background(), tok))
	c := newConnector(t, tokens, &fakeAudit{}, "https://x.example.com")

	ref := injectRef(v.URL)
	ref.URL = v.URL + "/service-b"
	_, err := c.AccessToken(context.Background(), ref)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNeedsReconnect)
}

func TestSameOrigin(t *testing.T) {
	assert.True(t, sameOrigin("https://a.example/mcp", "https://a.example/mcp/v1"))
	assert.True(t, sameOrigin("https://A.Example/x", "https://a.example/y"))
	assert.False(t, sameOrigin("https://a.example/x", "https://b.example/x"))
	assert.False(t, sameOrigin("https://a.example/x", "http://a.example/x"))
}

// Dot segments must not walk out of the granted audience. url.Parse does NOT
// resolve "..", so an unnormalised prefix comparison reads /service-a/../service-b
// as a path UNDER the /service-a grant and attaches that grant to a sibling
// resource — the very substitution TestAccessToken_RefusesSiblingResourceOnSameOrigin
// exists to prevent, spelled differently.
func TestSameResourceAudience_DotSegmentsCannotEscapeTheGrant(t *testing.T) {
	const grant = "https://h.example/service-a"
	assert.False(t, sameResourceAudience(grant, "https://h.example/service-a/../service-b"),
		"a dot segment must not smuggle a sibling resource past the audience check")
	assert.False(t, sameResourceAudience(grant, "https://h.example/service-a/./../service-b"),
		"mixed dot segments must normalise before comparison")

	// Normalisation must not break the cases that are legitimately in-audience.
	assert.True(t, sameResourceAudience(grant, "https://h.example/service-a"))
	assert.True(t, sameResourceAudience(grant, "https://h.example/service-a/"))
	assert.True(t, sameResourceAudience(grant, "https://h.example/service-a/v1"))
	assert.True(t, sameResourceAudience(grant, "https://h.example/service-a/v1/../v2"),
		"a dot segment that stays inside the grant is still in-audience")
	assert.False(t, sameResourceAudience(grant, "https://h.example/service-ab"),
		"prefix confusion must stay closed")
	// An origin-wide grant stays origin-wide.
	assert.True(t, sameResourceAudience("https://h.example", "https://h.example/anything"))
}
