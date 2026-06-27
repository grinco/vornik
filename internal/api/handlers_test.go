package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

func TestNewServer(t *testing.T) {
	logger := zerolog.Nop()
	taskRepo := &mocks.MockTaskRepository{}

	server := NewServer(
		WithLogger(logger),
		WithTaskRepository(taskRepo),
	)

	assert.NotNil(t, server)
	assert.NotNil(t, server.taskRepo)
}

func TestDecodeJSONBodyRejectsMultipleTopLevelValues(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(`{"taskType":"a"} {"taskType":"b"}`))
	rec := httptest.NewRecorder()
	var body CreateTaskRequest
	err := decodeJSONBody(rec, req, maxTaskRequestBytes, &body)
	if err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("expected multiple JSON values error, got %v", err)
	}
}

func TestServer_Healthz(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	server.Healthz(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "ok")
}

func TestServer_LivenessEndpoint(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))
	router := NewRouter(server, &config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	// /health/live now aliases /livez (was /healthz pre-drain split);
	// body says "alive" so operators can tell which probe responded.
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alive")
}

func TestServer_ReadinessEndpoint(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))
	router := NewRouter(server, &config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "ready")
}

func TestServer_Readyz(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	server.Readyz(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "ready")
}

// TestServer_Livez confirms the dedicated liveness endpoint returns
// 200 with the "alive" body — separate from /readyz so a draining
// daemon can answer "yes, still alive, just not ready" without making
// k8s decide we need a kill -9.
func TestServer_Livez(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()))

	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()

	server.Livez(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alive")
}

// TestServer_Livez_StaysUpWhileDraining is the whole point of the
// /livez vs /readyz split: a SIGTERM-initiated drain must not make
// the k8s liveness probe trip (which would escalate to kill -9 and
// cut in-flight work short). /livez stays 200, /readyz flips to 503.
func TestServer_Livez_StaysUpWhileDraining(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()))
	server.SetDraining(true)

	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()
	server.Livez(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "/livez must stay 200 during drain")
	assert.Contains(t, rec.Body.String(), "alive")
}

// TestServer_Readyz_DrainingReturns503 confirms the drain bit short-
// circuits /readyz with a stable JSON body. Load balancers + k8s
// readyness probes use the status code; observability dashboards
// scrape the body for the "draining" tag so an alert can distinguish
// drain from dependency failure.
func TestServer_Readyz_DrainingReturns503(t *testing.T) {
	server := NewServer(
		WithLogger(zerolog.Nop()),
		// Provide a working DB ping so we can be sure the 503 is
		// from the drain short-circuit, not a check failure.
		WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}),
	)
	server.SetDraining(true)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	server.Readyz(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "draining")
}

// TestServer_SetDrainingRoundtrip: the bit is a one-way switch from
// the SIGTERM handler's perspective, but the setter accepts both
// values for unit testing + future use (e.g. an admin "rollback drain"
// CLI). IsDraining reads the cached atomic without taking a lock so
// every /readyz request can call it.
func TestServer_SetDrainingRoundtrip(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()))
	assert.False(t, server.IsDraining(), "fresh server should not be draining")
	server.SetDraining(true)
	assert.True(t, server.IsDraining())
	server.SetDraining(false)
	assert.False(t, server.IsDraining())
}

// TestServer_Readyz_RedactsErrorDetail guards the public-disclosure
// fix. /readyz is unauthenticated; surfacing pq.Error() text leaks
// DB hostnames and auth diagnostics. The body must carry only the
// generic "check failed" — the verbatim error stays in the log.
func TestServer_Readyz_RedactsErrorDetail(t *testing.T) {
	sensitive := `dial tcp postgres-primary.internal:5432: password authentication failed for user "vornik_app"`
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(&mocks.MockTaskRepository{
			PingFunc: func(ctx context.Context) error { return fmt.Errorf("%s", sensitive) },
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	server.Readyz(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "postgres-primary.internal", "DB hostname must not leak in /readyz body")
	assert.NotContains(t, body, "password authentication failed", "auth error text must not leak")
	assert.NotContains(t, body, "vornik_app", "DB user must not leak")
	assert.Contains(t, body, "check failed")
}

func TestServer_MetricsEndpoint(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))
	router := NewRouter(server, &config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	// Should return 200 with Prometheus-formatted output
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/plain; version=0.0.4; charset=utf-8", rec.Header().Get("Content-Type"), "Expected Prometheus content type")

	// Verify it contains standard Prometheus metrics
	body := rec.Body.String()
	assert.True(t, strings.Contains(body, "# HELP") || strings.Contains(body, "# TYPE"),
		"Response should be Prometheus-formatted")
}

func TestRouter_AuthDoesNotUseFallbackKeys(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))
	cfg := &config.Config{
		API: config.APIConfig{
			AuthEnabled: true,
		},
	}
	router := NewRouter(server, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/tasks", nil)
	req.Header.Set("Authorization", "Bearer vornik-dev-key")
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRouter_ConfigEndpointsRequireAuth(t *testing.T) {
	SetConfigHandlers(NewConfigHandlers(nil))
	t.Cleanup(func() { SetConfigHandlers(nil) })

	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))
	cfg := &config.Config{
		API: config.APIConfig{
			AuthEnabled: true,
			APIKeys:     []string{"secret-key"},
		},
	}
	router := NewRouter(server, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config/reload-status", nil)
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestServer_CreateTask_InvalidJSON(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))

	req := httptest.NewRequest(http.MethodPost, "/projects/test/tasks", bytes.NewBufferString("invalid"))
	rec := httptest.NewRecorder()

	server.CreateTask(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestServer_CreateTask_MissingTaskType(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/projects/test/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	server.CreateTask(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestServer_CreateTask_IdempotencyReturnsExistingTask(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetByIdempotencyKeyFunc: func(ctx context.Context, projectID, idempotencyKey string) (*persistence.Task, error) {
			require.Equal(t, "project-1", projectID)
			require.Equal(t, "idem-1", idempotencyKey)
			return &persistence.Task{
				ID:             "task-existing",
				ProjectID:      projectID,
				Status:         persistence.TaskStatusQueued,
				IdempotencyKey: &idempotencyKey,
			}, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	body := `{"taskType":"test","idempotencyKey":"idem-1"}`
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	server.CreateTask(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "task-existing")
	assert.Equal(t, 0, taskRepo.CallCount.Create)
}

// TestServer_CreateTask_IdempotencyHeaderAlias verifies the
// `Idempotency-Key:` HTTP header (12-data-plane-api.md §4) is accepted as
// an alias for the JSON body field: header-only resolves to the header
// value, body-only resolves to the body value, and when both are present
// the body wins (backward-compat).
func TestServer_CreateTask_IdempotencyHeaderAlias(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		header      string
		expectedKey string
	}{
		{name: "header-only", body: `{"taskType":"test"}`, header: "idem-hdr", expectedKey: "idem-hdr"},
		{name: "body-only", body: `{"taskType":"test","idempotencyKey":"idem-body"}`, header: "", expectedKey: "idem-body"},
		{name: "both-body-wins", body: `{"taskType":"test","idempotencyKey":"idem-body"}`, header: "idem-hdr", expectedKey: "idem-body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seenLookupKey string
			taskRepo := &mocks.MockTaskRepository{
				GetByIdempotencyKeyFunc: func(ctx context.Context, projectID, idempotencyKey string) (*persistence.Task, error) {
					seenLookupKey = idempotencyKey
					return nil, persistence.ErrNotFound
				},
			}
			server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

			req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks", bytes.NewBufferString(tc.body))
			if tc.header != "" {
				req.Header.Set("Idempotency-Key", tc.header)
			}
			rec := httptest.NewRecorder()

			server.CreateTask(rec, req)

			require.Equal(t, http.StatusAccepted, rec.Code)
			require.Equal(t, 1, taskRepo.CallCount.Create)
			require.NotNil(t, taskRepo.LastCall.Task.IdempotencyKey)
			assert.Equal(t, tc.expectedKey, *taskRepo.LastCall.Task.IdempotencyKey)
			assert.Equal(t, tc.expectedKey, seenLookupKey)
		})
	}
}

func TestServer_CreateTask_DuplicateKeyReturnsExistingTask(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		CreateFunc: func(ctx context.Context, task *persistence.Task) error {
			return persistence.ErrDuplicateKey
		},
		GetByIdempotencyKeyFunc: func(ctx context.Context, projectID, idempotencyKey string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:             "task-existing",
				ProjectID:      projectID,
				Status:         persistence.TaskStatusQueued,
				IdempotencyKey: &idempotencyKey,
			}, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	body := `{"taskType":"test","idempotencyKey":"idem-1"}`
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	server.CreateTask(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "task-existing")
}

func TestServer_CreateTask_RejectsOversizedBody(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))

	req := httptest.NewRequest(http.MethodPost, "/projects/test/tasks", bytes.NewBuffer(make([]byte, maxTaskRequestBytes+1)))
	rec := httptest.NewRecorder()

	server.CreateTask(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestServer_GetTaskLogs_FallsBackToExecutionError(t *testing.T) {
	msg := "failed\n--- Container Log (last 50 lines) ---\nline one\nline two"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "project-1"}, nil
		},
	}
	execRepo := &mockExecutionRepo{
		getByTaskID: func(ctx context.Context, taskID string) (*persistence.Execution, error) {
			return &persistence.Execution{ID: "exec-1", TaskID: taskID, ProjectID: "project-1", ErrorMessage: &msg}, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo), WithExecutionRepository(execRepo))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/tasks/task-1/logs?tail=2", nil)
	rec := httptest.NewRecorder()

	server.GetTaskLogs(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "line one\nline two", strings.TrimSpace(rec.Body.String()))
}

