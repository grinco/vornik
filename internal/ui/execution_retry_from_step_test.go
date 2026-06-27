package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// retryFromStepFakeExecutor captures invocations of the ResumePaused
// hook so tests can assert the handler called it (and didn't call
// Cancel) without standing up a real executor.
type retryFromStepFakeExecutor struct {
	mu             sync.Mutex
	resumePaused   []string
	cancelled      []string
	resumePausedFn func(string) error
}

func (e *retryFromStepFakeExecutor) Cancel(taskID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancelled = append(e.cancelled, taskID)
	return nil
}

// Pause is part of ExecutorInterface for the pause-cancel fix;
// retry-from-step doesn't exercise it. No-op stub.
func (e *retryFromStepFakeExecutor) Pause(_ string) error { return nil }

func (e *retryFromStepFakeExecutor) ResumePaused(execID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resumePaused = append(e.resumePaused, execID)
	if e.resumePausedFn != nil {
		return e.resumePausedFn(execID)
	}
	return nil
}

// ResumeTask is unused by the retry-from-step flow; stub satisfies
// the 2026-05-26 ExecutorInterface extension. Returning a sentinel
// error keeps the bare flip-to-QUEUED fallback path active for any
// indirect resume call that lands here.
func (e *retryFromStepFakeExecutor) ResumeTask(_ string) error {
	return fmt.Errorf("retryFromStepFakeExecutor: no active execution")
}

// NotifyChildTerminal is part of ExecutorInterface but isn't
// exercised by the retry-from-step flow; the fake just records the
// call so the interface stays satisfied without standing up a
// parent-child fixture.
func (e *retryFromStepFakeExecutor) NotifyChildTerminal(_ context.Context, _ string) {}

// retryFromStepFakeOutcomeRepo records SupersedeAfter invocations
// and serves a controllable List response. Implements the full
// ExecutionStepOutcomeRepository interface; the methods the test
// doesn't exercise are no-ops.
type retryFromStepFakeOutcomeRepo struct {
	mu              sync.Mutex
	rows            []*persistence.ExecutionStepOutcome
	supersedeCalls  []supersedeCall
	supersedeReturn int64
}

type supersedeCall struct {
	executionID string
	cutoff      time.Time
}

func (r *retryFromStepFakeOutcomeRepo) Record(_ context.Context, _ *persistence.ExecutionStepOutcome) error {
	return nil
}
func (r *retryFromStepFakeOutcomeRepo) Finalize(_ context.Context, _, _, _, _ string, _ *string) error {
	return nil
}
func (r *retryFromStepFakeOutcomeRepo) FinalizePending(_ context.Context, _, _, _, _, _ string, _ *string) (string, string, error) {
	return "", "", nil
}
func (r *retryFromStepFakeOutcomeRepo) SweepPending(_ context.Context, _, _ string) ([]persistence.SweepResult, error) {
	return nil, nil
}
func (r *retryFromStepFakeOutcomeRepo) SupersedeAfter(_ context.Context, executionID string, cutoff time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.supersedeCalls = append(r.supersedeCalls, supersedeCall{executionID: executionID, cutoff: cutoff})
	return r.supersedeReturn, nil
}
func (r *retryFromStepFakeOutcomeRepo) CountByRoleModelOutcome(_ context.Context, _ string, _ time.Time, _ time.Time, _ string) ([]persistence.RoleModelOutcomeCount, error) {
	return nil, nil
}
func (r *retryFromStepFakeOutcomeRepo) List(_ context.Context, _ persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*persistence.ExecutionStepOutcome, len(r.rows))
	copy(out, r.rows)
	return out, nil
}

