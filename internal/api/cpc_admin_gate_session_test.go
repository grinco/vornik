package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
)

// stampSessionIdentity returns a context carrying a chain-resolved
// session Identity with the given role — the shape AuthMiddleware
// stamps for a browser session. Auth is marked enabled so the gate
// doesn't short-circuit through the auth-disabled bypass.
func stampSessionIdentity(role string) context.Context {
	ctx := context.WithValue(context.Background(), authEnabledKey, true)
	id := &auth.Identity{
		Backend: "session",
		Extra: map[string]any{
			auth.ExtraSessionRole: role,
			auth.ExtraSessionID:   "sess-x",
		},
	}
	return context.WithValue(ctx, identityKey, id)
}

func TestRequireAdminGate_SessionAdminPasses(t *testing.T) {
	s := &Server{adminConfig: config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-vornik-admin"}}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cpc", nil).
		WithContext(stampSessionIdentity("admin"))
	rec := httptest.NewRecorder()
	if !s.requireAdminGate(rec, req) {
		t.Fatalf("session-admin should pass the gate; status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRequireAdminGate_SessionUserForbidden(t *testing.T) {
	s := &Server{adminConfig: config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-vornik-admin"}}}
	// A non-admin session presents no api key → the gate falls through
	// to the key check and 401s (no key). This pins that role!=admin
	// does NOT smuggle past the gate.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cpc", nil).
		WithContext(stampSessionIdentity("user"))
	rec := httptest.NewRecorder()
	if s.requireAdminGate(rec, req) {
		t.Fatal("session-user must not pass the admin gate")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no admin credential)", rec.Code)
	}
}

func TestRequireAdminGate_KeyPathUnchanged(t *testing.T) {
	const adminKey = "sk-vornik-admin"
	s := &Server{adminConfig: config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey}}}
	ctx := context.WithValue(context.Background(), authEnabledKey, true)
	ctx = context.WithValue(ctx, apiKeyKey, adminKey)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cpc", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	if !s.requireAdminGate(rec, req) {
		t.Fatalf("admin key should still pass; status=%d", rec.Code)
	}
}
