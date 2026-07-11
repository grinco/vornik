package api

// Coverage tests for handlers.go branches left thin by handlers_test.go
// / wizard_auth_off_test.go:
//   - ProjectWizardConverse / Commit error-classification switches
//     (committed / turn-limit / not-found / not-ready / writer-disabled /
//      project-exists / generic) plus the 405 / nil / validation guards
//   - projectWizardRouter unknown-path 404 + trailing-slash dispatch
//   - PauseExecution / ResumeExecution executor-error + missing-dependency
//     branches
//
// Reuses WithProjectWizard / WithExecutionRepository / WithExecutor /
// mockPauseResumeExecutor / mocks.MockExecutionRepository and the
// authEnabledKey context convention from the sibling test files. The
// wizard stub here (hcovWizard) carries injectable errors the existing
// stubWizard lacks for Converse/Commit.

import (
	"context"
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
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// hcovWizard is a ProjectWizard stub with injectable Converse/Commit
// errors so the handler's error-classification switch can be driven
// branch-by-branch.
type hcovWizard struct {
	converseErr        error
	commitErr          error
	confirmScheduleErr error
	converseEnvelope   *ProjectWizardEnvelope
}

func (w *hcovWizard) Converse(_ context.Context, _, _, _ string) (*ProjectWizardResult, error) {
	if w.converseErr != nil {
		return nil, w.converseErr
	}
	if w.converseEnvelope != nil {
		return &ProjectWizardResult{SessionID: "s1", Envelope: w.converseEnvelope}, nil
	}
	return &ProjectWizardResult{SessionID: "s1"}, nil
}

func (w *hcovWizard) Commit(_ context.Context, sessionID, _ string) (*ProjectWizardCommitResult, error) {
	if w.commitErr != nil {
		return nil, w.commitErr
	}
	return &ProjectWizardCommitResult{SessionID: sessionID, ProjectID: "p1", URL: "/ui/projects/p1"}, nil
}

func (w *hcovWizard) Cancel(_ context.Context, _, _ string) error { return nil }

func (w *hcovWizard) ConfirmSchedule(_ context.Context, _, _, _ string) error {
	return w.confirmScheduleErr
}

func hcovWizardServer(w ProjectWizard) *Server {
	return NewServer(WithLogger(zerolog.Nop()), WithProjectWizard(w))
}

// authOffPost builds an auth-off POST request so the single-tenant
// operator fallback resolves and the handler proceeds past the 401.
func hcovAuthOffPost(method, path, body string) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	return req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
}

// --- ProjectWizardConverse guards + error switch -------------------

func TestProjectWizardConverse_WrongMethod_405(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{})
	req := hcovAuthOffPost(http.MethodGet, "/api/v1/projects/wizard/converse", "")
	rec := httptest.NewRecorder()
	s.ProjectWizardConverse(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestProjectWizardConverse_NotWired_503(t *testing.T) {
	s := NewServer(WithLogger(zerolog.Nop()))
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/converse", `{"message":"hi"}`)
	rec := httptest.NewRecorder()
	s.ProjectWizardConverse(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "WIZARD_DISABLED")
}

func TestProjectWizardConverse_EmptyMessage_400(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{})
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/converse", `{"message":"   "}`)
	rec := httptest.NewRecorder()
	s.ProjectWizardConverse(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "message is required")
}

func TestProjectWizardConverse_BadJSON_400(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{})
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/converse", `{bad`)
	rec := httptest.NewRecorder()
	s.ProjectWizardConverse(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestProjectWizardConverse_ErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"committed", errors.New("session already committed"), http.StatusGone, "SESSION_COMMITTED"},
		{"turn-limit", errors.New("turn limit reached"), http.StatusTooManyRequests, "TURN_LIMIT"},
		{"empty-msg", errors.New("empty user message"), http.StatusBadRequest, "VALIDATION_ERROR"},
		{"not-found", errors.New("session not found"), http.StatusNotFound, "NOT_FOUND"},
		{"generic", errors.New("model timed out"), http.StatusBadGateway, "WIZARD_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := hcovWizardServer(&hcovWizard{converseErr: tc.err})
			req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/converse", `{"message":"go"}`)
			rec := httptest.NewRecorder()
			s.ProjectWizardConverse(rec, req)
			assert.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.code)
		})
	}
}

