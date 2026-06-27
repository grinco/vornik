package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestPauseTask_RoutesRunningTaskThroughExecutor pins the
// regression that surfaced as exec_8bec1d…5e89 (2026-05-10):
// the API endpoint flipped task.status to PAUSED in the DB but
// did not call executor.Pause(). The running goroutine kept its
// activeExecutions[taskID] entry; the next Resume tried to
// re-dispatch and got "task is already being executed" → the
// scheduler released the lease with FAILED status. Every
// pause/resume cycle terminal-failed.
//
// Post-fix: when task.status == RUNNING the API calls
// executor.Pause first, which stops the container, blocks until
// activeExecutions is cleared, then flips the DB row. The DB
// transition path runs only when the executor reports "no
// active execution" (PENDING / QUEUED / WAITING tasks have no
// goroutine to stop).
func TestPauseTask_RoutesRunningTaskThroughExecutor(t *testing.T) {
	const projectID = "test-proj"
	const taskID = "task-running-1"

	pauseCalled := false
	executor := &mockPauseResumeExecutor{
		pauseStatus: &executor.PauseStatus{
			TaskID: taskID, ExecutionID: "exec-1", PausedAt: time.Now(),
		},
	}
	// Wrap to capture calls.
	executorWrapper := &recordingExecutor{ExecutorInterface: executor, onPause: func(string) { pauseCalled = true }}

	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        id,
				ProjectID: projectID,
				Status:    persistence.TaskStatusRunning,
			}, nil
		},
		// TransitionConditional should NOT be called when executor.Pause
		// succeeded — the executor handles the DB flip internally.
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			t.Fatal("TransitionConditional should not be called when executor.Pause succeeded")
			return false, nil
		},
	}
	msgRepo := &stubTaskMessageRepo{}
	s := buildPauseResumeServer(taskRepo, msgRepo, executorWrapper)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/tasks/"+taskID+"/pause", nil)
	req = withProjectAndTaskRouteVars(req, projectID, taskID)
	rec := httptest.NewRecorder()
	s.PauseTask(rec, req)

	if !pauseCalled {
		t.Fatal("executor.Pause must be called for a running task; got DB-only flip")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestPauseTask_FallsBackToDBForNonRunningTask — a task that's
// QUEUED / PENDING / WAITING_FOR_CHILDREN has no executor
// goroutine. executor.Pause would return "no active execution".
// The API must then fall through to the bare DB transition.
func TestPauseTask_FallsBackToDBForNonRunningTask(t *testing.T) {
	const projectID = "test-proj"
	const taskID = "task-queued-1"

	transitionCalled := false
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        id,
				ProjectID: projectID,
				Status:    persistence.TaskStatusQueued, // not running
			}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			transitionCalled = true
			if to != persistence.TaskStatusPaused {
				t.Fatalf("expected to=PAUSED, got %s", to)
			}
			return true, nil
		},
	}
	msgRepo := &stubTaskMessageRepo{}
	executor := &mockPauseResumeExecutor{}

	s := buildPauseResumeServer(taskRepo, msgRepo, executor)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/tasks/"+taskID+"/pause", nil)
	req = withProjectAndTaskRouteVars(req, projectID, taskID)
	rec := httptest.NewRecorder()
	s.PauseTask(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !transitionCalled {
		t.Error("DB TransitionConditional must run for non-running tasks (no executor goroutine to stop)")
	}
}

// TestResumeTask_StaysOnDBPath — Resume relies on the scheduler
// re-leasing and dispatching, so the API just clears the lease +
// flips status QUEUED. We don't currently call executor.Resume
// from the API (that would bypass the scheduler). This test
// documents that contract so a future "wire executor.Resume"
// change comes with a deliberate decision rather than silently
// breaking the dispatch path.
func TestResumeTask_StaysOnDBPath(t *testing.T) {
	const projectID = "test-proj"
	const taskID = "task-paused-1"

	transitionCalled := false
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        id,
				ProjectID: projectID,
				Status:    persistence.TaskStatusPaused,
			}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
			transitionCalled = true
			if to != persistence.TaskStatusQueued {
				t.Fatalf("resume should target QUEUED, got %s", to)
			}
			if !opts.ClearLease {
				t.Error("resume must set ClearLease=true so scheduler can re-lease")
			}
			return true, nil
		},
	}
	msgRepo := &stubTaskMessageRepo{}
	executor := &mockPauseResumeExecutor{}

	s := buildPauseResumeServer(taskRepo, msgRepo, executor)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/tasks/"+taskID+"/resume", nil)
	req = withProjectAndTaskRouteVars(req, projectID, taskID)
	rec := httptest.NewRecorder()
	s.ResumeTask(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !transitionCalled {
		t.Error("resume must run the DB transition + clear lease so scheduler re-dispatches")
	}
}

// recordingExecutor wraps an ExecutorInterface and notifies a
// hook on Pause so tests can assert "the API actually called the
// executor". Mirrors the existing mockPauseResumeExecutor shape.
type recordingExecutor struct {
	ExecutorInterface
	onPause func(string)
}

func (r *recordingExecutor) Pause(taskID string) (*executor.PauseStatus, error) {
	if r.onPause != nil {
		r.onPause(taskID)
	}
	return r.ExecutorInterface.Pause(taskID)
}

// stubTaskMessageRepo is the smallest TaskMessageRepository
// shape these tests need: Insert is called when the API writes
// the system message recording the operator action; the
// remaining interface methods return nil for the unused paths.
type stubTaskMessageRepo struct {
	inserted []persistence.TaskMessage
}

func (s *stubTaskMessageRepo) Insert(_ context.Context, m *persistence.TaskMessage) error {
	s.inserted = append(s.inserted, *m)
	return nil
}
func (s *stubTaskMessageRepo) List(context.Context, persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return nil, nil
}
func (s *stubTaskMessageRepo) GetOpenCheckpoint(context.Context, string) (*persistence.TaskMessage, error) {
	return nil, nil
}
func (s *stubTaskMessageRepo) MarkCheckpointResolved(context.Context, string, string) error {
	return nil
}

func buildPauseResumeServer(
	taskRepo persistence.TaskRepository,
	msgRepo persistence.TaskMessageRepository,
	exec ExecutorInterface,
) *Server {
	return NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(msgRepo),
		WithExecutor(exec),
	)
}

// withProjectAndTaskRouteVars stamps the path parameters the
// handler reads. The router would normally inject these; in
// tests we set them manually so PauseTask/ResumeTask resolve the
// right task without standing up the full routing tree.
func withProjectAndTaskRouteVars(req *http.Request, projectID, taskID string) *http.Request {
	req.SetPathValue("p", projectID)
	req.SetPathValue("id", taskID)
	return req
}
