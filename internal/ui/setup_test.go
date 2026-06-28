package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/onboarding"
)

func TestSetupPage_Renders(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}))
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Installation setup") {
		t.Fatalf("setup page missing expected heading, body=%s", rec.Body.String())
	}
}

// setupAuthBackend is a minimal auth.SessionBackend that stamps the given
// role into the resolved identity. Used only in tests.
type setupAuthBackend struct{ role string }

func (b setupAuthBackend) Name() string { return "session" }
func (b setupAuthBackend) Authenticate(context.Context, auth.Credential) (*auth.Identity, error) {
	return &auth.Identity{
		Backend: "session",
		Extra:   map[string]any{auth.ExtraSessionRole: b.role},
	}, nil
}

// setupAuthRequest creates an HTTP request that has been processed by the
// auth middleware, so SessionRoleFromContext returns the given role.
func setupAuthRequest(method, target, role string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "session"})
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	var captured *http.Request
	api.AuthMiddleware(api.AuthConfig{
		Enabled:        true,
		SessionBackend: setupAuthBackend{role: role},
	})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = r
	})).ServeHTTP(httptest.NewRecorder(), req)
	return captured
}

// TestSetupPage_BlocksProjectScopedUser verifies that a project-scoped
// (RoleUser) browser session is denied access to the installation setup
// page. The setup page mutates daemon-wide config and creates projects —
// operations restricted to admin scope.
func TestSetupPage_BlocksProjectScopedUser(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}))
	req := setupAuthRequest(http.MethodGet, "/setup", "user")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for RoleUser on /ui/setup", rec.Code)
	}
}

// TestSetupPage_AdminCanAccess verifies that an admin session can access
// the setup page.
func TestSetupPage_AdminCanAccess(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}))
	req := setupAuthRequest(http.MethodGet, "/setup", "admin")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for admin on /ui/setup", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Installation setup") {
		t.Fatalf("setup page missing expected heading for admin")
	}
}

func TestSetupPage_RendersChatForm(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}))
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		`name="endpoint"`,
		`name="api_key"`,
		`name="model"`,
		"Test connection",
		`hx-post="/api/v1/setup/session`,
		"restart-banner",
		"hx-on::after-request",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("setup page missing %q", want)
		}
	}
}

func TestSetupPage_StatesHttpProviderOnlyScope(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}))
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	// The wizard handles the single OpenAI-compatible http provider; router
	// is out of scope. The page must say so.
	if !strings.Contains(rec.Body.String(), "router") {
		t.Error("setup page should mention router is out of scope / advanced")
	}
}
