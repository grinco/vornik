package ui

// Regression tests for the 2026-05-27 security audit findings
// (#1-#8: cross-project IDOR in UI handlers). Each test drives
// one handler with a project-scoped request context aimed at a
// FOREIGN project's row. All handlers must return 404 (not 200,
// not 500) so existence isn't leaked.
//
// Test contract:
//   - A scoped key for project "A" cannot read/mutate project
//     "B"'s task / execution / artifact via the UI.
//   - The legitimate same-project request still works (sanity
//     check that the gate isn't always-fail).
//
// Pattern: build a Server with mocked repos that return a B-
// project row by ID; stamp the request context with
// api.ContextWithScopeForTesting(ctx, "A"); invoke the handler;
// assert 404. Then repeat with scope "B" and assert 200/302.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// projectBTask builds a Task owned by project "B" for the
// IDOR regression cases. Status is RUNNING so cancel/retry
// gates are exercised (not "skipped due to wrong state").
func projectBTask(id string) *persistence.Task {
	return &persistence.Task{
		ID:        id,
		ProjectID: "B",
		Status:    persistence.TaskStatusRunning,
		Attempt:   1,
	}
}

func projectBExecution(id, taskID string) *persistence.Execution {
	return &persistence.Execution{
		ID:        id,
		TaskID:    taskID,
		ProjectID: "B",
		Status:    persistence.ExecutionStatusFailed,
	}
}

func projectBArtifact(id string) *persistence.Artifact {
	mime := "application/octet-stream"
	return &persistence.Artifact{
		ID:          id,
		ProjectID:   "B",
		Name:        "file.txt",
		StoragePath: "/tmp/non-existent-blackhole",
		MimeType:    &mime,
	}
}

// scopedRequest builds an *http.Request stamped with auth-on +
// a project-A scope so the helper hits the cross-project branch.
func scopedRequest(method, path string, scopes ...string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	ctx := api.ContextWithScopeForTesting(req.Context(), scopes...)
	return req.WithContext(ctx)
}

// --- Task IDOR -------------------------------------------------------

// TestIDOR_TaskDetail_ScopedKeyCannotReadForeign: a scoped key
// for project A must get 404 (not 200) on a project-B task.
func TestIDOR_TaskDetail_ScopedKeyCannotReadForeign(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return projectBTask("task_b"), nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := scopedRequest(http.MethodGet, "/tasks/task_b", "A")
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped key A reading task in B: status=%d, want 404 (existence-not-leaked)", rec.Code)
	}
}

// TestIDOR_TaskDetail_SameProjectStillWorks: the gate isn't
// always-fail — the legitimate scope still loads.
func TestIDOR_TaskDetail_SameProjectStillWorks(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return projectBTask("task_b"), nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := scopedRequest(http.MethodGet, "/tasks/task_b", "B")
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Errorf("scoped key B reading task in B: got 404, want 200; gate is over-blocking")
	}
}

// TestIDOR_TaskStatusPartial_ScopedKeyCannotPoll: the status
// pill polls every few seconds — an IDOR here leaks transition
// observations over time.
func TestIDOR_TaskStatusPartial_ScopedKeyCannotPoll(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return projectBTask("task_b"), nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := scopedRequest(http.MethodGet, "/ui/tasks/task_b/status", "A")
	rec := httptest.NewRecorder()
	srv.TaskStatusPartial(rec, req, "task_b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped status pill IDOR: status=%d, want 404", rec.Code)
	}
}

// TestIDOR_TaskCancel_ScopedKeyCannotMutateForeign: the cancel
// path must NOT flip status on a foreign task. Verified by
// asserting the UpdateStatus mock never fires.
func TestIDOR_TaskCancel_ScopedKeyCannotMutateForeign(t *testing.T) {
	updateCalled := false
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return projectBTask("task_b"), nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error {
			updateCalled = true
			return nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := scopedRequest(http.MethodPost, "/ui/tasks/task_b/cancel", "A")
	rec := httptest.NewRecorder()
	srv.TaskCancel(rec, req, "task_b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped cancel IDOR: status=%d, want 404", rec.Code)
	}
	if updateCalled {
		t.Errorf("scoped cancel IDOR: UpdateStatus fired against foreign task — IDOR mutation succeeded")
	}
}

