package service

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
)

func TestEmailSessionStore_LoadEmptyHistory(t *testing.T) {
	store := newEmailSessionStore(nil, "project-x", nil)
	sess, err := store.Load(context.Background(), conversation.ChannelMessage{
		SessionID: "thread-1",
		Source:    "email",
		SpeakerID: "a@x.com",
		Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(sess.History) != 0 {
		t.Errorf("expected empty history, got %d messages", len(sess.History))
	}
	if sess.ActiveProject != "project-x" {
		t.Errorf("ActiveProject = %q, want project-x", sess.ActiveProject)
	}
}

func TestEmailSessionStore_LoadEmptyProjectIDIsTolerated(t *testing.T) {
	store := newEmailSessionStore(nil, "", nil)
	sess, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "t1"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if sess.ActiveProject != "" {
		t.Errorf("ActiveProject = %q, want empty", sess.ActiveProject)
	}
}

func TestEmailSessionStore_AppendReplacesHistory(t *testing.T) {
	store := newEmailSessionStore(nil, "project-x", nil)
	msg := conversation.ChannelMessage{SessionID: "thread-A"}
	result := dispatcher.Result{
		Messages: []chat.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		},
	}
	if err := store.Append(context.Background(), msg, result); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got := store.snapshotHistory("thread-A")
	if len(got) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(got))
	}
	// A second Append with a longer result must REPLACE, not extend.
	result2 := dispatcher.Result{
		Messages: []chat.Message{
			{Role: "user", Content: "second turn"},
			{Role: "assistant", Content: "second reply"},
			{Role: "user", Content: "third turn"},
			{Role: "assistant", Content: "third reply"},
		},
	}
	if err := store.Append(context.Background(), msg, result2); err != nil {
		t.Fatalf("Append #2: %v", err)
	}
	got2 := store.snapshotHistory("thread-A")
	if len(got2) != 4 {
		t.Errorf("after second Append snapshot len = %d, want 4", len(got2))
	}
}

func TestEmailSessionStore_AppendEmptyResultPreservesHistory(t *testing.T) {
	// Defensive path: dispatcher errors that produce an empty Result
	// must not wipe the in-memory history.
	store := newEmailSessionStore(nil, "project-x", nil)
	msg := conversation.ChannelMessage{SessionID: "thread-B"}
	_ = store.Append(context.Background(), msg, dispatcher.Result{
		Messages: []chat.Message{{Role: "user", Content: "kept"}},
	})
	_ = store.Append(context.Background(), msg, dispatcher.Result{Messages: nil})
	got := store.snapshotHistory("thread-B")
	if len(got) != 1 {
		t.Errorf("snapshot len = %d, want 1 (empty Result must not clear)", len(got))
	}
}

func TestEmailSessionStore_IsolationBetweenSessions(t *testing.T) {
	store := newEmailSessionStore(nil, "project-x", nil)
	_ = store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "thread-1"},
		dispatcher.Result{Messages: []chat.Message{{Role: "user", Content: "in 1"}}},
	)
	_ = store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "thread-2"},
		dispatcher.Result{Messages: []chat.Message{{Role: "user", Content: "in 2"}}},
	)
	if len(store.snapshotHistory("thread-1")) != 1 {
		t.Error("thread-1 history lost")
	}
	if len(store.snapshotHistory("thread-2")) != 1 {
		t.Error("thread-2 history lost")
	}
	if len(store.snapshotHistory("thread-3")) != 0 {
		t.Error("thread-3 should have empty history")
	}
}

func TestEmailSessionStore_LoadReturnsCopyNotAlias(t *testing.T) {
	// Mutating the returned slice must not affect future Loads.
	store := newEmailSessionStore(nil, "project-x", nil)
	msg := conversation.ChannelMessage{SessionID: "thread-X"}
	_ = store.Append(context.Background(), msg, dispatcher.Result{
		Messages: []chat.Message{{Role: "user", Content: "original"}},
	})
	sess1, _ := store.Load(context.Background(), msg)
	if len(sess1.History) == 0 {
		t.Fatal("Load returned empty history")
	}
	sess1.History[0].Content = "TAMPERED"

	sess2, _ := store.Load(context.Background(), msg)
	if sess2.History[0].Content != "original" {
		t.Errorf("Load returned alias, not copy — history mutated to %q", sess2.History[0].Content)
	}
}
