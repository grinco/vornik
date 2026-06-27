package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// routeReplyToTask is the third surface (after the api and ui
// handlers) that has to honor LLD §7.0's terminal-directive
// re-queue semantics. The bug we're pinning: a directive posted as
// a reply in a forum topic (or via reply-id) against a FAILED task
// used to write the task_messages row and stop — the switch only
// covered AWAITING_INPUT / AWAITING_EXTERNAL / COMPLETED. With the
// fix, terminal states route through RequeueTerminalTask so the
// task re-queues with attempt=1 and a fresh max_attempts budget.

type fakeMsgRepoForRoute struct {
	inserts []*persistence.TaskMessage
}

func (f *fakeMsgRepoForRoute) Insert(_ context.Context, m *persistence.TaskMessage) error {
	m.ID = "tmsg-fake"
	f.inserts = append(f.inserts, m)
	return nil
}
func (f *fakeMsgRepoForRoute) List(context.Context, persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return nil, nil
}
func (f *fakeMsgRepoForRoute) GetOpenCheckpoint(context.Context, string) (*persistence.TaskMessage, error) {
	return nil, nil
}
func (f *fakeMsgRepoForRoute) MarkCheckpointResolved(context.Context, string, string) error {
	return nil
}

type routeCallTrace struct {
	requeueTerminal bool
	requeueAttempt  int
	requeueMaxAtts  int
	transition      bool
	transitionFrom  []persistence.TaskStatus
}

func runRouteReply(t *testing.T, taskStatus persistence.TaskStatus) (*routeCallTrace, *fakeMsgRepoForRoute) {
	t.Helper()
	// Telegram /sendMessage server — captures the ack but we don't
	// care about its body for this test. Just needs to return ok.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	t.Cleanup(ts.Close)

	trace := &routeCallTrace{}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:          id,
				ProjectID:   "test-project",
				Status:      taskStatus,
				Priority:    50,
				Attempt:     6,
				MaxAttempts: 6,
			}, nil
		},
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, attempt, maxAtts int) (bool, error) {
			trace.requeueTerminal = true
			trace.requeueAttempt = attempt
			trace.requeueMaxAtts = maxAtts
			return true, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			trace.transition = true
			trace.transitionFrom = from
			return true, nil
		},
	}
	msgRepo := &fakeMsgRepoForRoute{}
	chatClient := chat.NewClient("x", "k", "m")
	bot, err := NewBot(BotConfig{
		Token:        "test",
		AllowedUsers: map[int64]UserAccess{1: {Allowed: true, Projects: []string{"*"}}},
	}, chatClient,
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(msgRepo),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = ts.URL

	handled, err := bot.routeReplyToTask(context.Background(),
		&Message{ChatID: 1, UserID: 1, Username: "vadim", Text: "create the missing test file — coverage is wrong"},
		"task-route-1", "test-project")
	if err != nil {
		t.Fatalf("routeReplyToTask: %v", err)
	}
	if !handled {
		t.Fatal("routeReplyToTask must claim the message (handled=true) when taskMessageRepo + taskRepo are wired")
	}
	return trace, msgRepo
}

// TestRouteReply_DirectiveOnFailed_TerminalRequeue is the regression
// case for the "directive replied in a forum topic on a FAILED task
// goes nowhere" report.
func TestRouteReply_DirectiveOnFailed_TerminalRequeue(t *testing.T) {
	trace, msgs := runRouteReply(t, persistence.TaskStatusFailed)
	if !trace.requeueTerminal {
		t.Fatalf("FAILED must trigger RequeueTerminalTask; transition=%v", trace.transition)
	}
	if trace.transition {
		t.Fatal("FAILED must NOT trigger TransitionConditional")
	}
	if trace.requeueAttempt != 1 {
		t.Errorf("attempt must reset to 1; got %d", trace.requeueAttempt)
	}
	if trace.requeueMaxAtts != 6 {
		t.Errorf("max_attempts must be preserved (6); got %d", trace.requeueMaxAtts)
	}
	if len(msgs.inserts) != 1 || msgs.inserts[0].MessageKind != persistence.TaskMessageKindDirective {
		t.Errorf("expected one directive message in audit log; got %+v", msgs.inserts)
	}
}

func TestRouteReply_DirectiveOnCancelled_TerminalRequeue(t *testing.T) {
	trace, _ := runRouteReply(t, persistence.TaskStatusCancelled)
	if !trace.requeueTerminal {
		t.Fatal("CANCELLED must trigger RequeueTerminalTask")
	}
	if trace.requeueAttempt != 1 {
		t.Errorf("attempt must reset to 1; got %d", trace.requeueAttempt)
	}
}

// TestRouteReply_DirectiveOnCompleted_ResetsAttempt — companion bug
// fix: pre-change, COMPLETED used TransitionConditional and the
// attempt counter was preserved. Now it routes through
// RequeueTerminalTask to mirror api/ui behavior.
func TestRouteReply_DirectiveOnCompleted_ResetsAttempt(t *testing.T) {
	trace, _ := runRouteReply(t, persistence.TaskStatusCompleted)
	if !trace.requeueTerminal {
		t.Fatal("COMPLETED must now route through RequeueTerminalTask, not TransitionConditional")
	}
	if trace.transition {
		t.Fatal("COMPLETED must NOT trigger TransitionConditional anymore")
	}
	if trace.requeueAttempt != 1 {
		t.Errorf("attempt must reset to 1; got %d", trace.requeueAttempt)
	}
}

func TestRouteReply_DirectiveOnAwaitingInput_ConditionalRequeue(t *testing.T) {
	trace, _ := runRouteReply(t, persistence.TaskStatusAwaitingInput)
	if !trace.transition {
		t.Fatal("AWAITING_INPUT must trigger TransitionConditional")
	}
	if trace.requeueTerminal {
		t.Fatal("AWAITING_INPUT must NOT trigger RequeueTerminalTask — counter must not reset")
	}
}

// TestRouteReply_DirectiveOnClosed_NotRequeued asserts CLOSED stays
// terminal — archival is one-way per LLD §7.0; operator must hit
// /retry to revive.
func TestRouteReply_DirectiveOnClosed_NotRequeued(t *testing.T) {
	trace, msgs := runRouteReply(t, persistence.TaskStatusClosed)
	if trace.requeueTerminal || trace.transition {
		t.Fatalf("CLOSED must NOT trigger any re-queue path (terminal=%v transition=%v)",
			trace.requeueTerminal, trace.transition)
	}
	if len(msgs.inserts) != 1 {
		t.Errorf("the directive message itself still gets written for audit; got %d", len(msgs.inserts))
	}
}
