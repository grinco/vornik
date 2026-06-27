package dispatcher

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// pollingTaskRepo embeds the mock and overrides only Get to walk a
// scripted task-status transcript across calls. Lets the test pin
// the polling sequence the wait_for_task tool walks without
// re-implementing the full TaskRepository surface.
type pollingTaskRepo struct {
	*mocks.MockTaskRepository
	mu         sync.Mutex
	transcript []*persistence.Task
	idx        int
}

func (r *pollingTaskRepo) Get(_ context.Context, _ string) (*persistence.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.transcript) == 0 {
		return nil, nil
	}
	if r.idx >= len(r.transcript) {
		t := r.transcript[len(r.transcript)-1]
		cp := *t
		return &cp, nil
	}
	t := r.transcript[r.idx]
	r.idx++
	cp := *t
	return &cp, nil
}

func newPollingRepo(transcript ...*persistence.Task) *pollingTaskRepo {
	return &pollingTaskRepo{
		MockTaskRepository: &mocks.MockTaskRepository{},
		transcript:         transcript,
	}
}

// TestWaitForTask_ReturnsTerminalImmediately — task already at COMPLETED
// when the tool is called. The wait short-circuits with the result.
func TestWaitForTask_ReturnsTerminalImmediately(t *testing.T) {
	repo := newPollingRepo(
		&persistence.Task{ID: "task_x", ProjectID: "p", Status: persistence.TaskStatusCompleted},
	)
	te := &ToolExecutor{taskRepo: repo}
	res := te.waitForTask(context.Background(), `{"task_id":"task_x","timeout_seconds":5}`, []string{"p"})
	// Dispatcher renders the task reference via idfmt.Short — "task_x"
	// (typed prefix + last 4 chars) becomes "T-sk_x".
	assert.Contains(t, res.Content, "T-sk_x")
	assert.Contains(t, res.Content, "COMPLETED")
}

// TestWaitForTask_PollsUntilTerminal — polls walk QUEUED → RUNNING →
// COMPLETED across calls. The tool returns the COMPLETED summary.
// Wall-clock asserted to be small so a regression where the polling
// loop sleeps unnecessarily would surface as a test timeout.
func TestWaitForTask_PollsUntilTerminal(t *testing.T) {
	repo := newPollingRepo(
		&persistence.Task{ID: "task_x", ProjectID: "p", Status: persistence.TaskStatusQueued},
		&persistence.Task{ID: "task_x", ProjectID: "p", Status: persistence.TaskStatusRunning},
		&persistence.Task{ID: "task_x", ProjectID: "p", Status: persistence.TaskStatusCompleted},
	)
	te := &ToolExecutor{taskRepo: repo}
	start := time.Now()
	res := te.waitForTask(context.Background(), `{"task_id":"task_x"}`, []string{"p"})
	elapsed := time.Since(start)
	assert.Contains(t, res.Content, "COMPLETED")
	require.Less(t, elapsed, 6*time.Second, "wait should converge in a few seconds, not idle near the timeout")
}

// TestWaitForTask_TimesOutWhenStillRunning — the task never reaches
// terminal within the timeout. The tool returns a clear message
// telling the dispatcher to either wait again or give up.
func TestWaitForTask_TimesOutWhenStillRunning(t *testing.T) {
	repo := newPollingRepo(
		&persistence.Task{ID: "task_x", ProjectID: "p", Status: persistence.TaskStatusRunning},
	)
	te := &ToolExecutor{taskRepo: repo}
	res := te.waitForTask(context.Background(), `{"task_id":"task_x","timeout_seconds":1}`, []string{"p"})
	assert.True(t,
		strings.Contains(res.Content, "timed out") || strings.Contains(res.Content, "still"),
		"timeout result should explain the state; got: %q", res.Content)
}

// TestWaitForTask_FailedTaskIncludesError — failure surfaces both
// status and the last_error so the dispatcher can mention the
// reason when it tells the user the task didn't produce data.
func TestWaitForTask_FailedTaskIncludesError(t *testing.T) {
	errMsg := "Tool iteration limit (50) reached."
	cls := persistence.TaskFailureClassToolIterationLimit
	repo := newPollingRepo(
		&persistence.Task{ID: "task_x", ProjectID: "p", Status: persistence.TaskStatusFailed, LastError: &errMsg, LastErrorClass: &cls},
	)
	te := &ToolExecutor{taskRepo: repo}
	res := te.waitForTask(context.Background(), `{"task_id":"task_x","timeout_seconds":2}`, []string{"p"})
	assert.Contains(t, res.Content, "FAILED")
	assert.Contains(t, res.Content, "iteration limit")
	assert.Contains(t, res.Content, persistence.TaskFailureClassToolIterationLimit)
}

// TestWaitForTask_RequiresTaskID — a missing task_id is a hard error
// the model can correct on the next turn rather than a silent block.
func TestWaitForTask_RequiresTaskID(t *testing.T) {
	te := &ToolExecutor{}
	res := te.waitForTask(context.Background(), `{}`, []string{"p"})
	assert.Contains(t, strings.ToLower(res.Content), "task_id")
}
