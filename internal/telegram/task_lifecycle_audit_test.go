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

// Regression coverage for the project-authorization gap in
// routeReplyToTask: a reply (via reply-id OR a forum topic) must not
// inject an operator directive/answer into a task whose project the
// replying user is not scoped for. Pre-fix, the per-project check was
// absent and the task_message row was written regardless of scope.

// auditFakeMsgRepo records inserts so the test can assert that a denied
// reply writes NOTHING to task_messages.
type auditFakeMsgRepo struct {
	inserts []*persistence.TaskMessage
}

func (f *auditFakeMsgRepo) Insert(_ context.Context, m *persistence.TaskMessage) error {
	m.ID = "audit-tmsg"
	f.inserts = append(f.inserts, m)
	return nil
}
func (f *auditFakeMsgRepo) List(context.Context, persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return nil, nil
}
func (f *auditFakeMsgRepo) GetOpenCheckpoint(context.Context, string) (*persistence.TaskMessage, error) {
	return nil, nil
}
func (f *auditFakeMsgRepo) MarkCheckpointResolved(context.Context, string, string) error {
	return nil
}

// auditTelegramServer returns a stub /sendMessage + /sendForumMessage
// server that always succeeds; the test only cares about whether a
// task_message row was written, not the ack body.
func auditTelegramServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// scopedTaskRepo loads a task whose project the denied user is NOT
// allowed for. A transition/requeue call here would be a bug — the
// authz gate must short-circuit before any write.
func auditScopedTaskRepo(trace *auditCallTrace) *mocks.MockTaskRepository {
	return &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:          id,
				ProjectID:   "ibkr-trader", // sensitive live project
				Status:      persistence.TaskStatusAwaitingInput,
				Priority:    50,
				Attempt:     1,
				MaxAttempts: 6,
			}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			trace.transitioned = true
			return true, nil
		},
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, _, _ int) (bool, error) {
			trace.requeued = true
			return true, nil
		},
	}
}

type auditCallTrace struct {
	transitioned bool
	requeued     bool
}

// TestRouteReply_ScopedUserDenied_NotifTrackerPath: a user scoped only
// to "snake" replies (reply-id path) to a notification for an
// "ibkr-trader" task. The reply must be denied — no task_message row,
// no transition — and the function must still claim the message
// (handled=true) so it does not fall through to the dispatcher.
func TestRouteReply_ScopedUserDenied_NotifTrackerPath(t *testing.T) {
	ts := auditTelegramServer(t)
	trace := &auditCallTrace{}
	msgRepo := &auditFakeMsgRepo{}
	chatClient := chat.NewClient("x", "k", "m")

	bot, err := NewBot(BotConfig{
		Token: "test",
		AllowedUsers: map[int64]UserAccess{
			777: {Allowed: true, Projects: []string{"snake"}},
		},
	}, chatClient,
		WithTaskRepository(auditScopedTaskRepo(trace)),
		WithTaskMessageRepository(msgRepo),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = ts.URL

	handled, err := bot.routeReplyToTask(context.Background(),
		&Message{ChatID: 42, UserID: 777, Username: "scoped", Text: "cancel that trade"},
		"task-ibkr-1", "ibkr-trader")
	if err != nil {
		t.Fatalf("routeReplyToTask: %v", err)
	}
	if !handled {
		t.Fatal("denied reply must be claimed (handled=true) so it does not fall through to the dispatcher")
	}
	if len(msgRepo.inserts) != 0 {
		t.Fatalf("denied reply must NOT write a task_message row; got %d inserts", len(msgRepo.inserts))
	}
	if trace.transitioned || trace.requeued {
		t.Fatalf("denied reply must NOT transition/requeue the task (transition=%v requeue=%v)", trace.transitioned, trace.requeued)
	}
}

// TestRouteReply_ScopedUserDenied_ForumPath drives the forum surface
// via routeForumReplyIfApplicable. Note the forum path passes the
// thread-row UUID as the projectID arg, NOT a project id — so the gate
// must authorize against the loaded task.ProjectID, which this test
// confirms by denying a "snake"-scoped user on an "ibkr-trader" task.
func TestRouteReply_ScopedUserDenied_ForumPath(t *testing.T) {
	ts := auditTelegramServer(t)
	trace := &auditCallTrace{}
	msgRepo := &auditFakeMsgRepo{}
	chatClient := chat.NewClient("x", "k", "m")

	threadRepo := newStubThreadRepo()
	if err := threadRepo.Insert(context.Background(), &persistence.TelegramTaskThread{
		TaskID: "task-ibkr-1", ChatID: -1001, ThreadID: 88, TopicName: "ibkr task",
	}); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	bot, err := NewBot(BotConfig{
		Token: "test",
		AllowedUsers: map[int64]UserAccess{
			777: {Allowed: true, Projects: []string{"snake"}},
		},
	}, chatClient,
		WithTaskRepository(auditScopedTaskRepo(trace)),
		WithTaskMessageRepository(msgRepo),
		WithForumChatID(-1001, 0),
		WithTelegramThreadRepository(threadRepo),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = ts.URL

	handled, err := bot.routeForumReplyIfApplicable(context.Background(),
		&Message{ChatID: -1001, UserID: 777, Username: "scoped", MessageThreadID: 88, Text: "force-close everything"})
	if err != nil {
		t.Fatalf("routeForumReplyIfApplicable: %v", err)
	}
	if !handled {
		t.Fatal("denied forum reply must be claimed (handled=true)")
	}
	if len(msgRepo.inserts) != 0 {
		t.Fatalf("denied forum reply must NOT write a task_message row; got %d inserts", len(msgRepo.inserts))
	}
	if trace.transitioned || trace.requeued {
		t.Fatalf("denied forum reply must NOT transition/requeue the task (transition=%v requeue=%v)", trace.transitioned, trace.requeued)
	}
}

// TestRouteReply_AuthorizedUser_NoRegression confirms the gate does not
// break the happy path: a user scoped to the task's project still has
// the reply recorded and the task re-queued.
func TestRouteReply_AuthorizedUser_NoRegression(t *testing.T) {
	ts := auditTelegramServer(t)
	trace := &auditCallTrace{}
	msgRepo := &auditFakeMsgRepo{}
	chatClient := chat.NewClient("x", "k", "m")

	bot, err := NewBot(BotConfig{
		Token: "test",
		AllowedUsers: map[int64]UserAccess{
			777: {Allowed: true, Projects: []string{"ibkr-trader"}},
		},
	}, chatClient,
		WithTaskRepository(auditScopedTaskRepo(trace)),
		WithTaskMessageRepository(msgRepo),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = ts.URL

	handled, err := bot.routeReplyToTask(context.Background(),
		&Message{ChatID: 42, UserID: 777, Username: "owner", Text: "proceed"},
		"task-ibkr-1", "ibkr-trader")
	if err != nil {
		t.Fatalf("routeReplyToTask: %v", err)
	}
	if !handled {
		t.Fatal("authorized reply must be handled=true")
	}
	if len(msgRepo.inserts) != 1 {
		t.Fatalf("authorized reply must write exactly one task_message; got %d", len(msgRepo.inserts))
	}
	if !trace.transitioned {
		t.Fatal("authorized reply against AWAITING_INPUT must transition (re-queue) the task")
	}
}
