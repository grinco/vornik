package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// retryFromStepFakeExecutor records RetryFromStep invocations so the tests
// can assert the handler DELEGATES the rewind rather than performing it.
// Until 2026-09-04 the handler wrote the state snapshot itself and these
// tests asserted on the snapshot; the executor is now the only writer
// (2026-09-04-execution-pause-write-ownership-design.md §3.2), so what the
// UI owns — and what these tests pin — is the request validation, the
// project scope, the FAILED/CANCELLED precondition, the delegation, and the
// mapping of the executor's sentinels to HTTP statuses.
type retryFromStepFakeExecutor struct {
	mu            sync.Mutex
	retries       []retryCall
	cancelled     []string
	resumePaused  []string
	retryFromStep func(execID, stepID string) error
}

type retryCall struct{ execID, stepID string }

func (e *retryFromStepFakeExecutor) Cancel(taskID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancelled = append(e.cancelled, taskID)
	return nil
}

func (e *retryFromStepFakeExecutor) Pause(_ string) error { return nil }

func (e *retryFromStepFakeExecutor) ResumePaused(execID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resumePaused = append(e.resumePaused, execID)
	return nil
}

func (e *retryFromStepFakeExecutor) RetryFromStep(_ context.Context, execID, stepID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.retries = append(e.retries, retryCall{execID: execID, stepID: stepID})
	if e.retryFromStep != nil {
		return e.retryFromStep(execID, stepID)
	}
	return nil
}

func (e *retryFromStepFakeExecutor) ResumeTask(_ string) error {
	return fmt.Errorf("retryFromStepFakeExecutor: no active execution")
}

func (e *retryFromStepFakeExecutor) NotifyChildTerminal(_ context.Context, _ string) {}

func (e *retryFromStepFakeExecutor) CancelIfActive(string) (bool, error) { return false, nil }

func (e *retryFromStepFakeExecutor) CancelChildren(_ context.Context, _ string) {}

// retryFromStepRig wires a Server with the repositories the handler reads
// and an executor fake. The execution repository's write methods are
// deliberately ABSENT from the mock: a handler that writes a snapshot or a
// status itself panics on the nil func, which is the regression this rig
// exists to catch.
func retryFromStepRig(t *testing.T, exec *persistence.Execution, task *persistence.Task) (*Server, *retryFromStepFakeExecutor) {
	t.Helper()
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			if id == exec.ID {
				return exec, nil
			}
			return nil, persistence.ErrNotFound
		},
	}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			if id == task.ID {
				return task, nil
			}
			return nil, persistence.ErrNotFound
		},
	}
	executorFake := &retryFromStepFakeExecutor{}
	server := NewServer(
		WithTaskRepository(taskRepo),
		WithExecutionRepository(execRepo),
		WithExecutor(executorFake),
	)
	return server, executorFake
}

func postRetryFromStep(t *testing.T, s *Server, stepID string) *httptest.ResponseRecorder {
	t.Helper()
	const execID = "e1"
	form := strings.NewReader("step_id=" + stepID)
	req := httptest.NewRequest(http.MethodPost, "/ui/executions/"+execID+"/retry-from-step", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ExecutionRetryFromStep(rec, req, execID)
	return rec
}

func failedExecution() (*persistence.Execution, *persistence.Task) {
	failedStepID := "implement"
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		ProjectID:      "p1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan", "research", "review"},
		CurrentStepID:  &failedStepID,
	}
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusFailed}
	return exec, task
}

// TestExecutionRetryFromStep_DelegatesToExecutor — the happy path is a
// delegation: the executor receives exactly (execution, chosen step), the
// handler writes nothing itself (the mock has no write funcs and would
// panic), and the browser is redirected to the execution page.
func TestExecutionRetryFromStep_DelegatesToExecutor(t *testing.T) {
	exec, task := failedExecution()
	s, exe := retryFromStepRig(t, exec, task)

	rec := postRetryFromStep(t, s, "review")

	require.Equal(t, http.StatusSeeOther, rec.Code, "want 303 redirect after successful retry: %s", rec.Body.String())
	assert.Equal(t, "/ui/executions/e1", rec.Header().Get("Location"))
	require.Len(t, exe.retries, 1, "executor.RetryFromStep must be invoked exactly once")
	assert.Equal(t, retryCall{execID: "e1", stepID: "review"}, exe.retries[0])
	assert.Empty(t, exe.resumePaused, "the executor relaunches inside RetryFromStep; the handler must not also kick ResumePaused")
	assert.Empty(t, exe.cancelled, "Cancel must NOT be called — retry is the opposite operation")
}

