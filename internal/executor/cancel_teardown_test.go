package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/runtime"
)

// Cancelling is two obligations — flip the row, stop the process — and only the
// first is transactional. Every teardown site used to gate on a task-status
// snapshot read BEFORE the transition, while the transition itself accepts
// PENDING/QUEUED/LEASED/RUNNING/WAITING_FOR_CHILDREN. So a task observed as
// LEASED that reached RUNNING before the conditional write had its row flipped
// to CANCELLED and its container left alive: no row claimed it, no reaper
// looked for it, and it billed until it finished — then wrote its result
// against a terminal task.
//
// Reported 2026-08-20: stopping benchmark arms left child work in flight, and
// reaching an idle bench meant stopping agent containers by hand.
// See https://docs.vornik.io §4.7.

// teardownRuntime records which containers were asked to stop, and can fail on
// demand. Purpose-built rather than reusing MockRuntime, which always succeeds
// and does not record ids — and the failure path is half of what is under test.
type teardownRuntime struct {
	mu       sync.Mutex
	stopped  []string
	failWith error
}

func (r *teardownRuntime) StopContainer(_ context.Context, containerID string, _ bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = append(r.stopped, containerID)
	return r.failWith
}

func (r *teardownRuntime) stoppedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stopped...)
}

func (r *teardownRuntime) StartContainer(context.Context, *runtime.ContainerConfig) (string, error) {
	return "", nil
}
func (r *teardownRuntime) InspectContainer(context.Context, string) (*runtime.Container, error) {
	return nil, nil
}
func (r *teardownRuntime) WaitForExit(context.Context, string, time.Duration) (int, error) {
	return 0, nil
}
func (r *teardownRuntime) GetContainerByTask(context.Context, string) (*runtime.Container, error) {
	return nil, nil
}
func (r *teardownRuntime) RemoveContainer(context.Context, string, bool) error { return nil }
func (r *teardownRuntime) Logs(context.Context, string, int) (string, error)   { return "", nil }

// liveExecutor returns an executor that believes it is running taskID with a
// container attached, wired to rt. Repos come from the package's shared mocks
// because Cancel writes the terminal status through them.
func liveExecutor(t *testing.T, taskID, containerID string, rt *teardownRuntime) *Executor {
	t.Helper()
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	tr.AddTask(&persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusRunning})
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil)
	e.mu.Lock()
	e.activeExecutions[taskID] = &executionHandle{
		taskID:      taskID,
		projectID:   "p1",
		containerID: containerID,
		cancel:      func() {},
	}
	e.mu.Unlock()
	return e
}

// THE RACE. The teardown decision comes from the executor's own live map, not
// from a status snapshot — so a task whose row read LEASED is still stopped
// when the executor is in fact running it.
func TestCancelIfActive_TearsDownWhateverTheRowSaid(t *testing.T) {
	rt := &teardownRuntime{}
	e := liveExecutor(t, "task-1", "container-1", rt)

	had, err := e.CancelIfActive("task-1")

	if !had {
		t.Fatal("the executor is running this task — hadExecution must be true however the DB row read")
	}
	if err != nil {
		t.Fatalf("stopping a healthy container must not error: %v", err)
	}
	if got := rt.stoppedIDs(); len(got) != 1 || got[0] != "container-1" {
		t.Errorf("the container must actually be asked to stop; stopped=%v", got)
	}
}

// "Nothing to stop" is the ordinary answer for a queued task and must not read
// as a failure — otherwise a cascade over queued children fills the log with
// warnings and the real failures stop standing out.
func TestCancelIfActive_NoLiveExecutionIsNotAnError(t *testing.T) {
	rt := &teardownRuntime{}
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), NewMockTaskRepo(), nil)

	had, err := e.CancelIfActive("task-absent")

	if had {
		t.Error("hadExecution must be false when the executor is running nothing for this task")
	}
	if err != nil {
		t.Errorf("absence is routine, not an error; got %v", err)
	}
	if got := rt.stoppedIDs(); len(got) != 0 {
		t.Errorf("nothing should have been stopped; stopped=%v", got)
	}
}

