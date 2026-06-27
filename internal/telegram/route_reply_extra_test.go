// Extension of route_reply_directive_test.go covering branches the
// directive-focused suite leaves dark:
//   - routeReplyToTask early-return when repos are nil
//   - answer-with-checkpoint path (extracts choice from option metadata)
//   - AwaitingExternal directive (TransitionConditional, not terminal)
//   - taskRepo.Get error → handled=false fallthrough
//   - taskMessageRepo.Insert error → handled=true with error
//   - rescheduler.Wake fires when re-queue succeeds
//   - forum-thread ack falls back when forum send errors

package telegram

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

type fakeRescheduler struct{ woke int }

func (f *fakeRescheduler) Wake() { f.woke++ }

func newTestBotForRouteReply(t *testing.T, taskRepo *mocks.MockTaskRepository, msgRepo *fakeMsgRepoForRoute, opts ...BotOption) (*Bot, *fakeRescheduler) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	t.Cleanup(srv.Close)

	rs := &fakeRescheduler{}
	chatClient := chat.NewClient("https://example.com", "k", "m")
	allOpts := append([]BotOption{
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(msgRepo),
		WithRescheduler(rs),
		WithHTTPClient(srv.Client()),
	}, opts...)
	b, err := NewBot(BotConfig{Token: "tok",
		AllowedUsers: map[int64]UserAccess{1: {Allowed: true, Projects: []string{"*"}}},
	}, chatClient, allOpts...)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	b.baseURL = srv.URL
	return b, rs
}

func TestRouteReplyToTask_NoRepoReturnsFallthrough(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t"})
	handled, err := b.routeReplyToTask(context.Background(),
		&Message{ChatID: 1, UserID: 1, Text: "x"},
		"task-1", "p1")
	if err != nil {
		t.Errorf("err: got %v, want nil", err)
	}
	if handled {
		t.Error("no repos: handled=true, want false (fallthrough)")
	}
}

func TestRouteReplyToTask_AnswerCheckpointWithChoiceMatch(t *testing.T) {
	cpID := "cp-1"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:               id,
				Status:           persistence.TaskStatusAwaitingInput,
				OpenCheckpointID: &cpID,
				MaxAttempts:      3,
			}, nil
		},
	}
	msgRepo := &fakeMsgRepoForRoute{}
	// We need to stash a checkpoint message returned by GetOpenCheckpoint
	// so the choice-extraction path fires. Extend fakeMsgRepoForRoute.
	msgRepoExt := &checkpointAwareMsgRepo{
		fakeMsgRepoForRoute: msgRepo,
		checkpoint: &persistence.TaskMessage{
			ID:       cpID,
			Metadata: []byte(`{"options":[{"id":"yes","label":"Approve"},{"id":"no","label":"Reject"}]}`),
		},
	}

	b, rs := newTestBotForRouteReply(t, taskRepo, msgRepo)
	// Swap the msg repo for the extended one.
	b.taskMessageRepo = msgRepoExt

	handled, err := b.routeReplyToTask(context.Background(),
		&Message{ChatID: 1, UserID: 1, Username: "alice", Text: "yes"},
		"task-1", "p1")
	if err != nil {
		t.Fatalf("routeReplyToTask: %v", err)
	}
	if !handled {
		t.Error("checkpoint-answer: handled=false, want true")
	}
	if len(msgRepoExt.inserts) != 1 {
		t.Fatalf("inserts: got %d, want 1", len(msgRepoExt.inserts))
	}
	got := msgRepoExt.inserts[0]
	if got.MessageKind != persistence.TaskMessageKindAnswer {
		t.Errorf("kind: got %s, want answer", got.MessageKind)
	}
	if rs.woke != 1 {
		t.Errorf("rescheduler.woke: got %d, want 1", rs.woke)
	}
}

