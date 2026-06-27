package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLookupAPIKeyAcceptsMatchingKey locks in that lookupAPIKey still
// accepts a legitimate key. The constant-time walk must not silently
// reject valid keys because it iterates the entire map.
func TestLookupAPIKeyAcceptsMatchingKey(t *testing.T) {
	keys := map[string][]string{
		"key-alpha": {"proj-1"},
		"key-beta":  nil,
		"key-gamma": {"proj-2", "proj-3"},
	}

	projects, ok := lookupAPIKey(keys, "key-beta")
	require.True(t, ok)
	require.Nil(t, projects)

	projects, ok = lookupAPIKey(keys, "key-gamma")
	require.True(t, ok)
	require.Equal(t, []string{"proj-2", "proj-3"}, projects)
}

// TestLookupAPIKeyRejectsUnknownKey ensures an unknown key produces ok=false.
func TestLookupAPIKeyRejectsUnknownKey(t *testing.T) {
	keys := map[string][]string{
		"key-alpha": nil,
		"key-beta":  {"proj-1"},
	}

	_, ok := lookupAPIKey(keys, "key-unknown")
	require.False(t, ok)
}

// TestLookupAPIKeyRejectsPrefixAndSuffix ensures we don't accidentally
// accept a candidate that is a prefix/suffix of a configured key. This
// would be a classic bug if the constant-time compare ever got replaced
// with a HasPrefix-style shortcut.
func TestLookupAPIKeyRejectsPrefixAndSuffix(t *testing.T) {
	keys := map[string][]string{
		"secret-1234567890": nil,
	}

	for _, candidate := range []string{
		"secret-123",
		"secret-1234567890extra",
		"",
		"secret-1234567891", // one byte different at the end
	} {
		_, ok := lookupAPIKey(keys, candidate)
		require.Falsef(t, ok, "candidate %q must be rejected", candidate)
	}
}

// TestLookupAPIKeyEmptyConfig rejects everything when no keys are
// configured. Without this check the middleware would collapse into an
// always-open state.
func TestLookupAPIKeyEmptyConfig(t *testing.T) {
	_, ok := lookupAPIKey(nil, "anything")
	require.False(t, ok)

	_, ok = lookupAPIKey(map[string][]string{}, "anything")
	require.False(t, ok)
}

// TestLookupAPIKey_TimingIndependentOfPresentedKeyLength is the
// length-leak regression sentinel. Before the fix, raw key bytes
// were passed to subtle.ConstantTimeCompare, which returns 0 in
// O(1) when the two slice lengths differ — leaking the length
// distribution of the configured keys to an attacker who could
// time the response.
//
// After the fix, both sides are hashed through apikey.Hash first
// (64-char hex SHA-256), so the comparison length is always the
// same regardless of the presented key's length. This test
// asserts the function still functionally accepts/rejects
// correctly across wildly different presented-key lengths — a
// behavioural witness that the hashing path is wired correctly.
// Pre-fix: a 0-byte presented key returned in O(1) from
// ConstantTimeCompare on the length mismatch. Post-fix: both
// sides hash to 64 chars and the compare runs to completion.
func TestLookupAPIKey_TimingIndependentOfPresentedKeyLength(t *testing.T) {
	keys := map[string][]string{
		"sk-vornik-projectA-randomtoken12345678901234567890": {"projectA"},
	}
	cases := []struct {
		name       string
		presented  string
		wantAccept bool
	}{
		{"empty", "", false},
		{"one-char", "x", false},
		{"same-length-wrong", "sk-vornik-projectA-randomtoken12345678901234567891", false},
		{"long", string(make([]byte, 1024)), false},
		{"exact-match", "sk-vornik-projectA-randomtoken12345678901234567890", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, got := lookupAPIKey(keys, c.presented)
			if got != c.wantAccept {
				t.Errorf("lookupAPIKey(%q) accept = %v, want %v", c.presented, got, c.wantAccept)
			}
		})
	}
}

// authMiddlewareTestHandler is a simple sink the middleware tests use
// to confirm whether a request reached the protected route.
func authMiddlewareTestHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

func newAuthRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, path, nil)
	require.NoError(t, err)
	return req
}