// The distinction a bare bool would lose: a container the executor IS running
// and CANNOT stop is still alive and still billing while its row says
// CANCELLED. Swallowing that rebuilds a quieter version of this same bug —
// which is what the old code did, discarding StopContainer's error outright.
func TestCancelIfActive_TeardownFailureSurfaces(t *testing.T) {
	rt := &teardownRuntime{failWith: errors.New("podman socket timeout")}
	e := liveExecutor(t, "task-1", "container-1", rt)

	had, err := e.CancelIfActive("task-1")

	if !had {
		t.Fatal("hadExecution must be true — there was an execution")
	}
	if err == nil {
		t.Fatal("a container that could not be stopped must surface: it is still running and still billing")
	}
}

// cascadeExecutor builds an executor over a child tree, with the executor
// actually running the named tasks.
func cascadeExecutor(tasks map[string]*persistence.Task, childrenOf map[string][]string,
	live map[string]string, rt *teardownRuntime,
) *Executor {
	repo := &mocks.MockTaskRepository{
		GetChildrenFunc: func(_ context.Context, id string) ([]*persistence.Task, error) {
			var out []*persistence.Task
			for _, cid := range childrenOf[id] {
				out = append(out, tasks[cid])
			}
			return out, nil
		},
		TransitionConditionalFunc: func(_ context.Context, id string, from []persistence.TaskStatus,
			to persistence.TaskStatus, _ persistence.TransitionOpts,
		) (bool, error) {
			task := tasks[id]
			for _, s := range from {
				if task.Status == s {
					task.Status = to
					return true, nil
				}
			}
			return false, nil
		},
	}
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), repo, nil)
	e.mu.Lock()
	for taskID, containerID := range live {
		e.activeExecutions[taskID] = &executionHandle{
			taskID: taskID, projectID: "p1", containerID: containerID, cancel: func() {},
		}
	}
	e.mu.Unlock()
	return e
}

// The cascade must stop every descendant the executor is actually running, not
// only the ones whose stale tree-walk read happened to say RUNNING. child-leased
// read as LEASED and is live — the exact shape that leaked containers.
func TestCancelChildren_StopsLiveChildReadAsLeased(t *testing.T) {
	parentID := "parent-1"
	tasks := map[string]*persistence.Task{
		"parent-1":     {ID: "parent-1", Status: persistence.TaskStatusCancelled},
		"child-leased": {ID: "child-leased", ParentTaskID: &parentID, Status: persistence.TaskStatusLeased},
	}
	rt := &teardownRuntime{}
	e := cascadeExecutor(tasks,
		map[string][]string{"parent-1": {"child-leased"}},
		map[string]string{"child-leased": "container-child"},
		rt)

	e.CancelChildren(context.Background(), parentID)

	if tasks["child-leased"].Status != persistence.TaskStatusCancelled {
		t.Fatalf("child-leased = %s, want CANCELLED", tasks["child-leased"].Status)
	}
	if got := rt.stoppedIDs(); len(got) != 1 || got[0] != "container-child" {
		t.Errorf("a live descendant must be stopped whatever status the tree walk read; stopped=%v", got)
	}
}

// A descendant that lost the transition race is already terminal and is not
// ours to stop.
func TestCancelChildren_NoTeardownForAlreadyTerminalChild(t *testing.T) {
	parentID := "parent-1"
	tasks := map[string]*persistence.Task{
		"parent-1":   {ID: "parent-1", Status: persistence.TaskStatusCancelled},
		"child-done": {ID: "child-done", ParentTaskID: &parentID, Status: persistence.TaskStatusCompleted},
	}
	rt := &teardownRuntime{}
	e := cascadeExecutor(tasks,
		map[string][]string{"parent-1": {"child-done"}},
		map[string]string{"child-done": "container-done"},
		rt)

	e.CancelChildren(context.Background(), parentID)

	if got := rt.stoppedIDs(); len(got) != 0 {
		t.Errorf("a task that did not transition must not be torn down; stopped=%v", got)
	}
}