// TestProjectWizardConverse_TierThreeBundleReachesHTTPResponse pins
// the fix for the "tier-3 bundle never reaches the browser" defect: a
// Converse result whose envelope carries a populated Bundle must
// serialise a populated "bundle" object into the actual HTTP JSON
// response body (not just survive in-process as a Go struct field) —
// that's what the wizard-page JS's `if (env.bundle)` branch reads.
func TestProjectWizardConverse_TierThreeBundleReachesHTTPResponse(t *testing.T) {
	bundle := map[string]any{
		"project": map[string]any{"projectId": "custom-proj"},
		"swarm":   map[string]any{"swarmId": "custom-swarm"},
		"workflows": []any{
			map[string]any{"workflowId": "wf1"},
		},
		"plan": map[string]any{
			"steps":              []any{"Step one"},
			"schedule":           "every 6 hours",
			"cost_band":          "$1-5/day",
			"approvals":          []any{"review before send"},
			"approvals_bypassed": []any{"auto-approve small changes"},
		},
	}
	s := hcovWizardServer(&hcovWizard{
		converseEnvelope: &ProjectWizardEnvelope{
			Message:       "Here is your custom automation.",
			ReadyToCommit: true,
			Bundle:        bundle,
		},
	})
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/converse", `{"message":"build me something custom"}`)
	rec := httptest.NewRecorder()
	s.ProjectWizardConverse(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var decoded struct {
		Envelope struct {
			Bundle map[string]any `json:"bundle"`
		} `json:"envelope"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &decoded))
	require.NotNil(t, decoded.Envelope.Bundle, "expected a populated bundle object in the HTTP JSON response, got body: %s", rec.Body.String())
	plan, ok := decoded.Envelope.Bundle["plan"].(map[string]any)
	require.True(t, ok, "bundle.plan should be present, got %v", decoded.Envelope.Bundle)
	assert.Equal(t, "every 6 hours", plan["schedule"])
	assert.Equal(t, []any{"Step one"}, plan["steps"])
}

// TestProjectWizardConverse_NonTierThreeOmitsBundleFromHTTPResponse is
// the negative half of the fix above: a turn with no bundle must NOT
// serialise a "bundle" key at all (not `"bundle":{}`, not
// `"bundle":null`) — the wizard-page JS's `if (env.bundle)` guard
// would otherwise trip on a present-but-empty key.
func TestProjectWizardConverse_NonTierThreeOmitsBundleFromHTTPResponse(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{
		converseEnvelope: &ProjectWizardEnvelope{
			Message:       "Here is your draft.",
			ReadyToCommit: false,
		},
	})
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/converse", `{"message":"hi"}`)
	rec := httptest.NewRecorder()
	s.ProjectWizardConverse(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"bundle"`, "non-tier-3 turn must omit the bundle key entirely")
}

// --- ProjectWizardCommit guards + error switch ---------------------

func TestProjectWizardCommit_WrongMethod_405(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{})
	req := hcovAuthOffPost(http.MethodGet, "/api/v1/projects/wizard/s1/commit", "")
	rec := httptest.NewRecorder()
	s.ProjectWizardCommit(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestProjectWizardCommit_NotWired_503(t *testing.T) {
	s := NewServer(WithLogger(zerolog.Nop()))
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/s1/commit", "")
	rec := httptest.NewRecorder()
	s.ProjectWizardCommit(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestProjectWizardCommit_HappyPath(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{})
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/s1/commit", "")
	rec := httptest.NewRecorder()
	s.ProjectWizardCommit(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "p1")
}

func TestProjectWizardCommit_ErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"not-ready", errors.New("session is not ready to commit"), http.StatusConflict, "NOT_READY"},
		{"no-proposal", errors.New("session has no proposal"), http.StatusConflict, "NO_PROPOSAL"},
		{"revalidation", errors.New("re-validation failed: bad workflow"), http.StatusUnprocessableEntity, "VALIDATION_ERROR"},
		{"writer-disabled", errors.New("project writer not wired"), http.StatusServiceUnavailable, "WRITER_DISABLED"},
		{"not-found", errors.New("session not found"), http.StatusNotFound, "NOT_FOUND"},
		{"project-exists", errors.New("write project: project p1 already exists"), http.StatusConflict, "PROJECT_EXISTS"},
		{"bundle-commit-failed", errors.New("projectwizard: bundle commit failed: a guardrail check failed at commit time: workflow count"), http.StatusConflict, "BUNDLE_COMMIT_FAILED"},
		{"generic", errors.New("disk full"), http.StatusInternalServerError, "WIZARD_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := hcovWizardServer(&hcovWizard{commitErr: tc.err})
			req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/s1/commit", "")
			rec := httptest.NewRecorder()
			s.ProjectWizardCommit(rec, req)
			assert.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.code)
		})
	}
}

