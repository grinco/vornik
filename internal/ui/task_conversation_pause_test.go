package ui

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func quietLogger() zerolog.Logger { return zerolog.Nop() }

// pauseSpy records every Pause invocation. Other ExecutorInterface
// methods are no-op stubs; the UI's pause path only touches Pause.
type pauseSpy struct {
	mu              sync.Mutex
	paused          []string
	pauseErr        error
	resumes         []string
	resumeTaskCalls []string
	resumeTaskErr   error
	resumeTaskOK    bool // when true, ResumeTask returns nil (happy path)
	cancels         []string
	notifies        []string
}

func (p *pauseSpy) Cancel(taskID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancels = append(p.cancels, taskID)
	return nil
}

func (p *pauseSpy) Pause(taskID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = append(p.paused, taskID)
	return p.pauseErr
}

func (p *pauseSpy) ResumePaused(execID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resumes = append(p.resumes, execID)
	return nil
}

// ResumeTask satisfies the 2026-05-26 ExecutorInterface extension.
// Defaults to returning a sentinel "no active execution" error so
// existing tests fall through to the bare flip-to-QUEUED path
// (preserving their assertions) without explicit per-test wiring.
// Tests that care about the in-place resume path replace this stub.
var resumeTaskNotApplicable = errResumeTaskNotApplicable{}

type errResumeTaskNotApplicable struct{}

func (errResumeTaskNotApplicable) Error() string { return "no active execution" }

func (p *pauseSpy) ResumeTask(taskID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resumeTaskCalls = append(p.resumeTaskCalls, taskID)
	if p.resumeTaskErr != nil {
		return p.resumeTaskErr
	}
	if p.resumeTaskOK {
		return nil
	}
	return resumeTaskNotApplicable
}

func (p *pauseSpy) NotifyChildTerminal(_ context.Context, childTaskID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notifies = append(p.notifies, childTaskID)
}

// fakeTaskMessageRepo: minimal stub matching
// persistence.TaskMessageRepository. The UI pause path only fires
// Insert; the rest exist so we satisfy the interface.
type fakeTaskMessageRepo struct{}

func (f *fakeTaskMessageRepo) Insert(_ context.Context, _ *persistence.TaskMessage) error { return nil }
func (f *fakeTaskMessageRepo) List(_ context.Context, _ persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return nil, nil
}
func (f *fakeTaskMessageRepo) GetOpenCheckpoint(_ context.Context, _ string) (*persistence.TaskMessage, error) {
	return nil, nil
}
func (f *fakeTaskMessageRepo) MarkCheckpointResolved(_ context.Context, _, _ string) error {
	return nil
}

// TestUIPauseTask_RunningTaskGoesThroughExecutor: the regression
// guard for T-…1c44. A RUNNING task's pause MUST route through
// executor.Pause so the goroutine actually stops — otherwise
// the in-flight merge step can overwrite PAUSED with FAILED.
func TestUIPauseTask_RunningTaskGoesThroughExecutor(t *testing.T) {
	taskID := "task_running"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
		},
	}
	spy := &pauseSpy{}
	s := NewServer(
		WithLogger(quietLogger()),
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&fakeTaskMessageRepo{}),
		WithExecutor(spy),
	)

	task := &persistence.Task{ID: taskID, Status: persistence.TaskStatusRunning}
	notice := s.uiPauseTask(context.Background(), task)

	assert.Equal(t, "paused", notice)
	require.Len(t, spy.paused, 1, "executor.Pause must be called for RUNNING tasks")
	assert.Equal(t, taskID, spy.paused[0])
}

// TestUIPauseTask_NonRunningSkipsExecutor: a QUEUED or PAUSED
// task has no goroutine to cancel; the executor.Pause call would
// just return "no active execution". Skip it and do the bare DB
// flip — same as the pre-bug behaviour for these states.
func TestUIPauseTask_NonRunningSkipsExecutor(t *testing.T) {
	taskID := "task_queued"
	transitions := 0
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			transitions++
			return true, nil
		},
	}
	spy := &pauseSpy{}
	s := NewServer(
		WithLogger(quietLogger()),
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&fakeTaskMessageRepo{}),
		WithExecutor(spy),
	)

	task := &persistence.Task{ID: taskID, Status: persistence.TaskStatusQueued}
	notice := s.uiPauseTask(context.Background(), task)

	assert.Equal(t, "paused", notice)
	assert.Empty(t, spy.paused, "QUEUED task should not call executor.Pause")
	assert.Equal(t, 1, transitions, "QUEUED task should still hit the bare TransitionConditional")
}

// TestUIPauseTask_BenignNoActiveExecutionFallsThrough: if the
// goroutine has already finished between the caller's task-load
// and our executor.Pause call, the executor returns "no active
// execution". That's a benign race; the UI should fall through
// to the bare TransitionConditional so the status still flips.
func TestUIPauseTask_BenignNoActiveExecutionFallsThrough(t *testing.T) {
	taskID := "task_race"
	transitions := 0
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			transitions++
			return true, nil
		},
	}
	spy := &pauseSpy{pauseErr: errors.New("no active execution for task " + taskID)}
	s := NewServer(
		WithLogger(quietLogger()),
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&fakeTaskMessageRepo{}),
		WithExecutor(spy),
	)

	task := &persistence.Task{ID: taskID, Status: persistence.TaskStatusRunning}
	notice := s.uiPauseTask(context.Background(), task)

	assert.Equal(t, "paused", notice)
	require.Len(t, spy.paused, 1, "executor.Pause should still be called once")
	assert.Equal(t, 1, transitions, "benign race should fall through to TransitionConditional")
}