func TestServer_IngestWebhook_CreatesTaskFromSignedEvent(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo), WithProjectRegistry(reg), WithWebhookEventRepository(webhookRepo))
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"evt-1","issue":{"title":"Fix login"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, 1, taskRepo.CallCount.Create)
	created := taskRepo.LastCall.Task
	require.NotNil(t, created)
	assert.Equal(t, "project-1", created.ProjectID)
	assert.Equal(t, "webhook:github:evt-1", *created.IdempotencyKey)
	assert.Contains(t, string(created.Payload), "Fix login")
	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, persistence.WebhookEventStatusAccepted, webhookRepo.events[0].Status)
	assert.Equal(t, "evt-1", webhookRepo.events[0].EventID)
	assert.Equal(t, created.ID, *webhookRepo.events[0].TaskID)
}

func TestServer_IngestWebhook_RejectsBadSignature(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo), WithProjectRegistry(reg), WithWebhookEventRepository(webhookRepo))
	router := NewRouter(server, &config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", strings.NewReader(`{"id":"evt-1"}`))
	req.Header.Set("X-Vornik-Signature", "sha256=bad")
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, 0, taskRepo.CallCount.Create)
	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, persistence.WebhookEventStatusRejected, webhookRepo.events[0].Status)
	assert.Equal(t, "invalid_signature", webhookRepo.events[0].ErrorCode)
	assert.Equal(t, "evt-1", webhookRepo.events[0].EventID)
}

func TestServer_IngestWebhook_RecordsDuplicateEvent(t *testing.T) {
	reg := testWebhookRegistry(t)
	existing := &persistence.Task{
		ID:             "task-existing",
		ProjectID:      "project-1",
		Status:         persistence.TaskStatusQueued,
		IdempotencyKey: strPtr("webhook:github:evt-1"),
	}
	taskRepo := &mocks.MockTaskRepository{
		GetByIdempotencyKeyFunc: func(context.Context, string, string) (*persistence.Task, error) {
			return existing, nil
		},
	}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo), WithProjectRegistry(reg), WithWebhookEventRepository(webhookRepo))
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"evt-1","issue":{"title":"Fix login"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, 0, taskRepo.CallCount.Create)
	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, persistence.WebhookEventStatusDuplicate, webhookRepo.events[0].Status)
	assert.Equal(t, "task-existing", *webhookRepo.events[0].TaskID)
}