// TestAuthMiddleware_PublicEndpointsBypass confirms /healthz and the
// other liveness probes never see the auth gate even with auth enabled.
func TestAuthMiddleware_PublicEndpointsBypass(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-1": nil},
	})
	for _, path := range []string{"/healthz", "/readyz", "/health/live", "/health/ready", "/metrics"} {
		var reached bool
		rec := httptest.NewRecorder()
		mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, newAuthRequest(t, path))
		require.Truef(t, reached, "path %q must bypass auth", path)
		require.Equalf(t, http.StatusOK, rec.Code, "path %q must return 200", path)
	}
}

// TestAuthMiddleware_AuthDisabledLetsEverythingThrough — when the
// operator runs without auth, the middleware does not gate any path.
// This includes webhooks, since the IngestWebhook handler still
// enforces HMAC on each delivery against the source's secret.
func TestAuthMiddleware_AuthDisabledLetsEverythingThrough(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{Enabled: false})
	var reached bool
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, newAuthRequest(t, "/api/v1/projects/foo/tasks"))
	require.True(t, reached)
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestAuthMiddleware_RejectsMissingAPIKey returns 401 for a normal
// API path with no credentials.
func TestAuthMiddleware_RejectsMissingAPIKey(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-1": nil},
	})
	var reached bool
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, newAuthRequest(t, "/api/v1/projects/foo/tasks"))
	require.False(t, reached, "handler must NOT be reached without an API key")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestAuthMiddleware_RejectsInvalidAPIKey returns 401 even with a
// well-formed key that isn't in the configured set.
func TestAuthMiddleware_RejectsInvalidAPIKey(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})
	var reached bool
	req := newAuthRequest(t, "/api/v1/projects/foo/tasks")
	req.Header.Set("X-API-Key", "key-impostor")
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req)
	require.False(t, reached)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestAuthMiddleware_AcceptsValidAPIKeyAndStashesContext — happy path
// for a properly authenticated request: handler runs and the context
// carries the API-key-allowed projects so ProjectAuthMiddleware can
// scope further down.
func TestAuthMiddleware_AcceptsValidAPIKeyAndStashesContext(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": {"proj-a", "proj-b"}},
	})
	var ctxProjects []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v, ok := r.Context().Value(projectIDKey).([]string); ok {
			ctxProjects = v
		}
		w.WriteHeader(http.StatusOK)
	})

	req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
	req.Header.Set("X-API-Key", "key-real")
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"proj-a", "proj-b"}, ctxProjects)
}

// TestAuthMiddleware_AcceptsBasicAuthAsBearer — a browser can't send
// `Authorization: Bearer ...` from a plain URL, so we accept the
// bearer as the password field of HTTP Basic. Username is ignored.
// Operators flip api.auth_enabled=true and access /ui in a browser;
// the browser prompts via WWW-Authenticate, the user types any
// username + their bearer, and every subsequent request carries the
// credential automatically.
func TestAuthMiddleware_AcceptsBasicAuthAsBearer(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": {"proj-a"}},
	})
	var ctxKey string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v, ok := r.Context().Value(apiKeyKey).(string); ok {
			ctxKey = v
		}
		w.WriteHeader(http.StatusOK)
	})

	req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
	req.SetBasicAuth("api", "key-real")
	// Same-origin signal so the CSRF gate (A3) doesn't pre-empt the
	// credential-extraction path this test exercises.
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"Basic Auth with bearer-as-password must pass the standard validation path")
	require.Equal(t, "key-real", ctxKey,
		"the validated bearer must reach context so downstream cost attribution works")
}

// TestAuthMiddleware_AcceptsBasicAuthWithBearerPrefix — some clients
// prepend "Bearer " to the Basic password field. Strip it cleanly so
// the inner validation sees the raw key.
func TestAuthMiddleware_AcceptsBasicAuthWithBearerPrefix(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": {"proj-a"}},
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
	req.SetBasicAuth("api", "Bearer key-real")
	// Same-origin signal so the CSRF gate (A3) doesn't pre-empt the
	// credential-extraction path this test exercises.
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"`Bearer <key>` in the Basic password field must be unwrapped")
}

