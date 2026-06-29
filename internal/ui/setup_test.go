package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/registry"
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
		`id="fetch-models-btn"`,
		`id="save-continue-btn"`,
		`id="test-conn-btn"`,
		"restart-banner",
		`fetch('/api/v1/setup/models'`,
		`/api/v1/setup/session/`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("setup page missing %q", want)
		}
	}
	// The fragile htmx dynamic-retarget pattern must be gone — it silently
	// dropped the commit pill + restart banner (the #2 regression).
	for _, bad := range []string{
		"hx-on::after-request",
		`hx-post="/api/v1/setup/session/new/commit"`,
	} {
		if strings.Contains(body, bad) {
			t.Errorf("setup page still uses removed htmx pattern %q", bad)
		}
	}
}

// TestSetupPage_ProjectTemplateLinkIsUIScoped guards against the onboarding
// regression where the "Open project templates" button pointed at
// /projects/new instead of the /ui/-prefixed route.
func TestSetupPage_ProjectTemplateLinkIsUIScoped(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}))
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `href="/ui/projects/new"`) {
		t.Errorf("setup page missing UI-scoped template link")
	}
	if strings.Contains(body, `/ui/projects/new/wizard`) {
		t.Errorf("setup page should not advertise project wizard while the path is disabled")
	}
	for _, bad := range []string{
		`href="/projects/new"`,
		`href="/projects/new/wizard"`,
	} {
		if strings.Contains(body, bad) {
			t.Errorf("setup page still emits non-UI link %q (missing /ui/ prefix)", bad)
		}
	}
}

// TestSetupPage_RendersMemoryStep verifies the Step-3 memory form is in the
// page (hidden until chat config is saved) and wired to the memory endpoints.
func TestSetupPage_RendersMemoryStep(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}))
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		`id="memory-step"`,
		`id="mem-enabled"`,
		`id="mem-endpoint"`,
		`id="mem-model"`,
		`/memory/validate`,
		`/memory/commit`,
		"Step 3 — Configure memory",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("setup page missing memory-step marker %q", want)
		}
	}
}

func TestSetupPage_CompletedChatAndMemoryAdvanceToDispatcher(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "projects", "assistant.yaml"), []byte(`projectId: assistant
displayName: Assistant
swarmId: assistant-swarm
defaultWorkflowId: assistant-workflow
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "swarms", "assistant-swarm.md"), []byte(`---
swarmId: assistant-swarm
leadRole: dispatcher
roles:
  - name: dispatcher
    runtime:
      image: noop:dispatcher
---
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workflows", "assistant-workflow.md"), []byte(`---
workflowId: assistant-workflow
entrypoint: dispatch
steps:
  dispatch:
    type: agent
    role: dispatcher
    prompt: "route chat"
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := reg.Load(root); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Chat.Endpoint = "http://chat.example/v1"
	cfg.Chat.Model = "gpt-live"
	cfg.Memory.Enabled = true
	cfg.Memory.EmbeddingEndpoint = "http://embed.example/v1"
	cfg.Memory.EmbeddingModel = "embed-live"
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}), WithProjectRegistry(reg))
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	for _, bad := range []string{
		`id="chat-config-form"`,
		`Step 3 — Configure memory`,
		`/ui/projects/new/wizard`,
	} {
		if strings.Contains(body, bad) {
			t.Errorf("setup page should not render completed/broken setup affordance %q", bad)
		}
	}
	for _, want := range []string{
		"Completed setup",
		"Step 4 — Configure dispatcher project",
		`id="dispatcher-project-id"`,
		`id="dispatcher-save-btn"`,
		`fetch('/api/v1/setup/dispatcher'`,
		`value="assistant"`,
		"telegram.dispatcher_project_id",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("setup page missing dispatcher-next marker %q", want)
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
