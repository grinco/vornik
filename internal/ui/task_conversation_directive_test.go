package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// UI re-queue semantics mirror task_conversation_directive_test.go in
// the api package — kept in lockstep so the state machine can't
// disagree depending on which surface the operator used.

type fakeUIMessageRepo struct {
	inserts []*persistence.TaskMessage
}

func (f *fakeUIMessageRepo) Insert(_ context.Context, m *persistence.TaskMessage) error {
	m.ID = "tmsg-fake"
	f.inserts = append(f.inserts, m)
	return nil
}
func (f *fakeUIMessageRepo) List(context.Context, persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return nil, nil
}
func (f *fakeUIMessageRepo) GetOpenCheckpoint(context.Context, string) (*persistence.TaskMessage, error) {
	return nil, nil
}
func (f *fakeUIMessageRepo) MarkCheckpointResolved(context.Context, string, string) error {
	return nil
}

type uiDirectiveCall struct {
	requeueTerminal bool
	requeueAttempt  int
	requeueMaxAtts  int
	transition      bool
}

func runUIDirective(t *testing.T, status persistence.TaskStatus) (string, *uiDirectiveCall, *fakeUIMessageRepo) {
	t.Helper()
	called := &uiDirectiveCall{}
	taskRepo := &mocks.MockTaskRepository{
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, attempt, maxAtts int) (bool, error) {
			called.requeueTerminal = true
			called.requeueAttempt = attempt
			called.requeueMaxAtts = maxAtts
			return true, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			called.transition = true
			return true, nil
		},
	}
	msgRepo := &fakeUIMessageRepo{}
	s := NewServer(
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(msgRepo),
	)
	task := &persistence.Task{
		ID:          "task-ui-dir-1",
		ProjectID:   "proj-ui",
		Status:      status,
		Attempt:     6,
		MaxAttempts: 6,
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/task-ui-dir-1/directive",
		strings.NewReader("content=create+the+missing+test+file"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	notice := s.uiPostMessage(context.Background(), task, req, persistence.TaskMessageKindMessage, true)
	return notice, called, msgRepo
}

// TestUI_DirectiveOnFailed_TerminalRequeue is the regression for the
// "I can't send a directive to a failed task via UI" report. With
// the template gate widened (only CLOSED is hidden) and the UI
// handler routing terminal states through RequeueTerminalTask, a
// directive on FAILED must re-queue the task and reset attempt=1.
func TestUI_DirectiveOnFailed_TerminalRequeue(t *testing.T) {
	notice, called, msgs := runUIDirective(t, persistence.TaskStatusFailed)
	if !called.requeueTerminal {
		t.Fatalf("FAILED must invoke RequeueTerminalTask; got transition=%v", called.transition)
	}
	if called.transition {
		t.Fatalf("FAILED must NOT invoke TransitionConditional")
	}
	if called.requeueAttempt != 1 {
		t.Errorf("attempt must reset to 1; got %d", called.requeueAttempt)
	}
	if called.requeueMaxAtts != 6 {
		t.Errorf("max_attempts must be preserved (6); got %d", called.requeueMaxAtts)
	}
	if notice != "directive-requeued" {
		t.Errorf("notice = %q, want directive-requeued", notice)
	}
	if len(msgs.inserts) != 1 || msgs.inserts[0].MessageKind != persistence.TaskMessageKindDirective {
		t.Errorf("expected one directive message inserted; got %+v", msgs.inserts)
	}
}

func TestUI_DirectiveOnCancelled_TerminalRequeue(t *testing.T) {
	notice, called, _ := runUIDirective(t, persistence.TaskStatusCancelled)
	if !called.requeueTerminal {
		t.Fatal("CANCELLED must invoke RequeueTerminalTask")
	}
	if called.requeueAttempt != 1 {
		t.Errorf("attempt must reset to 1; got %d", called.requeueAttempt)
	}
	if notice != "directive-requeued" {
		t.Errorf("notice = %q, want directive-requeued", notice)
	}
}

// TestUI_DirectiveOnCompleted_ResetsAttempt — previously COMPLETED
// went through TransitionConditional and the attempt counter
// stayed at 6. Now it routes through RequeueTerminalTask, so a
// course-correct on a completed task gets a fresh budget.
func TestUI_DirectiveOnCompleted_ResetsAttempt(t *testing.T) {
	_, called, _ := runUIDirective(t, persistence.TaskStatusCompleted)
	if !called.requeueTerminal {
		t.Fatal("COMPLETED must now invoke RequeueTerminalTask, not TransitionConditional")
	}
	if called.transition {
		t.Fatal("COMPLETED must NOT invoke TransitionConditional anymore")
	}
	if called.requeueAttempt != 1 {
		t.Errorf("attempt must reset to 1; got %d", called.requeueAttempt)
	}
}

func TestUI_DirectiveOnAwaitingInput_ConditionalRequeue(t *testing.T) {
	_, called, _ := runUIDirective(t, persistence.TaskStatusAwaitingInput)
	if !called.transition {
		t.Fatal("AWAITING_INPUT must invoke TransitionConditional")
	}
	if called.requeueTerminal {
		t.Fatal("AWAITING_INPUT must NOT invoke RequeueTerminalTask — counter must not reset")
	}
}

func TestUI_DirectiveOnClosed_NotRequeued(t *testing.T) {
	notice, called, msgs := runUIDirective(t, persistence.TaskStatusClosed)
	if called.requeueTerminal || called.transition {
		t.Fatalf("CLOSED must NOT trigger any re-queue path (terminal=%v transition=%v)", called.requeueTerminal, called.transition)
	}
	if notice != "directive-recorded" {
		t.Errorf("notice = %q, want directive-recorded (message written, not re-queued)", notice)
	}
	if len(msgs.inserts) != 1 {
		t.Errorf("the directive message itself still gets written for audit; got %d", len(msgs.inserts))
	}
}