// TestIDOR_TaskBulkCancel_ScopedKeyCannotMutateForeignList: bulk
// cancel must filter out IDs the caller can't touch. cancelOne
// returning false (scope-rejected) is the in-line check.
func TestIDOR_TaskBulkCancel_ScopedKeyCannotMutateForeignList(t *testing.T) {
	updateCount := 0
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return projectBTask("task_b"), nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error {
			updateCount++
			return nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	form := strings.NewReader("task_ids=task_b1&task_ids=task_b2")
	req := scopedRequest(http.MethodPost, "/ui/tasks-bulk/cancel", "A")
	req.Body = io.NopCloser(form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.TaskBulkCancel(rec, req)
	if updateCount != 0 {
		t.Errorf("scoped bulk cancel IDOR: %d UpdateStatus calls landed against foreign tasks", updateCount)
	}
}

// TestIDOR_TaskRetry_ScopedKeyCannotRequeueForeign: retry must
// refuse to RequeueTerminalTask a foreign task.
func TestIDOR_TaskRetry_ScopedKeyCannotRequeueForeign(t *testing.T) {
	requeueCalled := false
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			t := projectBTask("task_b")
			t.Status = persistence.TaskStatusFailed
			return t, nil
		},
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, _ int, _ int) (bool, error) {
			requeueCalled = true
			return true, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := scopedRequest(http.MethodPost, "/ui/tasks/task_b/retry", "A")
	rec := httptest.NewRecorder()
	srv.TaskRetry(rec, req, "task_b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped retry IDOR: status=%d, want 404", rec.Code)
	}
	if requeueCalled {
		t.Errorf("scoped retry IDOR: RequeueTerminalTask fired against foreign task")
	}
}

// --- Execution IDOR -------------------------------------------------

// TestIDOR_ExecutionCancel_ScopedKeyCannotMutateForeign: the
// execution cancel path mutates both execution + task status.
// Neither should fire when the caller's scope can't see the row.
func TestIDOR_ExecutionCancel_ScopedKeyCannotMutateForeign(t *testing.T) {
	execUpdated := false
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return projectBExecution("exec_b", "task_b"), nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.ExecutionStatus) error {
			execUpdated = true
			return nil
		},
	}
	taskUpdated := false
	taskRepo := &mocks.MockTaskRepository{
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error {
			taskUpdated = true
			return nil
		},
	}
	srv := NewServer(WithExecutionRepository(execRepo), WithTaskRepository(taskRepo))
	req := scopedRequest(http.MethodPost, "/ui/executions/exec_b/cancel", "A")
	rec := httptest.NewRecorder()
	srv.ExecutionCancel(rec, req, "exec_b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped execution cancel IDOR: status=%d, want 404", rec.Code)
	}
	if execUpdated || taskUpdated {
		t.Errorf("scoped execution cancel IDOR: exec_updated=%v task_updated=%v — IDOR mutation succeeded", execUpdated, taskUpdated)
	}
}

// TestIDOR_ExecutionRetryFromStep_ScopedKeyCannotMutateForeign:
// retry-from-step rewinds + resumes. A cross-project mutation
// here would be catastrophic.
func TestIDOR_ExecutionRetryFromStep_ScopedKeyCannotMutateForeign(t *testing.T) {
	saveCalled := false
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return projectBExecution("exec_b", "task_b"), nil
		},
		SaveStateSnapshotFunc: func(_ context.Context, _ string, _ []byte, _ string, _ []string) error {
			saveCalled = true
			return nil
		},
	}
	srv := NewServer(WithExecutionRepository(execRepo),
		WithTaskRepository(&mocks.MockTaskRepository{}))
	form := strings.NewReader("step_id=foo")
	req := scopedRequest(http.MethodPost, "/ui/executions/exec_b/retry-from-step", "A")
	req.Body = io.NopCloser(form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ExecutionRetryFromStep(rec, req, "exec_b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped retry-from-step IDOR: status=%d, want 404", rec.Code)
	}
	if saveCalled {
		t.Errorf("scoped retry-from-step IDOR: SaveStateSnapshot fired against foreign execution")
	}
}

// TestIDOR_ExecutionStatusPartial_ScopedKeyCannotPoll: same
// observation-leak concern as task status pill.
func TestIDOR_ExecutionStatusPartial_ScopedKeyCannotPoll(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return projectBExecution("exec_b", "task_b"), nil
		},
	}
	srv := NewServer(WithExecutionRepository(execRepo))
	req := scopedRequest(http.MethodGet, "/ui/executions/exec_b/status", "A")
	rec := httptest.NewRecorder()
	srv.ExecutionStatusPartial(rec, req, "exec_b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped execution status pill IDOR: status=%d, want 404", rec.Code)
	}
}

