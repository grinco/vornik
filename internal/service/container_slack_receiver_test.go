package service

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
)

// TestSlackSessionStore_LoadEmptyHistory — a fresh store returns an
// empty history snapshot with the configured ActiveProject pin.
func TestSlackSessionStore_LoadEmptyHistory(t *testing.T) {
	store := newSlackSessionStore(nil, "project-x")
	sess, err := store.Load(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Source:    "slack",
		SpeakerID: "U_alice",
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

// TestSlackSessionStore_LoadEmptyProjectIDIsTolerated — degenerate
// test wiring may pass an empty projectID; Load must not panic and
// must return an empty ActiveProject.
func TestSlackSessionStore_LoadEmptyProjectIDIsTolerated(t *testing.T) {
	store := newSlackSessionStore(nil, "")
	sess, err := store.Load(context.Background(), conversation.ChannelMessage{
		SessionID: "T1/C1#ts",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if sess.ActiveProject != "" {
		t.Errorf("ActiveProject = %q, want empty", sess.ActiveProject)
	}
}

// TestSlackSessionStore_AppendReplacesHistory — Append replaces the
// stored history with the dispatcher's post-turn slice. Mirrors the
// emailSessionStore contract.
func TestSlackSessionStore_AppendReplacesHistory(t *testing.T) {
	store := newSlackSessionStore(nil, "project-x")
	msg := conversation.ChannelMessage{SessionID: "T1/C1#10.0"}
	result := dispatcher.Result{
		Messages: []chat.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		},
	}
	if err := store.Append(context.Background(), msg, result); err != nil {
		t.Fatalf("Append: %v", err)
	}
	snap := store.snapshotHistory("T1/C1#10.0")
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	if snap[0].Content != "hi" || snap[1].Content != "hello" {
		t.Errorf("snapshot content wrong: %+v", snap)
	}

	// Subsequent Append replaces — doesn't accumulate.
	result2 := dispatcher.Result{
		Messages: []chat.Message{{Role: "system", Content: "fresh"}},
	}
	if err := store.Append(context.Background(), msg, result2); err != nil {
		t.Fatalf("Append second: %v", err)
	}
	snap = store.snapshotHistory("T1/C1#10.0")
	if len(snap) != 1 || snap[0].Content != "fresh" {
		t.Errorf("after replace snap = %+v, want single 'fresh'", snap)
	}
}

// TestSlackSessionStore_AppendEmptySliceLeavesHistory — defensive
// branch from the design doc: an empty post-turn slice (dispatcher
// errored before producing anything) must NOT wipe state.
func TestSlackSessionStore_AppendEmptySliceLeavesHistory(t *testing.T) {
	store := newSlackSessionStore(nil, "p")
	msg := conversation.ChannelMessage{SessionID: "T/C#1"}
	// Seed history.
	_ = store.Append(context.Background(), msg, dispatcher.Result{
		Messages: []chat.Message{{Role: "user", Content: "before"}},
	})
	// Append with empty messages — should skip, not wipe.
	if err := store.Append(context.Background(), msg, dispatcher.Result{}); err != nil {
		t.Fatalf("Append empty: %v", err)
	}
	snap := store.snapshotHistory("T/C#1")
	if len(snap) != 1 || snap[0].Content != "before" {
		t.Errorf("after empty append snap = %+v, want preserved 'before'", snap)
	}
}

// TestSlackSessionStore_IsolationBetweenSessions — two distinct
// SessionIDs don't bleed into each other.
func TestSlackSessionStore_IsolationBetweenSessions(t *testing.T) {
	store := newSlackSessionStore(nil, "p")
	_ = store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "T/C#a"},
		dispatcher.Result{Messages: []chat.Message{{Role: "user", Content: "in a"}}},
	)
	_ = store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "T/C#b"},
		dispatcher.Result{Messages: []chat.Message{{Role: "user", Content: "in b"}}},
	)
	if len(store.snapshotHistory("T/C#a")) != 1 {
		t.Error("session a history lost")
	}
	if len(store.snapshotHistory("T/C#b")) != 1 {
		t.Error("session b history lost")
	}
	if len(store.snapshotHistory("T/C#c")) != 0 {
		t.Error("session c should be empty")
	}
}

// TestSlackSessionStore_LoadCopiesHistory — the slice returned by
// Load must be a copy so callers mutating it don't corrupt the
// store. Same invariant the GitHub + email session stores maintain.
func TestSlackSessionStore_LoadCopiesHistory(t *testing.T) {
	store := newSlackSessionStore(nil, "p")
	msg := conversation.ChannelMessage{SessionID: "T/C#1"}
	_ = store.Append(context.Background(), msg, dispatcher.Result{
		Messages: []chat.Message{{Role: "user", Content: "original"}},
	})
	sess, _ := store.Load(context.Background(), msg)
	if len(sess.History) != 1 {
		t.Fatalf("History len = %d, want 1", len(sess.History))
	}
	sess.History[0].Content = "MUTATED"
	snap := store.snapshotHistory("T/C#1")
	if snap[0].Content != "original" {
		t.Errorf("store leaked mutation: %q (want %q)", snap[0].Content, "original")
	}
}
