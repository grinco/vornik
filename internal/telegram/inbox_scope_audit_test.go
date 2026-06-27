package telegram

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestRenderInbox_ScopedUserOnlySeesOwnProjects guards the fix that /inbox
// post-filters AWAITING_INPUT tasks against the requesting user's allowed
// project set. Pre-fix it listed every project's awaiting tasks to any
// caller. A wildcard / unrestricted user still sees everything.
func TestRenderInbox_ScopedUserOnlySeesOwnProjects(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "task-in-a", ProjectID: "proj-a"},
				{ID: "task-in-b", ProjectID: "proj-b"},
				{ID: "task-in-a2", ProjectID: "proj-a"},
			}, nil
		},
	}
	chatClient := chat.NewClient("http://nope.invalid", "k", "m")
	bot, err := NewBot(BotConfig{Token: "x", AllowedUsers: map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"proj-a"}}, // scoped to proj-a
		222: {Allowed: true, Projects: []string{"*"}},      // wildcard
	}}, chatClient)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.taskRepo = repo

	// Scoped user 111 must see proj-a tasks but NOT proj-b's.
	scoped, err := bot.renderInbox(context.Background(), 111)
	if err != nil {
		t.Fatalf("renderInbox: %v", err)
	}
	if strings.Contains(scoped, "task-in-b") {
		t.Errorf("scoped user saw a foreign project's awaiting task:\n%s", scoped)
	}
	if !strings.Contains(scoped, "task-in-a") {
		t.Errorf("scoped user should see own project's awaiting task:\n%s", scoped)
	}

	// Wildcard user 222 sees everything (no restriction).
	wild, err := bot.renderInbox(context.Background(), 222)
	if err != nil {
		t.Fatalf("renderInbox(wildcard): %v", err)
	}
	if !strings.Contains(wild, "task-in-a") || !strings.Contains(wild, "task-in-b") {
		t.Errorf("wildcard user should see all projects' awaiting tasks:\n%s", wild)
	}
}