// retryFromStepRig wires up a Server with all the repos the handler
// reaches. Keeps the per-test setup terse and the assertions focused
// on behaviour rather than plumbing.
func retryFromStepRig(t *testing.T, exec *persistence.Execution, task *persistence.Task, outcomes []*persistence.ExecutionStepOutcome) (
	*Server,
	*mocks.MockExecutionRepository,
	*mocks.MockTaskRepository,
	*retryFromStepFakeOutcomeRepo,
	*retryFromStepFakeExecutor,
) {
	t.Helper()
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			if id == exec.ID {
				return exec, nil
			}
			return nil, persistence.ErrNotFound
		},
		UpdateStatusFunc: func(_ context.Context, id string, status persistence.ExecutionStatus) error {
			if id == exec.ID {
				exec.Status = status
			}
			return nil
		},
		SaveStateSnapshotFunc: func(_ context.Context, id string, snapshot []byte, currentStepID string, completedSteps []string) error {
			if id == exec.ID {
				exec.StateSnapshot = append([]byte{}, snapshot...)
				exec.CompletedSteps = append([]string{}, completedSteps...)
				exec.CurrentStepID = &currentStepID
			}
			return nil
		},
	}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			if id == task.ID {
				return task, nil
			}
			return nil, persistence.ErrNotFound
		},
		UpdateStatusFunc: func(_ context.Context, id string, status persistence.TaskStatus) error {
			if id == task.ID {
				task.Status = status
			}
			return nil
		},
	}
	outcomeRepo := &retryFromStepFakeOutcomeRepo{rows: outcomes}
	executorFake := &retryFromStepFakeExecutor{}
	server := NewServer(
		WithTaskRepository(taskRepo),
		WithExecutionRepository(execRepo),
		WithStepOutcomeRepository(outcomeRepo),
		WithExecutor(executorFake),
	)
	return server, execRepo, taskRepo, outcomeRepo, executorFake
}

// TestExecutionRetryFromStep_HappyPath_TruncatesCompletedSteps —
// canonical happy path: a 4-step execution failed at step "implement";
// operator picks "review" (which completed before "implement") and
// the handler:
//   - rewinds CompletedSteps to [plan, research] (everything before
//     "review")
//   - sets CurrentStepID = "review"
//   - marks downstream outcomes (recorded_at > review's recorded_at)
//     as superseded
//   - flips execution to Paused with PauseReasonRetryFromStep
//   - flips task off FAILED into RUNNING
//   - kicks executor.ResumePaused so the resume happens in-process
func TestExecutionRetryFromStep_HappyPath_TruncatesCompletedSteps(t *testing.T) {
	t0 := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	failedStepID := "implement"
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		ProjectID:      "p1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan", "research", "review"},
		CurrentStepID:  &failedStepID,
	}
	task := &persistence.Task{
		ID:        "t1",
		ProjectID: "p1",
		Status:    persistence.TaskStatusFailed,
	}
	outcomes := []*persistence.ExecutionStepOutcome{
		{ID: "o1", StepID: "plan", Outcome: "ok", RecordedAt: t0},
		{ID: "o2", StepID: "research", Outcome: "ok", RecordedAt: t0.Add(time.Minute)},
		{ID: "o3", StepID: "review", Outcome: "ok", RecordedAt: t0.Add(2 * time.Minute)},
		{ID: "o4", StepID: "implement", Outcome: "failed", RecordedAt: t0.Add(3 * time.Minute)},
	}
	s, _, _, outcomeRepo, exe := retryFromStepRig(t, exec, task, outcomes)

	form := strings.NewReader("step_id=review")
	req := httptest.NewRequest(http.MethodPost, "/ui/executions/e1/retry-from-step", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ExecutionRetryFromStep(rec, req, "e1")

	require.Equal(t, http.StatusSeeOther, rec.Code, "want 303 redirect after successful retry: %s", rec.Body.String())
	assert.Equal(t, "/ui/executions/e1", rec.Header().Get("Location"))

	// Execution state must have been rewound: CompletedSteps cut at
	// "review" (which becomes the new CurrentStepID and is re-run).
	assert.Equal(t, []string{"plan", "research"}, exec.CompletedSteps,
		"CompletedSteps must end JUST BEFORE the chosen step — the chosen step is re-run, not skipped")
	require.NotNil(t, exec.CurrentStepID)
	assert.Equal(t, "review", *exec.CurrentStepID, "CurrentStepID becomes the chosen step")
	assert.Equal(t, persistence.ExecutionStatusPaused, exec.Status,
		"execution must be Paused so ResumePaused (or daemon Recover) drives the resume")

	// State snapshot carries the rewound info + the pause reason so
	// recoverExecution finds an auto-resumable Paused row.
	var state map[string]any
	require.NoError(t, json.Unmarshal(exec.StateSnapshot, &state))
	assert.Equal(t, "review", state["currentStepId"])
	assert.Equal(t, executor.PauseReasonRetryFromStep, state["pausedReason"],
		"pause reason must be the retry sentinel so Recover() auto-resumes if the in-process kick failed")

	// Task must be off the terminal FAILED state — RUNNING matches
	// what recoverExecution wants to see.
	assert.Equal(t, persistence.TaskStatusRunning, task.Status,
		"task must be lifted off FAILED so recoverExecution doesn't refuse the resume")

	// SupersedeAfter called once with the chosen step's recorded_at
	// as the cutoff — downstream rows (implement) get relabelled.
	require.Len(t, outcomeRepo.supersedeCalls, 1, "SupersedeAfter must be invoked exactly once")
	assert.Equal(t, "e1", outcomeRepo.supersedeCalls[0].executionID)
	assert.Equal(t, t0.Add(2*time.Minute), outcomeRepo.supersedeCalls[0].cutoff,
		"cutoff must equal the chosen step's RecordedAt — strict-after means the chosen row stays intact")

	// In-process ResumePaused was kicked.
	require.Len(t, exe.resumePaused, 1, "executor.ResumePaused must fire once for the rewound execution")
	assert.Equal(t, "e1", exe.resumePaused[0])
	assert.Empty(t, exe.cancelled, "Cancel must NOT be called — retry is the opposite operation")
}