// TestExecutionRetryFromStep_FailedStepItself — retrying the step that
// broke is delegated like any other; the executor accepts the current step
// (executor.TestRetryFromStep_AcceptsCurrentStep). The UI does not decide
// which steps are reachable.
func TestExecutionRetryFromStep_FailedStepItself(t *testing.T) {
	exec, task := failedExecution()
	s, exe := retryFromStepRig(t, exec, task)

	rec := postRetryFromStep(t, s, "implement")

	require.Equal(t, http.StatusSeeOther, rec.Code, "body=%s", rec.Body.String())
	require.Len(t, exe.retries, 1)
	assert.Equal(t, "implement", exe.retries[0].stepID)
}

// TestExecutionRetryFromStep_RefusesWhileRunning — the UI's own precondition:
// only FAILED or CANCELLED executions offer the action, so a RUNNING one is
// refused before the executor is consulted.
func TestExecutionRetryFromStep_RefusesWhileRunning(t *testing.T) {
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning}
	s, exe := retryFromStepRig(t, exec, task)

	rec := postRetryFromStep(t, s, "plan")

	assert.Equal(t, http.StatusBadRequest, rec.Code, "running execution must refuse retry — racing the scheduler would corrupt state")
	assert.Empty(t, exe.retries, "the executor must not be consulted for a non-terminal execution")
}

// TestExecutionRetryFromStep_UnknownStepRefused — the executor's
// ErrRetryStepNotInExecution is a client error naming the step.
func TestExecutionRetryFromStep_UnknownStepRefused(t *testing.T) {
	exec, task := failedExecution()
	s, exe := retryFromStepRig(t, exec, task)
	exe.retryFromStep = func(_, stepID string) error {
		return fmt.Errorf("%w: step=%q", executor.ErrRetryStepNotInExecution, stepID)
	}

	rec := postRetryFromStep(t, s, "mystery")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"mystery"`)
}

// TestExecutionRetryFromStep_ExecutorStateSentinelsAre409 — a rewind the
// executor refuses because of the execution's state (already executing in
// this process, or not terminal by its own reading) is a conflict, not a
// validation error and not a 500: the same mapping the API handler uses.
func TestExecutionRetryFromStep_ExecutorStateSentinelsAre409(t *testing.T) {
	for _, sentinel := range []error{executor.ErrRetryAlreadyExecuting, executor.ErrRetryNotTerminal} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			exec, task := failedExecution()
			s, exe := retryFromStepRig(t, exec, task)
			exe.retryFromStep = func(_, _ string) error { return sentinel }

			rec := postRetryFromStep(t, s, "review")

			assert.Equal(t, http.StatusConflict, rec.Code)
		})
	}
}

// TestExecutionRetryFromStep_UnexpectedExecutorErrorIs500 — anything else the
// executor returns (a persistence failure mid-rewind) is an internal error
// and is NOT echoed to the browser.
func TestExecutionRetryFromStep_UnexpectedExecutorErrorIs500(t *testing.T) {
	exec, task := failedExecution()
	s, exe := retryFromStepRig(t, exec, task)
	exe.retryFromStep = func(_, _ string) error { return errors.New("persist reset state: disk on fire") }

	rec := postRetryFromStep(t, s, "review")

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "disk on fire")
}

// TestExecutionRetryFromStep_NoExecutorIs503 — a server without an executor
// cannot rewind anything and says so, AFTER the scope and status checks so
// an out-of-project caller still sees only "not found".
func TestExecutionRetryFromStep_NoExecutorIs503(t *testing.T) {
	exec, task := failedExecution()
	s, _ := retryFromStepRig(t, exec, task)
	s.executor = nil

	rec := postRetryFromStep(t, s, "review")

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestExecutionRetryFromStep_EmptyFormFieldRejected — a missing step_id from
// a malformed form post is a client error, not a silent default.
func TestExecutionRetryFromStep_EmptyFormFieldRejected(t *testing.T) {
	exec, task := failedExecution()
	s, exe := retryFromStepRig(t, exec, task)

	rec := postRetryFromStep(t, s, "")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, exe.retries)
}
