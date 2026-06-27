package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite" // in-memory DB for the orphan-FK --fix probe

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

// ---------------------------------------------------------------------
// Shared wave-2 test doubles.
// ---------------------------------------------------------------------

// w2BlockingLimiter is a ratelimit.ProjectLimiter whose Check always
// reports Blocked — the only deterministic way to drive the
// RATE_LIMITED branch of admitWebhookTask without time-based windows.
type w2BlockingLimiter struct {
	recordCalls int
}

func (l *w2BlockingLimiter) Check(_ *registry.Project, _ time.Time) ratelimit.Decision {
	return ratelimit.Decision{Blocked: true, Reason: "minute cap exceeded"}
}
func (l *w2BlockingLimiter) Record(_ string, _ time.Time) { l.recordCalls++ }

// w2SignWebhook mirrors signWebhook but is namespaced so this file is
// self-contained when read in isolation. (signWebhook from
// handlers_test.go is reused for the HTTP-level tests below.)

// ---------------------------------------------------------------------
// IngestWebhook — ingress paths not covered by the existing suite.
// ---------------------------------------------------------------------

// TestW2APIIngestWebhook_NotConfiguredWhenNoTaskRepoNorRelay: a node
// with a project registry but neither a taskRepo nor a webhookRelay
// can't ingest, so the handler must 503 WEBHOOK_NOT_CONFIGURED before
// touching the body.
func TestW2APIIngestWebhook_NotConfiguredWhenNoTaskRepoNorRelay(t *testing.T) {
	reg := testWebhookRegistry(t)
	server := NewServer(WithLogger(zerolog.Nop()), WithProjectRegistry(reg))

	body := []byte(`{"id":"evt-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()
	server.IngestWebhook(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "WEBHOOK_NOT_CONFIGURED", env.Error.Code)
}

// TestW2APIIngestWebhook_MissingProjectOrSourceSegment: a path with no
// project/source segments fails the extractWebhookPath guard with 400
// VALIDATION_ERROR — before any registry or repo lookup.
func TestW2APIIngestWebhook_MissingProjectOrSourceSegment(t *testing.T) {
	srv, _, _ := newWebhookTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	srv.IngestWebhook(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)
}

// TestW2APIIngestWebhook_DuplicateDeliveryReturns202WithoutCreate: a
// delivery whose idempotency key already maps to an existing task must
// be deduped — the handler returns 202 with the EXISTING task id,
// records a "duplicate" audit row, and never calls Create again. This
// is the replay/idempotency guard via the derived webhook idempotency
// key.
func TestW2APIIngestWebhook_DuplicateDeliveryReturns202WithoutCreate(t *testing.T) {
	reg := testWebhookRegistry(t)
	existing := &persistence.Task{ID: "task-existing", ProjectID: "project-1", Status: persistence.TaskStatusQueued}
	taskRepo := &mocks.MockTaskRepository{
		GetByIdempotencyKeyFunc: func(_ context.Context, _, key string) (*persistence.Task, error) {
			// existingWebhookTask resolves the duplicate by key.
			require.Equal(t, "webhook:github:evt-dup", key)
			return existing, nil
		},
	}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)

	body := []byte(`{"id":"evt-dup","issue":{"title":"hi"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()
	server.IngestWebhook(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, 0, taskRepo.CallCount.Create, "duplicate delivery must not create a second task")

	var resp CreateTaskResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "task-existing", resp.TaskID)

	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, persistence.WebhookEventStatusDuplicate, webhookRepo.events[0].Status)
	require.NotNil(t, webhookRepo.events[0].TaskID)
	assert.Equal(t, "task-existing", *webhookRepo.events[0].TaskID)
}

// TestW2APIIngestWebhook_RateLimitedBlocksWithAudit: when the project
// rate limiter reports Blocked, admission fails closed — 429
// RATE_LIMITED, a rejected audit row coded "RATE_LIMITED", and no task
// created.
func TestW2APIIngestWebhook_RateLimitedBlocksWithAudit(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	limiter := &w2BlockingLimiter{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
		WithRateLimiter(limiter),
	)

	body := []byte(`{"id":"evt-rl","issue":{"title":"hi"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()
	server.IngestWebhook(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "RATE_LIMITED", env.Error.Code)
	assert.Equal(t, 0, taskRepo.CallCount.Create)
	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, persistence.WebhookEventStatusRejected, webhookRepo.events[0].Status)
	assert.Equal(t, "RATE_LIMITED", webhookRepo.events[0].ErrorCode)
}

// TestW2APIIngestWebhook_InvalidJSONRejected: a signature-valid body
// that isn't a JSON object is rejected post-verification with 400
// VALIDATION_ERROR and an "invalid_json" audit row.
func TestW2APIIngestWebhook_InvalidJSONRejected(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)

	body := []byte(`not json at all`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()
	server.IngestWebhook(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)
	assert.Equal(t, 0, taskRepo.CallCount.Create)
	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, "invalid_json", webhookRepo.events[0].ErrorCode)
}

// TestW2APIIngestWebhook_CreateTaskFailureReturns500: a non-duplicate
// Create error surfaces as 500 INTERNAL_ERROR with a
// "create_task_failed" audit row — the row is recorded so an operator
// can see why a verified delivery never produced a task.
func TestW2APIIngestWebhook_CreateTaskFailureReturns500(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{
		CreateFunc: func(_ context.Context, _ *persistence.Task) error { return errors.New("db down") },
	}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)

	body := []byte(`{"id":"evt-fail","issue":{"title":"hi"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()
	server.IngestWebhook(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "INTERNAL_ERROR", env.Error.Code)
	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, "create_task_failed", webhookRepo.events[0].ErrorCode)
}

// TestW2APIIngestWebhook_BodyOverLimitRejected: a body larger than
// maxWebhookBodyBytes is short-circuited by the MaxBytesReader read and
// rejected 400 VALIDATION_ERROR with an "invalid_body" audit row,
// before any signature verification.
func TestW2APIIngestWebhook_BodyOverLimitRejected(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)

	big := bytes.Repeat([]byte("a"), maxWebhookBodyBytes+1024)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(big))
	rec := httptest.NewRecorder()
	server.IngestWebhook(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)
	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, "invalid_body", webhookRepo.events[0].ErrorCode)
}