// TestExecutionRetryFromStep_FailedStepItself — operator can retry
// from the failed step itself. CompletedSteps stay intact (all
// previously-completed steps are preserved); CurrentStepID becomes
// the failed step which the resume loop re-runs. Cutoff defaults to
// the failed step's recorded_at, so SupersedeAfter is a no-op (no
// rows strictly after).
func TestExecutionRetryFromStep_FailedStepItself(t *testing.T) {
	t0 := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	failedStepID := "implement"
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		ProjectID:      "p1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan", "research", "review"},
		CurrentStepID:  &failedStepID,
	}
	task := &persistence.Task{
		ID:        "t1",
		ProjectID: "p1",
		Status:    persistence.TaskStatusFailed,
	}
	outcomes := []*persistence.ExecutionStepOutcome{
		{ID: "o1", StepID: "plan", Outcome: "ok", RecordedAt: t0},
		{ID: "o2", StepID: "implement", Outcome: "failed", RecordedAt: t0.Add(3 * time.Minute)},
	}
	s, _, _, outcomeRepo, _ := retryFromStepRig(t, exec, task, outcomes)

	form := strings.NewReader("step_id=implement")
	req := httptest.NewRequest(http.MethodPost, "/ui/executions/e1/retry-from-step", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ExecutionRetryFromStep(rec, req, "e1")

	require.Equal(t, http.StatusSeeOther, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, []string{"plan", "research", "review"}, exec.CompletedSteps,
		"completed steps must stay intact when retrying from the failed step itself")
	require.NotNil(t, exec.CurrentStepID)
	assert.Equal(t, "implement", *exec.CurrentStepID)
	require.Len(t, outcomeRepo.supersedeCalls, 1)
	assert.Equal(t, t0.Add(3*time.Minute), outcomeRepo.supersedeCalls[0].cutoff,
		"cutoff = failed step's recorded_at — leaves the failed row intact, supersedes nothing further")
}

// TestExecutionRetryFromStep_RefusesWhileRunning — retry is only
// valid on terminal-non-success states. A RUNNING execution must
// refuse the request rather than racing the scheduler.
func TestExecutionRetryFromStep_RefusesWhileRunning(t *testing.T) {
	exec := &persistence.Execution{
		ID:        "e1",
		TaskID:    "t1",
		ProjectID: "p1",
		Status:    persistence.ExecutionStatusRunning, // ← refuse
	}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning}
	s, _, _, outcomeRepo, exe := retryFromStepRig(t, exec, task, nil)

	form := strings.NewReader("step_id=plan")
	req := httptest.NewRequest(http.MethodPost, "/ui/executions/e1/retry-from-step", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ExecutionRetryFromStep(rec, req, "e1")

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"running execution must refuse retry — racing the scheduler would corrupt state")
	assert.Empty(t, outcomeRepo.supersedeCalls)
	assert.Empty(t, exe.resumePaused, "ResumePaused must NOT fire on refused request")
}

