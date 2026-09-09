package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// Pause is gated on a stale task status (backlog 2026-08-21, filed while
// fixing the cancel teardown in 68550db9; 05-scheduler.md §4.8).
//
// PauseTask read task.Status at handler entry and only asked the executor
// when that snapshot said RUNNING. Its bare fallback transition accepts
// LEASED and RUNNING alike, so a task observed LEASED that reached RUNNING
// before the conditional write had its row flipped to PAUSED while its
// container kept executing — the same stale-snapshot gate that leaked
// containers on the cancel path. The rule §4.7 states for cancel applies
// unchanged: whether a container is running is answered by the executor's
// live map, never by a status snapshot.

// pauseLiveMapSpy answers Pause with a fixed result and counts the calls.
// Only Pause is reached by these tests; the embedded nil interface covers
// the rest of ExecutorInterface.
type pauseLiveMapSpy struct {
	ExecutorInterface
	pauseCalls int
	pauseErr   error
}

func (s *pauseLiveMapSpy) Pause(string) (*executor.PauseStatus, error) {
	s.pauseCalls++
	if s.pauseErr != nil {
		return nil, s.pauseErr
	}
	return &executor.PauseStatus{}, nil
}

// The executor is asked regardless of what the snapshot says. A LEASED
// snapshot with a live execution behind it is paused through the executor
// (which stops the container and writes PAUSED itself), and the bare
// transition is not attempted.
func TestPauseTask_AsksTheExecutorEvenWhenTheSnapshotIsNotRunning(t *testing.T) {
	transitioned := false
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusLeased), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			transitioned = true
			return true, nil
		},
	}
	exec := &pauseLiveMapSpy{}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	srv.executor = exec
	req := httptest.NewRequest(http.MethodPost, tcURL("/pause"), nil)
	rec := httptest.NewRecorder()
	srv.PauseTask(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if exec.pauseCalls != 1 {
		t.Fatalf("the executor must be asked whatever the snapshot said; Pause calls = %d", exec.pauseCalls)
	}
	if transitioned {
		t.Errorf("executor.Pause succeeded (it wrote PAUSED itself); the bare transition must not run on top of it")
	}
}

// When the executor reports nothing running, the bare transition may flip a
// task that has not started — but never a RUNNING row. A RUNNING row with no
// live execution behind it is either a snapshot that went stale or a run
// this daemon does not own; flipping it to PAUSED is the leak.
func TestPauseTask_BareFlipNeverAcceptsRunningAfterTheLiveMapSaidNothing(t *testing.T) {
	var from []persistence.TaskStatus
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusLeased), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, f []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			from = f
			if to != persistence.TaskStatusPaused {
				t.Errorf("target: got %s", to)
			}
			return true, nil
		},
	}
	exec := &pauseLiveMapSpy{pauseErr: fmt.Errorf("%w: task x", executor.ErrNoActiveExecution)}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	srv.executor = exec
	req := httptest.NewRequest(http.MethodPost, tcURL("/pause"), nil)
	rec := httptest.NewRecorder()
	srv.PauseTask(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if from == nil {
		t.Fatal("the bare transition must run when nothing is executing")
	}
	if slices.Contains(from, persistence.TaskStatusRunning) {
		t.Errorf("the bare transition accepted RUNNING after the live map said nothing was running: %v", from)
	}
	if !slices.Contains(from, persistence.TaskStatusLeased) {
		t.Errorf("a leased task that has not started must still be pausable: %v", from)
	}
}

// A real executor failure (not the no-active-execution sentinel) is
// surfaced, not swallowed into a bare flip that would leave the container
// running behind a PAUSED row.
func TestPauseTask_ExecutorFailureIsSurfacedNotFlippedAround(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusLeased), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			t.Error("the bare transition must not run when the executor failed to pause a live execution")
			return true, nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	srv.executor = &pauseLiveMapSpy{pauseErr: errors.New("podman stop: socket timeout")}
	req := httptest.NewRequest(http.MethodPost, tcURL("/pause"), nil)
	rec := httptest.NewRecorder()
	srv.PauseTask(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}