// ---------------------------------------------------------------------
// enqueueVerifiedWebhook — pipeline branches reachable without HMAC.
// ---------------------------------------------------------------------

// TestW2APIEnqueueVerifiedWebhook_TemplateRenderEmptyRejected: a source
// template that resolves to "" (the ${...} path is missing from the
// event) must reject with VALIDATION_ERROR and a "template_error" audit
// row rather than create a task with an empty task type.
func TestW2APIEnqueueVerifiedWebhook_TemplateRenderEmptyRejected(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)
	project := reg.GetProject("project-1")
	source, ok := findWebhookSource(project, "github")
	require.True(t, ok)
	// github source template is "GitHub issue: ${issue.title}" — but the
	// fallback default "webhook event" applies only to an empty template.
	// Override to a pure-ref template that resolves empty.
	source.TaskTypeTemplate = "${missing.path}"

	rec := httptest.NewRecorder()
	body := []byte(`{"id":"evt-tmpl","action":"opened"}`)
	server.enqueueVerifiedWebhook(context.Background(), rec, project, source, body, "d-1")

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)
	assert.Equal(t, 0, taskRepo.CallCount.Create)
	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, "template_error", webhookRepo.events[0].ErrorCode)
}

// TestW2APIEnqueueVerifiedWebhook_RequireForgeEventDropsNonForge: a
// source with RequireForgeEvent and no forge classifier wired produces
// zero forgeJob, so the delivery is filtered (200 status:filtered) with
// a "not_a_forge_event" audit row — no task created.
func TestW2APIEnqueueVerifiedWebhook_RequireForgeEventDropsNonForge(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)
	project := reg.GetProject("project-1")
	source, ok := findWebhookSource(project, "github")
	require.True(t, ok)
	source.RequireForgeEvent = true

	rec := httptest.NewRecorder()
	body := []byte(`{"id":"evt-nf","issue":{"title":"hi"}}`)
	server.enqueueVerifiedWebhook(context.Background(), rec, project, source, body, "d-2")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "filtered")
	assert.Equal(t, 0, taskRepo.CallCount.Create)
	require.Len(t, webhookRepo.events, 1)
	assert.Equal(t, persistence.WebhookEventStatusFiltered, webhookRepo.events[0].Status)
	assert.Equal(t, "not_a_forge_event", webhookRepo.events[0].ErrorCode)
}