// TestExecutionRetryFromStep_UnknownStepRefused — a step_id that
// isn't in CompletedSteps and isn't the failed step is rejected. A
// typo or stale browser tab shouldn't quietly produce a half-rewound
// execution.
func TestExecutionRetryFromStep_UnknownStepRefused(t *testing.T) {
	failedStepID := "implement"
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		ProjectID:      "p1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan", "research"},
		CurrentStepID:  &failedStepID,
	}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusFailed}
	s, _, _, outcomeRepo, _ := retryFromStepRig(t, exec, task, nil)

	form := strings.NewReader("step_id=mystery")
	req := httptest.NewRequest(http.MethodPost, "/ui/executions/e1/retry-from-step", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ExecutionRetryFromStep(rec, req, "e1")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, outcomeRepo.supersedeCalls, "no supersede on a refused request")
}

// TestExecutionRetryFromStep_EmptyFormFieldRejected — a missing
// step_id from a malformed form post is a client error, not a
// silent default. Catches the "submit without picking from the
// dropdown" path.
func TestExecutionRetryFromStep_EmptyFormFieldRejected(t *testing.T) {
	failedStepID := "implement"
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan"},
		CurrentStepID:  &failedStepID,
	}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusFailed}
	s, _, _, _, _ := retryFromStepRig(t, exec, task, nil)

	form := strings.NewReader("step_id=")
	req := httptest.NewRequest(http.MethodPost, "/ui/executions/e1/retry-from-step", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ExecutionRetryFromStep(rec, req, "e1")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestExecutionRetryFromStep_MethodGuard — only POST is allowed.
// Browsing to the URL directly mustn't trigger a state mutation.
func TestExecutionRetryFromStep_MethodGuard(t *testing.T) {
	exec := &persistence.Execution{ID: "e1", Status: persistence.ExecutionStatusFailed}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusFailed}
	s, _, _, _, _ := retryFromStepRig(t, exec, task, nil)

	req := httptest.NewRequest(http.MethodGet, "/ui/executions/e1/retry-from-step", nil)
	rec := httptest.NewRecorder()
	s.ExecutionRetryFromStep(rec, req, "e1")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// TestExecutionRetryFromStep_NotFound — non-existent execution
// returns 404 (and definitely doesn't crash the handler).
func TestExecutionRetryFromStep_NotFound(t *testing.T) {
	exec := &persistence.Execution{ID: "e1", Status: persistence.ExecutionStatusFailed}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusFailed}
	s, _, _, _, _ := retryFromStepRig(t, exec, task, nil)

	form := strings.NewReader("step_id=plan")
	req := httptest.NewRequest(http.MethodPost, "/ui/executions/missing/retry-from-step", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ExecutionRetryFromStep(rec, req, "missing")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestExecutionRetryFromStep_SaveStateFailureReturns500 — a DB
// hiccup persisting the rewound state must NOT leave the operator
// thinking the retry succeeded. The handler returns 500 so the
// browser's redirect doesn't fire and the operator sees the error.
func TestExecutionRetryFromStep_SaveStateFailureReturns500(t *testing.T) {
	failedStepID := "implement"
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan", "research"},
		CurrentStepID:  &failedStepID,
	}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusFailed}
	s, execRepo, _, _, exe := retryFromStepRig(t, exec, task, nil)
	// Force SaveStateSnapshot to error after the supersede call,
	// proving the handler aborts before flipping execution status.
	execRepo.SaveStateSnapshotFunc = func(context.Context, string, []byte, string, []string) error {
		return assert.AnError
	}

	form := strings.NewReader("step_id=research")
	req := httptest.NewRequest(http.MethodPost, "/ui/executions/e1/retry-from-step", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ExecutionRetryFromStep(rec, req, "e1")

	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"save-state failure must surface as 500 so the operator's redirect doesn't fire")
	assert.NotEqual(t, persistence.ExecutionStatusPaused, exec.Status,
		"flip to paused must NOT happen when save-state failed — the row is in a known-good (still FAILED) state")
	assert.Empty(t, exe.resumePaused, "ResumePaused must NOT fire when persist failed")
}

