package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// recordingNotifier captures every NotifyTaskCompleted call so the
// test can assert on (count, success, message) without dragging in
// the full Telegram bot. Mirrors how the real bot's NotifyTaskCompleted
// is the side-effect we care about (it both sends a message AND
// removes watchers — both must be deferred until the task is truly
// done).
type recordingNotifier struct {
	mu    sync.Mutex
	calls []notifyCall
}

type notifyCall struct {
	taskID  string
	success bool
	message string
	attempt int
}

func (r *recordingNotifier) NotifyTaskCompleted(_ context.Context, task *persistence.Task, success bool, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, notifyCall{
		taskID:  task.ID,
		success: success,
		message: message,
		attempt: task.Attempt,
	})
}

func (r *recordingNotifier) snapshot() []notifyCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]notifyCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// TestHandleFailure_DefersNotificationWhenRetryRemains — the headline
// regression: a task fails on attempt 1 of 3. Older code fired the
// telegram notifier immediately, which (a) sent a misleading "task
// failed" before the retry ran and (b) wiped the watcher list (the
// telegram bot's NotifyTaskCompleted unconditionally calls
// RemoveWatchers after sending). The eventual success on attempt 2
// then had no chat ID to notify, so operators only ever heard about
// the failure that wasn't terminal. Fix: handleFailure must skip the
// notifier call when the scheduler is going to retry.
func TestHandleFailure_DefersNotificationWhenRetryRemains(t *testing.T) {
	e, _, er, _, tr := setup()
	notifier := &recordingNotifier{}
	e.SetCompletionNotifier(notifier)

	task := &persistence.Task{
		ID:          "t-retry",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusRunning,
		Attempt:     1, // first attempt of 3 — scheduler will retry
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(task)
	exec := &persistence.Execution{
		ID:        "e-retry-1",
		TaskID:    task.ID,
		ProjectID: task.ProjectID,
		Status:    persistence.ExecutionStatusRunning,
	}
	require.NoError(t, er.Create(context.Background(), exec))

	e.handleFailure(context.Background(), task, exec, errors.New("transient blip"))

	assert.Empty(t, notifier.snapshot(),
		"non-final execution failure must NOT notify watchers — would clobber the watcher list before the retry runs")
}

// TestHandleFailure_NotifiesOnTerminalAttempt — the complement: when
// the task has exhausted its retry budget, handleFailure IS the
// terminal event for the task and must fire exactly one notification.
// Without this, operators get no signal that the task gave up.
func TestHandleFailure_NotifiesOnTerminalAttempt(t *testing.T) {
	e, _, er, _, tr := setup()
	notifier := &recordingNotifier{}
	e.SetCompletionNotifier(notifier)

	task := &persistence.Task{
		ID:          "t-final",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusRunning,
		Attempt:     3, // last attempt — scheduler won't retry
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(task)
	exec := &persistence.Execution{
		ID:        "e-final-1",
		TaskID:    task.ID,
		ProjectID: task.ProjectID,
		Status:    persistence.ExecutionStatusRunning,
	}
	require.NoError(t, er.Create(context.Background(), exec))

	e.handleFailure(context.Background(), task, exec, errors.New("permanent failure"))

	calls := notifier.snapshot()
	require.Len(t, calls, 1, "terminal failure must fire exactly one notification")
	assert.Equal(t, "t-final", calls[0].taskID)
	assert.False(t, calls[0].success, "terminal failure notification must report success=false")
}

// TestHandleFailure_FailRetrySucceedSequence — the end-to-end shape
// the user reported: task fails once (with retries available), then
// succeeds. Operators must receive ONE message, and it must be the
// success — not the misleading intermediate failure.
func TestHandleFailure_FailRetrySucceedSequence(t *testing.T) {
	e, _, er, _, tr := setup()
	notifier := &recordingNotifier{}
	e.SetCompletionNotifier(notifier)

	task := &persistence.Task{
		ID:          "t-seq",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusRunning,
		Attempt:     1,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(task)

	exec1 := &persistence.Execution{
		ID:        "e-seq-1",
		TaskID:    task.ID,
		ProjectID: task.ProjectID,
		Status:    persistence.ExecutionStatusRunning,
	}
	require.NoError(t, er.Create(context.Background(), exec1))
	e.handleFailure(context.Background(), task, exec1, errors.New("attempt 1 blew up"))

	// Scheduler would now bump task.Attempt to 2 and re-queue.
	task.Attempt = 2
	task.Status = persistence.TaskStatusRunning

	exec2 := &persistence.Execution{
		ID:        "e-seq-2",
		TaskID:    task.ID,
		ProjectID: task.ProjectID,
		Status:    persistence.ExecutionStatusRunning,
	}
	require.NoError(t, er.Create(context.Background(), exec2))
	// Success path — handleSuccess always notifies.
	e.handleSuccess(context.Background(), task, exec2, "", []byte(`{"message":"all green"}`))

	calls := notifier.snapshot()
	require.Len(t, calls, 1,
		"fail-then-succeed must produce exactly one user-visible notification (the success), not two")
	assert.True(t, calls[0].success, "the surviving notification must be the success, not the intermediate failure")
	assert.Contains(t, calls[0].message, "all green")
}

// recordingJudge captures every Run call so tests can assert
// that fireJudgeIfEnabled wired through to the runner. Mirrors
// the goroutine-based fire-and-forget contract: Run signals the
// done channel, the test waits on it with a bounded timeout.
type recordingJudge struct {
	mu    sync.Mutex
	calls []string
	done  chan struct{}
}

func newRecordingJudge() *recordingJudge {
	return &recordingJudge{done: make(chan struct{}, 16)}
}

func (r *recordingJudge) Run(_ context.Context, task *persistence.Task) error {
	r.mu.Lock()
	r.calls = append(r.calls, task.ID)
	r.mu.Unlock()
	r.done <- struct{}{}
	return nil
}

func (r *recordingJudge) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// TestHandleFailure_FiresJudgeOnTerminalFailure pins the user-
// reported gap (2026-05-03): Phase 1 produced ten high-severity
// signals on a project, but the Phase 3 verdict panel stayed
// empty because high-severity signals failed every step, the
// task hit terminal failure, and handleFailure never called the
// judge runner — the judge was wired only on the success path.
// Operators saw "the layer is broken" when in fact the judge
// simply wasn't being invoked. The fix runs the judge on
// terminal failures too, so the verdict feed reflects all
// terminal tasks regardless of outcome.
func TestHandleFailure_FiresJudgeOnTerminalFailure(t *testing.T) {
	e, _, er, _, tr := setup()
	e.SetCompletionNotifier(&recordingNotifier{})

	// Wire a registry that says "this project has the judge enabled".
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {
				ID: "p1",
				HallucinationJudge: registry.ProjectHallucinationJudge{
					Enabled: true,
					Model:   "claude-haiku-4-5",
				},
			},
		},
	})

	rec := newRecordingJudge()
	e.judgeRunner = rec

	task := &persistence.Task{
		ID:          "t-fail-final",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusRunning,
		Attempt:     3, // last attempt — scheduler won't retry
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(task)
	exec := &persistence.Execution{
		ID:        "e-fail-final-1",
		TaskID:    task.ID,
		ProjectID: task.ProjectID,
		Status:    persistence.ExecutionStatusRunning,
	}
	require.NoError(t, er.Create(context.Background(), exec))

	e.handleFailure(context.Background(), task, exec, errors.New("hallucination block, retries exhausted"))

	select {
	case <-rec.done:
	case <-time.After(2 * time.Second):
		t.Fatal("judge runner was not invoked on terminal failure within 2s — fireJudgeIfEnabled missing from handleFailure path")
	}
	calls := rec.snapshot()
	require.Len(t, calls, 1, "judge must fire exactly once per terminal task")
	assert.Equal(t, "t-fail-final", calls[0])
}

// TestHandleFailure_DoesNotFireJudgeWhenRetryRemains — the
// complement: judge must NOT fire on intermediate failures
// (cost discipline + the runner's idempotency check would short-
// circuit the second call anyway, but firing on every attempt
// burns ctx and goroutine churn). Mirrors the notifier policy
// already exercised in TestHandleFailure_DefersNotificationWhenRetryRemains.
func TestHandleFailure_DoesNotFireJudgeWhenRetryRemains(t *testing.T) {
	e, _, er, _, tr := setup()
	e.SetCompletionNotifier(&recordingNotifier{})
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", HallucinationJudge: registry.ProjectHallucinationJudge{Enabled: true, Model: "x"}},
		},
	})

	rec := newRecordingJudge()
	e.judgeRunner = rec

	task := &persistence.Task{
		ID:          "t-retry-mid",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusRunning,
		Attempt:     1, // first attempt — scheduler will retry
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(task)
	exec := &persistence.Execution{
		ID:        "e-retry-mid-1",
		TaskID:    task.ID,
		ProjectID: task.ProjectID,
		Status:    persistence.ExecutionStatusRunning,
	}
	require.NoError(t, er.Create(context.Background(), exec))

	e.handleFailure(context.Background(), task, exec, errors.New("transient"))

	select {
	case <-rec.done:
		t.Fatal("judge fired on a non-terminal failure — must wait for terminal status")
	case <-time.After(200 * time.Millisecond):
		// expected: nothing fired
	}
	assert.Empty(t, rec.snapshot(), "judge must not fire when scheduler will retry")
}

