package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/persistence"
)

func TestSetupStatusRoute_ReturnsFreshInstallJSON(t *testing.T) {
	srv := NewServer(
		WithConfig(&config.Config{}),
		WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}),
	)
	router := NewRouter(srv, &config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/setup/status", nil)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got onboarding.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.FreshInstall {
		t.Fatalf("expected fresh install, got %+v", got)
	}
}

// TestSetupStatusRoute_BlocksProjectScopedUser verifies that a
// project-scoped (RoleUser) session cannot read the setup status
// endpoint. Setup status exposes daemon-wide config state that should
// only be visible to admin-capable operators.
func TestSetupStatusRoute_BlocksProjectScopedUser(t *testing.T) {
	srv := NewServer(
		WithConfig(&config.Config{}),
		WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}),
	)
	router := NewRouter(srv, &config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/setup/status", nil)
	req = req.WithContext(stampSetupIdentity(req.Context(), "user"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for RoleUser", rec.Code)
	}
}

// TestSetupStatusRoute_AdminCanRead verifies that an admin session can
// read the setup status endpoint.
func TestSetupStatusRoute_AdminCanRead(t *testing.T) {
	srv := NewServer(
		WithConfig(&config.Config{}),
		WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}),
	)
	router := NewRouter(srv, &config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/setup/status", nil)
	req = req.WithContext(stampSetupIdentity(req.Context(), "admin"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for admin", rec.Code)
	}
	var got onboarding.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// TestSetupStatusRoute_RejectsPost verifies that the endpoint only
// accepts GET requests.
func TestSetupStatusRoute_RejectsPost(t *testing.T) {
	srv := NewServer(
		WithConfig(&config.Config{}),
		WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}),
	)
	router := NewRouter(srv, &config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/status", nil)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 for POST", rec.Code)
	}
}

// stampSetupIdentity creates a context with an auth Identity carrying the
// given session role, suitable for testing setup handlers.
func stampSetupIdentity(ctx context.Context, role string) context.Context {
	id := &auth.Identity{
		Backend: "session",
		Extra: map[string]any{
			auth.ExtraSessionRole: role,
		},
	}
	return context.WithValue(ctx, identityKey, id)
}

func TestSetupServerOptions_PopulateFields(t *testing.T) {
	srv := NewServer(
		WithSetupSessions(stubSetupSessions{}),
		WithSetupValidator(stubValidator{}),
		WithSetupConfigPath("/tmp/config.yaml"),
		WithSetupSecretsDir("/tmp/secrets"),
	)
	if srv.setupConfigPath != "/tmp/config.yaml" {
		t.Errorf("setupConfigPath = %q", srv.setupConfigPath)
	}
	if srv.setupSecretsDir != "/tmp/secrets" {
		t.Errorf("setupSecretsDir = %q", srv.setupSecretsDir)
	}
	if srv.setupSessions == nil {
		t.Error("setupSessions not wired")
	}
	if srv.setupValidator == nil {
		t.Error("setupValidator not wired")
	}
}

// stubSetupSessions satisfies persistence.InstallationOnboardingSessionRepository
// for wiring smoke tests. Methods return zero values; full behavior is
// exercised in the handler tests via a temp-dir-backed real repo where
// practical, or via this stub where only the wiring matters.
type stubSetupSessions struct{}

func (stubSetupSessions) Insert(context.Context, *persistence.InstallationOnboardingSession) error {
	return nil
}
func (stubSetupSessions) Get(context.Context, string) (*persistence.InstallationOnboardingSession, error) {
	return nil, persistence.ErrNotFound
}
func (stubSetupSessions) Update(context.Context, *persistence.InstallationOnboardingSession) error {
	return nil
}
func (stubSetupSessions) CommitTo(context.Context, string, string) error { return nil }
func (stubSetupSessions) Cancel(context.Context, string, string) error   { return nil }
func (stubSetupSessions) ListByOperator(context.Context, string, int) ([]*persistence.InstallationOnboardingSession, error) {
	return nil, nil
}
func (stubSetupSessions) HasCommitted(context.Context) (bool, error) { return false, nil }

// stubValidator satisfies onboarding.ChatValidatorInterface and the
// optional setupModelLister capability the /setup/models handler probes.
type stubValidator struct {
	result    onboarding.ChatValidationResult
	models    []chat.ModelInfo
	modelsErr error
}

