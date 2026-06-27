package telegram

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// Inbound forum routing (Phase 29). The four cases below match the
// LLD §8 call graph: each path that routeForumReplyIfApplicable
// can take, asserting whether it claims the message or falls
// through to the legacy notifTracker / dispatcher path.

func TestRouteForumReplyIfApplicable_NotEnabled(t *testing.T) {
	chatClient := chat.NewClient("x", "k", "m")
	bot, _ := NewBot(BotConfig{Token: "t"}, chatClient)
	handled, err := bot.routeForumReplyIfApplicable(context.Background(), &Message{
		ChatID: -1, MessageThreadID: 99, Text: "hi",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handled {
		t.Error("routeForumReplyIfApplicable must return handled=false when forum disabled")
	}
}

func TestRouteForumReplyIfApplicable_ThreadIDZero(t *testing.T) {
	chatClient := chat.NewClient("x", "k", "m")
	bot, _ := NewBot(BotConfig{Token: "t"}, chatClient,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(newStubThreadRepo()),
	)
	handled, _ := bot.routeForumReplyIfApplicable(context.Background(), &Message{
		ChatID: -1, MessageThreadID: 0, Text: "hi",
	})
	if handled {
		t.Error("thread_id=0 (main topic) must not be routed as a forum reply")
	}
}

func TestRouteForumReplyIfApplicable_UnknownThreadFallsThrough(t *testing.T) {
	chatClient := chat.NewClient("x", "k", "m")
	repo := newStubThreadRepo()
	bot, _ := NewBot(BotConfig{Token: "t"}, chatClient,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(repo),
	)
	handled, err := bot.routeForumReplyIfApplicable(context.Background(), &Message{
		ChatID: -1, MessageThreadID: 999, Text: "hi",
	})
	if err != nil {
		t.Errorf("ErrNotFound must NOT surface as a routing error: %v", err)
	}
	if handled {
		t.Error("unknown thread must fall through to dispatcher")
	}
}

func TestRouteForumReplyIfApplicable_SlashCommandFallsThrough(t *testing.T) {
	chatClient := chat.NewClient("x", "k", "m")
	repo := newStubThreadRepo()
	_ = repo.Insert(context.Background(), &persistence.TelegramTaskThread{
		TaskID: "t1", ChatID: -1, ThreadID: 5, TopicName: "x",
	})
	bot, _ := NewBot(BotConfig{Token: "t"}, chatClient,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(repo),
	)
	handled, _ := bot.routeForumReplyIfApplicable(context.Background(), &Message{
		ChatID: -1, MessageThreadID: 5, Text: "/help",
	})
	if handled {
		t.Error("slash commands in a forum topic must fall through to the dispatcher, not be recorded as task directives")
	}
}