// TestAuthMiddleware_EmitsBasicChallengeOn401 — every 401 must carry
// the WWW-Authenticate header so a browser pops a credential dialog
// instead of rendering the JSON error body verbatim. The JSON body
// is still served (non-browser clients ignore the header); pairing
// both makes the same response useful to both audiences.
func TestAuthMiddleware_EmitsBasicChallengeOn401(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name string
		req  func() *http.Request
	}{
		{"no_credentials", func() *http.Request {
			return newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
		}},
		{"wrong_bearer", func() *http.Request {
			r := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
			r.Header.Set("Authorization", "Bearer not-a-real-key")
			return r
		}},
		{"wrong_basic", func() *http.Request {
			r := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
			r.SetBasicAuth("api", "not-a-real-key")
			// Same-origin so the CSRF gate (A3) doesn't pre-empt the 401
			// credential challenge this case verifies.
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			return r
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mw(handler).ServeHTTP(rec, c.req())
			require.Equal(t, http.StatusUnauthorized, rec.Code)
			challenge := rec.Header().Get("WWW-Authenticate")
			require.NotEmpty(t, challenge,
				"401 must include WWW-Authenticate so browsers prompt")
			require.Contains(t, challenge, "Basic",
				`challenge scheme must be Basic so browsers recognise it (got %q)`, challenge)
			require.Contains(t, challenge, `realm="vornik"`,
				`challenge realm helps the user recognise which credential to use (got %q)`, challenge)
		})
	}
}

// TestAuthMiddleware_BearerWinsOverBasic — a request carrying BOTH
// shapes (rare, mostly seen in HTTP-debugger output) must validate
// through the same path. Because Authorization can only hold one
// scheme at a time, BasicAuth() and the Bearer branch are mutually
// exclusive at the wire level — this test pins the behaviour
// against future refactors that might forget that property.
func TestAuthMiddleware_BasicAuthEmptyPasswordIgnored(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Basic with empty password → not a credential. Must 401.
	req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
	req.SetBasicAuth("api", "")
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code,
		"Basic Auth with empty password must NOT bypass the gate")
}

// TestAuthMiddleware_CSRF_BasicMutatingCrossSiteBlocked — the
// reason Basic Auth + a mutating endpoint is dangerous: browsers
// auto-attach the cached Basic credential on every request, so an
// attacker-controlled tab can issue a POST that the daemon would
// otherwise accept as the operator. With this gate, the request
// is refused with 403 / CSRF_BLOCKED.
func TestAuthMiddleware_CSRF_BasicMutatingCrossSiteBlocked(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
	req.Method = http.MethodPost
	req.SetBasicAuth("api", "key-real")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code,
		"cross-site POST via Basic must 403, not pass to handler")
	require.Contains(t, rec.Body.String(), "CSRF_BLOCKED")
}

// TestAuthMiddleware_CSRF_GitSmartHTTPExempt — git's smart-HTTP clone/push
// RPCs are POSTs authenticated via HTTP Basic (the only auth git speaks over
// HTTP) with NO Sec-Fetch-Site and NO Origin (git is not a browser). Without
// an exemption the CSRF gate's fail-closed branch 403s every clone AND push
// RPC, silently breaking git-over-HTTPS. The /api/v1/git/ routes carry an API
// key (bearer-equivalent secret, not an ambient cookie) and are fully
// authenticated by gitHTTPAuth, so they must bypass the browser-oriented CSRF
// gate — exactly like Bearer/X-API-Key clients do. Regression for the
// git-over-HTTPS 403 CSRF_BLOCKED incident (2026-06-21).
func TestAuthMiddleware_CSRF_GitSmartHTTPExempt(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})
	var reached bool
	handler := authMiddlewareTestHandler(&reached)

	cases := []struct{ path, contentType string }{
		{"/api/v1/git/assistant.git/git-upload-pack", "application/x-git-upload-pack-request"},   // clone/fetch RPC
		{"/api/v1/git/assistant.git/git-receive-pack", "application/x-git-receive-pack-request"}, // push RPC
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			reached = false
			req := newAuthRequest(t, tc.path)
			req.Method = http.MethodPost
			req.SetBasicAuth("git-access", "key-real")
			req.Header.Set("Content-Type", tc.contentType)
			// No Sec-Fetch-Site, no Origin — the exact shape a git CLI sends,
			// which a normal route would reject as CSRF_BLOCKED.
			rec := httptest.NewRecorder()
			mw(handler).ServeHTTP(rec, req)
			require.NotContains(t, rec.Body.String(), "CSRF_BLOCKED",
				"git smart-HTTP POST with the git content-type must be exempt from the CSRF gate")
			require.True(t, reached, "git smart-HTTP request must reach the handler")
		})
	}
}

