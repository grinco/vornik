package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/auth"
)

// stubSessionBackend is a fake auth.Backend standing in for the real
// SessionBackend. It admits exactly the cookie token it was
// configured with, returning a session Identity; any other token
// (including the dead-cookie case) returns ErrNoCredential so the
// chain falls through to the key backends — mirroring the real
// backend's "stale cookie must not block a Bearer" contract.
type stubSessionBackend struct {
	token    string   // the cookie value this backend accepts
	projects []string // Principal.Projects to stamp on the identity
	role     string   // session role
	sessID   string   // ui_sessions.id
	userID   string   // users.id
	rejected bool     // when true, the configured token is hard-rejected
	calls    int      // number of Authenticate invocations
}

func (b *stubSessionBackend) Name() string { return "session" }

func (b *stubSessionBackend) Authenticate(_ context.Context, cred auth.Credential) (*auth.Identity, error) {
	b.calls++
	if cred.SessionToken == "" || cred.SessionToken != b.token {
		return nil, auth.ErrNoCredential
	}
	if b.rejected {
		return nil, auth.ErrUnauthorized
	}
	return &auth.Identity{
		Subject:     "user:" + b.userID,
		Projects:    b.projects,
		DisplayName: "Test User",
		Extra: map[string]any{
			auth.ExtraSessionRole:   b.role,
			auth.ExtraSessionID:     b.sessID,
			auth.ExtraSessionUserID: b.userID,
		},
	}, nil
}

func sessionCfg(backend auth.Backend) AuthConfig {
	return AuthConfig{
		Enabled:        true,
		StaticAPIKeys:  map[string][]string{"static-key-1": {"proj-static"}},
		SessionBackend: backend,
	}
}

// TestSessionBackend_PrependedFirst proves the session backend wins
// over a key backend when both could match — order matters because
// the cookie is the unambiguous, cheap-to-check credential.
func TestSessionBackend_PrependedFirst(t *testing.T) {
	sb := &stubSessionBackend{token: "good", projects: []string{"proj-a"}, role: "user", sessID: "sess-1", userID: "u1"}
	var got *auth.Identity
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-a/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "good"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got == nil || got.Backend != "session" {
		t.Fatalf("identity = %+v, want backend session", got)
	}
}

// TestSessionBackend_CookieExtracted confirms the middleware copies
// the vornik_session cookie into Credential.SessionToken.
func TestSessionBackend_CookieExtracted(t *testing.T) {
	sb := &stubSessionBackend{token: "tok42", projects: []string{"proj-a"}, role: "user", sessID: "s", userID: "u1"}
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-a/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "tok42"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if sb.calls == 0 {
		t.Fatal("session backend was never invoked — cookie not extracted")
	}
}

// TestSessionBackend_DeadCookieFallsThroughToBearer pins decision #1:
// a stale cookie must NOT block a valid Bearer on the same request.
func TestSessionBackend_DeadCookieFallsThroughToBearer(t *testing.T) {
	sb := &stubSessionBackend{token: "good", role: "user"}
	var got *auth.Identity
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-static/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "stale-dead-cookie"})
	req.Header.Set("Authorization", "Bearer static-key-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got == nil || got.Backend != "static-keys" {
		t.Fatalf("identity = %+v, want fallthrough to static-keys", got)
	}
}

// TestSessionBackend_MissingCredButCookiePresent confirms the
// missing-credential 401 does not fire when a session cookie is
// present and the backend is configured — the chain handles it.
func TestSessionBackend_MissingCredButCookiePresent(t *testing.T) {
	sb := &stubSessionBackend{token: "good", projects: []string{"proj-a"}, role: "user", sessID: "s", userID: "u1"}
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// No Authorization header; only the cookie.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-a/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "good"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// TestSessionBackend_CSRFGate proves a cross-site mutating request
// carrying a session cookie is blocked (browsers auto-attach cookies
// — the same CSRF vector as Basic Auth).
func TestSessionBackend_CSRFGate(t *testing.T) {
	sb := &stubSessionBackend{token: "good", projects: []string{"proj-a"}, role: "user", sessID: "s", userID: "u1"}
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj-a/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "good"})
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (CSRF)", rec.Code)
	}

	// Same-origin POST with the cookie is allowed.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj-a/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "good"})
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin status = %d, want 200", rec.Code)
	}
}

// TestSessionBackend_RedirectMissingCredential covers the browser
// redirect at the missing-credential 401 site: a UI GET with no
// credential redirects to /ui/login and clears any stale cookie.
func TestSessionBackend_RedirectMissingCredential(t *testing.T) {
	sb := &stubSessionBackend{token: "good"}
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))
	req := httptest.NewRequest(http.MethodGet, "/ui/dashboard", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/ui/login?next="+"%2Fui%2Fdashboard" {
		t.Fatalf("Location = %q", loc)
	}
	// Stale cookie cleared.
	foundClear := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "vornik_session" && c.MaxAge < 0 {
			foundClear = true
		}
	}
	if !foundClear {
		t.Error("stale vornik_session cookie not cleared on redirect")
	}
}

