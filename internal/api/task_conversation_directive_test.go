package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// PostTaskMessage re-queue semantics matrix. These tests pin the
// terminal-directive contract introduced when LLD §7.0 was extended
// to include FAILED and CANCELLED — previously a directive on a
// FAILED task wrote the message row but left the task FAILED, so
// operator course-correction was a silent no-op.
//
// Two strategies under test:
//   - Waiting states (AWAITING_INPUT / AWAITING_EXTERNAL) →
//     TransitionConditional, attempt counter untouched.
//   - Terminal states (FAILED / CANCELLED / COMPLETED) →
//     RequeueTerminalTask with attempt=1, max_attempts preserved.
//   - CLOSED is intentionally NOT re-queued (LLD §7.0): one-way
//     archive, operator must use /retry explicitly.

// fakeTaskMessageRepo is the minimum implementation needed by
// PostTaskMessage: it records inserts so tests can assert the kind /
// content that landed.
type fakeTaskMessageRepo struct {
	inserts []*persistence.TaskMessage
}

func (f *fakeTaskMessageRepo) Insert(_ context.Context, m *persistence.TaskMessage) error {
	m.ID = "tmsg-fake"
	f.inserts = append(f.inserts, m)
	return nil
}
func (f *fakeTaskMessageRepo) List(context.Context, persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return nil, nil
}
func (f *fakeTaskMessageRepo) GetOpenCheckpoint(context.Context, string) (*persistence.TaskMessage, error) {
	return nil, nil
}
func (f *fakeTaskMessageRepo) MarkCheckpointResolved(context.Context, string, string) error {
	return nil
}

// directiveCall captures arguments handed to the task repository so
// the test can assert which re-queue strategy fired.
type directiveCall struct {
	requeueTerminal bool
	requeueAttempt  int
	requeueMaxAtts  int
	transition      bool
	transitionFrom  []persistence.TaskStatus
	transitionOpts  persistence.TransitionOpts
}

// directiveCase drives one row of the re-queue matrix.
type directiveCase struct {
	status       persistence.TaskStatus
	wantRequeued bool // value in JSON response.requeued
}

func runDirectiveCase(t *testing.T, c directiveCase) (*httptest.ResponseRecorder, *directiveCall, *fakeTaskMessageRepo) {
	t.Helper()
	called := &directiveCall{}
	const (
		taskID    = "task-dir-1"
		projectID = "proj-dir"
	)
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			require.Equal(t, taskID, id)
			return &persistence.Task{
				ID:          taskID,
				ProjectID:   projectID,
				Status:      c.status,
				Attempt:     6, // exhausted budget in the original failure mode
				MaxAttempts: 6,
			}, nil
		},
		RequeueTerminalTaskFunc: func(_ context.Context, id string, attempt, maxAtts int) (bool, error) {
			called.requeueTerminal = true
			called.requeueAttempt = attempt
			called.requeueMaxAtts = maxAtts
			return true, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, _ persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
			called.transition = true
			called.transitionFrom = from
			called.transitionOpts = opts
			return true, nil
		},
	}
	msgRepo := &fakeTaskMessageRepo{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(msgRepo),
	)

	body := `{"kind":"directive","content":"the test file is missing because there is no source file yet — create both"}`
	url := "/api/v1/projects/" + projectID + "/tasks/" + taskID + "/messages"
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	rec := httptest.NewRecorder()
	server.PostTaskMessage(rec, req)
	return rec, called, msgRepo
}

