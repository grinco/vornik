package service

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/slack"
)

// INCIDENT 2026-07-30 — mention-less follow-ups inside a thread the bot was holding
// were dropped. The channel now asks whether it is already engaged in the thread; this
// is the store side of that question.
//
// "Engaged" means we hold conversation history for that session id. A thread we have
// never spoken in must answer false, or the bot would join conversations between
// colleagues that merely happen in a channel it belongs to.
func TestSlackSessionStore_ThreadEngagedFollowsStoredHistory(t *testing.T) {
	store := newSlackSessionStore(nil, "project-x")
	const engaged = "T123/C_general#1785367141.211839"
	const untouched = "T123/C_general#1785999999.000100"

	if store.ThreadEngaged(context.Background(), engaged) {
		t.Error("a thread with no history must not report as engaged")
	}

	if err := store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: engaged},
		dispatcher.Result{Messages: []chat.Message{
			{Role: "user", Content: "summarise the meeting notes"},
			{Role: "assistant", Content: "Here's the summary…"},
		}},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if !store.ThreadEngaged(context.Background(), engaged) {
		t.Error("a thread we have answered in must report as engaged")
	}
	if store.ThreadEngaged(context.Background(), untouched) {
		t.Error("engagement must be per-session, not per-channel")
	}
}

// ReadThread dereferenced s.persister unconditionally, so any deployment without
// channel-session persistence (SQLite, or an unwired store) crashed the daemon the
// first time the get_channel_thread tool — or now the engagement check — missed the
// in-memory map. A nil persister must read as "nothing stored".
func TestSlackSessionStore_ReadThreadWithoutPersisterDoesNotPanic(t *testing.T) {
	store := newSlackSessionStore(nil, "project-x")
	if store.persister != nil {
		t.Fatal("precondition: this store must have no persister")
	}

	history, err := store.ReadThread(context.Background(), "T123/C_general#nope")
	if err != nil {
		t.Fatalf("ReadThread: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("history = %d messages, want 0", len(history))
	}
	if store.ThreadEngaged(context.Background(), "T123/C_general#nope") {
		t.Error("unknown session with no persister must not report as engaged")
	}
}

// Compile-time proof the store satisfies the channel's contract. Without this the
// wiring in subsystem_slack_channels.go is the only thing holding the two together,
// and a signature drift would only surface at daemon boot.
func TestSlackSessionStore_SatisfiesThreadEngagementChecker(_ *testing.T) {
	var _ slack.ThreadEngagementChecker = newSlackSessionStore(nil, "project-x")
}

// The channel consults the checker on its detached dispatch context, which carries a
// deadline rather than cancellation. A store that ignored ctx would be fine; one that
// blocked forever would hold a turn open. Guard the cheap property: a cancelled context
// still returns promptly rather than hanging.
func TestSlackSessionStore_ThreadEngagedHonoursCancelledContext(t *testing.T) {
	store := newSlackSessionStore(nil, "project-x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan bool, 1)
	go func() { done <- store.ThreadEngaged(ctx, "T123/C_general#1.1") }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ThreadEngaged blocked on a cancelled context")
	}
}