// --- ProjectWizardConfirmSchedule guards + error switch -------------

func TestProjectWizardConfirmSchedule_WrongMethod_405(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{})
	req := hcovAuthOffPost(http.MethodGet, "/api/v1/projects/wizard/s1/confirm-schedule", "")
	rec := httptest.NewRecorder()
	s.ProjectWizardConfirmSchedule(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestProjectWizardConfirmSchedule_NotWired_503(t *testing.T) {
	s := NewServer(WithLogger(zerolog.Nop()))
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/s1/confirm-schedule", `{"cron":"24h"}`)
	rec := httptest.NewRecorder()
	s.ProjectWizardConfirmSchedule(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestProjectWizardConfirmSchedule_MissingSessionID_400(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{})
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard//confirm-schedule", `{"cron":"24h"}`)
	rec := httptest.NewRecorder()
	s.ProjectWizardConfirmSchedule(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestProjectWizardConfirmSchedule_MalformedBody_400(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{})
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/s1/confirm-schedule", `{not json`)
	rec := httptest.NewRecorder()
	s.ProjectWizardConfirmSchedule(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestProjectWizardConfirmSchedule_NoOperatorID_401(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{})
	// Auth ENABLED (no authEnabledKey override) and no X-Operator-Id ->
	// the single-tenant fallback doesn't fire, so operatorID resolves
	// empty.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/wizard/s1/confirm-schedule", strings.NewReader(`{"cron":"24h"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ProjectWizardConfirmSchedule(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestProjectWizardConfirmSchedule_HappyPath(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{})
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/s1/confirm-schedule", `{"cron":"24h"}`)
	rec := httptest.NewRecorder()
	s.ProjectWizardConfirmSchedule(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "schedule_confirmed")
}

func TestProjectWizardConfirmSchedule_ErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"committed", errors.New("projectwizard: session already committed"), http.StatusGone, "SESSION_COMMITTED"},
		{"cancelled", errors.New("projectwizard: session already cancelled"), http.StatusNotFound, "NOT_FOUND"},
		{"not-found", errors.New("projectwizard: not found"), http.StatusNotFound, "NOT_FOUND"},
		{"cron-required", errors.New("projectwizard: cron is required"), http.StatusBadRequest, "VALIDATION_ERROR"},
		{"generic", errors.New("disk full"), http.StatusInternalServerError, "WIZARD_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := hcovWizardServer(&hcovWizard{confirmScheduleErr: tc.err})
			req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/s1/confirm-schedule", `{"cron":"24h"}`)
			rec := httptest.NewRecorder()
			s.ProjectWizardConfirmSchedule(rec, req)
			assert.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.code)
		})
	}
}

// --- projectWizardRouter dispatch ----------------------------------