// A queued descendant has no container; the cascade must transition it without
// attempting or logging a teardown.
func TestCancelChildren_QueuedChildNeedsNoTeardown(t *testing.T) {
	parentID := "parent-1"
	tasks := map[string]*persistence.Task{
		"parent-1":     {ID: "parent-1", Status: persistence.TaskStatusCancelled},
		"child-queued": {ID: "child-queued", ParentTaskID: &parentID, Status: persistence.TaskStatusQueued},
	}
	rt := &teardownRuntime{}
	e := cascadeExecutor(tasks,
		map[string][]string{"parent-1": {"child-queued"}},
		nil, rt)

	e.CancelChildren(context.Background(), parentID)

	if tasks["child-queued"].Status != persistence.TaskStatusCancelled {
		t.Fatalf("child-queued = %s, want CANCELLED", tasks["child-queued"].Status)
	}
	if got := rt.stoppedIDs(); len(got) != 0 {
		t.Errorf("nothing to stop for a queued child; stopped=%v", got)
	}
}

// The dependent cascade must respect the transition result the same way the
// child cascade does: a dependent that was already terminal did not transition
// and is not ours to stop. Nothing pinned this until a review misread the diff
// as having dropped the guard — the guard was intact, the test was not.
func TestCancelBlockedDependents_NoTeardownForAlreadyTerminalDependent(t *testing.T) {
	tasks := map[string]*persistence.Task{
		"upstream": {ID: "upstream", Status: persistence.TaskStatusFailed},
		"dep-done": {ID: "dep-done", Status: persistence.TaskStatusCompleted},
	}
	rt := &teardownRuntime{}
	repo := &mocks.MockTaskRepository{
		GetDependentsFunc: func(_ context.Context, id string) ([]*persistence.Task, error) {
			if id == "upstream" {
				return []*persistence.Task{tasks["dep-done"]}, nil
			}
			return nil, nil
		},
		TransitionConditionalFunc: func(_ context.Context, id string, from []persistence.TaskStatus,
			to persistence.TaskStatus, _ persistence.TransitionOpts,
		) (bool, error) {
			task := tasks[id]
			for _, s := range from {
				if task.Status == s {
					task.Status = to
					return true, nil
				}
			}
			return false, nil
		},
	}
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), repo, nil)
	e.mu.Lock()
	e.activeExecutions["dep-done"] = &executionHandle{
		taskID: "dep-done", projectID: "p1", containerID: "container-dep", cancel: func() {},
	}
	e.mu.Unlock()

	e.cancelBlockedDependents(context.Background(), "upstream", "upstream failed", map[string]bool{})

	if tasks["dep-done"].Status != persistence.TaskStatusCompleted {
		t.Errorf("a terminal dependent must keep its status; got %s", tasks["dep-done"].Status)
	}
	if got := rt.stoppedIDs(); len(got) != 0 {
		t.Errorf("a dependent that did not transition must not be torn down; stopped=%v", got)
	}
}

// And the positive case for the same path: a live dependent read as LEASED is
// stopped, mirroring the child cascade.
func TestCancelBlockedDependents_StopsLiveDependentReadAsLeased(t *testing.T) {
	tasks := map[string]*persistence.Task{
		"upstream":   {ID: "upstream", Status: persistence.TaskStatusFailed},
		"dep-leased": {ID: "dep-leased", Status: persistence.TaskStatusLeased},
	}
	rt := &teardownRuntime{}
	repo := &mocks.MockTaskRepository{
		GetDependentsFunc: func(_ context.Context, id string) ([]*persistence.Task, error) {
			if id == "upstream" {
				return []*persistence.Task{tasks["dep-leased"]}, nil
			}
			return nil, nil
		},
		TransitionConditionalFunc: func(_ context.Context, id string, from []persistence.TaskStatus,
			to persistence.TaskStatus, _ persistence.TransitionOpts,
		) (bool, error) {
			task := tasks[id]
			for _, s := range from {
				if task.Status == s {
					task.Status = to
					return true, nil
				}
			}
			return false, nil
		},
	}
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), repo, nil)
	e.mu.Lock()
	e.activeExecutions["dep-leased"] = &executionHandle{
		taskID: "dep-leased", projectID: "p1", containerID: "container-dep", cancel: func() {},
	}
	e.mu.Unlock()

	e.cancelBlockedDependents(context.Background(), "upstream", "upstream failed", map[string]bool{})

	if got := rt.stoppedIDs(); len(got) != 1 || got[0] != "container-dep" {
		t.Errorf("a live dependent must be stopped whatever status the walk read; stopped=%v", got)
	}
}
