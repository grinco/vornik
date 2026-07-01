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

// TestSetupPage_HidesBrokenTemplateEntrypoints guards against advertising
// setup paths that are not part of the current onboarding flow.
func TestSetupPage_HidesBrokenTemplateEntrypoints(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}))
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, bad := range []string{
		"Open project templates",
		`href="/ui/projects/new"`,
		`href="/projects/new"`,
		`href="/ui/projects/new/wizard"`,
		`href="/projects/new/wizard"`,
	} {
		if strings.Contains(body, bad) {
			t.Errorf("setup page still emits disabled setup affordance %q", bad)
		}
	}
}

func TestSetupPage_FreshDefaultsAreNotConfigured(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Chat.Endpoint = "http://localhost:11434/v1"
	cfg.Chat.Model = "llama3.1"
	cfg.Memory.EmbeddingEndpoint = "http://localhost:11434/v1"
	cfg.Memory.EmbeddingModel = "nomic-embed-text"
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}))

	chat, memory, dispatcher := srv.setupStepState()
	if chat || memory || dispatcher {
		t.Fatalf("setupStepState() = chat:%v memory:%v dispatcher:%v, want all false for unenabled fresh defaults", chat, memory, dispatcher)
	}

	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		"Setup state",
		"Chat backend",
		"Memory / RAG",
		"Not configured",
		"Step 2 — Configure chat",
		"Step 3 — Configure memory / RAG",
		"Step 4 — Configure dispatcher project",
		`id="mem-test-btn" disabled`,
		`id="mem-save-btn" disabled`,
		"Create assistant project",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fresh setup page missing marker %q", want)
		}
	}
	if strings.Contains(body, `href="/ui/projects/new?slug=personal-assistant"`) {
		t.Error("fresh setup page should not enable assistant project creation before chat and memory are configured")
	}
}

// TestSetupPage_RendersMemoryStep verifies the Step-3 memory form is present
// and wired to the memory endpoints.
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

// newAssistantRegistry builds a registry with one loadable "assistant"
// project so the dispatcher step's project <select> (and its form) render.
func newAssistantRegistry(t *testing.T) *registry.Registry {
	t.Helper()
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
	return reg
}

func TestSetupPage_CompletedChatAndMemoryAdvanceToDispatcher(t *testing.T) {
	reg := newAssistantRegistry(t)
	cfg := &config.Config{}
	cfg.Chat.Enabled = true
	cfg.Chat.Provider = "http"
	cfg.Chat.Endpoint = "http://chat.example/v1"
	cfg.Chat.APIKey = "sk-live"
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
		`/ui/projects/new/wizard`,
		`id="dispatcher-save-btn" disabled`,
	} {
		if strings.Contains(body, bad) {
			t.Errorf("setup page should not render completed/broken setup affordance %q", bad)
		}
	}
	for _, want := range []string{
		"Setup state",
		`id="chat-config-form"`,
		"Step 3 — Configure memory",
		`id="mem-enabled" checked`,
		"Step 4 — Configure dispatcher project",
		`id="dispatcher-project-id"`,
		`id="dispatcher-save-btn"`,
		`fetch('/api/v1/setup/dispatcher'`,
		`value="assistant"`,
		`href="/ui/projects/new?slug=personal-assistant"`,
		"telegram.dispatcher_project_id",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("setup page missing dispatcher-next marker %q", want)
		}
	}
}

// TestSetupPage_MemoryOptOutStillUnlocksDispatcher is the regression test
// for the 2026-07-01 onboarding dead-end: SetupMemoryCommit supports
// committing the memory step with memory left disabled (it writes
// memory.enabled=false and completes the session), but the page gated the
// dispatcher step and the create-project CTA on MemoryConfigured — which is
// only true when memory is ENABLED and configured. A chat-only install
// (no embedding endpoint) therefore came back after the restart to a
// permanently disabled step 4 and could never finish the guide. Memory is
// optional: a configured chat backend alone must unlock the dispatcher step
// and the assistant-project CTA.
func TestSetupPage_MemoryOptOutStillUnlocksDispatcher(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chat.Enabled = true
	cfg.Chat.Provider = "http"
	cfg.Chat.Endpoint = "http://chat.example/v1"
	cfg.Chat.APIKey = "sk-live"
	cfg.Chat.Model = "gpt-live"
	cfg.Memory.Enabled = false
	srv := NewServer(
		WithOnboardingDetector(onboarding.Detector{Config: cfg}),
		WithProjectRegistry(newAssistantRegistry(t)),
	)
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	if strings.Contains(body, `id="dispatcher-save-btn" disabled`) {
		t.Error("dispatcher save button must not be disabled when chat is configured and memory is opted out")
	}
	if !strings.Contains(body, `href="/ui/projects/new?slug=personal-assistant"`) {
		t.Error("create-assistant-project CTA must be enabled when chat is configured and memory is opted out")
	}
	// The memory step must present itself as skippable, not a hard gate.
	if !strings.Contains(body, "Optional") {
		t.Error("memory step should be labelled Optional so opting out is a visible, supported path")
	}
}

