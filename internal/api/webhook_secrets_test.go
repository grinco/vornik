package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

// testWebhookRegistryWithAllowSecrets is a copy of testWebhookRegistry
// that flips allow_secrets on the github source. Used to verify the
// per-source opt-out pathway from BACKLOG.md line 346.
func testWebhookRegistryWithAllowSecrets(t *testing.T) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "project.yaml"), []byte(`
projectId: project-1
displayName: Project
swarmId: swarm-1
defaultWorkflowId: wf-1
defaultPriority: 42
webhooks:
  sources:
    - name: github
      secret: topsecret
      event_id_path: id
      task_type_template: "GitHub issue: ${issue.title}"
      allow_secrets: true
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm.md"), []byte(`---
swarmId: swarm-1
roles:
  - name: worker
    runtime:
      image: fake-agent
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"), []byte(`---
workflowId: wf-1
entrypoint: run
steps:
  run:
    type: agent
    prompt: "do work"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}

// newSecretsServer wires the secret-leak detector + the webhook
// scaffolding shared by every test in this file.
func newSecretsServer(t *testing.T, reg *registry.Registry, taskRepo *mocks.MockTaskRepository, webhookRepo *mockWebhookEventRepo) *Server {
	t.Helper()
	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	return NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
		WithSecrets(det, nil),
	)
}

// TestIngestWebhook_SecretLeakBlocked — headline behaviour: a signed,
// otherwise-valid payload that contains an OpenAI key gets refused at
// the secret-leak gate. No task is created and the event audit row
// records secret_leak.
func TestIngestWebhook_SecretLeakBlocked(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := newSecretsServer(t, reg, taskRepo, webhookRepo)
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"evt-1","issue":{"title":"key=sk-proj1234567890abcdefghijklmnopqrstuv"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, 0, taskRepo.CallCount.Create,
		"block-mode must refuse before task creation")
	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, persistence.WebhookEventStatusRejected, webhookRepo.events[0].Status)
	assert.Equal(t, "secret_leak", webhookRepo.events[0].ErrorCode)
	assert.Contains(t, rec.Body.String(), "SECRET_LEAK")
}

// TestIngestWebhook_AllowSecretsBypassesScan — when a source legitimately
// carries long high-entropy tokens (signed JWT delivery payloads, GitHub
// installation tokens), operators opt out via allow_secrets: true. The
// task gets created normally and no scan finding is logged in audit.
func TestIngestWebhook_AllowSecretsBypassesScan(t *testing.T) {
	reg := testWebhookRegistryWithAllowSecrets(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := newSecretsServer(t, reg, taskRepo, webhookRepo)
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"evt-1","issue":{"title":"key=sk-proj1234567890abcdefghijklmnopqrstuv"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, 1, taskRepo.CallCount.Create)
	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, persistence.WebhookEventStatusAccepted, webhookRepo.events[0].Status)
}

// TestIngestWebhook_CleanPayloadPasses — sanity check that the
// detector wiring doesn't break the existing happy path. A signed
// payload with no findings flows through to task creation as before.
func TestIngestWebhook_CleanPayloadPasses(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := newSecretsServer(t, reg, taskRepo, webhookRepo)
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"evt-1","issue":{"title":"Fix login"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, 1, taskRepo.CallCount.Create)
	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, persistence.WebhookEventStatusAccepted, webhookRepo.events[0].Status)
}

// TestIngestWebhook_DetectModeStillCreatesTask — operators staging the
// detector against a noisy upstream may set the webhook checkpoint to
// detect-only; the scan logs but doesn't refuse. Verifies the override
// pathway works and the body reaches task creation unmodified (as a
// signed payload couldn't be safely rewritten to a different shape
// anyway — that's why Block is the default).
func TestIngestWebhook_DetectModeStillCreatesTask(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
		WithSecrets(det, map[string]secrets.Action{
			secrets.CheckpointWebhook: secrets.ActionDetect,
		}),
	)
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"evt-1","issue":{"title":"key=sk-proj1234567890abcdefghijklmnopqrstuv"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, 1, taskRepo.CallCount.Create)
}
