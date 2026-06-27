package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// stubHintRepo satisfies persistence.ExecutionHintRepository.
// Captures inserts so handler-level assertions can verify the
// row shape; ConsumePending/ListByExecution are stubs since the
// API hint endpoint doesn't call them.
type stubHintRepo struct {
	inserted  []*persistence.ExecutionHint
	listed    []*persistence.ExecutionHint
	insertErr error
	listErr   error
	// forExecArgs captures the (executionID, taskID) the handler
	// passed to ListForExecution so tests can assert the task-scope
	// join key is plumbed through (LLD-drift audit §8.6).
	forExecExecID string
	forExecTaskID string
}

func (s *stubHintRepo) Insert(_ context.Context, h *persistence.ExecutionHint) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.inserted = append(s.inserted, h)
	return nil
}

func (s *stubHintRepo) ConsumePending(_ context.Context, _, _, _ string) ([]*persistence.ExecutionHint, error) {
	return nil, nil
}

func (s *stubHintRepo) ListByExecution(_ context.Context, _ string) ([]*persistence.ExecutionHint, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.listed, nil
}

func (s *stubHintRepo) ListForExecution(_ context.Context, executionID, taskID string) ([]*persistence.ExecutionHint, error) {
	s.forExecExecID = executionID
	s.forExecTaskID = taskID
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.listed, nil
}

func (s *stubHintRepo) ListPendingForTask(_ context.Context, _ string) ([]*persistence.ExecutionHint, error) {
	return nil, nil
}

func (s *stubHintRepo) ListByTask(_ context.Context, _ string) ([]*persistence.ExecutionHint, error) {
	return nil, nil
}

func newHintServer(t *testing.T, hintRepo persistence.ExecutionHintRepository, exec *persistence.Execution) *Server {
	t.Helper()
	opts := []ServerOption{}
	if hintRepo != nil {
		opts = append(opts, WithExecutionHintRepository(hintRepo))
	}
	if exec != nil {
		opts = append(opts, WithExecutionRepository(&stubExecRepoForFork{exec: exec}))
	}
	return NewServer(opts...)
}

func hintBody(t *testing.T, body any) *http.Request {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/executions/exec_1/hints", bytes.NewReader(buf))
	r.Header.Set("Content-Type", "application/json")
	// Tests in this file simulate the single-operator dev-mode
	// deployment (auth disabled) so the X-Operator-Id header is
	// honoured as the caller's identity. Production-style auth-
	// enabled deployments would refuse the header and require
	// an API key — covered by TestRequestOperatorID_*.
	r = r.WithContext(context.WithValue(r.Context(), authEnabledKey, false))
	return r
}

func TestExecutionHintCreate_HappyPath(t *testing.T) {
	repo := &stubHintRepo{}
	exec := &persistence.Execution{ID: "exec_1", ProjectID: "p1"}
	srv := newHintServer(t, repo, exec)

	req := hintBody(t, ExecutionHintRequest{
		StepID:  "summarise",
		Content: "skip the URL fetch on step 2 — the cache is stale",
	})
	req.Header.Set("X-Operator-Id", "op_1")
	rec := httptest.NewRecorder()
	srv.ExecutionHintCreate(rec, req, "exec_1")

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.inserted) != 1 {
		t.Fatalf("expected 1 inserted hint, got %d", len(repo.inserted))
	}
	got := repo.inserted[0]
	if got.ExecutionID != "exec_1" || got.StepID != "summarise" {
		t.Errorf("hint shape wrong: %+v", got)
	}
	if got.CreatedBy != "op_1" {
		t.Errorf("expected created_by=op_1, got %q", got.CreatedBy)
	}
	if got.ID == "" {
		t.Error("expected ID to be assigned")
	}
}

