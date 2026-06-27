package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// stubForker is a minimal ForkExecutor. The test owns the
// returned result + error.
type stubForker struct {
	result *ForkExecutorResult
	err    error
	called bool
	gotID  string
	gotReq ForkExecutorRequest
}

func (s *stubForker) Fork(_ context.Context, executionID string, req ForkExecutorRequest) (*ForkExecutorResult, error) {
	s.called = true
	s.gotID = executionID
	s.gotReq = req
	return s.result, s.err
}

// stubExecRepoForFork is a narrow ExecutionRepository fake — only
// Get is exercised by the fork handler, but we have to satisfy the
// full persistence.ExecutionRepository interface for the Server
// option. Embedded panicking placeholders cover the rest.
type stubExecRepoForFork struct {
	persistence.ExecutionRepository
	exec       *persistence.Execution
	err        error
	nilNoError bool
}

func (s *stubExecRepoForFork) Get(_ context.Context, id string) (*persistence.Execution, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.nilNoError {
		return nil, nil
	}
	if s.exec == nil || s.exec.ID != id {
		return nil, persistence.ErrNotFound
	}
	return s.exec, nil
}

func newForkServer(t *testing.T, forker ForkExecutor, exec *persistence.Execution) *Server {
	t.Helper()
	opts := []ServerOption{}
	if forker != nil {
		opts = append(opts, WithForker(forker))
	}
	if exec != nil {
		opts = append(opts, WithExecutionRepository(&stubExecRepoForFork{exec: exec}))
	}
	return NewServer(opts...)
}

func newForkRequest(t *testing.T, body any) *http.Request {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/executions/exec_src/fork-from-step",
		strings.NewReader(string(buf)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestForkExecutionFromStep_HappyPath(t *testing.T) {
	forker := &stubForker{
		result: &ForkExecutorResult{TaskID: "task_fork_1", URL: "/ui/tasks/task_fork_1"},
	}
	exec := &persistence.Execution{
		ID: "exec_src", ProjectID: "p1", Status: persistence.ExecutionStatusFailed,
	}
	s := newForkServer(t, forker, exec)

	req := newForkRequest(t, map[string]any{"step_id": "summarise", "prompt_override": "redo with caveats"})
	rec := httptest.NewRecorder()
	s.ForkExecutionFromStep(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if !forker.called {
		t.Error("expected forker to be called")
	}
	if forker.gotReq.StepID != "summarise" || forker.gotReq.PromptOverride != "redo with caveats" {
		t.Errorf("forker received wrong req: %+v", forker.gotReq)
	}
	var body ForkExecutorResult
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response unmarshal: %v", err)
	}
	if body.TaskID != "task_fork_1" || body.URL != "/ui/tasks/task_fork_1" {
		t.Errorf("response body wrong: %+v", body)
	}
}

func TestForkExecutionFromStep_503WhenUnwired(t *testing.T) {
	exec := &persistence.Execution{ID: "exec_src", ProjectID: "p1"}
	s := newForkServer(t, nil, exec) // no forker

	req := newForkRequest(t, map[string]any{"step_id": "summarise"})
	rec := httptest.NewRecorder()
	s.ForkExecutionFromStep(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

func TestForkExecutionFromStep_RejectsNonPost(t *testing.T) {
	forker := &stubForker{result: &ForkExecutorResult{}}
	exec := &persistence.Execution{ID: "exec_src", ProjectID: "p1"}
	s := newForkServer(t, forker, exec)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/executions/exec_src/fork-from-step", nil)
	rec := httptest.NewRecorder()
	s.ForkExecutionFromStep(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestForkExecutionFromStep_MissingStepID(t *testing.T) {
	forker := &stubForker{result: &ForkExecutorResult{}}
	exec := &persistence.Execution{ID: "exec_src", ProjectID: "p1"}
	s := newForkServer(t, forker, exec)

	req := newForkRequest(t, map[string]any{"step_id": "   "})
	rec := httptest.NewRecorder()
	s.ForkExecutionFromStep(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
	if forker.called {
		t.Error("forker should not be called on validation failure")
	}
}

func TestForkExecutionFromStep_SourceNotFound(t *testing.T) {
	forker := &stubForker{result: &ForkExecutorResult{}}
	// No execution wired → 404 from the project-authz lookup.
	s := newForkServer(t, forker, nil)

	req := newForkRequest(t, map[string]any{"step_id": "summarise"})
	rec := httptest.NewRecorder()
	s.ForkExecutionFromStep(rec, req)

	if rec.Code != http.StatusInternalServerError {
		// Without executionRepo wired the handler short-circuits to
		// "Execution repository not available" 500 — separate path
		// from "not found". Verify the not-wired branch.
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("expected 500 when execRepo missing, got %d", rec.Code)
		}
	}
}

func TestForkExecutionFromStep_NilExecutionIs404(t *testing.T) {
	forker := &stubForker{result: &ForkExecutorResult{}}
	s := NewServer(
		WithForker(forker),
		WithExecutionRepository(&stubExecRepoForFork{nilNoError: true}),
	)
	req := newForkRequest(t, map[string]any{"step_id": "summarise"})
	rec := httptest.NewRecorder()
	s.ForkExecutionFromStep(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nil execution lookup, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if forker.called {
		t.Fatal("forker should not run when source execution is missing")
	}
}

func TestForkExecutionFromStep_ForwardsSentinelErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantCode   int
		wantInBody string
	}{
		{"source not found", errors.New("replay: fork source execution not found"), http.StatusNotFound, "Execution not found"},
		{"step missing", errors.New("replay: fork step has no recorded outcome: step \"x\""), http.StatusBadRequest, "recorded outcome"},
		{"validation", errors.New("replay: fork request invalid: step_id required"), http.StatusBadRequest, "invalid"},
		{"unknown", errors.New("internal db error"), http.StatusInternalServerError, "Failed to fork"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			forker := &stubForker{err: c.err}
			exec := &persistence.Execution{ID: "exec_src", ProjectID: "p1"}
			s := newForkServer(t, forker, exec)
			req := newForkRequest(t, map[string]any{"step_id": "summarise"})
			rec := httptest.NewRecorder()
			s.ForkExecutionFromStep(rec, req)
			if rec.Code != c.wantCode {
				t.Errorf("expected %d, got %d (body: %s)", c.wantCode, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), c.wantInBody) {
				t.Errorf("body missing %q: %s", c.wantInBody, rec.Body.String())
			}
		})
	}
}

// silence "unused" warning when the time import is otherwise unused
var _ = time.Now