// TestW2APIEnqueueVerifiedWebhook_DuplicateViaExistingTask: the
// in-pipeline dedup (existingWebhookTask) returns the prior task and
// responds 202 without creating a new one.
func TestW2APIEnqueueVerifiedWebhook_DuplicateViaExistingTask(t *testing.T) {
	reg := testWebhookRegistry(t)
	prior := &persistence.Task{ID: "task-prior", ProjectID: "project-1", Status: persistence.TaskStatusRunning}
	taskRepo := &mocks.MockTaskRepository{
		GetByIdempotencyKeyFunc: func(_ context.Context, _, _ string) (*persistence.Task, error) {
			return prior, nil
		},
	}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)
	project := reg.GetProject("project-1")
	source, _ := findWebhookSource(project, "github")

	rec := httptest.NewRecorder()
	body := []byte(`{"id":"evt-x","issue":{"title":"hi"}}`)
	server.enqueueVerifiedWebhook(context.Background(), rec, project, source, body, "d-3")

	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, 0, taskRepo.CallCount.Create)
	var resp CreateTaskResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "task-prior", resp.TaskID)
}

// ---------------------------------------------------------------------
// ListWebhookEvents — repo-error and source/status filter parsing.
// ---------------------------------------------------------------------

// w2WebhookEventRepoErr is a webhook event repo whose List always errors.
type w2WebhookEventRepoErr struct{ mockWebhookEventRepo }

func (m *w2WebhookEventRepoErr) List(_ context.Context, _ persistence.WebhookEventFilter) ([]*persistence.WebhookEvent, error) {
	return nil, errors.New("db unreachable")
}

// TestW2APIListWebhookEvents_RepoListErrorReturns500: a List failure
// surfaces as 500 INTERNAL_ERROR, not an empty 200.
func TestW2APIListWebhookEvents_RepoListErrorReturns500(t *testing.T) {
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithWebhookEventRepository(&w2WebhookEventRepoErr{}),
	)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/webhooks/events", nil)
	rec := httptest.NewRecorder()
	server.ListWebhookEvents(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "INTERNAL_ERROR", env.Error.Code)
}

// w2FilterCapturingRepo records the filter List was called with.
type w2FilterCapturingRepo struct {
	mockWebhookEventRepo
	seen persistence.WebhookEventFilter
}

func (m *w2FilterCapturingRepo) List(_ context.Context, f persistence.WebhookEventFilter) ([]*persistence.WebhookEvent, error) {
	m.seen = f
	return []*persistence.WebhookEvent{}, nil
}

// TestW2APIListWebhookEvents_SourceAndStatusFilterPassedThrough: the
// source= and status= query params are trimmed and forwarded into the
// repo filter; the projectId scopes the filter.
func TestW2APIListWebhookEvents_SourceAndStatusFilterPassedThrough(t *testing.T) {
	repo := &w2FilterCapturingRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithWebhookEventRepository(repo))

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/projects/project-1/webhooks/events?source=%20github%20&status=%20ACCEPTED%20&limit=5", nil)
	rec := httptest.NewRecorder()
	server.ListWebhookEvents(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, repo.seen.ProjectID)
	assert.Equal(t, "project-1", *repo.seen.ProjectID)
	require.NotNil(t, repo.seen.Source)
	assert.Equal(t, "github", *repo.seen.Source, "source must be trimmed")
	require.NotNil(t, repo.seen.Status)
	assert.Equal(t, "ACCEPTED", *repo.seen.Status, "status must be trimmed")
	assert.Equal(t, 5, repo.seen.PageSize)
}

// ---------------------------------------------------------------------
// Config Reload — error / blocked / bad-body branches.
// ---------------------------------------------------------------------

