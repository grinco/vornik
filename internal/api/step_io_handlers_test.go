package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// The two boundary files are served as stored bytes with their hash, scoped
// like the prompt (step-I/O persistence design §5).
func TestGetExecutionStepIO_ServesTheStoredBytes(t *testing.T) {
	s, prompts, outcomes := promptServer(t)
	in, _ := prompts.Save(context.Background(), persistence.StepPromptInput, `{"taskId":"t1","context":{"prompt":"do it"}}`)
	res, _ := prompts.Save(context.Background(), persistence.StepPromptResult, `{"status":"COMPLETED"}`)
	exit := 0
	outcomes.rows = []*persistence.ExecutionStepOutcome{{
		ExecutionID: "exec_1", StepID: "plan", RecordedAt: time.Now(), ContainerExitCode: &exit,
		PromptHashes: persistence.StepPromptHashes{Input: in, Result: res},
	}}
	for path, want, hash := "/api/v1/executions/exec_1/steps/plan/input", `{"taskId":"t1","context":{"prompt":"do it"}}`, in; ; path, want, hash = "/api/v1/executions/exec_1/steps/plan/result", `{"status":"COMPLETED"}`, res {
		req := authDisabledReq(httptest.NewRequest(http.MethodGet, path, nil))
		rec := httptest.NewRecorder()
		s.apiV1ExecutionsHandler(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, want, rec.Body.String(), path)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		assert.Equal(t, hash, rec.Header().Get("X-Vornik-Content-Hash"))
		if hash == res {
			break
		}
	}
	// POST is refused.
	req := authDisabledReq(httptest.NewRequest(http.MethodPost, "/api/v1/executions/exec_1/steps/plan/input", nil))
	rec := httptest.NewRecorder()
	s.apiV1ExecutionsHandler(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// Not recorded (404, with a code that says whether a container ran at all),
// pruned (410), unknown execution (404) and another project (403) are four
// different answers.
func TestGetExecutionStepIO_NotRecordedPrunedUnknownAndScoped(t *testing.T) {
	s, _, outcomes := promptServer(t)
	get := func(path string) *httptest.ResponseRecorder {
		req := authDisabledReq(httptest.NewRequest(http.MethodGet, path, nil))
		rec := httptest.NewRecorder()
		s.apiV1ExecutionsHandler(rec, req)
		return rec
	}
	// No hash, no container: nothing to record.
	outcomes.rows = []*persistence.ExecutionStepOutcome{{ExecutionID: "exec_1", StepID: "plan", RecordedAt: time.Now()}}
	rec := get("/api/v1/executions/exec_1/steps/plan/result")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESULT_NOT_RECORDED_NO_CONTAINER")
	// No hash, a container ran: the daemon predated the design or the ceiling hit.
	exit := 0
	outcomes.rows = []*persistence.ExecutionStepOutcome{{ExecutionID: "exec_1", StepID: "plan", RecordedAt: time.Now(), ContainerExitCode: &exit}}
	rec = get("/api/v1/executions/exec_1/steps/plan/input")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), `"INPUT_NOT_RECORDED"`)
	// A hash the store no longer resolves: recorded, then pruned.
	outcomes.rows = []*persistence.ExecutionStepOutcome{{ExecutionID: "exec_1", StepID: "plan", RecordedAt: time.Now(), ContainerExitCode: &exit,
		PromptHashes: persistence.StepPromptHashes{Input: "pruned"}}}
	rec = get("/api/v1/executions/exec_1/steps/plan/input")
	assert.Equal(t, http.StatusGone, rec.Code)
	assert.Contains(t, rec.Body.String(), "GONE")
	// Unknown execution.
	assert.Equal(t, http.StatusNotFound, get("/api/v1/executions/exec_nope/steps/plan/input").Code)
	// Another project's key.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec_1/steps/plan/input", nil)
	req = req.WithContext(ContextWithProjectScope(req.Context(), "other-project"))
	rec = httptest.NewRecorder()
	s.apiV1ExecutionsHandler(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
