package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

type promptExecRepo struct {
	mockExecutionRepo
	exec *persistence.Execution
}

func (r *promptExecRepo) Get(_ context.Context, id string) (*persistence.Execution, error) {
	if r.exec != nil && r.exec.ID == id {
		return r.exec, nil
	}
	return nil, persistence.ErrNotFound
}

type memPromptRepo struct{ bodies map[string]string }

func (m *memPromptRepo) Save(_ context.Context, _ persistence.StepPromptPart, body string) (string, error) {
	h := persistence.HashStepPrompt(body)
	m.bodies[h] = body
	return h, nil
}
func (m *memPromptRepo) Get(_ context.Context, hash string) (*persistence.StepPrompt, error) {
	if b, ok := m.bodies[hash]; ok {
		return &persistence.StepPrompt{Hash: hash, Body: b}, nil
	}
	return nil, persistence.ErrNotFound
}
func (m *memPromptRepo) PruneUnreferenced(context.Context) (int64, error) { return 0, nil }

func TestMemPromptRepo_KeepsTheMissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "StepPromptRepository.Get", (&memPromptRepo{bodies: map[string]string{}}).Get)
}

func promptServer(t *testing.T) (*Server, *memPromptRepo, *stubStepOutcomeRepo) {
	t.Helper()
	prompts := &memPromptRepo{bodies: map[string]string{}}
	outcomes := &stubStepOutcomeRepo{}
	s := NewServer(WithLogger(zerolog.Nop()),
		WithExecutionRepository(&promptExecRepo{exec: &persistence.Execution{ID: "exec_1", ProjectID: "janka"}}),
		WithExecutionStepOutcomeRepository(outcomes),
		WithStepPromptRepository(prompts))
	return s, prompts, outcomes
}

// The read side serves what the model was told, as stored (redacted at
// write), scoped by the execution's project (step-prompt persistence design §7).
func TestGetExecutionStepPrompt_ServesTheStoredParts(t *testing.T) {
	s, prompts, outcomes := promptServer(t)
	sys, _ := prompts.Save(context.Background(), persistence.StepPromptSystem, "You are the planner.")
	usr, _ := prompts.Save(context.Background(), persistence.StepPromptUser, "Plan it.")
	outcomes.rows = []*persistence.ExecutionStepOutcome{{
		ExecutionID: "exec_1", StepID: "plan", RecordedAt: time.Date(2026, 9, 4, 7, 0, 0, 0, time.UTC),
		PromptHashes: persistence.StepPromptHashes{System: sys, User: usr, Tools: "pruned-hash"},
	}}

	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec_1/steps/plan/prompt", nil))
	rec := httptest.NewRecorder()
	s.apiV1ExecutionsHandler(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp StepPromptResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "You are the planner.", resp.Parts["system"])
	assert.Equal(t, "Plan it.", resp.Parts["user"])
	assert.Equal(t, "", resp.Parts["tools"], "a pruned part is reported empty, not a 404 for the whole step")
	assert.Equal(t, sys, resp.Hashes.System)
	assert.Equal(t, "2026-09-04T07:00:00Z", resp.RecordedAt)
}

func TestGetExecutionStepPrompt_NotRecordedUnknownAndScoped(t *testing.T) {
	s, _, outcomes := promptServer(t)
	// An outcome row with no hashes: the image predates the contract.
	outcomes.rows = []*persistence.ExecutionStepOutcome{{ExecutionID: "exec_1", StepID: "plan", RecordedAt: time.Now()}}
	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec_1/steps/plan/prompt", nil))
	rec := httptest.NewRecorder()
	s.apiV1ExecutionsHandler(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "PROMPT_NOT_RECORDED")

	// Unknown execution.
	req = authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec_nope/steps/plan/prompt", nil))
	rec = httptest.NewRecorder()
	s.apiV1ExecutionsHandler(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// A key scoped to another project is denied — the execution's project is
	// the boundary, as for every execution read.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec_1/steps/plan/prompt", nil)
	req = req.WithContext(ContextWithProjectScope(req.Context(), "other-project"))
	rec = httptest.NewRecorder()
	s.apiV1ExecutionsHandler(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Path shapes that are not the prompt route stay 404.
	for _, p := range []string{"/api/v1/executions/exec_1/steps/plan", "/api/v1/executions/exec_1/steps//prompt", "/api/v1/executions/exec_1/steps/plan/other"} {
		req = authDisabledReq(httptest.NewRequest(http.MethodGet, p, nil))
		rec = httptest.NewRecorder()
		s.apiV1ExecutionsHandler(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code, p)
	}
}