// TestW2APIConfigReload_ValidationFailureReturns400: a reload whose
// loader/validator returns an error (and is NOT blocked) responds 400
// with success=false and the error message.
func TestW2APIConfigReload_ValidationFailureReturns400(t *testing.T) {
	watcher := config.NewWatcher([]string{}, config.WithWatchLogger(zerolog.Nop()))
	reloader := config.NewConfigReloader(watcher, zerolog.Nop())
	reloader.SetLoader(func() error { return errors.New("bad yaml in project.yaml") })
	handlers := NewConfigHandlers(reloader)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/reload", nil)
	rec := httptest.NewRecorder()
	handlers.Reload(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var body ReloadResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.False(t, body.Success)
	assert.Contains(t, body.Message, "bad yaml")
}

// TestW2APIConfigReload_InvalidBodyReturns400: a non-JSON request body
// is a client error — 400 VALIDATION_ERROR before any reload attempt.
func TestW2APIConfigReload_InvalidBodyReturns400(t *testing.T) {
	watcher := config.NewWatcher([]string{}, config.WithWatchLogger(zerolog.Nop()))
	reloader := config.NewConfigReloader(watcher, zerolog.Nop())
	reloaded := false
	reloader.SetLoader(func() error { reloaded = true; return nil })
	reloader.SetValidator(func() error { return nil })
	reloader.SetActivator(func() error { return nil })
	handlers := NewConfigHandlers(reloader)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/reload", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	handlers.Reload(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)
	assert.False(t, reloaded, "an unparseable body must short-circuit before the reload runs")
}

// TestW2APIConfigReload_UnknownFieldRejected: decodeJSONBody uses
// DisallowUnknownFields, so a body with an unexpected key is a 400 — the
// reload never fires.
func TestW2APIConfigReload_UnknownFieldRejected(t *testing.T) {
	watcher := config.NewWatcher([]string{}, config.WithWatchLogger(zerolog.Nop()))
	reloader := config.NewConfigReloader(watcher, zerolog.Nop())
	reloader.SetLoader(func() error { return nil })
	reloader.SetValidator(func() error { return nil })
	reloader.SetActivator(func() error { return nil })
	handlers := NewConfigHandlers(reloader)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/reload", bytes.NewBufferString(`{"force":true,"bogus":1}`))
	rec := httptest.NewRecorder()
	handlers.Reload(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)
}

// ---------------------------------------------------------------------
// Doctor — HTTP handler shape + orphan-FK --fix DELETE path.
// ---------------------------------------------------------------------

// TestW2APIRunDoctor_RejectsNonPOST: the doctor endpoint mutates state
// under ?fix=true, so it only accepts POST; a GET is 405.
func TestW2APIRunDoctor_RejectsNonPOST(t *testing.T) {
	h := &DoctorHandlers{db: closedDB(t)}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/doctor", nil)
	rec := httptest.NewRecorder()
	h.RunDoctor(rec, req)

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "METHOD_NOT_ALLOWED", env.Error.Code)
}

// TestW2APIRunDoctor_ReportShapeAndStuckMetricsNilSafe: a full POST run
// against a closed DB returns 200 with a populated report (every check
// present) and a non-empty summary. apiMetrics is nil, exercising the
// nil-safe SetExecutionsStuck path inside checkStuckExecutions without a
// panic.
func TestW2APIRunDoctor_ReportShapeAndStuckMetricsNilSafe(t *testing.T) {
	h := &DoctorHandlers{db: closedDB(t)}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor", nil)
	rec := httptest.NewRecorder()
	h.RunDoctor(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var report DoctorReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report))
	assert.NotEmpty(t, report.Timestamp)
	assert.NotEmpty(t, report.Summary)
	assert.GreaterOrEqual(t, len(report.Checks), 20, "RunDoctor must aggregate the full check set")

	names := map[string]bool{}
	for _, c := range report.Checks {
		names[c.Name] = true
	}
	for _, want := range []string{"stale_leases", "orphaned_watchers", "stuck_executions", "orphan_fk_rows"} {
		assert.Truef(t, names[want], "report must include the %q check", want)
	}
}

// w2OrphanFKDB builds a SQLite schema mirroring the orphan_fk probe and
// seeds exactly one true orphan in task_llm_usage (→deleted task).
func w2OrphanFKDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	stmts := []string{
		`CREATE TABLE tasks (id TEXT PRIMARY KEY)`,
		`CREATE TABLE task_llm_usage (id TEXT PRIMARY KEY, task_id TEXT, source TEXT)`,
		`CREATE TABLE tool_audit_log (id TEXT PRIMARY KEY, task_id TEXT)`,
		`CREATE TABLE task_watchers (task_id TEXT)`,
		`INSERT INTO tasks (id) VALUES ('T1')`,
		`INSERT INTO task_llm_usage (id, task_id, source) VALUES ('u1','T1','workflow_step')`,
		`INSERT INTO task_llm_usage (id, task_id, source) VALUES ('u2','ghost','workflow_step')`,
		`INSERT INTO tool_audit_log (id, task_id) VALUES ('a1','T1')`,
	}
	for _, s := range stmts {
		_, err := db.Exec(s)
		require.NoError(t, err, s)
	}
	return db
}