// TestSessionBackend_RedirectChainFailure covers the redirect at the
// chain-failure 401 site: a UI GET with a dead cookie (no other
// credential) redirects to login rather than 401ing.
func TestSessionBackend_RedirectChainFailure(t *testing.T) {
	sb := &stubSessionBackend{token: "good"}
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))
	req := httptest.NewRequest(http.MethodGet, "/ui/projects", nil)
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "dead"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
}

// TestSessionBackend_NoRedirectForAPIClients confirms a non-UI / non-
// browser request keeps today's 401 (the flag-equivalence contract).
func TestSessionBackend_NoRedirectForAPIClients(t *testing.T) {
	sb := &stubSessionBackend{token: "good"}
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))
	// JSON API path, no Accept text/html → 401, no redirect.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p/tasks", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("api path status = %d, want 401", rec.Code)
	}

	// /ui GET but no text/html Accept (e.g. fetch()) → 401.
	req = httptest.NewRequest(http.MethodGet, "/ui/dashboard", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-html /ui status = %d, want 401", rec.Code)
	}
}

// TestSessionBackend_MethodKeyBreakGlass confirms a /ui page reached
// with ?method=key falls through to the 401+WWW-Authenticate dialog
// instead of redirecting (the break-glass path).
func TestSessionBackend_MethodKeyBreakGlass(t *testing.T) {
	sb := &stubSessionBackend{token: "good"}
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))
	req := httptest.NewRequest(http.MethodGet, "/ui/dashboard?method=key", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 break-glass", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("expected WWW-Authenticate challenge for break-glass")
	}
}

// TestSessionBackend_LoginPagePublic confirms /ui/login is reachable
// unauthenticated (the public exemption).
func TestSessionBackend_LoginPagePublic(t *testing.T) {
	sb := &stubSessionBackend{token: "good"}
	served := false
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/ui/login", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !served || rec.Code != http.StatusOK {
		t.Fatalf("served=%v status=%d, want public 200", served, rec.Code)
	}
}

// TestSessionBackend_StampAdminAllAccess proves an admin / star-group
// session (["*"]) stamps NO projectIDKey (legacy all-access) so the
// project IDOR guard waves it through every project.
func TestSessionBackend_StampAdminAllAccess(t *testing.T) {
	sb := &stubSessionBackend{token: "good", projects: []string{"*"}, role: "admin", sessID: "s", userID: "u1"}
	allowed := false
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed = requestAllowsProject(r, "any-project")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/any-project/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "good"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !allowed {
		t.Fatalf("admin all-access: status=%d allowed=%v", rec.Code, allowed)
	}
}

// TestSessionBackend_StampScopedProjects proves a scoped user gets
// exactly its project list stamped.
func TestSessionBackend_StampScopedProjects(t *testing.T) {
	sb := &stubSessionBackend{token: "good", projects: []string{"proj-a", "proj-b"}, role: "user", sessID: "s", userID: "u1"}
	var allowA, allowC bool
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowA = requestAllowsProject(r, "proj-a")
		allowC = requestAllowsProject(r, "proj-c")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-a/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "good"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !allowA {
		t.Error("scoped user denied its own project proj-a")
	}
	if allowC {
		t.Error("scoped user granted foreign project proj-c")
	}
}

// TestSessionBackend_AwaitingAccessDeniedAllProjects is the critical
// regression guard: a session user with ZERO projects (awaiting
// access) must NOT receive legacy all-access. Without the no-access
// sentinel, requestAllowsProject's "empty = all" branch would hand
// them every project.
func TestSessionBackend_AwaitingAccessDeniedAllProjects(t *testing.T) {
	sb := &stubSessionBackend{token: "good", projects: nil, role: "user", sessID: "s", userID: "u1"}
	var allowed bool
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed = requestAllowsProject(r, "proj-a")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-a/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "good"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if allowed {
		t.Fatal("awaiting-access session user was granted all-access — sentinel missing")
	}
}

// TestSessionBackend_ContextHelpers covers SessionRoleFromContext +
// SessionIDFromContext.
func TestSessionBackend_ContextHelpers(t *testing.T) {
	sb := &stubSessionBackend{token: "good", projects: []string{"*"}, role: "admin", sessID: "sess-99", userID: "u7"}
	var role, sid string
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role = SessionRoleFromContext(r.Context())
		sid = SessionIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "good"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if role != "admin" {
		t.Errorf("SessionRoleFromContext = %q, want admin", role)
	}
	if sid != "sess-99" {
		t.Errorf("SessionIDFromContext = %q, want sess-99", sid)
	}
	// Nil-safe.
	//nolint:staticcheck // SA1012: nil is deliberate — pins nil-safety of both helpers.
	if SessionRoleFromContext(nil) != "" || SessionIDFromContext(nil) != "" {
		t.Error("nil ctx must yield empty role/session id")
	}
}

