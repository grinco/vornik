package telegram

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestHandleTasksCmd_ListsRecent — /tasks renders recent tasks with a
// per-task /status hint.
func TestHandleTasksCmd_ListsRecent(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "task-1", ProjectID: "p", Status: persistence.TaskStatusRunning},
				{ID: "task-2", ProjectID: "p", Status: persistence.TaskStatusFailed},
			}, nil
		},
	}
	bot := makeBotWithTaskRepo(t, repo)
	out := bot.handleTasksCmd(context.Background(), 0, []string{"/tasks"})
	for _, want := range []string{"task-1", "task-2", "RUNNING", "FAILED", "/status task-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("/tasks output missing %q:\n%s", want, out)
		}
	}
}

// TestHandleStatusCmd_ShowsStatusAndStep — /status composes the task row with
// its execution's current step.
func TestHandleStatusCmd_ShowsStatusAndStep(t *testing.T) {
	step := "summarise"
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "p", Status: persistence.TaskStatusRunning}, nil
		},
	}
	bot := makeBotWithTaskRepo(t, repo)
	bot.execRepo = &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return &persistence.Execution{ID: "e1", CurrentStepID: &step}, nil
		},
	}
	out := bot.handleStatusCmd(context.Background(), 0, []string{"/status", "task-9"})
	for _, want := range []string{"task-9", "RUNNING", "summarise"} {
		if !strings.Contains(out, want) {
			t.Errorf("/status output missing %q:\n%s", want, out)
		}
	}
}

func TestHandleStatusCmd_MissingID(t *testing.T) {
	bot := makeBotWithTaskRepo(t, &mocks.MockTaskRepository{})
	if out := bot.handleStatusCmd(context.Background(), 0, []string{"/status"}); !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage message; got %q", out)
	}
}

// TestHandleCancelCmd — success flips + wakes; a non-cancellable task reports
// its live status.
func TestHandleCancelCmd(t *testing.T) {
	getRunning := func(_ context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, ProjectID: "p", Status: persistence.TaskStatusRunning}, nil
	}
	t.Run("cancels", func(t *testing.T) {
		repo := &mocks.MockTaskRepository{
			GetFunc:                   getRunning,
			TransitionToCancelledFunc: func(_ context.Context, _ string) (bool, error) { return true, nil },
		}
		bot := makeBotWithTaskRepo(t, repo)
		if out := bot.handleCancelCmd(context.Background(), 0, []string{"/cancel", "task-1"}); !strings.Contains(out, "Cancelled") {
			t.Errorf("expected cancelled confirmation; got %q", out)
		}
	})
	t.Run("not cancellable", func(t *testing.T) {
		repo := &mocks.MockTaskRepository{
			GetFunc:                   getRunning,
			TransitionToCancelledFunc: func(_ context.Context, _ string) (bool, error) { return false, nil },
		}
		bot := makeBotWithTaskRepo(t, repo)
		if out := bot.handleCancelCmd(context.Background(), 0, []string{"/cancel", "task-1"}); !strings.Contains(out, "not in a cancellable state") {
			t.Errorf("expected not-cancellable message; got %q", out)
		}
	})
}

// TestHandleRetryCmd — success re-queues; a non-terminal task is refused.
func TestHandleRetryCmd(t *testing.T) {
	getFailed := func(_ context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, ProjectID: "p", Status: persistence.TaskStatusFailed, MaxAttempts: 3}, nil
	}
	t.Run("requeues", func(t *testing.T) {
		repo := &mocks.MockTaskRepository{
			GetFunc:                 getFailed,
			RequeueTerminalTaskFunc: func(_ context.Context, _ string, _, _ int) (bool, error) { return true, nil },
		}
		bot := makeBotWithTaskRepo(t, repo)
		if out := bot.handleRetryCmd(context.Background(), 0, []string{"/retry", "task-1"}); !strings.Contains(out, "Re-queued") {
			t.Errorf("expected re-queued confirmation; got %q", out)
		}
	})
	t.Run("not retriable", func(t *testing.T) {
		repo := &mocks.MockTaskRepository{
			GetFunc:                 getFailed,
			RequeueTerminalTaskFunc: func(_ context.Context, _ string, _, _ int) (bool, error) { return false, nil },
		}
		bot := makeBotWithTaskRepo(t, repo)
		if out := bot.handleRetryCmd(context.Background(), 0, []string{"/retry", "task-1"}); !strings.Contains(out, "can't be retried") {
			t.Errorf("expected not-retriable message; got %q", out)
		}
	})
}

// TestTaskControl_ScopeDenied — a scoped operator can't act on a task in a
// project they lack access to; the response is indistinguishable from
// not-found (no existence leak).
func TestTaskControl_ScopeDenied(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj-b", Status: persistence.TaskStatusRunning}, nil
		},
		TransitionToCancelledFunc: func(_ context.Context, _ string) (bool, error) {
			t.Fatal("cancel must not be attempted for an out-of-scope task")
			return false, nil
		},
	}
	chatClient := chat.NewClient("http://nope.invalid", "k", "m")
	bot, err := NewBot(BotConfig{Token: "x", AllowedUsers: map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"proj-a"}}, // NOT proj-b
	}}, chatClient)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.taskRepo = repo

	for _, cmd := range [][]string{{"/status", "task-b"}, {"/cancel", "task-b"}, {"/retry", "task-b"}} {
		var out string
		switch cmd[0] {
		case "/status":
			out = bot.handleStatusCmd(context.Background(), 111, cmd)
		case "/cancel":
			out = bot.handleCancelCmd(context.Background(), 111, cmd)
		case "/retry":
			out = bot.handleRetryCmd(context.Background(), 111, cmd)
		}
		if !strings.Contains(out, "not found") {
			t.Errorf("%s on out-of-scope task should read not-found; got %q", cmd[0], out)
		}
	}
}