func (s stubValidator) Validate(context.Context, onboarding.ChatConfigProposal) onboarding.ChatValidationResult {
	return s.result
}

var errStubUpstream = fmt.Errorf("upstream 401: invalid api key")

func (s stubValidator) ListModels(_ context.Context, _, _ string) ([]chat.ModelInfo, error) {
	if s.modelsErr != nil {
		return nil, s.modelsErr
	}
	return s.models, nil
}

func TestSetupModels_ReturnsModelList(t *testing.T) {
	srv := NewServer(WithSetupValidator(stubValidator{models: []chat.ModelInfo{
		{ID: "gpt-4.1", Source: "live"},
		{ID: "gpt-4.1-mini", Source: "live"},
	}}))
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader(`{"endpoint":"https://x/v1","api_key":"sk-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/models", body)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(stampSetupIdentity(req.Context(), "admin"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Models []chat.ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Models) != 2 || got.Models[0].ID != "gpt-4.1" {
		t.Fatalf("unexpected models: %#v", got.Models)
	}
}

func TestSetupModels_UpstreamErrorIs502(t *testing.T) {
	srv := NewServer(WithSetupValidator(stubValidator{modelsErr: errStubUpstream}))
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader(`{"endpoint":"https://x/v1","api_key":"bad"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/models", body)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(stampSetupIdentity(req.Context(), "admin"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", rec.Code, rec.Body.String())
	}
}

func TestSetupModels_BlocksProjectScopedUser(t *testing.T) {
	srv := NewServer(WithSetupValidator(stubValidator{}))
	router := NewRouter(srv, &config.Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/models", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(stampSetupIdentity(req.Context(), "user"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for RoleUser", rec.Code)
	}
}

func TestSetupSessionCreate_ReturnsSessionJSON(t *testing.T) {
	srv := NewServer(WithSetupSessions(&inMemorySetupSessions{}))
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader(`{"selected_use_case":"generic-assistant"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session", body)
	req = req.WithContext(stampSetupIdentity(req.Context(), "admin"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["current_step"] != "choose-purpose" {
		t.Errorf("current_step = %v, want choose-purpose", got["current_step"])
	}
	if got["id"] == nil || got["id"] == "" {
		t.Error("session id missing")
	}
}

func TestSetupSessionCreate_BlocksProjectScopedUser(t *testing.T) {
	srv := NewServer(WithSetupSessions(&inMemorySetupSessions{}))
	router := NewRouter(srv, &config.Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session", strings.NewReader(`{}`)).
		WithContext(stampSetupIdentity(context.Background(), "user"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestSetupSessionValidate_ReturnsResultAndStoresOnSession(t *testing.T) {
	sessions := &inMemorySetupSessions{}
	sess := &persistence.InstallationOnboardingSession{
		ID: "s1", OperatorID: defaultSingleTenantOperatorID, CurrentStep: "choose-purpose",
		SelectedUseCase: "generic-assistant", Transcript: []byte("[]"),
	}
	_ = sessions.Insert(context.Background(), sess)

	srv := NewServer(
		WithSetupSessions(sessions),
		WithSetupValidator(stubValidator{result: onboarding.ChatValidationResult{
			EndpointOK: true, ModelsListed: true, ModelKnown: true, PingOK: true,
			ModelOptions: []chat.ModelInfo{{ID: "gpt-4.1"}},
		}}),
	)
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader(`{"endpoint":"http://chat.example","api_key":"k","model":"gpt-4.1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/validate", body).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got onboarding.ChatValidationResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.PingOK {
		t.Errorf("expected PingOK, got %+v", got)
	}
	stored, _ := sessions.Get(context.Background(), "s1")
	if len(stored.ValidationResults) == 0 {
		t.Error("validation result not stored on session")
	}
	if len(stored.ProposedConfig) == 0 {
		t.Error("proposed_config not stored on session")
	}
}

func TestSetupSessionValidate_AcceptsFormPost(t *testing.T) {
	sessions := &inMemorySetupSessions{}
	_ = sessions.Insert(context.Background(), &persistence.InstallationOnboardingSession{
		ID: "s1", OperatorID: defaultSingleTenantOperatorID, CurrentStep: "choose-purpose",
		SelectedUseCase: "generic-assistant", Transcript: []byte("[]"),
	})
	srv := NewServer(
		WithSetupSessions(sessions),
		WithSetupValidator(stubValidator{result: onboarding.ChatValidationResult{PingOK: true}}),
	)
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader("endpoint=http%3A%2F%2Fchat.example&api_key=k&model=gpt-4.1")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/validate", body).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := sessions.Get(context.Background(), "s1")
	if !strings.Contains(string(stored.ProposedConfig), "chat.example") {
		t.Fatalf("form proposal was not stored, got %s", stored.ProposedConfig)
	}
}

func TestSetupSessionValidate_HTMXReturnsHTML(t *testing.T) {
	sessions := &inMemorySetupSessions{}
	_ = sessions.Insert(context.Background(), &persistence.InstallationOnboardingSession{
		ID: "s1", OperatorID: defaultSingleTenantOperatorID, CurrentStep: "choose-purpose",
		SelectedUseCase: "generic-assistant", Transcript: []byte("[]"),
	})
	srv := NewServer(
		WithSetupSessions(sessions),
		WithSetupValidator(stubValidator{result: onboarding.ChatValidationResult{
			PingOK: true, ModelsListed: true,
		}}),
	)
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader("endpoint=http%3A%2F%2Fchat.example&api_key=k&model=gpt-4.1")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/validate", body).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Connection validated") || strings.Contains(body, `"ping_ok"`) {
		t.Fatalf("HTMX response should be rendered HTML, got: %s", body)
	}
}

