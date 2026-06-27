package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// newWebhookTestServer builds a *Server wired with a project registry
// (project-1/github, secret "topsecret") and a bare MockTaskRepository.
// It returns the server, the resolved project, and the github source.
// Mirrors the setup pattern used by webhook_security_test.go and
// webhook_secrets_test.go; extracted here so Task-2, Task-4, and
// Task-7 tests can all share it.
func newWebhookTestServer(t *testing.T) (*Server, *registry.Project, registry.ProjectWebhookSource) {
	t.Helper()
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)
	project := reg.GetProject("project-1")
	if project == nil {
		t.Fatal("newWebhookTestServer: project-1 not found in registry")
	}
	source, ok := findWebhookSource(project, "github")
	if !ok {
		t.Fatal("newWebhookTestServer: github source not found in project-1")
	}
	return srv, project, source
}

// enqueueVerifiedWebhook must accept an already-verified payload and run the
// post-verification pipeline (template → create-task), writing the same
// response IngestWebhook would. Here we assert it does NOT 401/verify (the
// caller already verified) and produces a task-accepted response for a
// minimal valid source. Uses a recorder; the seam is what's under test.
func TestEnqueueVerifiedWebhook_CreatesTask(t *testing.T) {
	srv, project, source := newWebhookTestServer(t)
	rec := httptest.NewRecorder()
	body := []byte(`{"action":"opened"}`)
	srv.enqueueVerifiedWebhook(context.Background(), rec, project, source, body, "delivery-123")
	if rec.Code != 202 {
		t.Fatalf("want 202 Accepted from enqueueVerifiedWebhook, got %d: %s", rec.Code, rec.Body.String())
	}
}