// TestTaskWillRetry_TableExhausts the predicate that gates the
// notification logic. Mirrors scheduler.TaskCompleted's decision:
// task.Attempt < task.MaxAttempts ⇒ retry; otherwise terminal.
// MaxAttempts == 0 means retries are disabled so the first failure
// is the only one — must NOT defer notification.
func TestTaskWillRetry_Table(t *testing.T) {
	e, _, _, _, _ := setup()
	cases := []struct {
		name        string
		attempt     int
		maxAttempts int
		want        bool
	}{
		{"first of three", 1, 3, true},
		{"second of three", 2, 3, true},
		{"final of three", 3, 3, false},
		{"single attempt run", 1, 1, false},
		{"retries disabled (MaxAttempts=0)", 1, 0, false},
		{"defensive: nil-ish maxAttempts negative", 1, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &persistence.Task{Attempt: tc.attempt, MaxAttempts: tc.maxAttempts}
			assert.Equal(t, tc.want, e.taskWillRetry(task))
		})
	}
	// Nil task is the defensive default — never retry.
	assert.False(t, e.taskWillRetry(nil), "nil task must return false")
}

// TestHandleFailure_LeavesStatusLeasedWhenRetryRemains — the user-
// reported issue from 2026-05-05: tasks were briefly visible as
// FAILED in the UI between handleFailure and the scheduler's
// re-queue (and stranded as terminal-FAILED if anything disrupted
// the handoff). Pre-fix handleFailure unconditionally set
// task.Status = FAILED. Post-fix the status only flips when the
// retry budget is actually exhausted; mid-budget failures leave
// the status as it was (LEASED / RUNNING) so the scheduler's
// ReleaseLease is the sole authority for the final status.
//
// LastError + LastErrorClass must still persist on every attempt
// so operators see WHY each retry happened.
func TestHandleFailure_LeavesStatusLeasedWhenRetryRemains(t *testing.T) {
	e, _, er, _, tr := setup()
	task := &persistence.Task{
		ID:          "t-leased",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased, // scheduler holds the lease
		Attempt:     1,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(task)
	exec := &persistence.Execution{
		ID:        "e-leased-1",
		TaskID:    task.ID,
		ProjectID: task.ProjectID,
		Status:    persistence.ExecutionStatusRunning,
	}
	require.NoError(t, er.Create(context.Background(), exec))

	e.handleFailure(context.Background(), task, exec, errors.New("attempt 1 broke"))

	got, err := tr.Get(context.Background(), "t-leased")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, persistence.TaskStatusLeased, got.Status,
		"mid-budget failure must leave Status as LEASED — pre-fix it briefly flipped to FAILED, "+
			"confusing operators and risking terminal stranding if the scheduler's re-queue handoff failed")
	require.NotNil(t, got.LastError)
	assert.Contains(t, *got.LastError, "attempt 1 broke",
		"LastError must persist on every attempt regardless of retry status")
}

// TestHandleFailure_SetsFailedWhenBudgetExhausted — the complement:
// last attempt failed, no more retries → handleFailure IS terminal
// for the task and must set Status to FAILED.
func TestHandleFailure_SetsFailedWhenBudgetExhausted(t *testing.T) {
	e, _, er, _, tr := setup()
	task := &persistence.Task{
		ID:          "t-final",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusRunning,
		Attempt:     3, // last attempt
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(task)
	exec := &persistence.Execution{
		ID:        "e-final-1",
		TaskID:    task.ID,
		ProjectID: task.ProjectID,
		Status:    persistence.ExecutionStatusRunning,
	}
	require.NoError(t, er.Create(context.Background(), exec))

	e.handleFailure(context.Background(), task, exec, errors.New("permanent failure"))

	got, err := tr.Get(context.Background(), "t-final")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, persistence.TaskStatusFailed, got.Status,
		"final-attempt failure must set Status to FAILED — handleFailure IS the terminal event")
}