// TestW2APICheckOrphanFKRows_FixDeletesOnlyTrueOrphans: with fix=true,
// the probe DELETEs the dangling row (u2→ghost) and reports OK once all
// orphans are cleaned, leaving the valid row (u1→T1) intact.
func TestW2APICheckOrphanFKRows_FixDeletesOnlyTrueOrphans(t *testing.T) {
	db := w2OrphanFKDB(t)
	h := &DoctorHandlers{db: db}

	got := h.checkOrphanFKRows(t.Context(), true)
	assert.Equal(t, "orphan_fk_rows", got.Name)
	assert.Equal(t, "OK", got.Status, "all orphans cleaned → OK")
	assert.Equal(t, 1, got.Fixed)

	// The valid row survives; the orphan is gone.
	var remaining int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM task_llm_usage`).Scan(&remaining))
	assert.Equal(t, 1, remaining, "only the dangling row may be deleted")

	var ghost int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM task_llm_usage WHERE task_id='ghost'`).Scan(&ghost))
	assert.Equal(t, 0, ghost)
}

// ---------------------------------------------------------------------
// respondError / respondJSON variants.
// ---------------------------------------------------------------------

// TestW2APIRespondJSON_EncodesBodyAndContentType: respondJSON writes the
// status, application/json content-type, and the encoded value.
func TestW2APIRespondJSON_EncodesBodyAndContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	respondJSON(rec, http.StatusCreated, map[string]any{"ok": true, "n": 7})

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, true, got["ok"])
	assert.Equal(t, float64(7), got["n"])
}

// TestW2APIRespondError_EmptyMessagePreservesCode: respondError with an
// empty message still emits the code and the envelope shape — the
// webhook invalid-signature path relies on this (it deliberately omits
// the detail message).
func TestW2APIRespondError_EmptyMessagePreservesCode(t *testing.T) {
	rec := httptest.NewRecorder()
	respondError(rec, http.StatusUnauthorized, "INVALID_SIGNATURE", "")

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_SIGNATURE", env.Error.Code)
	assert.Equal(t, "", env.Error.Message)
}

// ---------------------------------------------------------------------
// decodeJSONBody — multi-value and unknown-field guards.
// ---------------------------------------------------------------------

// TestW2APIDecodeJSONBody_RejectsTrailingSecondValue: two JSON values in
// one body (a smuggling vector) is rejected with the "multiple JSON
// values" error after the first decode succeeds.
func TestW2APIDecodeJSONBody_RejectsTrailingSecondValue(t *testing.T) {
	var dst struct {
		Force bool `json:"force"`
	}
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(`{"force":true}{"force":false}`))
	rec := httptest.NewRecorder()
	err := decodeJSONBody(rec, req, 1<<20, &dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple JSON values")
}

// TestW2APIDecodeJSONBody_RejectsUnknownField: DisallowUnknownFields
// turns an unexpected key into a decode error.
func TestW2APIDecodeJSONBody_RejectsUnknownField(t *testing.T) {
	var dst struct {
		Force bool `json:"force"`
	}
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(`{"force":true,"sneaky":1}`))
	rec := httptest.NewRecorder()
	err := decodeJSONBody(rec, req, 1<<20, &dst)
	require.Error(t, err)
}

// TestW2APIDecodeJSONBody_SingleValueOK: the happy path decodes and
// returns nil with the field populated.
func TestW2APIDecodeJSONBody_SingleValueOK(t *testing.T) {
	var dst struct {
		Force bool `json:"force"`
	}
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(`{"force":true}`))
	rec := httptest.NewRecorder()
	require.NoError(t, decodeJSONBody(rec, req, 1<<20, &dst))
	assert.True(t, dst.Force)
}

// ---------------------------------------------------------------------
// Webhook pure helpers not covered by the existing suite.
// ---------------------------------------------------------------------

// TestW2APIWebhookEventIDFromBodyOrHeader_PrefersBodyPath: a body whose
// event_id_path resolves wins over the header delivery id.
func TestW2APIWebhookEventIDFromBodyOrHeader_PrefersBodyPath(t *testing.T) {
	body := []byte(`{"id":"from-body"}`)
	got := webhookEventIDFromBodyOrHeader(body, "id", "from-header")
	assert.Equal(t, "from-body", got)
}

// TestW2APIWebhookEventIDFromBodyOrHeader_FallsBackToHeader: when the
// body path is absent the header delivery id is used.
func TestW2APIWebhookEventIDFromBodyOrHeader_FallsBackToHeader(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	got := webhookEventIDFromBodyOrHeader(body, "id", "delivery-9")
	assert.Equal(t, "delivery-9", got)
}

