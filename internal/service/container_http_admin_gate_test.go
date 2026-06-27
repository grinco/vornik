// End-to-end regression tests for the /ui/admin gate WRAP ORDER.
//
// Incident (pre-existing slice-1 wiring, found in the github-login
// phase-3 review, fixed 2026-06-05): admin.Middleware was wrapped
// OUTSIDE uiSubtreeHandler, so it matched the UN-stripped path
// "/ui/admin/..." against its "/admin" prefix — pathInGate never
// fired and every /ui/admin/* page was reachable by ANY authenticated
// caller. The gate's own unit tests passed because they fed it
// already-stripped paths. These tests therefore drive the REAL
// production chain (api.AuthMiddleware → wrapUIAdminGate, i.e. the
// gate inside the /ui strip) end-to-end, exactly as initHTTPServer
// wires it.
package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
)

// gateStubSessionBackend admits one fixed cookie token as a session
// with the configured role.
type gateStubSessionBackend struct {
	token string
	role  string
}

func (b *gateStubSessionBackend) Name() string { return "session" }

func (b *gateStubSessionBackend) Authenticate(_ context.Context, cred auth.Credential) (*auth.Identity, error) {
	if cred.SessionToken == "" || cred.SessionToken != b.token {
		return nil, auth.ErrNoCredential
	}
	return &auth.Identity{
		Subject:  "user:u1",
		Projects: []string{"*"},
		Extra: map[string]any{
			auth.ExtraSessionRole:   b.role,
			auth.ExtraSessionID:     "sess-1",
			auth.ExtraSessionUserID: "u1",
		},
	}, nil
}

// buildGateChain assembles the production order: AuthMiddleware
// outermost, then the admin gate inside the /ui strip, then a stub UI
// recording what it served.
func buildGateChain(t *testing.T, authEnabled bool, sb auth.Backend) (http.Handler, *string) {
	t.Helper()
	var served string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	adminCfg := config.AdminConfig{Enabled: true, AllowedKeys: []string{"admin-key"}}
	h := wrapUIAdminGate(zerolog.Nop(), adminCfg, inner)
	h = api.AuthMiddleware(api.AuthConfig{
		Enabled: authEnabled,
		StaticAPIKeys: map[string][]string{
			"admin-key": nil,
			"plain-key": {"proj-a"},
		},
		SessionBackend: sb,
	})(h)
	return h, &served
}

func gateGet(h http.Handler, path, bearer, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "vornik_session", Value: cookie})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestUIAdminGate_NonAdminKeyBlocked — THE incident pin: an
// authenticated but non-admin caller must NOT reach /ui/admin/*.
func TestUIAdminGate_NonAdminKeyBlocked(t *testing.T) {
	h, served := buildGateChain(t, true, nil)
	rec := gateGet(h, "/ui/admin/audit", "plain-key", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (gate must engage on /ui/admin/*); served=%q body=%q",
			rec.Code, *served, rec.Body.String())
	}
	if *served != "" {
		t.Fatalf("inner UI handler reached (%q) — gate disengaged", *served)
	}
}

// TestUIAdminGate_AdminKeyPasses — the allowlisted key still works.
func TestUIAdminGate_AdminKeyPasses(t *testing.T) {
	h, served := buildGateChain(t, true, nil)
	rec := gateGet(h, "/ui/admin/audit", "admin-key", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if *served != "/admin/audit" {
		t.Fatalf("inner path = %q, want stripped /admin/audit", *served)
	}
}

// TestUIAdminGate_SessionAdminPasses / _SessionUserBlocked — the
// phase-3 role split, through the full chain.
func TestUIAdminGate_SessionAdminPasses(t *testing.T) {
	h, served := buildGateChain(t, true, &gateStubSessionBackend{token: "tok", role: "admin"})
	rec := gateGet(h, "/ui/admin/audit", "", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if *served != "/admin/audit" {
		t.Fatalf("inner path = %q, want /admin/audit", *served)
	}
}

func TestUIAdminGate_SessionUserBlocked(t *testing.T) {
	h, served := buildGateChain(t, true, &gateStubSessionBackend{token: "tok", role: "user"})
	rec := gateGet(h, "/ui/admin/audit", "", "tok")
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401/403 for a session user; served=%q", rec.Code, *served)
	}
	if *served != "" {
		t.Fatalf("inner UI handler reached (%q) for a session user", *served)
	}
}

// TestUIAdminGate_AuthOffAdmits — homelab mode: gate admits everyone
// (existing contract, must not regress).
func TestUIAdminGate_AuthOffAdmits(t *testing.T) {
	h, served := buildGateChain(t, false, nil)
	rec := gateGet(h, "/ui/admin/audit", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with auth off", rec.Code)
	}
	if *served != "/admin/audit" {
		t.Fatalf("inner path = %q, want /admin/audit", *served)
	}
}

// TestUIAdminGate_NonAdminPathUnaffected — /ui/tasks passes for a
// plain key; the gate only bites the admin subtree.
func TestUIAdminGate_NonAdminPathUnaffected(t *testing.T) {
	h, served := buildGateChain(t, true, nil)
	rec := gateGet(h, "/ui/tasks", "plain-key", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on a non-admin path; body=%q", rec.Code, rec.Body.String())
	}
	if *served != "/tasks" {
		t.Fatalf("inner path = %q, want /tasks", *served)
	}
}