func TestServer_IngestWebhook_HonorsProjectRateLimit(t *testing.T) {
	reg := testWebhookRegistry(t)
	reg.GetProject("project-1").RateLimit.TasksPerHour = 1
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
		WithRateLimiter(ratelimit.New()),
	)
	router := NewRouter(server, &config.Config{})

	for i, wantStatus := range []int{http.StatusAccepted, http.StatusTooManyRequests} {
		body := []byte(`{"id":"evt-` + string(rune('1'+i)) + `","issue":{"title":"Fix login"}}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
		req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
		rec := httptest.NewRecorder()

		router.Handler().ServeHTTP(rec, req)
		require.Equal(t, wantStatus, rec.Code, rec.Body.String())
	}

	assert.Equal(t, 1, taskRepo.CallCount.Create)
	require.Len(t, webhookRepo.events, 2)
	assert.Equal(t, persistence.WebhookEventStatusAccepted, webhookRepo.events[0].Status)
	assert.Equal(t, persistence.WebhookEventStatusRejected, webhookRepo.events[1].Status)
	assert.Equal(t, "RATE_LIMITED", webhookRepo.events[1].ErrorCode)
}

func TestServer_IngestWebhook_HonorsProjectBudget(t *testing.T) {
	reg := testWebhookRegistry(t)
	reg.GetProject("project-1").Budget.DailyHardUSD = 1
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
		WithLLMUsageRepository(&mockLLMUsageRepo{cost: 1.25}),
	)
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"evt-1","issue":{"title":"Fix login"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, 0, taskRepo.CallCount.Create)
	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, persistence.WebhookEventStatusRejected, webhookRepo.events[0].Status)
	assert.Equal(t, "BUDGET_EXCEEDED", webhookRepo.events[0].ErrorCode)
}

func TestServer_IngestWebhook_ThrottlesRejectedAuditRows(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo), WithProjectRegistry(reg), WithWebhookEventRepository(webhookRepo))
	router := NewRouter(server, &config.Config{})

	for i := 0; i < maxWebhookRejectedAuditEventsPerMinute+5; i++ {
		body := []byte(`{"id":"evt-bad"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
		req.Header.Set("X-Vornik-Signature", "sha256=bad")
		rec := httptest.NewRecorder()

		router.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}

	assert.Equal(t, 0, taskRepo.CallCount.Create)
	assert.Len(t, webhookRepo.events, maxWebhookRejectedAuditEventsPerMinute)
}

type mockExecutionRepo struct {
	getByTaskID func(ctx context.Context, taskID string) (*persistence.Execution, error)
}

type mockWebhookEventRepo struct {
	events []*persistence.WebhookEvent
}

type mockLLMUsageRepo struct {
	cost float64
}

func (m *mockLLMUsageRepo) Record(context.Context, *persistence.TaskLLMUsage) error { return nil }
func (m *mockLLMUsageRepo) Upsert(context.Context, *persistence.TaskLLMUsage) error { return nil }
func (m *mockLLMUsageRepo) List(context.Context, persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	return nil, nil
}
func (m *mockLLMUsageRepo) SumCostByProject(context.Context, string, time.Time, time.Time) (float64, error) {
	return m.cost, nil
}
func (m *mockLLMUsageRepo) SumCost(context.Context, time.Time, time.Time) (float64, error) {
	return m.cost, nil
}
func (m *mockLLMUsageRepo) AggregateByRoleModel(context.Context, time.Time, time.Time, int, string) ([]persistence.RoleModelSpend, error) {
	return nil, nil
}

// Spend deep-dive aggregations — empty stubs to satisfy the
// interface for tests that don't exercise the spend dashboard.
func (m *mockLLMUsageRepo) AggregateByProject(context.Context, time.Time, time.Time, int) ([]persistence.ProjectSpend, error) {
	return nil, nil
}
func (m *mockLLMUsageRepo) AggregateBySource(context.Context, time.Time, time.Time, string) ([]persistence.SourceSpend, error) {
	return nil, nil
}
func (m *mockLLMUsageRepo) TimeSeriesByDay(context.Context, time.Time, time.Time, string) ([]persistence.DailySpend, error) {
	return nil, nil
}
func (m *mockLLMUsageRepo) TopTasks(context.Context, time.Time, time.Time, int, string) ([]persistence.TaskSpend, error) {
	return nil, nil
}
func (m *mockLLMUsageRepo) TaskCostBreakdown(context.Context, string) ([]persistence.StepSpend, error) {
	return nil, nil
}

func (m *mockWebhookEventRepo) Record(_ context.Context, event *persistence.WebhookEvent) error {
	m.events = append(m.events, event)
	return nil
}

func (m *mockWebhookEventRepo) List(_ context.Context, filter persistence.WebhookEventFilter) ([]*persistence.WebhookEvent, error) {
	out := m.events
	if filter.ProjectID != nil {
		out = nil
		for _, event := range m.events {
			if event.ProjectID == *filter.ProjectID {
				out = append(out, event)
			}
		}
	}
	if filter.PageSize > 0 && len(out) > filter.PageSize {
		out = out[:filter.PageSize]
	}
	return out, nil
}

func (m *mockExecutionRepo) Create(context.Context, *persistence.Execution) error { return nil }
func (m *mockExecutionRepo) Get(context.Context, string) (*persistence.Execution, error) {
	return nil, persistence.ErrNotFound
}
func (m *mockExecutionRepo) GetByTaskID(ctx context.Context, taskID string) (*persistence.Execution, error) {
	if m.getByTaskID != nil {
		return m.getByTaskID(ctx, taskID)
	}
	return nil, persistence.ErrNotFound
}
func (m *mockExecutionRepo) GetByTaskIDs(ctx context.Context, taskIDs []string) (map[string]*persistence.Execution, error) {
	out := make(map[string]*persistence.Execution, len(taskIDs))
	for _, id := range taskIDs {
		exec, err := m.GetByTaskID(ctx, id)
		if err == nil && exec != nil {
			out[id] = exec
		}
	}
	return out, nil
}
func (m *mockExecutionRepo) Update(context.Context, *persistence.Execution) error { return nil }
func (m *mockExecutionRepo) List(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	return nil, nil
}
func (m *mockExecutionRepo) Count(context.Context, persistence.ExecutionFilter) (int64, error) {
	return 0, nil
}
func (m *mockExecutionRepo) UpdateStatus(context.Context, string, persistence.ExecutionStatus) error {
	return nil
}
func (m *mockExecutionRepo) SaveStateSnapshot(context.Context, string, []byte, string, []string) error {
	return nil
}
func (m *mockExecutionRepo) SetWorkflowSnapshot(context.Context, string, []byte) error {
	return nil
}
func (m *mockExecutionRepo) GetWorkflowSnapshot(context.Context, string) ([]byte, error) {
	return nil, nil
}
func (m *mockExecutionRepo) RecordCompletion(context.Context, string, []byte) error { return nil }
func (m *mockExecutionRepo) RecordFailure(context.Context, string, string, string) error {
	return nil
}
func (m *mockExecutionRepo) SupersedeNonTerminalForTask(context.Context, string) (int64, error) {
	return 0, nil
}

func (m *mockExecutionRepo) SupersedeOrphanPausedExecutions(context.Context) (int64, error) {
	return 0, nil
}
func (m *mockExecutionRepo) CountByStatus(context.Context, string) (map[persistence.ExecutionStatus]int64, error) {
	return nil, nil
}
func (m *mockExecutionRepo) GetRoleQuality(context.Context, string, time.Duration) (map[string]*persistence.RoleQuality, error) {
	return nil, nil
}

func testWebhookRegistry(t *testing.T) *registry.Registry {
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
    role: worker
    prompt: "do work"
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

func signWebhook(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestServer_GetTask_MissingIDs(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))

	req := httptest.NewRequest(http.MethodGet, "/projects//tasks/", nil)
	rec := httptest.NewRecorder()

	server.GetTask(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestServer_GetTask_NotFound(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return nil, persistence.ErrNotFound
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/test/tasks/nonexistent", nil)
	rec := httptest.NewRecorder()

	server.GetTask(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServer_GetTask_SQLNoRowsMapsToNotFound(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return nil, sql.ErrNoRows
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/test/tasks/nonexistent", nil)
	rec := httptest.NewRecorder()

	server.GetTask(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServer_GetTask_Success(t *testing.T) {
	now := time.Now()
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "project-1",
				Status:    persistence.TaskStatusQueued,
				Priority:  50,
				Payload:   []byte(`{"taskType":"test"}`),
				CreatedAt: now,
			}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/project-1/tasks/task-1", nil)
	rec := httptest.NewRecorder()

	server.GetTask(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "task-1")
}

func TestServer_GetTask_IncludesLastError(t *testing.T) {
	now := time.Now()
	lastError := "podman run failed: image not found"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "project-1",
				Status:    persistence.TaskStatusFailed,
				Priority:  50,
				Payload:   []byte(`{"taskType":"test"}`),
				LastError: &lastError,
				CreatedAt: now,
			}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/project-1/tasks/task-1", nil)
	rec := httptest.NewRecorder()

	server.GetTask(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"lastError":"podman run failed: image not found"`)
}

func TestServer_ListTasks_MissingProjectID(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))

	req := httptest.NewRequest(http.MethodGet, "/projects//tasks", nil)
	rec := httptest.NewRecorder()

	server.ListTasks(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestServer_ListTasks_WithRepo(t *testing.T) {
	now := time.Now()
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{
					ID:        "task-1",
					ProjectID: "project-1",
					Status:    persistence.TaskStatusQueued,
					Priority:  50,
					Payload:   []byte(`{"taskType":"test"}`),
					CreatedAt: now,
				},
			}, nil
		},
		// Total now comes from a separate Count call so paginated
		// clients can tell when to stop. The audit fix means the
		// number is no longer tied to the current page length.
		CountFunc: func(_ context.Context, _ persistence.TaskFilter) (int64, error) {
			return 1, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/project-1/tasks", nil)
	rec := httptest.NewRecorder()

	server.ListTasks(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"total":1`)
}

// TestServer_ListTasks_TotalIsFullCountNotPageSize is the audit fix
// in action: when Count reports 42 but the page only carries 5
// tasks, the response Total must be 42 so a client paging through
// can tell when to stop. Pre-fix the handler returned len(page),
// which made client-side pagination impossible.
func TestServer_ListTasks_TotalIsFullCountNotPageSize(t *testing.T) {
	now := time.Now()
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			page := make([]*persistence.Task, 5)
			for i := range page {
				page[i] = &persistence.Task{
					ID:        fmt.Sprintf("task-%d", i),
					ProjectID: "project-1",
					Status:    persistence.TaskStatusQueued,
					Priority:  50,
					CreatedAt: now,
				}
			}
			return page, nil
		},
		CountFunc: func(_ context.Context, _ persistence.TaskFilter) (int64, error) {
			return 42, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodGet, "/projects/project-1/tasks?pageSize=5", nil)
	rec := httptest.NewRecorder()
	server.ListTasks(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"total":42`,
		"Total must be the full count, not the current page size")
}

func TestGenerateID(t *testing.T) {
	id := persistence.GenerateID("task")
	assert.Contains(t, id, "task_")
	assert.NotEmpty(t, id)
}

func TestStrPtr(t *testing.T) {
	ptr := strPtr("test")
	require.NotNil(t, ptr)
	assert.Equal(t, "test", *ptr)

	nilPtr := strPtr("")
	assert.Nil(t, nilPtr)
}

func TestMarshalTaskPayload(t *testing.T) {
	data, err := marshalTaskPayload(map[string]string{"key": "value"})
	assert.NoError(t, err)
	assert.Contains(t, string(data), "key")
}

func TestParseIntParam(t *testing.T) {
	assert.Equal(t, 10, parseIntParam("10", 5))
	assert.Equal(t, 5, parseIntParam("", 5))
	assert.Equal(t, 5, parseIntParam("invalid", 5))
}

func TestParsePageSize(t *testing.T) {
	// In-range value passes through.
	assert.Equal(t, 50, parsePageSize("50", 20))
	// Empty / garbage falls back to default.
	assert.Equal(t, 20, parsePageSize("", 20))
	assert.Equal(t, 20, parsePageSize("not-a-number", 20))
	// Zero / negative are nonsensical for LIMIT — fall back to default
	// rather than letting the repo omit the LIMIT clause and stream
	// every row in the table.
	assert.Equal(t, 20, parsePageSize("0", 20))
	assert.Equal(t, 20, parsePageSize("-1", 20))
	// Above the cap we clamp instead of trusting the caller. Otherwise a
	// pageSize=2147483647 buffers an unbounded result set in the daemon.
	assert.Equal(t, maxPageSize, parsePageSize("2147483647", 20))
}

func TestParseOffset(t *testing.T) {
	assert.Equal(t, 0, parseOffset(""))
	assert.Equal(t, 0, parseOffset("garbage"))
	assert.Equal(t, 0, parseOffset("-5"))
	assert.Equal(t, 100, parseOffset("100"))
}

func TestRandomString(t *testing.T) {
	id := persistence.GenerateID("test")
	// Format: test_YYYYMMDDHHMMSS_<16 hex chars>
	assert.True(t, len(id) > 20, "expected ID longer than 20 chars")
}

func TestServer_PathExtraction(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/projects/project-1/tasks/task-1", nil)

	assert.Equal(t, "project-1", extractProjectID(req))
	assert.Equal(t, "task-1", extractTaskID(req))
}

func TestServer_PathExtractionRejectsInvalidID(t *testing.T) {
	// Non-charset characters in the segment must be rejected so future
	// handlers that feed the ID into a filesystem path or shell command
	// don't have to re-derive the guarantee. ServeMux already cleans
	// `..` before reaching us; this layer adds a charset cap.
	cases := []struct {
		name string
		path string
	}{
		{"dot-segment", "/projects/..%2Fetc/tasks/task-1"},
		{"space-encoded", "/projects/foo%20bar/tasks/task-1"},
		{"shell-meta", "/projects/foo;rm/tasks/task-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			assert.Equal(t, "", extractProjectID(req), "expected invalid project ID to be rejected")
		})
	}
}

func TestIsValidPathID(t *testing.T) {
	assert.True(t, isValidPathID("project-1"))
	assert.True(t, isValidPathID("task_20260506_abcdef0123456789"))
	assert.True(t, isValidPathID("ABC123"))
	assert.False(t, isValidPathID(""))
	assert.False(t, isValidPathID(".."))
	assert.False(t, isValidPathID("foo/bar"))
	assert.False(t, isValidPathID("foo bar"))
	assert.False(t, isValidPathID("foo;rm"))
	// Length cap — prevents pathologically long IDs from reaching the
	// repo layer.
	assert.False(t, isValidPathID(strings.Repeat("a", maxPathIDLen+1)))
}

func TestRouter_PublicEndpointsBypassAuth(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))
	cfg := &config.Config{
		API: config.APIConfig{AuthEnabled: true},
	}
	router := NewRouter(server, cfg)

	healthReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthRec := httptest.NewRecorder()
	router.Handler().ServeHTTP(healthRec, healthReq)
	assert.Equal(t, http.StatusOK, healthRec.Code)

	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRec := httptest.NewRecorder()
	router.Handler().ServeHTTP(metricsRec, metricsReq)
	assert.Equal(t, http.StatusOK, metricsRec.Code)
}

func TestRouter_ProtectedEndpointsRequireAuth(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))
	cfg := &config.Config{
		API: config.APIConfig{AuthEnabled: true},
	}
	router := NewRouter(server, cfg)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/project-1/tasks", bytes.NewBufferString(`{"taskType":"test"}`))
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRouter_ProtectedEndpointsAllowValidAuth(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))
	cfg := &config.Config{
		API: config.APIConfig{AuthEnabled: true, APIKeys: []string{"secret-key"}},
	}
	router := NewRouter(server, cfg)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/project-1/tasks", bytes.NewBufferString(`{"taskType":"test"}`))
	req.Header.Set("Authorization", "Bearer secret-key")
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code)
}

func TestRespondJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	respondJSON(rec, http.StatusOK, map[string]string{"status": "ok"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
}

// RetryTask tests

func TestServer_RetryTask_Success_FailedTask(t *testing.T) {
	now := time.Now()
	var releaseOpts persistence.ReleaseOptions
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "project-1",
				Status:    persistence.TaskStatusFailed,
				Priority:  50,
				Attempt:   1,
				CreatedAt: now,
				UpdatedAt: now,
			}, nil
		},
		// RetryTask now uses the atomic terminal-to-QUEUED
		// transition (RequeueTerminalTask). Capture the args so
		// existing assertions on attempt counters still apply.
		RequeueTerminalTaskFunc: func(ctx context.Context, taskID string, attempt, maxAttempts int) (bool, error) {
			assert.Equal(t, "task-1", taskID)
			releaseOpts = persistence.ReleaseOptions{Attempt: attempt, MaxAttempts: maxAttempts}
			return true, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/retry", nil)
	rec := httptest.NewRecorder()

	server.RetryTask(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Contains(t, rec.Body.String(), `"status":"QUEUED"`)
	assert.Contains(t, rec.Body.String(), `"attempt":2`)
	assert.Equal(t, 2, releaseOpts.Attempt)
}

func TestServer_RetryTask_Success_CancelledTask(t *testing.T) {
	now := time.Now()
	var releaseOpts persistence.ReleaseOptions
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "project-1",
				Status:    persistence.TaskStatusCancelled,
				Priority:  50,
				Attempt:   2,
				CreatedAt: now,
				UpdatedAt: now,
			}, nil
		},
		RequeueTerminalTaskFunc: func(ctx context.Context, taskID string, attempt, maxAttempts int) (bool, error) {
			releaseOpts = persistence.ReleaseOptions{Attempt: attempt, MaxAttempts: maxAttempts}
			return true, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/retry", nil)
	rec := httptest.NewRecorder()

	server.RetryTask(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Contains(t, rec.Body.String(), `"status":"QUEUED"`)
	assert.Contains(t, rec.Body.String(), `"attempt":3`)
	assert.Equal(t, 3, releaseOpts.Attempt)
}

func TestServer_RetryTask_Success_CompletedTask(t *testing.T) {
	now := time.Now()
	var releaseOpts persistence.ReleaseOptions
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "project-1",
				Status:    persistence.TaskStatusCompleted,
				Priority:  50,
				Attempt:   1,
				CreatedAt: now,
				UpdatedAt: now,
			}, nil
		},
		RequeueTerminalTaskFunc: func(ctx context.Context, taskID string, attempt, maxAttempts int) (bool, error) {
			releaseOpts = persistence.ReleaseOptions{Attempt: attempt, MaxAttempts: maxAttempts}
			return true, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/retry", nil)
	rec := httptest.NewRecorder()

	server.RetryTask(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Contains(t, rec.Body.String(), `"status":"QUEUED"`)
	assert.Equal(t, 2, releaseOpts.Attempt)
}

func TestServer_RetryTask_InvalidState_RunningTask(t *testing.T) {
	now := time.Now()
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "project-1",
				Status:    persistence.TaskStatusRunning,
				Priority:  50,
				Attempt:   1,
				CreatedAt: now,
				UpdatedAt: now,
			}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/retry", nil)
	rec := httptest.NewRecorder()

	server.RetryTask(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_STATE")
	assert.Contains(t, rec.Body.String(), "RUNNING")
}

func TestServer_RetryTask_InvalidState_QueuedTask(t *testing.T) {
	now := time.Now()
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "project-1",
				Status:    persistence.TaskStatusQueued,
				Priority:  50,
				Attempt:   1,
				CreatedAt: now,
				UpdatedAt: now,
			}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/retry", nil)
	rec := httptest.NewRecorder()

	server.RetryTask(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_STATE")
}

func TestServer_RetryTask_NotFound(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return nil, persistence.ErrNotFound
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/nonexistent/retry", nil)
	rec := httptest.NewRecorder()

	server.RetryTask(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "NOT_FOUND")
}

func TestServer_RetryTask_ProjectMismatch(t *testing.T) {
	now := time.Now()
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "different-project",
				Status:    persistence.TaskStatusFailed,
				Priority:  50,
				Attempt:   1,
				CreatedAt: now,
				UpdatedAt: now,
			}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/retry", nil)
	rec := httptest.NewRecorder()

	server.RetryTask(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "NOT_FOUND")
}

func TestServer_RetryTask_ResetAttempts(t *testing.T) {
	now := time.Now()
	var releaseOpts persistence.ReleaseOptions
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "project-1",
				Status:    persistence.TaskStatusFailed,
				Priority:  50,
				Attempt:   5,
				CreatedAt: now,
				UpdatedAt: now,
			}, nil
		},
		RequeueTerminalTaskFunc: func(ctx context.Context, taskID string, attempt, maxAttempts int) (bool, error) {
			releaseOpts = persistence.ReleaseOptions{Attempt: attempt, MaxAttempts: maxAttempts}
			return true, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	body := `{"resetAttempts": true}`
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/retry", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	server.RetryTask(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Contains(t, rec.Body.String(), `"attempt":1`)
	assert.Equal(t, 1, releaseOpts.Attempt)
}

// TestServer_RetryTask_MalformedBody guards against silently accepting
// invalid JSON on retry. Before the fix, a parse error was discarded and
// the handler continued with a zero-valued request, which meant flags like
// resetAttempts could be lost without the client realising.
func TestServer_RetryTask_MalformedBody(t *testing.T) {
	getCalls := 0
	releaseCalls := 0
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			getCalls++
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "project-1",
				Status:    persistence.TaskStatusFailed,
				Priority:  50,
				Attempt:   1,
			}, nil
		},
		RequeueTerminalTaskFunc: func(ctx context.Context, taskID string, attempt, maxAttempts int) (bool, error) {
			releaseCalls++
			return true, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	body := `{this-is-not-json`
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/retry", bytes.NewBufferString(body))
	// Content-Length must be non-zero to trigger decoding.
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()

	server.RetryTask(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"malformed body must produce 400, not silent acceptance")
	assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
	// The handler must refuse early — no release/requeue side effects.
	assert.Equal(t, 0, releaseCalls,
		"ReleaseLease should not be called when the request body is rejected")
}

// TestServer_RetryTask_UnknownField ensures DisallowUnknownFields is
// wired — a client that sends a typo in the body must see a 400 instead
// of a silent success with their flag ignored.
func TestServer_RetryTask_UnknownField(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "project-1",
				Status:    persistence.TaskStatusFailed,
				Priority:  50,
				Attempt:   1,
			}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	body := `{"reset_attempts": true}` // wrong spelling — canonical is resetAttempts
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/retry", bytes.NewBufferString(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()

	server.RetryTask(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
}

// TestServer_RetryTask_TerminalRaceReturns409 — the headline post-
// audit behaviour. The handler reads the task as terminal, but by
// the time the atomic transition fires, an operator has already
// requeued it (or the row otherwise moved out of the FAILED/
// CANCELLED/COMPLETED set). The atomic UPDATE returns 0 rows; the
// handler must surface 409 with the live status, not silently
// re-queue an in-flight task. Pre-fix the legacy ReleaseLease(""
// leaseID) path would have happily clobbered the active state.
func TestServer_RetryTask_TerminalRaceReturns409(t *testing.T) {
	now := time.Now()
	calls := 0
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			calls++
			// First Get: handler sees task as FAILED.
			// Second Get (after the conflict): live row is RUNNING.
			if calls == 1 {
				return &persistence.Task{
					ID: "task-1", ProjectID: "project-1",
					Status: persistence.TaskStatusFailed, Attempt: 1, CreatedAt: now,
				}, nil
			}
			return &persistence.Task{
				ID: "task-1", ProjectID: "project-1",
				Status: persistence.TaskStatusRunning, Attempt: 2, CreatedAt: now,
			}, nil
		},
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, _, _ int) (bool, error) {
			return false, nil // race lost — task is no longer terminal
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/retry", nil)
	rec := httptest.NewRecorder()
	server.RetryTask(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "RUNNING")
	assert.Contains(t, rec.Body.String(), "INVALID_STATE")
}

func TestServer_RetryTask_MissingIDs(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))

	req := httptest.NewRequest(http.MethodPost, "/projects//tasks//retry", nil)
	rec := httptest.NewRecorder()

	server.RetryTask(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// CancelTask tests

func TestServer_CancelTask_NotFound(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return nil, persistence.ErrNotFound
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/cancel", nil)
	rec := httptest.NewRecorder()

	server.CancelTask(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServer_CancelTask_WrongStatus(t *testing.T) {
	now := time.Now()
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "project-1",
				Status:    persistence.TaskStatusCompleted,
				Priority:  50,
				CreatedAt: now,
			}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/cancel", nil)
	rec := httptest.NewRecorder()

	server.CancelTask(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_STATE")
}

func TestServer_CancelTask_Success(t *testing.T) {
	now := time.Now()
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "project-1",
				Status:    persistence.TaskStatusQueued,
				Priority:  50,
				CreatedAt: now,
			}, nil
		},
		// CancelTask now uses the atomic TransitionToCancelled
		// transition rather than UpdateStatus directly, so the
		// success path goes through TransitionToCancelledFunc.
		TransitionToCancelledFunc: func(ctx context.Context, id string) (bool, error) {
			return true, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/cancel", nil)
	rec := httptest.NewRecorder()

	server.CancelTask(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "cancelled")
}

// TestServer_CancelTask_TerminalRaceDoesNotOverwrite is the headline
// post-audit behaviour: the handler reads the task as RUNNING, but
// the task COMPLETED in the gap before the atomic transition. The
// new TransitionToCancelled returns 0 rows; the handler must NOT
// then call UpdateStatus(CANCELLED) (which the legacy code did,
// silently overwriting the COMPLETED terminal state). It must
// surface 409 with the live status instead.
func TestServer_CancelTask_TerminalRaceDoesNotOverwrite(t *testing.T) {
	now := time.Now()
	calls := 0
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			calls++
			if calls == 1 {
				return &persistence.Task{
					ID: "task-1", ProjectID: "project-1",
					Status: persistence.TaskStatusRunning, Priority: 50, CreatedAt: now,
				}, nil
			}
			return &persistence.Task{
				ID: "task-1", ProjectID: "project-1",
				Status: persistence.TaskStatusCompleted, Priority: 50, CreatedAt: now,
			}, nil
		},
		TransitionToCancelledFunc: func(_ context.Context, _ string) (bool, error) {
			return false, nil // race: task already terminal
		},
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error {
			t.Fatal("UpdateStatus must not be called when atomic transition fails — that was the TOCTOU bug")
			return nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/cancel", nil)
	rec := httptest.NewRecorder()
	server.CancelTask(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "COMPLETED")
}

func TestServer_CancelTask_WrongProject(t *testing.T) {
	now := time.Now()
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        "task-1",
				ProjectID: "different-project",
				Status:    persistence.TaskStatusQueued,
				Priority:  50,
				CreatedAt: now,
			}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/cancel", nil)
	rec := httptest.NewRecorder()

	server.CancelTask(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// GetExecution tests

func TestServer_GetExecution_NotFound(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Execution, error) {
			return nil, persistence.ErrNotFound
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec-1", nil)
	rec := httptest.NewRecorder()

	server.GetExecution(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServer_GetExecution_Success(t *testing.T) {
	now := time.Now()
	startedAt := now.Add(-5 * time.Minute)
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID:         "exec-1",
				TaskID:     "task-1",
				ProjectID:  "project-1",
				WorkflowID: "workflow-1",
				Status:     persistence.ExecutionStatusRunning,
				StartedAt:  &startedAt,
				CreatedAt:  now,
				UpdatedAt:  now,
			}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec-1", nil)
	rec := httptest.NewRecorder()

	server.GetExecution(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "exec-1")
	assert.Contains(t, rec.Body.String(), "task-1")
}

func TestServer_GetExecution_ForbiddenProject(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID:        "exec-1",
				TaskID:    "task-1",
				ProjectID: "project-2",
				Status:    persistence.ExecutionStatusRunning,
			}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec-1", nil)
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-1"}))
	rec := httptest.NewRecorder()

	server.GetExecution(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "FORBIDDEN")
}

// ListExecutions tests

func TestServer_ListExecutions_EmptyList(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/project-1/executions", nil)
	rec := httptest.NewRecorder()

	server.ListExecutions(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"total":0`)
}

func TestServer_ListExecutions_FilteredList(t *testing.T) {
	now := time.Now()
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{
				{
					ID:        "exec-1",
					TaskID:    "task-1",
					ProjectID: "project-1",
					Status:    persistence.ExecutionStatusRunning,
					CreatedAt: now,
				},
			}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/project-1/executions?status=running&taskId=task-1", nil)
	rec := httptest.NewRecorder()

	server.ListExecutions(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "exec-1")
}

func TestServer_ListExecutions_MissingProjectID(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))

	req := httptest.NewRequest(http.MethodGet, "/projects//executions", nil)
	rec := httptest.NewRecorder()

	server.ListExecutions(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// Defence-in-depth regression — handlers must refuse cross-project URL
// IDs even when reached without ProjectAuthMiddleware in front.

func TestServer_GetTask_ForbiddenProject(t *testing.T) {
	// taskRepo unused — scope check must fire before the DB lookup.
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(&mocks.MockTaskRepository{
			GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
				t.Fatalf("GetTask reached the repository despite scope denial")
				return nil, nil
			},
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/project-2/tasks/task-1", nil)
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-1"}))
	rec := httptest.NewRecorder()

	server.GetTask(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "FORBIDDEN")
}

func TestServer_ListTasks_ForbiddenProject(t *testing.T) {
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(&mocks.MockTaskRepository{
			ListFunc: func(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
				t.Fatalf("ListTasks reached the repository despite scope denial")
				return nil, nil
			},
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/project-2/tasks", nil)
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-1"}))
	rec := httptest.NewRecorder()

	server.ListTasks(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "FORBIDDEN")
}

func TestServer_ListExecutions_ForbiddenProject(t *testing.T) {
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(&mocks.MockExecutionRepository{
			ListFunc: func(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
				t.Fatalf("ListExecutions reached the repository despite scope denial")
				return nil, nil
			},
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/project-2/executions", nil)
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-1"}))
	rec := httptest.NewRecorder()

	server.ListExecutions(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "FORBIDDEN")
}

// extractExecutionID test

func TestExtractExecutionID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec-123", nil)
	assert.Equal(t, "exec-123", extractExecutionID(req))
}

// PauseExecution tests

func TestServer_PauseExecution_Success(t *testing.T) {
	now := time.Now()
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID:        "exec-1",
				TaskID:    "task-1",
				ProjectID: "project-1",
				Status:    persistence.ExecutionStatusRunning,
				CreatedAt: now,
			}, nil
		},
	}

	// Create a mock executor that implements the Pause method
	mockExec := &mockPauseResumeExecutor{
		pauseStatus: &executor.PauseStatus{
			TaskID:      "task-1",
			ExecutionID: "exec-1",
			PausedAt:    time.Now(),
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
		WithExecutor(mockExec),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/pause", nil)
	rec := httptest.NewRecorder()

	server.PauseExecution(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "paused")
	assert.Contains(t, rec.Body.String(), "PAUSED")
}

func TestServer_PauseExecution_NotRunning(t *testing.T) {
	now := time.Now()
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID:        "exec-1",
				TaskID:    "task-1",
				ProjectID: "project-1",
				Status:    persistence.ExecutionStatusCompleted,
				CreatedAt: now,
			}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/pause", nil)
	rec := httptest.NewRecorder()

	server.PauseExecution(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_STATE")
}

func TestServer_PauseExecution_NotFound(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Execution, error) {
			return nil, persistence.ErrNotFound
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/nonexistent/pause", nil)
	rec := httptest.NewRecorder()

	server.PauseExecution(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServer_PauseExecution_ForbiddenProject(t *testing.T) {
	now := time.Now()
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID:        "exec-1",
				TaskID:    "task-1",
				ProjectID: "project-2",
				Status:    persistence.ExecutionStatusRunning,
				CreatedAt: now,
			}, nil
		},
	}

	mockExec := &mockPauseResumeExecutor{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
		WithExecutor(mockExec),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/pause", nil)
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-1"}))
	rec := httptest.NewRecorder()

	server.PauseExecution(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// ResumeExecution tests

func TestServer_ResumeExecution_Success(t *testing.T) {
	now := time.Now()
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID:        "exec-1",
				TaskID:    "task-1",
				ProjectID: "project-1",
				Status:    persistence.ExecutionStatusPaused,
				CreatedAt: now,
			}, nil
		},
	}

	mockExec := &mockPauseResumeExecutor{
		resumeStatus: &executor.ResumeStatus{
			TaskID:      "task-1",
			ExecutionID: "exec-1",
			ResumedAt:   time.Now(),
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
		WithExecutor(mockExec),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/resume", nil)
	rec := httptest.NewRecorder()

	server.ResumeExecution(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "resumed")
	assert.Contains(t, rec.Body.String(), "RUNNING")
}

func TestServer_ResumeExecution_NotPaused(t *testing.T) {
	now := time.Now()
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID:        "exec-1",
				TaskID:    "task-1",
				ProjectID: "project-1",
				Status:    persistence.ExecutionStatusRunning,
				CreatedAt: now,
			}, nil
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/resume", nil)
	rec := httptest.NewRecorder()

	server.ResumeExecution(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_STATE")
}

func TestServer_ResumeExecution_NotFound(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Execution, error) {
			return nil, persistence.ErrNotFound
		},
	}

	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/nonexistent/resume", nil)
	rec := httptest.NewRecorder()

	server.ResumeExecution(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServer_ResumeExecution_ForbiddenProject(t *testing.T) {
	now := time.Now()
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID:        "exec-1",
				TaskID:    "task-1",
				ProjectID: "project-2",
				Status:    persistence.ExecutionStatusPaused,
				CreatedAt: now,
			}, nil
		},
	}

	mockExec := &mockPauseResumeExecutor{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
		WithExecutor(mockExec),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/resume", nil)
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-1"}))
	rec := httptest.NewRecorder()

	server.ResumeExecution(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// --- MCP proxy endpoints ---

// fakeMCPExecutor is a minimal MCPExecutor stub. Returns a fixed tools
// list per project and records Execute calls so tests can assert
// scoping without standing up a real MCP server.
type fakeMCPExecutor struct {
	tools      map[string][]chat.Tool
	executeRet string
	executeErr error

	lastProject  string
	lastTool     string
	lastArgsJSON string
}

func (f *fakeMCPExecutor) Tools(projectID string) []chat.Tool {
	return f.tools[projectID]
}

func (f *fakeMCPExecutor) Execute(_ context.Context, projectID, qualifiedName, argsJSON string) (string, error) {
	f.lastProject = projectID
	f.lastTool = qualifiedName
	f.lastArgsJSON = argsJSON
	return f.executeRet, f.executeErr
}

// TestServer_ListMCPTools_ScopedByProject verifies the
// GET /projects/{id}/mcp/tools endpoint returns the executor's
// project-scoped tool list. This is the path agent containers hit via
// mcp-bridge's HTTP mode.
func TestServer_ListMCPTools_ScopedByProject(t *testing.T) {
	f := &fakeMCPExecutor{
		tools: map[string][]chat.Tool{
			"alice": {
				{Type: "function", Function: chat.ToolFunction{Name: "mcp__gmail__search_emails"}},
				{Type: "function", Function: chat.ToolFunction{Name: "mcp__gmail__read_email"}},
			},
			"bob": {
				{Type: "function", Function: chat.ToolFunction{Name: "mcp__gmail__search_emails"}},
			},
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithMCPExecutor(f))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/alice/mcp/tools", nil)
	rec := httptest.NewRecorder()
	server.ListMCPTools(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Tools []chat.Tool `json:"tools"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Tools, 2, "alice's project sees alice's two gmail tools")
}

// TestServer_ListMCPTools_NoExecutor_EmptyTools confirms the endpoint
// is safe on deployments without MCP configured — empty array, no 500.
func TestServer_ListMCPTools_NoExecutor_EmptyTools(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(&mocks.MockTaskRepository{PingFunc: func(ctx context.Context) error { return nil }}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/alice/mcp/tools", nil)
	rec := httptest.NewRecorder()
	server.ListMCPTools(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"tools":[]`)
}

// TestServer_CallMCPTool_ProjectScopedRouting is the critical path that
// agent tool calls take. Verifies the projectID from the URL is
// forwarded verbatim to the executor — a mismatch here would route one
// operator's call to another's MCP servers.
func TestServer_CallMCPTool_ProjectScopedRouting(t *testing.T) {
	f := &fakeMCPExecutor{executeRet: "hello world"}
	server := NewServer(WithLogger(zerolog.Nop()), WithMCPExecutor(f))

	body := `{"name":"mcp__gmail__search_emails","arguments":{"q":"from:boss"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/alice/mcp/tools/call",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.CallMCPTool(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "alice", f.lastProject, "project must be forwarded verbatim from URL")
	require.Equal(t, "mcp__gmail__search_emails", f.lastTool)
	require.Contains(t, f.lastArgsJSON, `"q":"from:boss"`)

	var resp CallMCPToolResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, "hello world", resp.Text)
}

// TestServer_CallMCPTool_ExecutorErrorSurfacesAsBadGateway — agent
// tool failures bubble up with diagnostic detail, not a generic 500.
func TestServer_CallMCPTool_ExecutorErrorSurfacesAsBadGateway(t *testing.T) {
	f := &fakeMCPExecutor{executeErr: errors.New(`tool "send_email" is not in allowed_tools`)}
	server := NewServer(WithLogger(zerolog.Nop()), WithMCPExecutor(f))

	body := `{"name":"mcp__gmail__send_email"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/alice/mcp/tools/call",
		strings.NewReader(body))
	rec := httptest.NewRecorder()
	server.CallMCPTool(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "allowed_tools")
}

// roleAllowlistTestServer builds a Server wired with a registry whose
// swarm has two roles — "quoter" (allowedTools: [get_quote]) and "coder"
// (no allowedTools) — plus a workflow whose steps map each role to a
// step ID. taskRepo/execRepo return a task in project-1 and an execution
// whose CurrentStepID points at the step for the given role. The request
// carries a task-scoped identity for boundTaskID. Used by the B2 tests.
func roleAllowlistTestServer(t *testing.T, f *fakeMCPExecutor) *Server {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "project.yaml"), []byte(`projectId: project-1
displayName: Project
swarmId: swarm-1
defaultWorkflowId: wf-1
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm.md"), []byte(`---
swarmId: swarm-1
roles:
  - name: quoter
    runtime:
      image: fake-agent
    permissions:
      allowedTools:
        - mcp__broker__get_quote
  - name: coder
    runtime:
      image: fake-agent
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"), []byte(`---
workflowId: wf-1
entrypoint: quote_step
steps:
  quote_step:
    type: agent
    role: quoter
    prompt: "quote"
    on_success: code_step
  code_step:
    type: agent
    role: coder
    prompt: "code"
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(root))

	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "project-1"}, nil
		},
	}
	return NewServer(
		WithLogger(zerolog.Nop()),
		WithMCPExecutor(f),
		WithProjectRegistry(reg),
		WithTaskRepository(taskRepo),
	)
}

// withTaskScopedIdentity returns a copy of r whose context carries a
// task-scoped DB key Identity (name "agent:task_<taskID>") and an
// execution-repo-derived current step role. The X-Task-ID header is set
// so CallMCPTool's existing task-binding accepts it.
func withRoleAllowlistRequest(t *testing.T, server *Server, taskID, currentStepID string) *Server {
	t.Helper()
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			step := currentStepID
			return &persistence.Execution{
				ID: "exec-1", TaskID: taskID, ProjectID: "project-1",
				WorkflowID: "wf-1", CurrentStepID: &step,
			}, nil
		},
	}
	WithExecutionRepository(execRepo)(server)
	return server
}

func taskScopedMCPRequest(taskID, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/project-1/mcp/tools/call",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Task-ID", taskID)
	id := &auth.Identity{
		Extra: map[string]any{
			auth.ExtraDBKeyRow: &persistence.APIKey{
				Name:      persistence.TaskKeyNamePrefix + taskID,
				ProjectID: "project-1",
			},
		},
	}
	ctx := context.WithValue(req.Context(), identityKey, id)
	return req.WithContext(ctx)
}

// TestServer_CallMCPTool_RoleAllowlistBlocksOutOfScopeTool — Finding B2.
// A task whose role allows only {get_quote} calling place_order must be
// refused 403 at the daemon, even though the project enables the broker
// MCP. Pre-fix CallMCPTool authorized by project only; a compromised
// narrow-role agent could reach any project-enabled tool.
func TestServer_CallMCPTool_RoleAllowlistBlocksOutOfScopeTool(t *testing.T) {
	f := &fakeMCPExecutor{executeRet: "should-not-run"}
	server := roleAllowlistTestServer(t, f)
	server = withRoleAllowlistRequest(t, server, "task-quoter", "quote_step")

	req := taskScopedMCPRequest("task-quoter", `{"name":"mcp__broker__place_order"}`)
	rec := httptest.NewRecorder()
	server.CallMCPTool(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Empty(t, f.lastTool, "executor must not be reached for an out-of-allowlist tool")
}

// TestServer_CallMCPTool_RoleAllowlistAllowsInScopeTool — the same
// quoter role calling its allowed tool must pass through (B2).
func TestServer_CallMCPTool_RoleAllowlistAllowsInScopeTool(t *testing.T) {
	f := &fakeMCPExecutor{executeRet: "120.50"}
	server := roleAllowlistTestServer(t, f)
	server = withRoleAllowlistRequest(t, server, "task-quoter", "quote_step")

	req := taskScopedMCPRequest("task-quoter", `{"name":"mcp__broker__get_quote"}`)
	rec := httptest.NewRecorder()
	server.CallMCPTool(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "mcp__broker__get_quote", f.lastTool)
}

// TestServer_CallMCPTool_RoleWithNoAllowlistUnaffected — a role with no
// declared allowedTools preserves the pre-B2 behavior (no restriction),
// so we don't break roles that don't opt in.
func TestServer_CallMCPTool_RoleWithNoAllowlistUnaffected(t *testing.T) {
	f := &fakeMCPExecutor{executeRet: "ok"}
	server := roleAllowlistTestServer(t, f)
	server = withRoleAllowlistRequest(t, server, "task-coder", "code_step")

	req := taskScopedMCPRequest("task-coder", `{"name":"mcp__broker__place_order"}`)
	rec := httptest.NewRecorder()
	server.CallMCPTool(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"a role with empty allowedTools must not be restricted")
	require.Equal(t, "mcp__broker__place_order", f.lastTool)
}

// budgetTestRegistry builds a single-project registry where the
// project carries the supplied daily/monthly soft+hard caps. Used
// by the CreateTask budget-gate tests below to exercise the three
// outcomes (hard breach 429, soft breach allow, repo error allow).
func budgetTestRegistry(t *testing.T, dailySoft, dailyHard float64) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))
	yaml := fmt.Sprintf(`projectId: project-1
displayName: Project
swarmId: swarm-1
defaultWorkflowId: wf-1
budget:
  daily_soft_usd: %f
  daily_hard_usd: %f
`, dailySoft, dailyHard)
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "project.yaml"), []byte(yaml), 0o644))
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
    role: worker
    prompt: "do work"
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