func TestSetupSessionValidate_RejectsForeignSession(t *testing.T) {
	sessions := &inMemorySetupSessions{}
	_ = sessions.Insert(context.Background(), &persistence.InstallationOnboardingSession{
		ID: "s1", OperatorID: "other-operator", CurrentStep: "choose-purpose",
		SelectedUseCase: "generic-assistant", Transcript: []byte("[]"),
	})
	srv := NewServer(
		WithSetupSessions(sessions),
		WithSetupValidator(stubValidator{result: onboarding.ChatValidationResult{PingOK: true}}),
	)
	router := NewRouter(srv, &config.Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/validate", strings.NewReader(`{"endpoint":"http://chat.example","api_key":"k","model":"m"}`)).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for foreign session", rec.Code)
	}
}

func TestSetupSessionValidate_BlocksProjectScopedUser(t *testing.T) {
	srv := NewServer(WithSetupSessions(&inMemorySetupSessions{}), WithSetupValidator(stubValidator{}))
	router := NewRouter(srv, &config.Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/validate", strings.NewReader(`{}`)).
		WithContext(stampSetupIdentity(context.Background(), "user"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestSetupSessionValidate_503WhenUnwired(t *testing.T) {
	srv := NewServer() // no sessions, no validator
	router := NewRouter(srv, &config.Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/validate", strings.NewReader(`{}`)).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// inMemorySetupSessions is an in-memory InstallationOnboardingSessionRepository
// for handler tests. NOT concurrent-safe beyond test usage.
type inMemorySetupSessions struct {
	mu       sync.Mutex
	sessions map[string]*persistence.InstallationOnboardingSession
}

func (r *inMemorySetupSessions) Insert(_ context.Context, s *persistence.InstallationOnboardingSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = map[string]*persistence.InstallationOnboardingSession{}
	}
	if s.ID == "" || s.OperatorID == "" || s.CurrentStep == "" || s.SelectedUseCase == "" {
		return fmt.Errorf("missing required field")
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now().UTC()
	}
	if len(s.Transcript) == 0 {
		s.Transcript = []byte("[]")
	}
	stored := *s
	r.sessions[s.ID] = &stored
	return nil
}
func (r *inMemorySetupSessions) Get(_ context.Context, id string) (*persistence.InstallationOnboardingSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *s
	return &cp, nil
}
func (r *inMemorySetupSessions) Update(_ context.Context, s *persistence.InstallationOnboardingSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[s.ID]; !ok {
		return persistence.ErrNotFound
	}
	if len(s.Transcript) == 0 {
		s.Transcript = []byte("[]")
	}
	s.UpdatedAt = time.Now().UTC()
	stored := *s
	r.sessions[s.ID] = &stored
	return nil
}
func (r *inMemorySetupSessions) CommitTo(_ context.Context, id, projectID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return persistence.ErrNotFound
	}
	if s.CommittedProjectID != nil {
		return persistence.ErrInvalidTransition
	}
	pid := projectID
	s.CommittedProjectID = &pid
	return nil
}
func (r *inMemorySetupSessions) Cancel(_ context.Context, id, op string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok || s.OperatorID != op {
		return persistence.ErrNotFound
	}
	if s.CommittedProjectID != nil {
		return persistence.ErrInvalidTransition
	}
	return nil
}
func (r *inMemorySetupSessions) ListByOperator(_ context.Context, _ string, _ int) ([]*persistence.InstallationOnboardingSession, error) {
	return nil, nil
}
func (r *inMemorySetupSessions) HasCommitted(_ context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessions {
		if s.CommittedProjectID != nil {
			return true, nil
		}
	}
	return false, nil
}

func newCommitServer(t *testing.T, validator onboarding.ChatValidatorInterface) (*Server, *inMemorySetupSessions, string, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("chat:\n  enabled: false\napi:\n  auth_enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretsDir := filepath.Join(dir, "secrets")
	sessions := &inMemorySetupSessions{}
	_ = sessions.Insert(context.Background(), &persistence.InstallationOnboardingSession{
		ID: "s1", OperatorID: defaultSingleTenantOperatorID, CurrentStep: "configure-chat",
		SelectedUseCase: "generic-assistant", Transcript: []byte("[]"),
	})
	srv := NewServer(
		WithSetupSessions(sessions),
		WithSetupValidator(validator),
		WithSetupConfigPath(configPath),
		WithSetupSecretsDir(secretsDir),
	)
	return srv, sessions, configPath, secretsDir
}

func TestSetupCommit_HardGatesOnPingFailure(t *testing.T) {
	srv, sessions, configPath, secretsDir := newCommitServer(t, stubValidator{result: onboarding.ChatValidationResult{
		EndpointOK: true, ModelsListed: true, ModelKnown: true, PingOK: false,
		Failures: []onboarding.CheckFailure{{Name: "ping_failed", Severity: "blocking"}},
	}})
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader(`{"endpoint":"http://chat.example","api_key":"k","model":"m","force":false}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/commit", body).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (hard gate), body=%s", rec.Code, rec.Body.String())
	}
	// Nothing written.
	if _, err := os.Stat(filepath.Join(secretsDir, "chat.env")); !os.IsNotExist(err) {
		t.Errorf("secret file must not exist on gated commit, err=%v", err)
	}
	got, _ := os.ReadFile(configPath)
	if strings.Contains(string(got), "${VORNIK_CHAT_API_KEY}") {
		t.Errorf("config must not be patched on gated commit, got: %s", got)
	}
	stored, _ := sessions.Get(context.Background(), "s1")
	if stored.CurrentStep != "configure-chat" {
		t.Errorf("current_step must not advance on gated commit, got %q", stored.CurrentStep)
	}
}

func TestSetupCommit_ForceBypassesGate(t *testing.T) {
	srv, _, _, secretsDir := newCommitServer(t, stubValidator{result: onboarding.ChatValidationResult{
		EndpointOK: true, ModelsListed: true, ModelKnown: true, PingOK: false,
	}})
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader(`{"endpoint":"http://chat.example","api_key":"k","model":"m","force":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/commit", body).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (force), body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(secretsDir, "chat.env")); err != nil {
		t.Errorf("secret file should exist on forced commit, err=%v", err)
	}
}

func TestSetupCommit_AcceptsFormPost(t *testing.T) {
	srv, sessions, configPath, _ := newCommitServer(t, stubValidator{result: onboarding.ChatValidationResult{PingOK: true}})
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader("endpoint=http%3A%2F%2Fchat.example&api_key=sk-real&model=gpt-4.1")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/commit", body).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	cfg, _ := os.ReadFile(configPath)
	if !strings.Contains(string(cfg), "endpoint: http://chat.example") {
		t.Fatalf("form commit did not patch endpoint, got: %s", cfg)
	}
	stored, _ := sessions.Get(context.Background(), "s1")
	if stored.CurrentStep != "configure-memory" {
		t.Fatalf("current_step = %q, want configure-memory", stored.CurrentStep)
	}
}

func TestSetupCommit_HTMXReturnsHTML(t *testing.T) {
	srv, _, _, _ := newCommitServer(t, stubValidator{result: onboarding.ChatValidationResult{PingOK: true}})
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader("endpoint=http%3A%2F%2Fchat.example&api_key=sk-real&model=gpt-4.1")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/commit", body).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Chat config saved") || strings.Contains(body, `"committed"`) {
		t.Fatalf("HTMX commit response should be rendered HTML, got: %s", body)
	}
}

func TestSetupCommit_HTMXGateFailureReturnsHTML(t *testing.T) {
	srv, _, _, secretsDir := newCommitServer(t, stubValidator{result: onboarding.ChatValidationResult{
		PingOK: false,
		Failures: []onboarding.CheckFailure{{
			Name:        "ping_failed",
			Message:     "model call failed",
			Remediation: "check the API key",
		}},
	}})
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader("endpoint=http%3A%2F%2Fchat.example&api_key=bad&model=gpt-4.1")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/commit", body).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so HTMX swaps warning HTML, body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Connection needs attention") || !strings.Contains(body, "ping_failed") {
		t.Fatalf("HTMX failure should be rendered HTML, got: %s", body)
	}
	if _, err := os.Stat(filepath.Join(secretsDir, "chat.env")); !os.IsNotExist(err) {
		t.Fatalf("gated HTMX commit must not write secret, err=%v", err)
	}
}

func TestSetupCommit_RejectsForeignSession(t *testing.T) {
	srv, sessions, configPath, _ := newCommitServer(t, stubValidator{result: onboarding.ChatValidationResult{PingOK: true}})
	stored, _ := sessions.Get(context.Background(), "s1")
	stored.OperatorID = "other-operator"
	if err := sessions.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(srv, &config.Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/commit", strings.NewReader(`{"endpoint":"http://chat.example","api_key":"k","model":"m"}`)).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for foreign session", rec.Code)
	}
	cfg, _ := os.ReadFile(configPath)
	if strings.Contains(string(cfg), "${VORNIK_CHAT_API_KEY}") {
		t.Fatalf("foreign commit must not patch config, got: %s", cfg)
	}
}

func TestSetupCommit_PersistsSecretAndConfig(t *testing.T) {
	srv, sessions, configPath, secretsDir := newCommitServer(t, stubValidator{result: onboarding.ChatValidationResult{
		EndpointOK: true, ModelsListed: true, ModelKnown: true, PingOK: true,
	}})
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader(`{"endpoint":"http://chat.example","api_key":"sk-real","model":"gpt-4.1","force":false}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/commit", body).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Secret file: 0600, correct line.
	secretPath := filepath.Join(secretsDir, "chat.env")
	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatalf("secret not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("secret mode = %o, want 0600", info.Mode().Perm())
	}
	secret, _ := os.ReadFile(secretPath)
	if !strings.Contains(string(secret), "VORNIK_CHAT_API_KEY=sk-real") {
		t.Errorf("secret content wrong: %s", secret)
	}

	// Config patched with the five keys + reference (not plaintext).
	cfg, _ := os.ReadFile(configPath)
	cfgStr := string(cfg)
	for _, want := range []string{"enabled: true", "provider: http", "endpoint: http://chat.example", "model: gpt-4.1", "api_key: ${VORNIK_CHAT_API_KEY}"} {
		if !strings.Contains(cfgStr, want) {
			t.Errorf("config missing %q, got: %s", want, cfgStr)
		}
	}
	if strings.Contains(cfgStr, "sk-real") {
		t.Errorf("plaintext key must not appear in config.yaml, got: %s", cfgStr)
	}

	// Session advanced.
	stored, _ := sessions.Get(context.Background(), "s1")
	if stored.CurrentStep != "configure-memory" {
		t.Errorf("current_step = %q, want configure-memory", stored.CurrentStep)
	}

	// Response declares restart-required.
	var resp struct {
		Committed       bool `json:"committed"`
		RestartRequired bool `json:"restart_required"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Committed || !resp.RestartRequired {
		t.Errorf("response = %+v, want committed+restart_required", resp)
	}
}

func TestSetupCommit_BlocksProjectScopedUser(t *testing.T) {
	srv, _, _, _ := newCommitServer(t, stubValidator{result: onboarding.ChatValidationResult{PingOK: true}})
	router := NewRouter(srv, &config.Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/commit", strings.NewReader(`{}`)).
		WithContext(stampSetupIdentity(context.Background(), "user"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestSetupCommit_SecretWriteFails_AbortsBeforeConfig(t *testing.T) {
	srv, _, configPath, secretsDir := newCommitServer(t, stubValidator{result: onboarding.ChatValidationResult{PingOK: true}})
	// Make secrets dir unwritable by pointing it at a file path's parent
	// that cannot be created (use a path under a file).
	srv.setupSecretsDir = filepath.Join(configPath, "cannot-be-dir")
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader(`{"endpoint":"http://chat.example","api_key":"k","model":"m"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/commit", body).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	got, _ := os.ReadFile(configPath)
	if !strings.Contains(string(got), "enabled: false") {
		t.Errorf("config must be untouched when secret write fails, got: %s", got)
	}
	_ = secretsDir // unused on this path
}

// TestSetupCommit_ConfigPatchFails_ReportsOrphanedSecret exercises the
// second half of the partial-failure honesty contract: the secret is
// written, then the config patch fails validation AFTER the secret lands
// on disk. The handler must roll back config.yaml to the seed, leave the
// orphaned chat.env in place (it is reused on retry), and report the
// failure honestly with a 500 naming the orphaned secret path.
//
// The failure is triggered via the real FileConfigWriter.Validate()
// pipeline: the seed config has api.auth_enabled:true with no api_keys,
// so the post-patch config (which only adds chat.* keys) fails the
// config loader's ValidateFile.
func TestSetupCommit_ConfigPatchFails_ReportsOrphanedSecret(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	// Seed with auth_enabled:true and no api_keys → writer.Validate()
	// fails after the chat keys are patched (chat patch adds no api_keys).
	if err := os.WriteFile(configPath, []byte("chat:\n  enabled: false\napi:\n  auth_enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretsDir := filepath.Join(dir, "secrets")
	sessions := &inMemorySetupSessions{}
	_ = sessions.Insert(context.Background(), &persistence.InstallationOnboardingSession{
		ID: "s1", OperatorID: defaultSingleTenantOperatorID, CurrentStep: "configure-chat",
		SelectedUseCase: "generic-assistant", Transcript: []byte("[]"),
	})
	srv := NewServer(
		WithSetupSessions(sessions),
		WithSetupValidator(stubValidator{result: onboarding.ChatValidationResult{PingOK: true}}),
		WithSetupConfigPath(configPath),
		WithSetupSecretsDir(secretsDir),
	)
	router := NewRouter(srv, &config.Config{})
	body := strings.NewReader(`{"endpoint":"http://chat.example","api_key":"sk-real","model":"gpt-4.1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/commit", body).
		WithContext(stampSetupIdentity(context.Background(), "admin"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (config patch fails post-secret), body=%s", rec.Code, rec.Body.String())
	}

	// The response must honestly name the orphaned secret path.
	respBody := rec.Body.String()
	secretPath := filepath.Join(secretsDir, "chat.env")
	if !strings.Contains(respBody, "orphaned") && !strings.Contains(respBody, secretPath) {
		t.Errorf("response must mention the orphaned secret, got: %s", respBody)
	}

	// The chat.env file MUST still exist on disk — it was written before
	// the config patch failed and is intentionally left for reuse on retry.
	if _, err := os.Stat(secretPath); err != nil {
		t.Errorf("orphaned chat.env must remain on disk, stat err=%v", err)
	}

	// config.yaml MUST have been rolled back to the seed: still
	// enabled:false, and the patched ${VORNIK_CHAT_API_KEY} reference
	// must NOT be present.
	cfg, _ := os.ReadFile(configPath)
	cfgStr := string(cfg)
	if !strings.Contains(cfgStr, "enabled: false") {
		t.Errorf("config must be rolled back to seed (enabled:false), got: %s", cfgStr)
	}
	if strings.Contains(cfgStr, "${VORNIK_CHAT_API_KEY}") {
		t.Errorf("config must not contain the patched api_key reference after rollback, got: %s", cfgStr)
	}

	// The session must NOT have advanced.
	stored, _ := sessions.Get(context.Background(), "s1")
	if stored.CurrentStep != "configure-chat" {
		t.Errorf("current_step = %q, want configure-chat (no advance on failure)", stored.CurrentStep)
	}
}
