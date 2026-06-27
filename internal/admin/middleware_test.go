package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/config"
)

// behaviourMatrix locks the admin-ui-design §10 contract:
//
//	admin.enabled=false                  → 404
//	admin.enabled=true, no API key       → 401
//	admin.enabled=true, non-admin key    → 403 ("admin scope required")
//	admin.enabled=true, admin key        → 200 + IsAdmin=true
//
// Each row is tested both on the landing route (`/admin/`) and on
// a nested route (`/admin/audit`) so a regression in the prefix
// matching can't slip past one path.
func TestMiddleware_BehaviourMatrix(t *testing.T) {
	const adminKey = "sk-vornik-admin-1"
	const userKey = "sk-vornik-user-1"

	cases := []struct {
		name         string
		cfg          config.AdminConfig
		key          string
		path         string
		sessionAdmin bool // when true, the session-admin checker returns true
		wantCode     int
		wantBody     string // substring; empty skips body check
		wantAdmin    bool
	}{
		{
			name:     "disabled returns 404 on landing",
			cfg:      config.AdminConfig{Enabled: false},
			key:      adminKey,
			path:     "/admin/",
			wantCode: http.StatusNotFound,
		},
		{
			name:     "disabled returns 404 on nested",
			cfg:      config.AdminConfig{Enabled: false},
			key:      adminKey,
			path:     "/admin/audit",
			wantCode: http.StatusNotFound,
		},
		{
			name:     "enabled+no-key returns 401 on landing",
			cfg:      config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey}},
			key:      "",
			path:     "/admin/",
			wantCode: http.StatusUnauthorized,
			wantBody: "admin authentication required",
		},
		{
			name:     "enabled+no-key returns 401 on nested",
			cfg:      config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey}},
			key:      "",
			path:     "/admin/audit",
			wantCode: http.StatusUnauthorized,
		},
		{
			name:     "enabled+non-admin returns 403 on landing",
			cfg:      config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey}},
			key:      userKey,
			path:     "/admin/",
			wantCode: http.StatusForbidden,
			wantBody: "admin scope required",
		},
		{
			name:     "enabled+non-admin returns 403 on nested",
			cfg:      config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey}},
			key:      userKey,
			path:     "/admin/audit",
			wantCode: http.StatusForbidden,
		},
		{
			name:      "enabled+admin passes through on landing",
			cfg:       config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey}},
			key:       adminKey,
			path:      "/admin/",
			wantCode:  http.StatusOK,
			wantAdmin: true,
		},
		{
			name:      "enabled+admin passes through on nested",
			cfg:       config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey}},
			key:       adminKey,
			path:      "/admin/audit",
			wantCode:  http.StatusOK,
			wantAdmin: true,
		},
		{
			// Session-admin (no api-key) passes the gate — github-login
			// phase 3.
			name:         "enabled+session-admin passes through on nested",
			cfg:          config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey}},
			key:          "",
			path:         "/admin/audit",
			sessionAdmin: true,
			wantCode:     http.StatusOK,
			wantAdmin:    true,
		},
		{
			// A session that is NOT admin and presents no key is a
			// non-admin caller → 401 (no key) keeps today's matrix.
			name:     "enabled+session-non-admin no key returns 401",
			cfg:      config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey}},
			key:      "",
			path:     "/admin/audit",
			wantCode: http.StatusUnauthorized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extract := func(_ *http.Request) string { return tc.key }
			sessionAdmin := func(_ *http.Request) bool { return tc.sessionAdmin }
			var sawAdmin bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sawAdmin = IsAdminFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			})

			handler := Middleware(tc.cfg, extract, nil, sessionAdmin, "/admin")(next)
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status: want %d, got %d (body=%q)", tc.wantCode, rec.Code, rec.Body.String())
			}
			if tc.wantBody != "" && !contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body: want substring %q, got %q", tc.wantBody, rec.Body.String())
			}
			if sawAdmin != tc.wantAdmin {
				t.Fatalf("IsAdmin: want %v, got %v", tc.wantAdmin, sawAdmin)
			}
		})
	}
}

