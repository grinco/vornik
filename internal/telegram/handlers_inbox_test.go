// Package telegram: test for the handleInbox package-level helper.
// handleInbox wraps Bot.renderInbox with logging + a default error
// message. renderInbox itself is exercised in render_inbox_test.go.
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

func TestHandleInbox_NilBot(t *testing.T) {
	got := handleInbox(context.Background(), nil, 1, 0)
	if !strings.Contains(got, "Bot unavailable") {
		t.Errorf("got %q", got)
	}
}

func TestHandleInbox_RenderError(t *testing.T) {
	chatClient := chat.NewClient("http://nope.invalid", "k", "m")
	bot, _ := NewBot(BotConfig{Token: "x"}, chatClient)
	bot.taskRepo = &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	got := handleInbox(context.Background(), bot, 1, 0)
	if !strings.Contains(got, "Failed to load inbox") {
		t.Errorf("got %q", got)
	}
}

func TestHandleInbox_Empty(t *testing.T) {
	chatClient := chat.NewClient("http://nope.invalid", "k", "m")
	bot, _ := NewBot(BotConfig{Token: "x"}, chatClient)
	bot.taskRepo = &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	got := handleInbox(context.Background(), bot, 1, 0)
	if !strings.Contains(got, "Inbox is empty") {
		t.Errorf("got %q", got)
	}
}