func TestProjectWizardRouter_UnknownPath_404(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{})
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/s1/bogus", "")
	rec := httptest.NewRecorder()
	s.projectWizardRouter(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestProjectWizardRouter_ConverseTrailingSlash(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{})
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/converse/", `{"message":"hi"}`)
	rec := httptest.NewRecorder()
	s.projectWizardRouter(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

func TestProjectWizardRouter_CommitTrailingSlash(t *testing.T) {
	s := hcovWizardServer(&hcovWizard{})
	req := hcovAuthOffPost(http.MethodPost, "/api/v1/projects/wizard/s1/commit/", "")
	rec := httptest.NewRecorder()
	s.projectWizardRouter(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

// --- PauseExecution / ResumeExecution error branches ---------------

func TestPauseExecution_NoRepo_500(t *testing.T) {
	s := NewServer(WithLogger(zerolog.Nop()))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/pause", nil)
	rec := httptest.NewRecorder()
	s.PauseExecution(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "Execution repository not available")
}

func TestPauseExecution_GetError_500(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(context.Context, string) (*persistence.Execution, error) {
			return nil, errors.New("db unreachable")
		},
	}
	s := NewServer(WithLogger(zerolog.Nop()), WithExecutionRepository(execRepo))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/pause", nil)
	rec := httptest.NewRecorder()
	s.PauseExecution(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestPauseExecution_NoExecutor_500(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(context.Context, string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID: "exec-1", TaskID: "t1", ProjectID: "p1",
				Status: persistence.ExecutionStatusRunning,
			}, nil
		},
	}
	// executor intentionally not wired.
	s := NewServer(WithLogger(zerolog.Nop()), WithExecutionRepository(execRepo))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/pause", nil)
	rec := httptest.NewRecorder()
	s.PauseExecution(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "Executor not available")
}

func TestPauseExecution_ExecutorError_500(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(context.Context, string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID: "exec-1", TaskID: "t1", ProjectID: "p1",
				Status: persistence.ExecutionStatusRunning,
			}, nil
		},
	}
	mockExec := &mockPauseResumeExecutor{pauseErr: errors.New("container stuck")}
	s := NewServer(WithLogger(zerolog.Nop()), WithExecutionRepository(execRepo), WithExecutor(mockExec))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/pause", nil)
	rec := httptest.NewRecorder()
	s.PauseExecution(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "Failed to pause execution")
}

func TestPauseExecution_MissingID_400(t *testing.T) {
	s := NewServer(WithLogger(zerolog.Nop()))
	// Path with no execution id segment.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/", nil)
	rec := httptest.NewRecorder()
	s.PauseExecution(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestResumeExecution_NotPaused_400(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(context.Context, string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID: "exec-1", TaskID: "t1", ProjectID: "p1",
				Status: persistence.ExecutionStatusRunning, // not paused
			}, nil
		},
	}
	s := NewServer(WithLogger(zerolog.Nop()), WithExecutionRepository(execRepo))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/resume", nil)
	rec := httptest.NewRecorder()
	s.ResumeExecution(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_STATE")
}

func TestResumeExecution_ExecutorError_500(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(context.Context, string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID: "exec-1", TaskID: "t1", ProjectID: "p1",
				Status: persistence.ExecutionStatusPaused,
			}, nil
		},
	}
	mockExec := &mockPauseResumeExecutor{resumeErr: errors.New("resume blew up")}
	s := NewServer(WithLogger(zerolog.Nop()), WithExecutionRepository(execRepo), WithExecutor(mockExec))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/resume", nil)
	rec := httptest.NewRecorder()
	s.ResumeExecution(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "Failed to resume execution")
}

func TestResumeExecution_Success(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(context.Context, string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID: "exec-1", TaskID: "t1", ProjectID: "p1",
				Status: persistence.ExecutionStatusPaused,
			}, nil
		},
	}
	mockExec := &mockPauseResumeExecutor{
		resumeStatus: &executor.ResumeStatus{TaskID: "t1", ExecutionID: "exec-1", ResumedAt: time.Now()},
	}
	s := NewServer(WithLogger(zerolog.Nop()), WithExecutionRepository(execRepo), WithExecutor(mockExec))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/resume", nil)
	rec := httptest.NewRecorder()
	s.ResumeExecution(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "RUNNING")
}