// TestSetupPage_RestartGuidanceIncludesCommand: the 2026-07-01 onboarding
// audit found every "Restart the daemon" banner told the operator *that* a
// restart is needed but not *how*. The quickstart audience runs the daemon
// as a systemd user service — the page must spell the command out.
func TestSetupPage_RestartGuidanceIncludesCommand(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}))
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "systemctl --user restart vornik") {
		t.Error("setup page restart guidance must include the concrete restart command")
	}
}

// TestSetupPage_SayHelloStepClosesTheLoop: after the dispatcher project is
// pinned, the guide must end with a hello-world moment — a link into the
// dispatcher project's chat — instead of stopping at config plumbing
// (2026-07-01 onboarding audit, finding 6).
func TestSetupPage_SayHelloStepClosesTheLoop(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chat.Enabled = true
	cfg.Chat.Provider = "http"
	cfg.Chat.Endpoint = "http://chat.example/v1"
	cfg.Chat.APIKey = "sk-live"
	cfg.Chat.Model = "gpt-live"
	cfg.Telegram.DispatcherProjectID = "assistant"
	srv := NewServer(
		WithOnboardingDetector(onboarding.Detector{Config: cfg}),
		WithProjectRegistry(newAssistantRegistry(t)),
	)
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "Say hello") {
		t.Error("setup page missing the final say-hello step")
	}
	if !strings.Contains(body, `href="/ui/projects/assistant/chat"`) {
		t.Error("say-hello step must deep-link into the dispatcher project's chat")
	}

	// Before the dispatcher is pinned there is nothing to link to yet — the
	// step should render as pending, without a chat link.
	fresh := NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}))
	rec = httptest.NewRecorder()
	fresh.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
	freshBody := rec.Body.String()
	if !strings.Contains(freshBody, "Say hello") {
		t.Error("say-hello step should be visible (pending) on a fresh install so users see the goal")
	}
	if strings.Contains(freshBody, `/chat"`) {
		t.Error("fresh setup page must not link to a project chat before a dispatcher project exists")
	}
}

// TestSayHelloProjectID covers the say-hello hardening from the companion
// review of the onboarding fixes (review-20260701-12e3.md findings 1+3):
// the Step-5 chat deep link must only ever interpolate an ID that (a) is
// shaped like a project ID — defense-in-depth on top of html/template's
// contextual escaping, since config.yaml can be hand-edited — and (b) is
// currently loaded in the registry, so a deleted/typo'd
// telegram.dispatcher_project_id renders the Waiting fallback instead of
// a 404 link.
func TestSayHelloProjectID(t *testing.T) {
	srv := NewServer(WithProjectRegistry(newAssistantRegistry(t)))
	if got := srv.sayHelloProjectID("assistant"); got != "assistant" {
		t.Errorf("sayHelloProjectID(assistant) = %q, want it passed through for a loaded project", got)
	}
	for _, bad := range []string{
		"",                    // unset
		"ghost",               // valid shape, but not a loaded project
		`assistant"><script>`, // HTML metacharacters
		"assi stant",          // whitespace
		"a/b",                 // path separator
		"%2e%2e",              // URL-encoded traversal
		".hidden",             // must start alphanumeric
	} {
		if got := srv.sayHelloProjectID(bad); got != "" {
			t.Errorf("sayHelloProjectID(%q) = %q, want empty", bad, got)
		}
	}
	// No registry wired: never emit a link we cannot verify.
	bare := NewServer()
	if got := bare.sayHelloProjectID("assistant"); got != "" {
		t.Errorf("sayHelloProjectID with nil registry = %q, want empty", got)
	}
}

// TestSetupPage_SayHelloWaitsOnStaleDispatcherProject: a dispatcher project
// that is configured but no longer loaded must not produce a dead chat link.
func TestSetupPage_SayHelloWaitsOnStaleDispatcherProject(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chat.Enabled = true
	cfg.Chat.Provider = "http"
	cfg.Chat.Endpoint = "http://chat.example/v1"
	cfg.Chat.APIKey = "sk-live"
	cfg.Chat.Model = "gpt-live"
	cfg.Telegram.DispatcherProjectID = "ghost"
	srv := NewServer(
		WithOnboardingDetector(onboarding.Detector{Config: cfg}),
		WithProjectRegistry(newAssistantRegistry(t)),
	)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
	body := rec.Body.String()
	if strings.Contains(body, "/ui/projects/ghost/chat") {
		t.Error("say-hello step must not link to a project that is not loaded in the registry")
	}
	if !strings.Contains(body, "Finish the steps above") {
		t.Error("say-hello step should fall back to the Waiting guidance for a stale dispatcher project")
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