// TestExecutionRetryFromStep_OutcomeRepoNilUsesNowCutoff — when the
// outcomeRepo isn't wired (test or upgrade-time configurations
// without the step-outcome table), the handler must still succeed
// — it just falls through with cutoff = NOW, which is a safe
// no-op for SupersedeAfter and lets the operator retry from any
// step regardless of audit-table state.
func TestExecutionRetryFromStep_OutcomeRepoNilUsesNowCutoff(t *testing.T) {
	failedStepID := "implement"
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan", "research"},
		CurrentStepID:  &failedStepID,
	}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusFailed}
	// Use the rig but THEN nil out the outcomeRepo via reflection
	// of the rig contract: rig wires outcomeRepo via
	// WithStepOutcomeRepository, and the field is exposed on
	// Server. The cleanest path is to NewServer without the
	// option entirely; recreate manually here.
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			if id == exec.ID {
				return exec, nil
			}
			return nil, persistence.ErrNotFound
		},
		UpdateStatusFunc: func(_ context.Context, _ string, status persistence.ExecutionStatus) error {
			exec.Status = status
			return nil
		},
		SaveStateSnapshotFunc: func(_ context.Context, _ string, snapshot []byte, currentStepID string, completedSteps []string) error {
			exec.StateSnapshot = append([]byte{}, snapshot...)
			exec.CompletedSteps = append([]string{}, completedSteps...)
			exec.CurrentStepID = &currentStepID
			return nil
		},
	}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) { return task, nil },
		UpdateStatusFunc: func(_ context.Context, _ string, status persistence.TaskStatus) error {
			task.Status = status
			return nil
		},
	}
	executorFake := &retryFromStepFakeExecutor{}
	s := NewServer(
		WithExecutionRepository(execRepo),
		WithTaskRepository(taskRepo),
		WithExecutor(executorFake),
		// deliberately NO WithStepOutcomeRepository — exercise nil branch
	)

	form := strings.NewReader("step_id=research")
	req := httptest.NewRequest(http.MethodPost, "/ui/executions/e1/retry-from-step", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ExecutionRetryFromStep(rec, req, "e1")
	require.Equal(t, http.StatusSeeOther, rec.Code,
		"nil outcomeRepo must NOT block the retry — the supersede call falls through and the rest of the rewind succeeds")
	assert.Equal(t, persistence.ExecutionStatusPaused, exec.Status)
}

// TestExecutionRouter_DefaultDispatchesToDetail — a vanilla GET
// to `/executions/{id}` (no action suffix) must reach the detail
// handler. Anchors the catch-all branch of the router switch so
// adding a new POST action above doesn't accidentally swallow
// detail GETs.
func TestExecutionRouter_DefaultDispatchesToDetail(t *testing.T) {
	exec := &persistence.Execution{
		ID:        "e1",
		TaskID:    "t1",
		ProjectID: "p1",
		Status:    persistence.ExecutionStatusCompleted,
	}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusCompleted}
	s, _, _, _, _ := retryFromStepRig(t, exec, task, nil)

	req := httptest.NewRequest(http.MethodGet, "/executions/e1", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	// Detail handler returns HTML 200 on success (or 404 if the
	// rig didn't wire enough repos). Either way, the request must
	// reach the detail path — assert by checking the response was
	// NOT the 303/400 shape the action handlers emit.
	require.NotEqual(t, http.StatusSeeOther, rec.Code,
		"GET /executions/{id} must not hit the retry-from-step branch (would 303)")
	require.NotEqual(t, http.StatusMethodNotAllowed, rec.Code)
}

// TestExecutionRouter_CancelPathDispatchesCancel — pins the
// /cancel branch. Together with the status, retry-from-step, and
// default tests above this anchors all four switch cases so a
// future refactor of the router (or a new action joining the
// switch) can't accidentally drop a branch silently.
func TestExecutionRouter_CancelPathDispatchesCancel(t *testing.T) {
	exec := &persistence.Execution{
		ID:        "e1",
		TaskID:    "t1",
		ProjectID: "p1",
		Status:    persistence.ExecutionStatusRunning,
	}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning}
	s, _, _, _, exe := retryFromStepRig(t, exec, task, nil)

	req := httptest.NewRequest(http.MethodPost, "/executions/e1/cancel", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusSeeOther, rec.Code, "cancel handler emits 303 on success")
	require.Equal(t, []string{"t1"}, exe.cancelled, "ExecutionCancel must reach the executor via the router")
}