// TestPostTaskMessage_DirectiveOnFailed_TerminalRequeue is the
// regression case for task_20260511141927_0baf7c6c8573fe97: directive
// on FAILED must trigger RequeueTerminalTask with a fresh attempt
// budget so the scheduler will lease the task again.
func TestPostTaskMessage_DirectiveOnFailed_TerminalRequeue(t *testing.T) {
	rec, called, msgs := runDirectiveCase(t, directiveCase{
		status:       persistence.TaskStatusFailed,
		wantRequeued: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	require.True(t, called.requeueTerminal, "FAILED must use RequeueTerminalTask")
	require.False(t, called.transition, "FAILED must not use TransitionConditional")
	assert.Equal(t, 1, called.requeueAttempt, "attempt must reset to 1 on terminal re-queue")
	assert.Equal(t, 6, called.requeueMaxAtts, "max_attempts must be preserved from the original task row")

	var resp struct {
		MessageID string `json:"messageId"`
		Requeued  bool   `json:"requeued"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Requeued, "response.requeued must be true on successful FAILED re-queue")
	require.Len(t, msgs.inserts, 1)
	assert.Equal(t, persistence.TaskMessageKindDirective, msgs.inserts[0].MessageKind)
}

// TestPostTaskMessage_DirectiveOnCancelled_TerminalRequeue —
// CANCELLED is in the terminal-directive set per LLD §7 row 2.
// Sometimes an operator cancels, fixes the brief, then wants to
// resume; this exercises that path.
func TestPostTaskMessage_DirectiveOnCancelled_TerminalRequeue(t *testing.T) {
	rec, called, _ := runDirectiveCase(t, directiveCase{
		status:       persistence.TaskStatusCancelled,
		wantRequeued: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	require.True(t, called.requeueTerminal)
	assert.Equal(t, 1, called.requeueAttempt)
}

// TestPostTaskMessage_DirectiveOnCompleted_ResetsAttempt — previously
// COMPLETED used TransitionConditional, which left the attempt
// counter alone. A task that completed with attempt=6 then got a
// course-correct directive would have 0 remaining retry budget. The
// new behaviour routes through RequeueTerminalTask so the budget
// resets to 1.
func TestPostTaskMessage_DirectiveOnCompleted_ResetsAttempt(t *testing.T) {
	rec, called, _ := runDirectiveCase(t, directiveCase{
		status:       persistence.TaskStatusCompleted,
		wantRequeued: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	require.True(t, called.requeueTerminal, "COMPLETED must now use the terminal-requeue primitive, not TransitionConditional")
	require.False(t, called.transition)
	assert.Equal(t, 1, called.requeueAttempt)
}

// TestPostTaskMessage_DirectiveOnAwaitingInput_ConditionalRequeue —
// waiting state, no failure occurred, so the attempt counter is
// preserved and TransitionConditional is the right primitive (it
// also clears the lease).
func TestPostTaskMessage_DirectiveOnAwaitingInput_ConditionalRequeue(t *testing.T) {
	rec, called, _ := runDirectiveCase(t, directiveCase{
		status:       persistence.TaskStatusAwaitingInput,
		wantRequeued: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	require.True(t, called.transition, "AWAITING_INPUT must use TransitionConditional, not RequeueTerminalTask")
	require.False(t, called.requeueTerminal)
	require.Equal(t, []persistence.TaskStatus{persistence.TaskStatusAwaitingInput}, called.transitionFrom)
	assert.True(t, called.transitionOpts.ClearLease, "ClearLease must be set when re-queueing from a waiting state")
}

// TestPostTaskMessage_DirectiveOnAwaitingExternal_ConditionalRequeue
// mirrors the AWAITING_INPUT case for the other waiting state.
func TestPostTaskMessage_DirectiveOnAwaitingExternal_ConditionalRequeue(t *testing.T) {
	rec, called, _ := runDirectiveCase(t, directiveCase{
		status:       persistence.TaskStatusAwaitingExternal,
		wantRequeued: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	require.True(t, called.transition)
	require.False(t, called.requeueTerminal)
}

// TestPostTaskMessage_DirectiveOnClosed_NotRequeued — CLOSED is the
// one terminal state that is NOT in the directive-requeue set per
// LLD §7.0. The message lands (audit trail) but the task does not
// re-enter the queue; operator must hit /retry explicitly.
func TestPostTaskMessage_DirectiveOnClosed_NotRequeued(t *testing.T) {
	rec, called, msgs := runDirectiveCase(t, directiveCase{
		status:       persistence.TaskStatusClosed,
		wantRequeued: false,
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	require.False(t, called.requeueTerminal, "CLOSED must NOT trigger terminal re-queue")
	require.False(t, called.transition, "CLOSED must NOT trigger any status transition")

	var resp struct {
		Requeued bool `json:"requeued"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.False(t, resp.Requeued, "response.requeued must be false for CLOSED — archival is one-way")
	require.Len(t, msgs.inserts, 1, "the directive message itself still gets written (audit trail)")
}

// TestPostTaskMessage_DirectiveOnRunning_NotRequeued — running tasks
// should accept the directive (it queues for the next round per LLD
// §7), but must NOT re-queue right now — the executor is still using
// the lease. The directive lands in task_messages and the lead picks
// it up on the next planning loop.
func TestPostTaskMessage_DirectiveOnRunning_NotRequeued(t *testing.T) {
	rec, called, msgs := runDirectiveCase(t, directiveCase{
		status:       persistence.TaskStatusRunning,
		wantRequeued: false,
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	require.False(t, called.requeueTerminal)
	require.False(t, called.transition)
	require.Len(t, msgs.inserts, 1)
}