// TestServer_CreateTask_BudgetHardBreachReturns429 — daily hard cap
// at $5, daily spend at $10. The budget gate must return 429
// BUDGET_EXCEEDED and the task must NOT reach the repo's Create.
// Pre-fix this path was untested; a regression in budget.Check or
// the handler's response code would have shipped silently.
func TestServer_CreateTask_BudgetHardBreachReturns429(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		CreateFunc: func(_ context.Context, _ *persistence.Task) error {
			t.Fatal("Create must not be called when the budget hard cap is breached")
			return nil
		},
	}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithLLMUsageRepository(&mockLLMUsageRepo{cost: 10.0}),
		WithProjectRegistry(budgetTestRegistry(t, 0, 5.0)),
	)

	body := `{"taskType":"work"}`
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Contains(t, rec.Body.String(), "BUDGET_EXCEEDED")
}

// TestServer_CreateTask_BudgetSoftBreachStillAdmits — daily soft cap
// at $1, daily spend at $2. The handler logs + notifies but the task
// must still land in the repo. Pre-fix the soft branch was untested.
func TestServer_CreateTask_BudgetSoftBreachStillAdmits(t *testing.T) {
	created := false
	taskRepo := &mocks.MockTaskRepository{
		GetByIdempotencyKeyFunc: func(_ context.Context, _, _ string) (*persistence.Task, error) {
			return nil, persistence.ErrNotFound
		},
		CreateFunc: func(_ context.Context, _ *persistence.Task) error {
			created = true
			return nil
		},
	}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithLLMUsageRepository(&mockLLMUsageRepo{cost: 2.0}),
		WithProjectRegistry(budgetTestRegistry(t, 1.0, 0)),
	)

	body := `{"taskType":"work","idempotencyKey":"k1"}`
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)

	assert.NotEqual(t, http.StatusTooManyRequests, rec.Code,
		"soft breach must not 429; the task is logged + admitted")
	assert.True(t, created, "task creation must proceed despite soft breach")
}