func TestExecutionHintCreate_503WhenUnwired(t *testing.T) {
	srv := NewServer()
	req := hintBody(t, ExecutionHintRequest{Content: "x"})
	rec := httptest.NewRecorder()
	srv.ExecutionHintCreate(rec, req, "exec_1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

func TestExecutionHintCreate_404WhenExecMissing(t *testing.T) {
	srv := NewServer(
		WithExecutionHintRepository(&stubHintRepo{}),
		WithExecutionRepository(&stubExecRepoForFork{exec: nil}),
	)
	req := hintBody(t, ExecutionHintRequest{Content: "x"})
	rec := httptest.NewRecorder()
	srv.ExecutionHintCreate(rec, req, "missing")
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestExecutionHintCreate_400OnMissingContent(t *testing.T) {
	repo := &stubHintRepo{}
	exec := &persistence.Execution{ID: "exec_1", ProjectID: "p1"}
	srv := newHintServer(t, repo, exec)
	req := hintBody(t, ExecutionHintRequest{Content: "   "})
	rec := httptest.NewRecorder()
	srv.ExecutionHintCreate(rec, req, "exec_1")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
	if len(repo.inserted) != 0 {
		t.Error("repo should not be touched on validation failure")
	}
}

func TestExecutionHintCreate_413OnLargeContent(t *testing.T) {
	repo := &stubHintRepo{}
	exec := &persistence.Execution{ID: "exec_1", ProjectID: "p1"}
	srv := newHintServer(t, repo, exec)
	huge := strings.Repeat("x", maxHintContentBytes+1)
	req := hintBody(t, ExecutionHintRequest{Content: huge})
	rec := httptest.NewRecorder()
	srv.ExecutionHintCreate(rec, req, "exec_1")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", rec.Code)
	}
}

func TestExecutionHintCreate_AnonymousByDefault(t *testing.T) {
	repo := &stubHintRepo{}
	exec := &persistence.Execution{ID: "exec_1", ProjectID: "p1"}
	srv := newHintServer(t, repo, exec)
	// No X-Operator-Id header → falls back to "anonymous".
	req := hintBody(t, ExecutionHintRequest{Content: "test"})
	rec := httptest.NewRecorder()
	srv.ExecutionHintCreate(rec, req, "exec_1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}
	if repo.inserted[0].CreatedBy != "anonymous" {
		t.Errorf("expected anonymous fallback, got %q", repo.inserted[0].CreatedBy)
	}
}

func TestExecutionHintList_ReturnsAll(t *testing.T) {
	repo := &stubHintRepo{listed: []*persistence.ExecutionHint{
		{ID: "h1", ExecutionID: "exec_1", Content: "first", CreatedBy: "op_1"},
		{ID: "h2", ExecutionID: "exec_1", Content: "second", CreatedBy: "op_1"},
	}}
	exec := &persistence.Execution{ID: "exec_1", TaskID: "task_99", ProjectID: "p1"}
	srv := newHintServer(t, repo, exec)
	r := httptest.NewRequest(http.MethodGet,
		"/api/v1/executions/exec_1/hints", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionHintList(rec, r, "exec_1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body struct {
		Hints []*persistence.ExecutionHint `json:"hints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Hints) != 2 {
		t.Errorf("expected 2 hints, got %d", len(body.Hints))
	}
	// §8.6: the handler must query via ListForExecution with the
	// execution's task ID so task-scoped (across-retries) hints are
	// included, not just execution-scoped ones.
	if repo.forExecExecID != "exec_1" || repo.forExecTaskID != "task_99" {
		t.Errorf("ListForExecution args = (%q,%q), want (exec_1, task_99)",
			repo.forExecExecID, repo.forExecTaskID)
	}
}

// newTaskHintServer wires a Server with a stub task repo + hint repo
// so the TaskHintCreate / TaskHintList handlers can be exercised
// without a real DB. The task lookup returns the supplied task; nil
// signals "not found".
func newTaskHintServer(t *testing.T, hintRepo persistence.ExecutionHintRepository, task *persistence.Task) *Server {
	t.Helper()
	tr := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			if task == nil {
				return nil, persistence.ErrNotFound
			}
			if task.ID != id {
				return nil, persistence.ErrNotFound
			}
			return task, nil
		},
	}
	opts := []ServerOption{WithTaskRepository(tr)}
	if hintRepo != nil {
		opts = append(opts, WithExecutionHintRepository(hintRepo))
	}
	return NewServer(opts...)
}

// TestTaskHintCreate_HappyPath — POST /tasks/{id}/hints inserts a
// task-scoped hint (TaskID set, ExecutionID empty). Carries across
// retries (a new execution for the same task will consume it at the
// first step). Guards the 2026-05-26 fix for ignored steering on
// retry.
func TestTaskHintCreate_HappyPath(t *testing.T) {
	repo := &stubHintRepo{}
	task := &persistence.Task{ID: "task_xyz", ProjectID: "p1"}
	srv := newTaskHintServer(t, repo, task)
	req := hintBody(t, ExecutionHintRequest{
		Content: "skip the scraper, use the cached snapshot",
	})
	req.Header.Set("X-Operator-Id", "op_alice")
	rec := httptest.NewRecorder()
	srv.TaskHintCreate(rec, req, "p1", "task_xyz")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.inserted) != 1 {
		t.Fatalf("expected 1 inserted hint, got %d", len(repo.inserted))
	}
	got := repo.inserted[0]
	if got.TaskID != "task_xyz" {
		t.Errorf("expected TaskID=task_xyz, got %q", got.TaskID)
	}
	if got.ExecutionID != "" {
		t.Errorf("task-scope hint must have empty ExecutionID, got %q", got.ExecutionID)
	}
	if got.CreatedBy != "op_alice" {
		t.Errorf("expected created_by=op_alice, got %q", got.CreatedBy)
	}
}

// TestTaskHintCreate_503WhenUnwired — without a hintRepo wired, the
// task hint endpoint returns 503 like its execution counterpart.
func TestTaskHintCreate_503WhenUnwired(t *testing.T) {
	srv := newTaskHintServer(t, nil, &persistence.Task{ID: "t", ProjectID: "p1"})
	req := hintBody(t, ExecutionHintRequest{Content: "x"})
	rec := httptest.NewRecorder()
	srv.TaskHintCreate(rec, req, "p1", "t")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

// TestTaskHintCreate_404WhenTaskMissing — task lookup misses → 404.
func TestTaskHintCreate_404WhenTaskMissing(t *testing.T) {
	srv := newTaskHintServer(t, &stubHintRepo{}, nil)
	req := hintBody(t, ExecutionHintRequest{Content: "x"})
	rec := httptest.NewRecorder()
	srv.TaskHintCreate(rec, req, "p1", "missing")
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

// TestTaskHintCreate_404OnProjectMismatch — a task that exists but
// belongs to a different project should look like NotFound (avoids
// leaking task IDs across project boundaries).
func TestTaskHintCreate_404OnProjectMismatch(t *testing.T) {
	task := &persistence.Task{ID: "task_xyz", ProjectID: "p2"}
	srv := newTaskHintServer(t, &stubHintRepo{}, task)
	req := hintBody(t, ExecutionHintRequest{Content: "x"})
	rec := httptest.NewRecorder()
	srv.TaskHintCreate(rec, req, "p1", "task_xyz")
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 on project mismatch, got %d", rec.Code)
	}
}

// TestTaskHintCreate_400OnMissingContent — defensive: empty content
// is a validation error and must not insert a row.
func TestTaskHintCreate_400OnMissingContent(t *testing.T) {
	repo := &stubHintRepo{}
	task := &persistence.Task{ID: "t", ProjectID: "p1"}
	srv := newTaskHintServer(t, repo, task)
	req := hintBody(t, ExecutionHintRequest{Content: "  "})
	rec := httptest.NewRecorder()
	srv.TaskHintCreate(rec, req, "p1", "t")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
	if len(repo.inserted) != 0 {
		t.Error("repo should not be touched on validation failure")
	}
}
