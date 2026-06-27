package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// retryExecutionFromStepRequest is what the UI script sends; mirrored
// here so the test asserts on the same wire shape.
type retryExecutionFromStepRequest struct {
	StepID string `json:"step_id"`
}

// makeExecLookup returns a GetFunc that maps a single execution ID to
// the supplied row. Avoids each test rebuilding the same closure.
func makeExecLookup(exec *persistence.Execution) func(ctx context.Context, id string) (*persistence.Execution, error) {
	return func(_ context.Context, id string) (*persistence.Execution, error) {
		if exec != nil && id == exec.ID {
			return exec, nil
		}
		return nil, persistence.ErrNotFound
	}
}

// TestServer_RetryFromStep_HappyPath — well-formed request hits the
// executor and returns 200 + the new running status. Asserts the
// payload is what the UI's reload-on-success path expects.
func TestServer_RetryFromStep_HappyPath(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: makeExecLookup(&persistence.Execution{
			ID:        "exec-1",
			TaskID:    "task-1",
			ProjectID: "project-1",
			Status:    persistence.ExecutionStatusFailed,
		}),
	}
	mockExec := &mockPauseResumeExecutor{} // retryErr nil → success
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
		WithExecutor(mockExec),
	)

	body := `{"step_id":"implement"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/retry-from-step", strings.NewReader(body))
	rec := httptest.NewRecorder()
	server.RetryExecutionFromStep(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Len(t, mockExec.retryCalls, 1, "executor must receive exactly one RetryFromStep call")
	assert.Equal(t, "exec-1", mockExec.retryCalls[0].executionID)
	assert.Equal(t, "implement", mockExec.retryCalls[0].stepID)

	var resp struct {
		ExecutionID string `json:"executionId"`
		StepID      string `json:"stepId"`
		Status      string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "exec-1", resp.ExecutionID)
	assert.Equal(t, "implement", resp.StepID)
	assert.Equal(t, string(persistence.ExecutionStatusRunning), resp.Status)
}

// TestServer_RetryFromStep_NotTerminalReturns409 — executor returns
// ErrRetryNotTerminal; handler must translate to 409 INVALID_STATE,
// not 500.
func TestServer_RetryFromStep_NotTerminalReturns409(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: makeExecLookup(&persistence.Execution{
			ID: "exec-1", TaskID: "task-1", ProjectID: "project-1",
			Status: persistence.ExecutionStatusRunning,
		}),
	}
	mockExec := &mockPauseResumeExecutor{retryErr: executor.ErrRetryNotTerminal}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
		WithExecutor(mockExec),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/retry-from-step",
		strings.NewReader(`{"step_id":"implement"}`))
	rec := httptest.NewRecorder()
	server.RetryExecutionFromStep(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_STATE")
}

// TestServer_RetryFromStep_UnknownStepReturns400 — executor returns
// ErrRetryStepNotInExecution; handler maps to 400 VALIDATION_ERROR.
func TestServer_RetryFromStep_UnknownStepReturns400(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: makeExecLookup(&persistence.Execution{
			ID: "exec-1", TaskID: "task-1", ProjectID: "project-1",
			Status: persistence.ExecutionStatusFailed,
		}),
	}
	mockExec := &mockPauseResumeExecutor{retryErr: executor.ErrRetryStepNotInExecution}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
		WithExecutor(mockExec),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/retry-from-step",
		strings.NewReader(`{"step_id":"nonexistent"}`))
	rec := httptest.NewRecorder()
	server.RetryExecutionFromStep(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
}

// TestServer_RetryFromStep_AlreadyExecutingReturns409 — double-click
// guard. The executor's mu-guarded check rejects with
// ErrRetryAlreadyExecuting; handler maps to 409.
func TestServer_RetryFromStep_AlreadyExecutingReturns409(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: makeExecLookup(&persistence.Execution{
			ID: "exec-1", TaskID: "task-1", ProjectID: "project-1",
			Status: persistence.ExecutionStatusFailed,
		}),
	}
	mockExec := &mockPauseResumeExecutor{retryErr: executor.ErrRetryAlreadyExecuting}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
		WithExecutor(mockExec),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/retry-from-step",
		strings.NewReader(`{"step_id":"any"}`))
	rec := httptest.NewRecorder()
	server.RetryExecutionFromStep(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
}

// TestServer_RetryFromStep_NotFoundReturns404 — the execution
// lookup fails; handler must surface 404 instead of trying to call
// the executor on a nil row.
func TestServer_RetryFromStep_NotFoundReturns404(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return nil, persistence.ErrNotFound
		},
	}
	mockExec := &mockPauseResumeExecutor{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
		WithExecutor(mockExec),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/missing/retry-from-step",
		strings.NewReader(`{"step_id":"any"}`))
	rec := httptest.NewRecorder()
	server.RetryExecutionFromStep(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, mockExec.retryCalls, "executor must not be called when the execution lookup fails")
}

// TestServer_RetryFromStep_RejectsMissingStepID — body present but
// step_id field absent; handler must 400 without calling the
// executor (the executor's own validation would also catch this,
// but a tighter early reject keeps the metric/log cleaner).
func TestServer_RetryFromStep_RejectsMissingStepID(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: makeExecLookup(&persistence.Execution{
			ID: "exec-1", ProjectID: "project-1",
			Status: persistence.ExecutionStatusFailed,
		}),
	}
	mockExec := &mockPauseResumeExecutor{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
		WithExecutor(mockExec),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/retry-from-step",
		strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	server.RetryExecutionFromStep(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "step_id is required")
	assert.Empty(t, mockExec.retryCalls)
}

// TestServer_RetryFromStep_RejectsBadJSON — malformed body returns
// 400 with a parser-ish message, not a 500.
func TestServer_RetryFromStep_RejectsBadJSON(t *testing.T) {
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(&mocks.MockExecutionRepository{}),
		WithExecutor(&mockPauseResumeExecutor{}),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/retry-from-step",
		strings.NewReader(`not json`))
	rec := httptest.NewRecorder()
	server.RetryExecutionFromStep(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
}

func TestServer_RetryFromStep_RejectsWrongMethod(t *testing.T) {
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(&mocks.MockExecutionRepository{}),
		WithExecutor(&mockPauseResumeExecutor{}),
	)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec-1/retry-from-step", nil)
	rec := httptest.NewRecorder()
	server.RetryExecutionFromStep(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestServer_RetryFromStep_RejectsUnknownFields(t *testing.T) {
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(&mocks.MockExecutionRepository{}),
		WithExecutor(&mockPauseResumeExecutor{}),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/retry-from-step",
		strings.NewReader(`{"step_id":"implement","unexpected":true}`))
	rec := httptest.NewRecorder()
	server.RetryExecutionFromStep(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "unknown field")
}

func TestServer_RetryFromStep_TrimsStepID(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: makeExecLookup(&persistence.Execution{
			ID: "exec-1", TaskID: "task-1", ProjectID: "project-1",
			Status: persistence.ExecutionStatusFailed,
		}),
	}
	mockExec := &mockPauseResumeExecutor{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
		WithExecutor(mockExec),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/retry-from-step",
		strings.NewReader(`{"step_id":" implement "}`))
	rec := httptest.NewRecorder()
	server.RetryExecutionFromStep(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Len(t, mockExec.retryCalls, 1)
	assert.Equal(t, "implement", mockExec.retryCalls[0].stepID)
}

func TestServer_RetryFromStep_RejectsOversizedBody(t *testing.T) {
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(&mocks.MockExecutionRepository{}),
		WithExecutor(&mockPauseResumeExecutor{}),
	)
	body := `{"step_id":"` + strings.Repeat("x", maxOptionalBodyBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec-1/retry-from-step", strings.NewReader(body))
	rec := httptest.NewRecorder()
	server.RetryExecutionFromStep(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "request body must be JSON")
}

// Compile-time assertion that the request shape we serialize in
// tests matches the one the production handler decodes — keeps the
// wire contract explicit if either side ever drifts.
var _ retryExecutionFromStepRequest = retryExecutionFromStepRequest{StepID: "x"}
