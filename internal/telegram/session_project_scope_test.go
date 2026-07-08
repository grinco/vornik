package telegram

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
)

// TestSessionStore_FollowupPinsToTaskProject is the 2026-07-08 fix for the
// "T-3641 COMPLETED but the inline text is wildly different from the report"
// report: a task-completion followup must run under the COMPLETED TASK's
// project (carried via ChannelSpecific["project_override"]), not whatever the
// operator switched the chat to after scheduling it.
func TestSessionStore_FollowupPinsToTaskProject(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	// Operator scheduled a task in project A, then switched the chat to B.
	bot.setActiveProject(100, "project-b")
	// The task's project (A) lane holds the originating context.
	bot.getConversation(100, "project-a").AddMessage(chat.Message{Role: "user", Content: "the A question"})

	store := NewSessionStore(bot, nil)

	// A NORMAL turn runs under the chat's current active project (B).
	normal, err := store.Load(context.Background(), conversation.ChannelMessage{
		Source: "telegram", SessionID: "100", SpeakerID: "42",
	})
	if err != nil {
		t.Fatalf("load normal: %v", err)
	}
	if normal.ActiveProject != "project-b" {
		t.Errorf("normal turn must run under active project B, got %q", normal.ActiveProject)
	}

	// The FOLLOWUP pins to the task's project (A) and sees A's lane history —
	// not B's persona/history.
	fu, err := store.Load(context.Background(), conversation.ChannelMessage{
		Source: "telegram", SessionID: "100", SpeakerID: "42",
		ChannelSpecific: map[string]string{"project_override": "project-a"},
	})
	if err != nil {
		t.Fatalf("load followup: %v", err)
	}
	if fu.ActiveProject != "project-a" {
		t.Errorf("followup must run under the task's project A, got %q", fu.ActiveProject)
	}
	if len(fu.History) != 1 || fu.History[0].Content != "the A question" {
		t.Errorf("followup must see project A's lane history, got %+v", fu.History)
	}
}

// TestBot_PerProjectLanesAreIsolated: a chat keeps a SEPARATE history lane per
// project, so switching projects mid-chat neither mixes nor destroys the other
// project's context.
func TestBot_PerProjectLanesAreIsolated(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	bot.getConversation(100, "project-a").AddMessage(chat.Message{Role: "user", Content: "in-A"})
	bot.getConversation(100, "project-b").AddMessage(chat.Message{Role: "user", Content: "in-B"})

	a := bot.getConversation(100, "project-a").GetMessages()
	b := bot.getConversation(100, "project-b").GetMessages()
	if len(a) != 1 || a[0].Content != "in-A" {
		t.Errorf("lane A polluted by lane B: %+v", a)
	}
	if len(b) != 1 || b[0].Content != "in-B" {
		t.Errorf("lane B polluted by lane A: %+v", b)
	}
	// The no-project lane is its own third lane.
	if got := bot.getConversation(100, "").GetMessages(); len(got) != 0 {
		t.Errorf("no-project lane should be empty, got %+v", got)
	}
}