func TestExecutionCancel_RepoMissingReturns503(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/ui/executions/e1/cancel", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionCancel(rec, req, "e1")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestExecutionRouter_StatusPathDispatchesPartial — pins the
// /status branch so a future refactor can't break the existing
// HTMX polling surface.
func TestExecutionRouter_StatusPathDispatchesPartial(t *testing.T) {
	exec := &persistence.Execution{
		ID:        "e1",
		TaskID:    "t1",
		ProjectID: "p1",
		Status:    persistence.ExecutionStatusRunning,
	}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning}
	s, _, _, _, _ := retryFromStepRig(t, exec, task, nil)

	req := httptest.NewRequest(http.MethodGet, "/executions/e1/status", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	// ExecutionStatusPartial writes a status badge fragment; the
	// exact HTML is covered elsewhere. Here we just anchor the
	// dispatch — non-303, non-404, has "RUNNING" somewhere.
	assert.Equal(t, http.StatusOK, rec.Code, "status partial must reach the partial handler, not 404/303")
	assert.Contains(t, rec.Body.String(), "RUNNING")
}

func TestExecutionStatusPartial_RepoMissingReturns503(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/executions/e1/status", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionStatusPartial(rec, req, "e1")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestExecutionRetryFromStep_RepoMissingReturns503(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/ui/executions/e1/retry-from-step", strings.NewReader("step_id=plan"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ExecutionRetryFromStep(rec, req, "e1")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestExecutionRouter_DispatchesRetryFromStep — the router's new
// 2026.6.0 case must route POST /executions/{id}/retry-from-step
// to ExecutionRetryFromStep (not fall through to ExecutionDetail).
// Tests via the public Handler() rather than calling the
// unexported router so the assertion exercises the same path the
// HTTP server uses.
func TestExecutionRouter_DispatchesRetryFromStep(t *testing.T) {
	failedStepID := "implement"
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan"},
		CurrentStepID:  &failedStepID,
	}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusFailed}
	s, _, _, _, exe := retryFromStepRig(t, exec, task, nil)

	form := strings.NewReader("step_id=plan")
	req := httptest.NewRequest(http.MethodPost, "/executions/e1/retry-from-step", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code,
		"router must dispatch the new path to the retry handler, not the detail handler")
	require.Len(t, exe.resumePaused, 1, "ResumePaused must have fired via the routed handler")
}

// TestExecutionRetryFromStep_FailedResumeStaysPaused — if the
// in-process ResumePaused fails (e.g. the executor isn't wired,
// or recoverExecution returns an error), the row stays Paused so
// the daemon's Recover() loop picks it up on next restart. The
// handler MUST still return 303 to the operator — the persisted
// state is correct, it's just the immediate kick that failed.
func TestExecutionRetryFromStep_FailedResumeStaysPaused(t *testing.T) {
	failedStepID := "implement"
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan", "research"},
		CurrentStepID:  &failedStepID,
	}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusFailed}
	s, _, _, _, exe := retryFromStepRig(t, exec, task, nil)
	// Force ResumePaused to fail to prove the handler doesn't roll
	// back the persisted state.
	exe.resumePausedFn = func(_ string) error {
		return assert.AnError
	}

	form := strings.NewReader("step_id=research")
	req := httptest.NewRequest(http.MethodPost, "/ui/executions/e1/retry-from-step", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ExecutionRetryFromStep(rec, req, "e1")

	require.Equal(t, http.StatusSeeOther, rec.Code,
		"the persisted state is correct even when in-process resume fails — daemon Recover() picks it up. Handler must NOT 500.")
	assert.Equal(t, persistence.ExecutionStatusPaused, exec.Status)
	assert.Len(t, exe.resumePaused, 1, "ResumePaused must still have been attempted exactly once")
}
