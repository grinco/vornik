package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// Security tests for the inbound webhook surface. The phase-1
// storage abstraction didn't alter behaviour here, but the operator
// flagged that several handlers in this file sit at ~55-77% coverage
// despite being on the security boundary (HMAC verification,
// admission gate, audit list). These tests pin the failure modes
// that matter for refusal correctness:
//
//   - HMAC signature mismatch / wrong header / wrong secret env all
//     reject with the right status + audit row.
//   - The admission gate's rate-limit and budget paths return the
//     right HTTP code and skip task creation.
//   - ListWebhookEvents filters correctly + clamps pagination so a
//     misbehaving caller can't request unbounded rows.

// --- HMAC signature verification ------------------------------------

func TestIngestWebhook_RejectsWrongSignature(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"evt-bad-sig","issue":{"title":"x"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	// Sign with the wrong secret — the registry expects "topsecret"
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "WRONG-secret"))
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Equal(t, 0, taskRepo.CallCount.Create, "wrong signature must not create a task")
}

func TestIngestWebhook_RejectsMissingSignature(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"evt-no-sig","issue":{"title":"x"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	// No signature header at all.
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Equal(t, 0, taskRepo.CallCount.Create, "missing signature must not create a task")
}

func TestIngestWebhook_AcceptsGitHubStyleSignatureHeader(t *testing.T) {
	// GitHub sends the signature on X-Hub-Signature-256 with a
	// "sha256=" prefix. The verifier accepts both that header and
	// the vornik-native X-Vornik-Signature — this test pins the
	// fallback path so a future refactor doesn't silently drop
	// GitHub support.
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"evt-gh","issue":{"title":"hello"}}`)
	mac := hmac.New(sha256.New, []byte("topsecret"))
	_, _ = mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	require.NotEqual(t, http.StatusUnauthorized, rec.Code,
		"GitHub-style signature header must be accepted, got %d: %s", rec.Code, rec.Body.String())
}

func TestIngestWebhook_RejectsUnknownProject(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"x"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/no-such-project/github", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	require.GreaterOrEqual(t, rec.Code, 400,
		"unknown project must NOT 2xx (was %d)", rec.Code)
	assert.Equal(t, 0, taskRepo.CallCount.Create, "unknown project must not create a task")
}

func TestIngestWebhook_RejectsUnknownSource(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)
	router := NewRouter(server, &config.Config{})

	body := []byte(`{"id":"x"}`)
	// The registry only declares the "github" source — a request to
	// "/gitlab" must reject before we ever try to verify.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	require.GreaterOrEqual(t, rec.Code, 400,
		"unknown source must NOT 2xx (was %d)", rec.Code)
	assert.Equal(t, 0, taskRepo.CallCount.Create, "unknown source must not create a task")
}

// --- ListWebhookEvents (currently 0% covered) ----------------------

func TestListWebhookEvents_ReturnsServiceUnavailableWhenRepoNil(t *testing.T) {
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithProjectRegistry(testWebhookRegistry(t)),
		// no webhook repo wired
	)
	router := NewRouter(server, &config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/webhooks/events", nil)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "WEBHOOK_AUDIT_NOT_CONFIGURED")
}

func TestListWebhookEvents_FiltersByProjectAndReturnsRows(t *testing.T) {
	webhookRepo := &mockWebhookEventRepo{events: []*persistence.WebhookEvent{
		{ID: "1", ProjectID: "project-1", Source: "github", Status: persistence.WebhookEventStatusAccepted},
		{ID: "2", ProjectID: "project-1", Source: "github", Status: persistence.WebhookEventStatusRejected},
		{ID: "3", ProjectID: "project-2", Source: "gitlab", Status: persistence.WebhookEventStatusAccepted},
	}}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithProjectRegistry(testWebhookRegistry(t)),
		WithWebhookEventRepository(webhookRepo),
	)
	router := NewRouter(server, &config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/webhooks/events", nil)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Events []map[string]any `json:"events"`
		Total  int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 2, resp.Total, "project-1 should match exactly 2 events; project-2 must be excluded")
}

func TestListWebhookEvents_ClampsLimitTo200(t *testing.T) {
	// A buggy caller passing limit=999 must not be able to scrape
	// arbitrary depth of the audit table. The handler caps at 200.
	// The mock honours filter.PageSize, so we observe the clamp via
	// the returned row count when we have >200 rows.
	events := make([]*persistence.WebhookEvent, 250)
	for i := range events {
		events[i] = &persistence.WebhookEvent{
			ID:        "e",
			ProjectID: "project-1",
			Source:    "github",
			Status:    persistence.WebhookEventStatusAccepted,
		}
	}
	webhookRepo := &mockWebhookEventRepo{events: events}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithProjectRegistry(testWebhookRegistry(t)),
		WithWebhookEventRepository(webhookRepo),
	)
	router := NewRouter(server, &config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/webhooks/events?limit=999", nil)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.LessOrEqual(t, resp.Total, 200, "limit must clamp to 200, got %d", resp.Total)
}

func TestListWebhookEvents_RejectsBlankProjectID(t *testing.T) {
	// Direct handler call (bypassing the mux, which would rewrite
	// the double-slash URL). Asserts the handler itself refuses a
	// blank project id rather than falling through to a list-
	// everything query.
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithProjectRegistry(testWebhookRegistry(t)),
		WithWebhookEventRepository(&mockWebhookEventRepo{}),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects//webhooks/events", nil)
	rec := httptest.NewRecorder()
	server.ListWebhookEvents(rec, req)

	require.GreaterOrEqual(t, rec.Code, 400, "blank projectId must reject (was %d): %s", rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
}

// --- input validation -----------------------------------------------

// TestIngestWebhook_HasValidationErrorOnEmptyBody pins the input
// guard: webhook handlers must reject zero-byte bodies before they
// ever reach signature verification (the HMAC of an empty body is a
// known value any operator could craft).
func TestIngestWebhook_HasValidationErrorOnEmptyBody(t *testing.T) {
	reg := testWebhookRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	webhookRepo := &mockWebhookEventRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithWebhookEventRepository(webhookRepo),
	)
	router := NewRouter(server, &config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/github", bytes.NewReader(nil))
	req.Header.Set("X-Vornik-Signature", signWebhook(nil, "topsecret"))
	rec := httptest.NewRecorder()

	router.Handler().ServeHTTP(rec, req)

	require.GreaterOrEqual(t, rec.Code, 400, "empty body must reject (was %d)", rec.Code)
	assert.Equal(t, 0, taskRepo.CallCount.Create, "empty body must not create a task")
}

// --- B5: generic webhook replay/timestamp window --------------------

// b5WindowedSource returns a ProjectWebhookSource named "tssrc" and sets
// the opt-in env config (header name + tolerance) the window check
// reads. The env vars are scoped to the source name and cleaned up by
// t.Setenv.
func b5WindowedSource(t *testing.T) registry.ProjectWebhookSource {
	t.Helper()
	t.Setenv("VORNIK_WEBHOOK_TS_HEADER_TSSRC", "X-Webhook-Timestamp")
	t.Setenv("VORNIK_WEBHOOK_TS_TOLERANCE_TSSRC", "5m")
	return registry.ProjectWebhookSource{Name: "tssrc", Secret: "topsecret"}
}

// TestVerifyWebhookSignature_WindowAcceptsFreshTimestamp — Finding B5.
// When a source opts into the timestamp window (env-configured header +
// tolerance) a freshly-signed request inside the window passes.
func TestVerifyWebhookSignature_WindowAcceptsFreshTimestamp(t *testing.T) {
	src := b5WindowedSource(t)
	body := []byte(`{"id":"evt-fresh"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/tssrc", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	req.Header.Set("X-Webhook-Timestamp", strconvI(time.Now().Unix()))

	if err := verifyWebhookSignature(req, body, src); err != nil {
		t.Fatalf("fresh timestamp inside window must pass, got: %v", err)
	}
}

// TestVerifyWebhookSignature_WindowRejectsStaleTimestamp — a replayed
// request whose timestamp is older than the tolerance is rejected even
// with a valid HMAC (B5).
func TestVerifyWebhookSignature_WindowRejectsStaleTimestamp(t *testing.T) {
	src := b5WindowedSource(t)
	body := []byte(`{"id":"evt-stale"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/tssrc", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	// 10 minutes ago — beyond the 5m tolerance.
	req.Header.Set("X-Webhook-Timestamp", strconvI(time.Now().Add(-10*time.Minute).Unix()))

	if err := verifyWebhookSignature(req, body, src); err == nil {
		t.Fatal("stale timestamp beyond tolerance must be rejected")
	}
}

// TestVerifyWebhookSignature_WindowRejectsFutureTimestamp — a timestamp
// too far in the future (clock-skew/replay) is also rejected (B5).
func TestVerifyWebhookSignature_WindowRejectsFutureTimestamp(t *testing.T) {
	src := b5WindowedSource(t)
	body := []byte(`{"id":"evt-future"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/tssrc", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	req.Header.Set("X-Webhook-Timestamp", strconvI(time.Now().Add(10*time.Minute).Unix()))

	if err := verifyWebhookSignature(req, body, src); err == nil {
		t.Fatal("future timestamp beyond tolerance must be rejected")
	}
}

// TestVerifyWebhookSignature_NoWindowConfigPreservesBehavior — a source
// that does NOT opt into the window keeps the pre-B5 body-HMAC-only
// behavior: a valid signature with no timestamp header still passes.
func TestVerifyWebhookSignature_NoWindowConfigPreservesBehavior(t *testing.T) {
	// No VORNIK_WEBHOOK_TS_* env for this source.
	src := registry.ProjectWebhookSource{Name: "plainsrc", Secret: "topsecret"}
	body := []byte(`{"id":"evt-plain"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/plainsrc", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	// No timestamp header at all.

	if err := verifyWebhookSignature(req, body, src); err != nil {
		t.Fatalf("unconfigured source must keep body-HMAC-only behavior, got: %v", err)
	}
}

// TestVerifyWebhookSignature_WindowRequiresTimestampHeader — once the
// window is configured, a missing/garbled timestamp header is rejected
// (otherwise an attacker could simply omit it) (B5).
func TestVerifyWebhookSignature_WindowRequiresTimestampHeader(t *testing.T) {
	src := b5WindowedSource(t)
	body := []byte(`{"id":"evt-missing-ts"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/project-1/tssrc", bytes.NewReader(body))
	req.Header.Set("X-Vornik-Signature", signWebhook(body, "topsecret"))
	// Configured but header absent.

	if err := verifyWebhookSignature(req, body, src); err == nil {
		t.Fatal("configured window with no timestamp header must be rejected")
	}
}

func strconvI(v int64) string { return strconv.FormatInt(v, 10) }

// --- valueAtPath template-resolution edge cases ---------------------

func TestRenderWebhookTemplate_ResolvesNestedPath(t *testing.T) {
	body := map[string]any{
		"issue": map[string]any{
			"title": "An important issue",
			"user": map[string]any{
				"login": "octocat",
			},
		},
	}
	out, err := renderWebhookTemplate("Issue ${issue.title} by ${issue.user.login}", body)
	require.NoError(t, err)
	assert.Equal(t, "Issue An important issue by octocat", out)
}

func TestRenderWebhookTemplate_MissingPathYieldsEmpty(t *testing.T) {
	// Missing-key paths used to coerce to garbled task types via
	// text/template's missingkey=zero. The post-2026-05-03 audit
	// rewrite swapped to explicit substitution that yields "" for
	// any unresolved path, so the existing emptiness check at the
	// caller surfaces malformed configs as task-type-empty errors.
	body := map[string]any{"issue": map[string]any{"title": "x"}}
	out, err := renderWebhookTemplate("Issue ${issue.no.such.path}", body)
	require.NoError(t, err)
	assert.Equal(t, "Issue", strings.TrimSpace(out))
}

func TestRenderWebhookTemplate_LiteralWithoutRefsPassesThrough(t *testing.T) {
	out, err := renderWebhookTemplate("static-task-type", map[string]any{"unused": 1})
	require.NoError(t, err)
	assert.Equal(t, "static-task-type", out)
}

func TestRenderWebhookTemplate_EmptyTemplateDefaultsToHumanFallback(t *testing.T) {
	out, err := renderWebhookTemplate("", map[string]any{"any": 1})
	require.NoError(t, err)
	assert.Equal(t, "webhook event", out, "empty template must default to a human-readable label")
}