// TestUIPauseTask_ExecutorPauseHardErrorFallsThroughToDBFlip:
// any error from executor.Pause that isn't "no active execution"
// is logged + we fall through. That preserves the operator's
// intent — the task status still flips to PAUSED even if the
// container-stop step failed (better than nothing).
func TestUIPauseTask_ExecutorPauseHardErrorFallsThroughToDBFlip(t *testing.T) {
	taskID := "task_hard_err"
	transitions := 0
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			transitions++
			return true, nil
		},
	}
	spy := &pauseSpy{pauseErr: errors.New("container stop failed: connection refused")}
	s := NewServer(
		WithLogger(quietLogger()),
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&fakeTaskMessageRepo{}),
		WithExecutor(spy),
	)
	task := &persistence.Task{ID: taskID, Status: persistence.TaskStatusRunning}
	notice := s.uiPauseTask(context.Background(), task)

	assert.Equal(t, "paused", notice)
	assert.Equal(t, 1, transitions, "hard executor error must still allow DB-level flip")
}

// TestUIPauseTask_FormRouteCallsExecutor: end-to-end form POST →
// uiTaskAction → uiPauseTask → executor.Pause. Guards against
// the routing layer accidentally regressing to uiSimpleFlip.
func TestUIPauseTask_FormRouteCallsExecutor(t *testing.T) {
	taskID := "task_form"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj-x", Status: persistence.TaskStatusRunning}, nil
		},
	}
	spy := &pauseSpy{}
	s := NewServer(
		WithLogger(quietLogger()),
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&fakeTaskMessageRepo{}),
		WithExecutor(spy),
	)

	body := url.Values{}
	req := httptest.NewRequest("POST", "/ui/tasks/"+taskID+"/pause", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.TaskConversationAction(rec, req)

	require.Len(t, spy.paused, 1, "form POST must route through executor.Pause")
	assert.Equal(t, taskID, spy.paused[0])
}

// TestUIResumeTask_PausedTaskCallsExecutorResumeTask — guards the
// 2026-05-26 fix: clicking Resume on a paused task must call
// executor.ResumeTask (which flips the existing PAUSED execution
// back to RUNNING in-place) rather than uiSimpleFlip → QUEUED
// (which dispatches a FRESH execution while the paused one sits
// parked, operator-observed regression).
func TestUIResumeTask_PausedTaskCallsExecutorResumeTask(t *testing.T) {
	taskID := "task_resume"
	transitions := 0
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusPaused}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			transitions++
			return true, nil
		},
	}
	// resumeTaskOK=true → ResumeTask returns nil (happy path —
	// executor successfully resumed in-place).
	spy := &pauseSpy{resumeTaskOK: true}
	s := NewServer(
		WithLogger(quietLogger()),
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&fakeTaskMessageRepo{}),
		WithExecutor(spy),
	)

	task := &persistence.Task{ID: taskID, Status: persistence.TaskStatusPaused}
	notice := s.uiResumeTask(context.Background(), task)

	assert.Equal(t, "resumed", notice)
	require.Len(t, spy.resumeTaskCalls, 1, "ResumeTask must be called on a paused task")
	assert.Equal(t, taskID, spy.resumeTaskCalls[0])
	assert.Equal(t, 0, transitions,
		"in-place resume must NOT touch the bare DB transition — that fallback creates a fresh execution")
}

// TestUIResumeTask_ResumeTaskMissExecutionFallsThroughToFlip — when
// executor.ResumeTask returns "no active execution" (e.g. task
// paused before any execution started, or paused execution lost
// across a daemon restart), the handler falls through to the bare
// PAUSED→QUEUED flip so the scheduler dispatches a fresh execution.
// Preserves the legacy behaviour for cases where in-place resume
// genuinely isn't possible.
func TestUIResumeTask_ResumeTaskMissExecutionFallsThroughToFlip(t *testing.T) {
	taskID := "task_fallback"
	transitions := 0
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusPaused}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			transitions++
			// Verify the fallback flips PAUSED → QUEUED.
			require.Contains(t, from, persistence.TaskStatusPaused)
			require.Equal(t, persistence.TaskStatusQueued, to)
			return true, nil
		},
	}
	// Default pauseSpy.ResumeTask returns the sentinel "no active
	// execution" error — exactly the case this test covers.
	spy := &pauseSpy{}
	s := NewServer(
		WithLogger(quietLogger()),
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&fakeTaskMessageRepo{}),
		WithExecutor(spy),
	)

	task := &persistence.Task{ID: taskID, Status: persistence.TaskStatusPaused}
	notice := s.uiResumeTask(context.Background(), task)

	assert.Equal(t, "resumed", notice)
	require.Len(t, spy.resumeTaskCalls, 1, "ResumeTask must be tried first even when it returns sentinel")
	assert.Equal(t, 1, transitions, "fallback to bare flip when in-place resume isn't applicable")
}