// TestAuthMiddleware_CSRF_GitPathForgedContentTypeStillBlocked — the git
// exemption is scoped to the git smart-HTTP content-type, which a cross-site
// browser fetch cannot set without a (blocked) CORS preflight. A POST to a
// git path WITHOUT that content-type (a forgery attempt) must STILL be
// CSRF-blocked, so the exemption can't be abused as a blanket path bypass.
func TestAuthMiddleware_CSRF_GitPathForgedContentTypeStillBlocked(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})
	var reached bool
	handler := authMiddlewareTestHandler(&reached)

	req := newAuthRequest(t, "/api/v1/git/assistant.git/git-receive-pack")
	req.Method = http.MethodPost
	req.SetBasicAuth("git-access", "key-real")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded") // browser-forgeable
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "CSRF_BLOCKED",
		"a git-path POST without the git content-type must remain CSRF-blocked")
	require.False(t, reached, "forged request must not reach the handler")
}

// TestAuthMiddleware_CSRF_BasicSameOriginAllowed — the happy path
// for browser UI users. POST from the vornik UI itself is
// same-origin and must pass through.
func TestAuthMiddleware_CSRF_BasicSameOriginAllowed(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})
	var reached bool
	handler := authMiddlewareTestHandler(&reached)

	for _, site := range []string{"same-origin", "same-site", "none"} {
		t.Run(site, func(t *testing.T) {
			reached = false
			req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
			req.Method = http.MethodPost
			req.SetBasicAuth("api", "key-real")
			req.Header.Set("Sec-Fetch-Site", site)
			rec := httptest.NewRecorder()
			mw(handler).ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code,
				"Sec-Fetch-Site=%q POST via Basic must pass", site)
			require.True(t, reached, "handler must be invoked")
		})
	}
}

// TestAuthMiddleware_CSRF_GETAlwaysAllowed — GET requests don't
// change state, so even an evil-tab cross-site GET via Basic is
// harmless and must pass (refusing it would break read-only API
// access from anywhere).
func TestAuthMiddleware_CSRF_GETAlwaysAllowed(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})
	var reached bool
	handler := authMiddlewareTestHandler(&reached)

	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(m, func(t *testing.T) {
			reached = false
			req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
			req.Method = m
			req.SetBasicAuth("api", "key-real")
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			rec := httptest.NewRecorder()
			mw(handler).ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code,
				"%s cross-site via Basic must still pass (read-only is CSRF-safe)", m)
			require.True(t, reached)
		})
	}
}

// TestAuthMiddleware_CSRF_BearerBypassesGate — CLI / MCP / curl
// callers use Authorization: Bearer (or X-API-Key) which browsers
// never auto-attach cross-origin. They're CSRF-immune by design,
// so the gate must not block them even on cross-site signals.
func TestAuthMiddleware_CSRF_BearerBypassesGate(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})
	var reached bool
	handler := authMiddlewareTestHandler(&reached)

	req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
	req.Method = http.MethodPost
	req.Header.Set("Authorization", "Bearer key-real")
	// Hostile signals — gate must IGNORE these for Bearer requests.
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"Bearer-authed POST must skip CSRF gate even with cross-site signals")
	require.True(t, reached)
}

// TestAuthMiddleware_CSRF_OriginFallback — older browsers /
// non-browser clients without Sec-Fetch-Site fall back to Origin.
// Matching Origin host → pass; mismatched host → block; absent
// Origin → pass (conservative — browsers always send Origin on
// cross-origin mutations).
func TestAuthMiddleware_CSRF_OriginFallback(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})
	var reached bool
	handler := authMiddlewareTestHandler(&reached)

	makeReq := func(origin string) *http.Request {
		req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
		req.Method = http.MethodPost
		req.SetBasicAuth("api", "key-real")
		// Deliberately no Sec-Fetch-Site so we exercise the
		// Origin-fallback branch.
		req.Host = "vornik.example.com:8080"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		return req
	}

	t.Run("matching_origin_passes", func(t *testing.T) {
		reached = false
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, makeReq("https://vornik.example.com:8080"))
		require.Equal(t, http.StatusOK, rec.Code)
		require.True(t, reached)
	})

	t.Run("mismatched_origin_blocked", func(t *testing.T) {
		reached = false
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, makeReq("https://evil.example.com"))
		require.Equal(t, http.StatusForbidden, rec.Code)
		require.False(t, reached)
	})

	t.Run("absent_origin_blocked", func(t *testing.T) {
		// A3 (https://docs.vornik.io): a mutating
		// Basic request with NEITHER Sec-Fetch-Site NOR Origin now fails
		// closed (was previously fail-open). Non-browser Basic clients
		// must send Origin or use Bearer/X-API-Key.
		reached = false
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, makeReq(""))
		require.Equal(t, http.StatusForbidden, rec.Code,
			"no Sec-Fetch-Site and no Origin on a mutating Basic request must fail closed (A3)")
		require.Contains(t, rec.Body.String(), "CSRF_BLOCKED")
		require.False(t, reached)
	})
}

