// Package telegram: tests for renderInbox — the /inbox command's
// formatter for tasks awaiting operator input.
package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func makeBotWithTaskRepo(t *testing.T, repo persistence.TaskRepository) *Bot {
	t.Helper()
	chatClient := chat.NewClient("http://nope.invalid", "k", "m")
	bot, err := NewBot(BotConfig{Token: "x"}, chatClient)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.taskRepo = repo
	return bot
}

func TestRenderInbox_NoRepo(t *testing.T) {
	bot := makeBotWithTaskRepo(t, nil)
	got, err := bot.renderInbox(context.Background(), 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(got, "Inbox unavailable") {
		t.Errorf("expected unavailable message; got %q", got)
	}
}

func TestRenderInbox_ListError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	bot := makeBotWithTaskRepo(t, repo)
	if _, err := bot.renderInbox(context.Background(), 0); err == nil {
		t.Errorf("expected error on list failure")
	}
}

func TestRenderInbox_Empty(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	bot := makeBotWithTaskRepo(t, repo)
	got, err := bot.renderInbox(context.Background(), 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(got, "Inbox is empty") {
		t.Errorf("expected empty message; got %q", got)
	}
}

func TestRenderInbox_WithTasks(t *testing.T) {
	phase := "planning"
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			if f.Status == nil || *f.Status != persistence.TaskStatusAwaitingInput {
				t.Errorf("filter status: got %v, want AWAITING_INPUT", f.Status)
			}
			if f.PageSize != 25 {
				t.Errorf("page size: got %d, want 25", f.PageSize)
			}
			return []*persistence.Task{
				{
					ID:           "task-a",
					Payload:      []byte(`{"context":{"prompt":"draft an email"}}`),
					CurrentPhase: &phase,
				},
				{
					ID:      "task-b",
					Payload: nil, // exercises the title-fallback branch
				},
			}, nil
		},
	}
	bot := makeBotWithTaskRepo(t, repo)
	got, err := bot.renderInbox(context.Background(), 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(got, "2 task(s) awaiting") {
		t.Errorf("expected count header; got %q", got)
	}
	if !strings.Contains(got, "draft an email") {
		t.Errorf("expected payload prompt as title; got %q", got)
	}
	if !strings.Contains(got, "task-b") {
		t.Errorf("expected fallback to task id; got %q", got)
	}
	if !strings.Contains(got, "planning") {
		t.Errorf("expected current phase; got %q", got)
	}
}