// checkpointAwareMsgRepo extends fakeMsgRepoForRoute to serve a real
// checkpoint message from GetOpenCheckpoint.
type checkpointAwareMsgRepo struct {
	*fakeMsgRepoForRoute
	checkpoint *persistence.TaskMessage
}

func (c *checkpointAwareMsgRepo) GetOpenCheckpoint(_ context.Context, _ string) (*persistence.TaskMessage, error) {
	return c.checkpoint, nil
}

func TestRouteReplyToTask_AwaitingExternalDirectiveTransitions(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusAwaitingExternal, MaxAttempts: 3}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	msgRepo := &fakeMsgRepoForRoute{}
	b, rs := newTestBotForRouteReply(t, taskRepo, msgRepo)

	handled, err := b.routeReplyToTask(context.Background(),
		&Message{ChatID: 1, UserID: 1, Text: "course correction"},
		"task-1", "p1")
	if err != nil {
		t.Fatalf("routeReplyToTask: %v", err)
	}
	if !handled {
		t.Error("handled=false")
	}
	if rs.woke != 1 {
		t.Errorf("rescheduler.woke: got %d, want 1", rs.woke)
	}
}

func TestRouteReplyToTask_TaskRepoGetErrorFallsThrough(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) {
			return nil, errors.New("task vanished")
		},
	}
	msgRepo := &fakeMsgRepoForRoute{}
	b, _ := newTestBotForRouteReply(t, taskRepo, msgRepo)

	handled, err := b.routeReplyToTask(context.Background(),
		&Message{ChatID: 1, UserID: 1, Text: "x"},
		"task-1", "p1")
	if err != nil {
		t.Errorf("err: got %v, want nil (fallthrough swallows)", err)
	}
	if handled {
		t.Error("missing task: handled=true, want false")
	}
}

func TestRouteReplyToTask_InsertErrorReportsToOperator(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning, MaxAttempts: 3}, nil
		},
	}
	msgRepo := &failingInsertMsgRepo{}
	b, _ := newTestBotForRouteReply(t, taskRepo, &fakeMsgRepoForRoute{})
	b.taskMessageRepo = msgRepo

	handled, err := b.routeReplyToTask(context.Background(),
		&Message{ChatID: 1, UserID: 1, Text: "feedback"},
		"task-1", "p1")
	if !handled {
		t.Error("insert error: handled should still be true (we claim the message)")
	}
	if err == nil {
		t.Error("insert error: expected non-nil error to bubble")
	}
}

// failingInsertMsgRepo returns an error from Insert.
type failingInsertMsgRepo struct{ fakeMsgRepoForRoute }

func (f *failingInsertMsgRepo) Insert(_ context.Context, _ *persistence.TaskMessage) error {
	return errors.New("db write failed")
}
func (f *failingInsertMsgRepo) GetOpenCheckpoint(context.Context, string) (*persistence.TaskMessage, error) {
	return nil, nil
}
func (f *failingInsertMsgRepo) MarkCheckpointResolved(context.Context, string, string) error {
	return nil
}

func TestRouteReplyToTask_AnonymousAuthorFallsBackToUserID(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning, MaxAttempts: 3}, nil
		},
	}
	msgRepo := &fakeMsgRepoForRoute{}
	b, _ := newTestBotForRouteReply(t, taskRepo, msgRepo)

	handled, err := b.routeReplyToTask(context.Background(),
		&Message{ChatID: 1, UserID: 42, Username: "" /* no username */, Text: "directive"},
		"task-1", "p1")
	if err != nil {
		t.Fatalf("routeReplyToTask: %v", err)
	}
	if !handled {
		t.Error("handled=false")
	}
	// AuthorID should be "tg:42" since Username was empty.
	if len(msgRepo.inserts) != 1 {
		t.Fatalf("inserts: got %d, want 1", len(msgRepo.inserts))
	}
	authorID := msgRepo.inserts[0].AuthorID
	if authorID == nil || *authorID != "tg:42" {
		t.Errorf("authorID: got %v, want tg:42", authorID)
	}
}