// TestAuthMiddleware_CSRF_A3_NoSignalsFailClosed is the focused A3
// regression (https://docs.vornik.io): the three
// behaviors the fix must hold together — Basic mutating with no signals
// blocks; the same request with Sec-Fetch-Site: same-origin passes; and
// a Bearer request with no signals still bypasses the gate entirely.
func TestAuthMiddleware_CSRF_A3_NoSignalsFailClosed(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})

	t.Run("basic_no_signals_blocked", func(t *testing.T) {
		var reached bool
		handler := authMiddlewareTestHandler(&reached)
		req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
		req.Method = http.MethodPost
		req.SetBasicAuth("api", "key-real")
		// No Sec-Fetch-Site, no Origin.
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code,
			"mutating Basic request with neither Sec-Fetch-Site nor Origin must 403 (A3)")
		require.Contains(t, rec.Body.String(), "CSRF_BLOCKED")
		require.False(t, reached)
	})

	t.Run("basic_same_origin_sec_fetch_passes", func(t *testing.T) {
		var reached bool
		handler := authMiddlewareTestHandler(&reached)
		req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
		req.Method = http.MethodPost
		req.SetBasicAuth("api", "key-real")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code,
			"Sec-Fetch-Site: same-origin must still pass via Basic")
		require.True(t, reached)
	})

	t.Run("bearer_no_signals_still_bypasses", func(t *testing.T) {
		var reached bool
		handler := authMiddlewareTestHandler(&reached)
		req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
		req.Method = http.MethodPost
		req.Header.Set("Authorization", "Bearer key-real")
		// No Sec-Fetch-Site, no Origin — Bearer must still bypass the gate.
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code,
			"Bearer request with no CSRF signals must bypass the gate (A3 must not regress Bearer)")
		require.True(t, reached)
	})
}

// TestAuthMiddleware_WebhookWithHMACOnly is the headline post-audit
// behaviour: a webhook delivery without an API key still passes the
// gate when an HMAC signature header is present, deferring the
// signature verification to IngestWebhook against the per-source
// secret. Pre-fix the same path was treated as fully public.
func TestAuthMiddleware_WebhookWithHMACOnly(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-1": nil},
	})
	for _, header := range []string{"X-Vornik-Signature", "X-Hub-Signature-256"} {
		var reached bool
		req := newAuthRequest(t, "/api/v1/webhooks/proj-a/github")
		req.Header.Set(header, "sha256=deadbeef")
		rec := httptest.NewRecorder()
		mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req)
		require.Truef(t, reached, "header %q must let request reach handler", header)
		require.Equalf(t, http.StatusOK, rec.Code, "header %q middleware response", header)
	}
}

// TestAuthMiddleware_GitHubAppWebhookWithHMACOnly — the first-class
// GitHub App route is HMAC-authenticated too. It must pass through
// API auth on signature presence so internal/github can verify the
// body against the app webhook secret.
func TestAuthMiddleware_GitHubAppWebhookWithHMACOnly(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-1": nil},
	})
	var reached bool
	req := newAuthRequest(t, "/api/v1/github-app/webhook")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req)
	require.True(t, reached, "GitHub App HMAC delivery must reach handler")
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestAuthMiddleware_WebhookRejectsWhenNoCredentials confirms the
// regression class fixed by the audit: a webhook URL with no API key
// AND no HMAC header is rejected at the middleware, so an
// unauthenticated probe can't enumerate project / source names by
// distinct error codes from the handler.
func TestAuthMiddleware_WebhookRejectsWhenNoCredentials(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-1": nil},
	})
	var reached bool
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, newAuthRequest(t, "/api/v1/webhooks/proj-a/github"))
	require.False(t, reached, "handler must not be reached for unauthenticated webhook")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestAuthMiddleware_WebhookWithAPIKey — operator path. A valid API