// TestSessionBackend_RejectedCookieHardStops proves a recognized-but-
// rejected session (ErrUnauthorized, e.g. disabled user) terminates
// the chain — it does NOT fall through to the key backends.
func TestSessionBackend_RejectedCookieHardStops(t *testing.T) {
	sb := &stubSessionBackend{token: "good", rejected: true}
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached on hard-stop")
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-static/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "good"})
	// A valid Bearer is also present, but the hard-stop wins.
	req.Header.Set("Authorization", "Bearer static-key-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 hard-stop", rec.Code)
	}
}

// TestSessionBackend_NilBackendIgnoresCookie confirms that with no
// SessionBackend configured, a vornik_session cookie has zero effect:
// no CSRF gate, no redirect, today's 401 behaviour.
func TestSessionBackend_NilBackendIgnoresCookie(t *testing.T) {
	cfg := AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"static-key-1": {"proj-static"}},
		// SessionBackend nil.
	}
	h := AuthMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))
	// UI GET with a cookie but no backend → 401, NOT a redirect.
	req := httptest.NewRequest(http.MethodGet, "/ui/dashboard", nil)
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "whatever"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no session feature)", rec.Code)
	}
}

// --- auth-disabled session resolution -------------------------------
//
// With api.auth_enabled=false the middleware enforces nothing, but it
// still RESOLVES a presented session cookie (same spirit as the
// disabled-path DB-key resolution above it: not for security — the
// request passes anyway — but so the UI knows who is signed in and
// logout can revoke the right row). Regression context: 2026-06-05,
// operator logged in via GitHub with auth disabled and no Sign out
// button rendered anywhere — the session was invisible to the app.

// TestSessionBackend_ResolvedWhenAuthDisabled: cookie → identity
// stamped (role + session id readable downstream) even with
// Enabled=false.
func TestSessionBackend_ResolvedWhenAuthDisabled(t *testing.T) {
	sb := &stubSessionBackend{token: "good", projects: []string{"proj-a"}, role: "admin", sessID: "sess-9", userID: "u1"}
	cfg := sessionCfg(sb)
	cfg.Enabled = false
	var got *auth.Identity
	var role, sessID string
	h := AuthMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = IdentityFromContext(r.Context())
		role = SessionRoleFromContext(r.Context())
		sessID = SessionIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "good"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got == nil || got.Backend != "session" {
		t.Fatalf("identity = %+v, want session identity stamped", got)
	}
	if role != "admin" || sessID != "sess-9" {
		t.Fatalf("role=%q sessID=%q, want admin/sess-9", role, sessID)
	}
}

// TestSessionBackend_AuthDisabledNoProjectStamp: the disabled path
// must NOT stamp projectIDKey — auth-off means no project scoping;
// stamping the no-access sentinel for an awaiting-access user would
// close pages that are open today.
func TestSessionBackend_AuthDisabledNoProjectStamp(t *testing.T) {
	// Zero projects = the awaiting-access shape that triggers the
	// sentinel on the ENABLED chain path.
	sb := &stubSessionBackend{token: "good", projects: nil, role: "user", sessID: "s", userID: "u1"}
	cfg := sessionCfg(sb)
	cfg.Enabled = false
	var hadProjects bool
	h := AuthMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadProjects = r.Context().Value(projectIDKey).([]string)
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "good"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if hadProjects {
		t.Fatal("projectIDKey stamped on the auth-disabled path — must not enforce scoping when auth is off")
	}
}

// TestSessionBackend_AuthDisabledDeadCookieUnstamped: a dead cookie
// with auth off passes through unstamped — no 401, no redirect, no
// identity. (The enabled path's redirect-to-login must not leak into
// disabled mode.)
func TestSessionBackend_AuthDisabledDeadCookieUnstamped(t *testing.T) {
	sb := &stubSessionBackend{token: "good", role: "user"}
	cfg := sessionCfg(sb)
	cfg.Enabled = false
	var got *auth.Identity
	h := AuthMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks", nil)
	req.Header.Set("Accept", "text/html") // would trigger redirect on the enabled path
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "stale"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 pass-through (got body=%s)", rec.Code, rec.Body.String())
	}
	if got != nil {
		t.Fatalf("identity = %+v, want nil for a dead cookie", got)
	}
}

// TestSessionBackend_AuthDisabledRejectedSessionUnstamped: a session
// whose user is disabled (backend returns ErrUnauthorized) passes
// through UNSTAMPED with auth off — disabled-user enforcement only
// means something once auth is enabled.
func TestSessionBackend_AuthDisabledRejectedSessionUnstamped(t *testing.T) {
	sb := &stubSessionBackend{token: "good", role: "user", rejected: true}
	cfg := sessionCfg(sb)
	cfg.Enabled = false
	var got *auth.Identity
	h := AuthMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "good"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 pass-through", rec.Code)
	}
	if got != nil {
		t.Fatalf("identity = %+v, want nil for a rejected session", got)
	}
}