// TestMiddleware_NonAdminPaths_PassThrough verifies that paths
// outside the gate's prefix are routed straight through.
func TestMiddleware_NonAdminPaths_PassThrough(t *testing.T) {
	cfg := config.AdminConfig{Enabled: false}
	extract := func(_ *http.Request) string { return "" }
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(cfg, extract, nil, nil, "/admin")(next)
	req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("non-admin path was not routed through")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
}

// TestMiddleware_AdminOnNonAdminPath_StampsIsAdmin checks that when
// an admin browses the rest of the UI, the gate still marks them so
// the shared nav partial renders the Admin link site-wide.
func TestMiddleware_AdminOnNonAdminPath_StampsIsAdmin(t *testing.T) {
	const adminKey = "sk-vornik-admin-1"
	cfg := config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey}}
	extract := func(_ *http.Request) string { return adminKey }

	var sawAdmin bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAdmin = IsAdminFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(cfg, extract, nil, nil, "/admin")(next)
	req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !sawAdmin {
		t.Fatal("admin browsing /tasks should still have IsAdmin=true on context")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
}

// TestMiddleware_PathBoundary verifies that pathInGate rejects a
// look-alike prefix ("/administrate") and accepts the bare prefix
// without a slash plus the trailing-slash form.
func TestMiddleware_PathBoundary(t *testing.T) {
	if pathInGate("/administrate", "/admin") {
		t.Fatal("look-alike path should not match")
	}
	if !pathInGate("/admin", "/admin") {
		t.Fatal("bare prefix should match")
	}
	if !pathInGate("/admin/audit", "/admin") {
		t.Fatal("trailing-slash form should match")
	}
	if !pathInGate("/anything", "") {
		t.Fatal("empty prefix should match everything")
	}
}

// TestPrincipalFromContext returns empty on a request the gate
// hasn't stamped.
func TestPrincipalFromContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	if got := PrincipalFromContext(req.Context()); got != "" {
		t.Fatalf("unstamped context: want \"\", got %q", got)
	}
}

func TestMiddleware_PrincipalIsFingerprintNotRawKey(t *testing.T) {
	const adminKey = "sk-vornik-admin-1"
	cfg := config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey}}
	extract := func(_ *http.Request) string { return adminKey }
	var got string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(cfg, extract, nil, nil, "/admin")(next)
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	want := "api_key_sha256:" + apikey.Hash(adminKey)[:16]
	if got != want {
		t.Fatalf("principal = %q, want %q", got, want)
	}
	if contains(got, adminKey) {
		t.Fatalf("principal leaked raw admin key: %q", got)
	}
}

// TestMiddleware_AuthDisabled_BypassesGate locks the
// single-operator semantics introduced 2026-05-24: when API
// auth is off the admin gate disengages — every request is
// stamped IsAdmin=true and passed through, regardless of
// whether admin.Enabled was set or what key (if any) was
// extracted. Without this, self-gating handlers like
// /ui/memory/operators returned 403 "admin scope required"
// for every caller on auth-disabled deployments.
func TestMiddleware_AuthDisabled_BypassesGate(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.AdminConfig
		path string
	}{
		{"admin gate enabled + gated path", config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-x"}}, "/admin/audit"},
		{"admin gate disabled + non-gated path", config.AdminConfig{Enabled: false}, "/memory/operators"},
		{"admin gate enabled + non-gated path", config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-x"}}, "/memory/operators"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extract := func(_ *http.Request) string { return "" }
			authOff := func(_ *http.Request) bool { return false }
			var sawAdmin bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sawAdmin = IsAdminFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})
			handler := Middleware(tc.cfg, extract, authOff, nil, "/admin")(next)
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (auth-disabled bypass)", rec.Code)
			}
			if !sawAdmin {
				t.Fatalf("IsAdmin = false; auth-disabled bypass must stamp IsAdmin=true")
			}
		})
	}
}

// TestMiddleware_NilAuthChecker_FailsClosedToEnabled verifies
// the nil-safe default: a daemon that forgets to wire the
// checker behaves as if auth is on (the safer choice for
// production deployments that DO have auth enabled).
func TestMiddleware_NilAuthChecker_FailsClosedToEnabled(t *testing.T) {
	cfg := config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}
	extract := func(_ *http.Request) string { return "" }
	handler := Middleware(cfg, extract, nil, nil, "/admin")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/admin/audit", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// No key, gate engaged → 401, not 200.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nil checker should fail closed to enabled; got %d", rec.Code)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