// key works for webhooks too, so internal scripts and the Telegram
// bridge can post webhook-shaped events without HMAC overhead.
func TestAuthMiddleware_WebhookWithAPIKey(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})
	var reached bool
	req := newAuthRequest(t, "/api/v1/webhooks/proj-a/github")
	req.Header.Set("X-API-Key", "key-real")
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req)
	require.True(t, reached, "valid API key should reach the handler")
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestAuthMiddleware_WebhookInvalidAPIKeyStillRejected — an API key
// that isn't in the configured set must be rejected even when an
// HMAC header is also present. Falling back silently to HMAC would
// create a confusing "looks authenticated, isn't" failure mode; the
// caller should fix their key, not have it ignored.
func TestAuthMiddleware_WebhookInvalidAPIKeyStillRejected(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})
	var reached bool
	req := newAuthRequest(t, "/api/v1/webhooks/proj-a/github")
	req.Header.Set("X-API-Key", "key-impostor")
	req.Header.Set("X-Vornik-Signature", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req)
	require.False(t, reached)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestProjectAuthMiddleware_AllowsScopedProject verifies the per-key
// project scope check on a path-extracted projectID.
func TestProjectAuthMiddleware_AllowsScopedProject(t *testing.T) {
	mw := ProjectAuthMiddleware()
	var reached bool
	req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
	ctx := context.WithValue(req.Context(), projectIDKey, []string{"proj-a", "proj-b"})
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req.WithContext(ctx))
	require.True(t, reached)
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestProjectAuthMiddleware_DeniesOtherProject confirms a key with
// scope ["proj-a"] cannot reach a /projects/proj-b/* route.
func TestProjectAuthMiddleware_DeniesOtherProject(t *testing.T) {
	mw := ProjectAuthMiddleware()
	var reached bool
	req := newAuthRequest(t, "/api/v1/projects/proj-b/tasks")
	ctx := context.WithValue(req.Context(), projectIDKey, []string{"proj-a"})
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req.WithContext(ctx))
	require.False(t, reached)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestProjectAuthMiddleware_DeniesCompanionKeyOnProjectREST(t *testing.T) {
	mw := ProjectAuthMiddleware()
	var reached bool
	req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
	ctx := context.WithValue(req.Context(), projectIDKey, []string{"proj-a"})
	ctx = context.WithValue(ctx, apiKeyClientKindKey, "claude-code")
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req.WithContext(ctx))
	require.False(t, reached)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "/api/v1/mcp/companion")
}

// TestProjectAuthMiddleware_DeniesCompanionKeyOnA2ARoute pins that the
// companion block covers the A2A surface, not just /api/v1/projects/.
// A2A task submission is the same allowlist/budget-bypass class as the
// REST task route, and extractProjectID returns a non-empty ID for it.
func TestProjectAuthMiddleware_DeniesCompanionKeyOnA2ARoute(t *testing.T) {
	mw := ProjectAuthMiddleware()
	var reached bool
	req := newAuthRequest(t, "/a2a/v1/agents/proj-a/research/tasks")
	ctx := context.WithValue(req.Context(), projectIDKey, []string{"proj-a"})
	ctx = context.WithValue(ctx, apiKeyClientKindKey, "claude-code")
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req.WithContext(ctx))
	require.False(t, reached)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "/api/v1/mcp/companion")
}

func TestProjectAuthMiddleware_AllowsCompanionKeyOnNonProjectRoute(t *testing.T) {
	mw := ProjectAuthMiddleware()
	var reached bool
	req := newAuthRequest(t, "/api/v1/mcp/companion")
	ctx := context.WithValue(req.Context(), apiKeyClientKindKey, "claude-code")
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req.WithContext(ctx))
	require.True(t, reached)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestProjectAuthMiddleware_DeniesOtherProjectOnA2AAgentRoute(t *testing.T) {
	mw := ProjectAuthMiddleware()
	var reached bool
	req := newAuthRequest(t, "/a2a/v1/agents/proj-b/research/tasks")
	ctx := context.WithValue(req.Context(), projectIDKey, []string{"proj-a"})
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req.WithContext(ctx))
	require.False(t, reached)
	require.Equal(t, http.StatusForbidden, rec.Code)
}