// TestW2APIWebhookEventIDFromBodyOrHeader_HashesWhenNothingElse: no body
// path, no header → a stable hash of the body.
func TestW2APIWebhookEventIDFromBodyOrHeader_HashesWhenNothingElse(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	got := webhookEventIDFromBodyOrHeader(body, "id", "")
	assert.Equal(t, hashWebhookBody(body), got)
	assert.NotEmpty(t, got)
}

// TestW2APIValueAtPath_TypesAndMisses: number formatting, nested path,
// non-object descent (returns ""), and nil leaf.
func TestW2APIValueAtPath_TypesAndMisses(t *testing.T) {
	m := map[string]any{
		"issue":  map[string]any{"number": float64(42), "title": "hi"},
		"flag":   true,
		"scalar": "x",
	}
	assert.Equal(t, "hi", valueAtPath(m, "issue.title"))
	assert.Equal(t, "42", valueAtPath(m, "issue.number"), "float64 must format without decimals")
	assert.Equal(t, "true", valueAtPath(m, "flag"))
	assert.Equal(t, "", valueAtPath(m, ""), "empty path yields empty")
	assert.Equal(t, "", valueAtPath(m, "scalar.deeper"), "descending into a non-object yields empty")
	assert.Equal(t, "", valueAtPath(m, "missing"), "absent key yields empty")
}

// TestW2APIHashAndFullHashDiffer: the short audit hash (8 bytes) and the
// full payload hash (32 bytes) are both hex and differ in length.
func TestW2APIHashAndFullHashDiffer(t *testing.T) {
	body := []byte(`{"id":"evt"}`)
	short := hashWebhookBody(body)
	full := fullWebhookBodyHash(body)
	assert.Len(t, short, 16, "8-byte prefix → 16 hex chars")
	assert.Len(t, full, 64, "32-byte sha256 → 64 hex chars")
	assert.True(t, strings.HasPrefix(full, short), "the short hash is a prefix of the full hash")
}

// TestW2APITruncateWebhookAuditField: over-length strings are clipped
// with an ellipsis; short strings and non-positive max pass through.
func TestW2APITruncateWebhookAuditField(t *testing.T) {
	assert.Equal(t, "abc", truncateWebhookAuditField("abc", 10))
	assert.Equal(t, "ab...", truncateWebhookAuditField("abcdef", 2))
	assert.Equal(t, "abcdef", truncateWebhookAuditField("abcdef", 0), "non-positive max disables truncation")
}

// TestW2APIWebhookSourceEnvSuffix: lower-cases→upper, digits pass, and
// any other byte (dash, dot) becomes underscore so the result is a valid
// env-var suffix.
func TestW2APIWebhookSourceEnvSuffix(t *testing.T) {
	assert.Equal(t, "GITHUB", webhookSourceEnvSuffix("github"))
	assert.Equal(t, "MY_SRC_2", webhookSourceEnvSuffix("my-src.2"))
	assert.Equal(t, "A_B", webhookSourceEnvSuffix("a b"))
}

// TestW2APIResolveFilterRef: a well-formed ${path} resolves against the
// event; anything not wrapped in ${...} yields "".
func TestW2APIResolveFilterRef(t *testing.T) {
	event := map[string]any{"action": "opened"}
	assert.Equal(t, "opened", resolveFilterRef("${action}", event))
	assert.Equal(t, "", resolveFilterRef("action", event), "bare path (no ${}) is not a reference")
	assert.Equal(t, "", resolveFilterRef("${missing}", event))
}

// TestW2APIMergeTopLevelJSON_PreservesAndAddsKey: merging attaches the
// new key without disturbing existing ones, and seeds an object when the
// payload was an empty object.
func TestW2APIMergeTopLevelJSON_PreservesAndAddsKey(t *testing.T) {
	out, err := mergeTopLevelJSON([]byte(`{"taskType":"x"}`), "forge_job", json.RawMessage(`{"is_change_request":true}`))
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &m))
	assert.JSONEq(t, `"x"`, string(m["taskType"]))
	assert.JSONEq(t, `{"is_change_request":true}`, string(m["forge_job"]))

	// Non-object payload is a hard error (caught at marshal time).
	_, err = mergeTopLevelJSON([]byte(`["not","an","object"]`), "k", json.RawMessage(`1`))
	require.Error(t, err)
}