// errBudgetRepo simulates a transient DB error from
// SumCostByProject. The api handler logs + proceeds (fail-open) so
// a flaky usage table can't block legitimate tasks. Pre-fix the
// fail-open branch had no test pinning the behaviour.
type errBudgetRepo struct {
	mockLLMUsageRepo
}

func (e *errBudgetRepo) SumCostByProject(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, errors.New("usage table unreachable")
}

func TestServer_CreateTask_BudgetRepoErrorAdmits(t *testing.T) {
	created := false
	taskRepo := &mocks.MockTaskRepository{
		GetByIdempotencyKeyFunc: func(_ context.Context, _, _ string) (*persistence.Task, error) {
			return nil, persistence.ErrNotFound
		},
		CreateFunc: func(_ context.Context, _ *persistence.Task) error {
			created = true
			return nil
		},
	}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithLLMUsageRepository(&errBudgetRepo{}),
		WithProjectRegistry(budgetTestRegistry(t, 0, 5.0)),
	)

	body := `{"taskType":"work","idempotencyKey":"k1"}`
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)

	assert.NotEqual(t, http.StatusTooManyRequests, rec.Code,
		"budget repo error must fail-open, not block legitimate tasks")
	assert.True(t, created, "fail-open: task creation must proceed when budget.Check errors")
}

func (m *mockLLMUsageRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}
func (m *mockLLMUsageRepo) MeanCostByWorkflow(_ context.Context, _, _ string, _, _ time.Time) (float64, int, error) {
	return 0, 0, nil
}

func (e *errBudgetRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}
