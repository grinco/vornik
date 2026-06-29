package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/persistence"
)

// stubMemoryValidator satisfies onboarding.MemoryValidatorInterface.
type stubMemoryValidator struct {
	result onboarding.MemoryValidationResult
}

func (s stubMemoryValidator) Validate(context.Context, onboarding.MemoryConfigProposal) onboarding.MemoryValidationResult {
	return s.result
}

func newMemoryCommitServer(t *testing.T, mv onboarding.MemoryValidatorInterface) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("memory:\n  enabled: false\napi:\n  auth_enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretsDir := filepath.Join(dir, "secrets")
	sessions := &inMemorySetupSessions{}
	_ = sessions.Insert(context.Background(), &persistence.InstallationOnboardingSession{
		ID: "s1", OperatorID: defaultSingleTenantOperatorID, CurrentStep: "configure-memory",
		SelectedUseCase: "generic-assistant", Transcript: []byte("[]"),
	})
	srv := NewServer(
		WithSetupSessions(sessions),
		WithSetupValidator(stubValidator{}),
		WithSetupMemoryValidator(mv),
		WithSetupConfigPath(configPath),
		WithSetupSecretsDir(secretsDir),
	)
	return srv, configPath, secretsDir
}

func memReq(t *testing.T, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(stampSetupIdentity(context.Background(), "admin"))
}

func TestSetupMemoryValidate_ReturnsResult(t *testing.T) {
	srv, _, _ := newMemoryCommitServer(t, stubMemoryValidator{result: onboarding.MemoryValidationResult{
		EmbeddingOK: true, ReturnedDimension: 1536, DimensionMatches: true,
	}})
	router := NewRouter(srv, &config.Config{})
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, memReq(t, "/api/v1/setup/session/s1/memory/validate",
		`{"enabled":true,"embedding_endpoint":"http://e/v1","embedding_model":"m"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"embedding_ok":true`) {
		t.Errorf("expected embedding_ok in body, got %s", rec.Body.String())
	}
}

func TestSetupMemoryCommit_GatesOnEmbeddingFailure(t *testing.T) {
	srv, configPath, secretsDir := newMemoryCommitServer(t, stubMemoryValidator{result: onboarding.MemoryValidationResult{
		EmbeddingOK: false,
		Failures:    []onboarding.CheckFailure{{Name: "embedding_unreachable", Severity: "blocking"}},
	}})
	router := NewRouter(srv, &config.Config{})
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, memReq(t, "/api/v1/setup/session/s1/memory/commit",
		`{"enabled":true,"embedding_endpoint":"http://e/v1","embedding_model":"m","force":false}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(secretsDir, "memory.env")); !os.IsNotExist(err) {
		t.Errorf("memory.env must not exist on gated commit")
	}
	got, _ := os.ReadFile(configPath)
	if strings.Contains(string(got), "embedding_endpoint") {
		t.Errorf("config must not be patched on gated commit, got: %s", got)
	}
}

func TestSetupMemoryCommit_EnabledWritesConfigAndSecret(t *testing.T) {
	srv, configPath, secretsDir := newMemoryCommitServer(t, stubMemoryValidator{result: onboarding.MemoryValidationResult{
		EmbeddingOK: true, ReturnedDimension: 1536, DimensionMatches: true,
	}})
	router := NewRouter(srv, &config.Config{})
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, memReq(t, "/api/v1/setup/session/s1/memory/commit",
		`{"enabled":true,"embedding_endpoint":"http://e/v1","embedding_api_key":"sk-emb","embedding_model":"m","embedding_dimension":1536}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(configPath)
	for _, want := range []string{"embedding_endpoint", "embedding_model", "${VORNIK_MEMORY_EMBEDDING_API_KEY}"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("config missing %q, got: %s", want, got)
		}
	}
	secret, err := os.ReadFile(filepath.Join(secretsDir, "memory.env"))
	if err != nil || !strings.Contains(string(secret), "VORNIK_MEMORY_EMBEDDING_API_KEY=sk-emb") {
		t.Errorf("memory.env missing key, err=%v content=%s", err, secret)
	}
}

func TestSetupMemoryCommit_DisabledWritesEnabledFalseNoSecret(t *testing.T) {
	srv, configPath, secretsDir := newMemoryCommitServer(t, stubMemoryValidator{result: onboarding.MemoryValidationResult{Skipped: true}})
	router := NewRouter(srv, &config.Config{})
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, memReq(t, "/api/v1/setup/session/s1/memory/commit", `{"enabled":false}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(secretsDir, "memory.env")); !os.IsNotExist(err) {
		t.Errorf("memory.env must not be written when memory disabled")
	}
	got, _ := os.ReadFile(configPath)
	if strings.Contains(string(got), "embedding_endpoint") {
		t.Errorf("disabled commit must not write embedding fields, got: %s", got)
	}
}

func TestSetupMemoryCommit_BlocksProjectScopedUser(t *testing.T) {
	srv, _, _ := newMemoryCommitServer(t, stubMemoryValidator{})
	router := NewRouter(srv, &config.Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session/s1/memory/commit", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(stampSetupIdentity(context.Background(), "user"))
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