// --- Artifact IDOR --------------------------------------------------

// TestIDOR_ArtifactDownload_ScopedKeyCannotReadForeign: artifact
// download must not stream content from foreign projects.
func TestIDOR_ArtifactDownload_ScopedKeyCannotReadForeign(t *testing.T) {
	artRepo := &mocks.MockArtifactRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Artifact, error) {
			return projectBArtifact("art_b"), nil
		},
	}
	srv := NewServer(WithArtifactRepository(artRepo))
	req := scopedRequest(http.MethodGet, "/ui/artifacts/art_b", "A")
	rec := httptest.NewRecorder()
	srv.ArtifactDownload(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped artifact download IDOR: status=%d, want 404", rec.Code)
	}
	if rec.Body.Len() > 0 && !strings.Contains(rec.Body.String(), "not found") {
		t.Errorf("scoped artifact download IDOR: response body contains content despite 404 status")
	}
}

// --- Streams + live -----------------------------------------------

// TestIDOR_TaskLogsStream_ScopedKeyCannotSubscribe: the log
// stream loads the task once to check scope, then opens the
// stream. A foreign task must return 404 before any data flows.
func TestIDOR_TaskLogsStream_ScopedKeyCannotSubscribe(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return projectBTask("task_b"), nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := scopedRequest(http.MethodGet, "/ui/tasks/task_b/logs/stream", "A")
	rec := httptest.NewRecorder()
	srv.TaskLogsStream(rec, req, "task_b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped log stream IDOR: status=%d, want 404", rec.Code)
	}
}

// TestIDOR_TaskEventsStream_ScopedKeyCannotSubscribe: the SSE
// event stream is the worst leak shape — long-lived, every
// status transition pushed live. Must refuse pre-subscription.
func TestIDOR_TaskEventsStream_ScopedKeyCannotSubscribe(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return projectBTask("task_b"), nil
		},
	}
	// NewServer always allocates a non-nil sseBus internally
	// (see server.go:1341), so the events-stream handler reaches
	// the scope check before Subscribe.
	srv := NewServer(WithTaskRepository(repo))
	req := scopedRequest(http.MethodGet, "/tasks/task_b/events", "A")
	rec := httptest.NewRecorder()
	srv.TaskEventsStream(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped events stream IDOR: status=%d, want 404", rec.Code)
	}
}

// TestIDOR_PostMortem_ScopedKeyCannotTriggerLLM: post-mortem
// generation fires a paid LLM call + persists a row. A scoped
// key for project A must not be able to burn budget on
// project B's task by guessing IDs.
func TestIDOR_PostMortem_ScopedKeyCannotTriggerLLM(t *testing.T) {
	explainerCalled := false
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return projectBTask("task_b"), nil
		},
	}
	srv := NewServer(
		WithTaskRepository(repo),
		WithPostMortemExplainer(&stubPostMortem{onGenerate: func() { explainerCalled = true }}),
	)
	req := scopedRequest(http.MethodPost, "/tasks/task_b/post-mortem", "A")
	rec := httptest.NewRecorder()
	srv.TaskPostMortemGenerate(rec, req, "task_b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped post-mortem IDOR: status=%d, want 404", rec.Code)
	}
	if explainerCalled {
		t.Errorf("scoped post-mortem IDOR: explainer fired (paid LLM call) on foreign task")
	}
}

// stubPostMortem implements ui.PostMortemExplainer. The Generate
// hook fires onGenerate so the test can detect a budget-burning
// IDOR even if the function returns "fine".
type stubPostMortem struct {
	onGenerate func()
}

func (s *stubPostMortem) Generate(_ context.Context, _ string, _ bool) (*PostMortemResult, error) {
	if s.onGenerate != nil {
		s.onGenerate()
	}
	return &PostMortemResult{Cached: false}, nil
}

// TestIDOR_TaskLive_ScopedKeyCannotObserve: the live page
// renders the task header + opens a WebSocket-driven timeline.
// A foreign task must 404 before the page is built.
func TestIDOR_TaskLive_ScopedKeyCannotObserve(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return projectBTask("task_b"), nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return nil, errors.New("not reached — scope check should fire first")
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithExecutionRepository(execRepo))
	req := scopedRequest(http.MethodGet, "/ui/tasks/task_b/live", "A")
	rec := httptest.NewRecorder()
	srv.TaskLive(rec, req, "task_b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped live page IDOR: status=%d, want 404", rec.Code)
	}
}
