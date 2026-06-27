package webchat

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
)

// TestChannel_Name — stable channel identifier surfaces in
// logs / metrics. Tied to the exported constant so dependent
// tests don't break on a copy edit.
func TestChannel_Name(t *testing.T) {
	c := New("p1", conversation.Speaker{ID: "u1"})
	assert.Equal(t, ChannelName, c.Name())
	assert.Equal(t, "web-chat", c.Name())
}

// TestChannel_SendCapturesText — Send appends to the internal
// Sent buffer; reading Sent back returns a copy (not the
// underlying slice), so a follow-up Send doesn't mutate a
// snapshot the test already read.
func TestChannel_SendCapturesText(t *testing.T) {
	c := New("p1", conversation.Speaker{ID: "u1"})
	id, err := c.Send(context.Background(), conversation.ChannelMessage{Text: "hello"})
	assert.NoError(t, err)
	assert.NotEmpty(t, id, "Send must return a non-empty id for non-empty text")

	snapshot := c.Sent()
	assert.Equal(t, []string{"hello"}, snapshot)

	_, _ = c.Send(context.Background(), conversation.ChannelMessage{Text: "second"})
	// Original snapshot stays the same.
	assert.Equal(t, []string{"hello"}, snapshot)
	// Fresh read sees both.
	assert.Equal(t, []string{"hello", "second"}, c.Sent())
}

// TestChannel_SendEmptyDropped — empty Send is a no-op. The
// receiver's sendReply guard short-circuits on empty text, but
// defence-in-depth: the channel itself drops empty payloads too.
func TestChannel_SendEmptyDropped(t *testing.T) {
	c := New("p1", conversation.Speaker{})
	_, err := c.Send(context.Background(), conversation.ChannelMessage{Text: ""})
	assert.NoError(t, err)
	assert.Empty(t, c.Sent())
}

// TestChannel_LifecycleNoops — Start/Stop are no-ops; the
// channel has no upstream to manage. Receiver argument is
// ignored.
func TestChannel_LifecycleNoops(t *testing.T) {
	c := New("p1", conversation.Speaker{})
	assert.NoError(t, c.Start(context.Background(), nil))
	assert.NoError(t, c.Stop())
	sessions, err := c.ListSessions(context.Background())
	assert.NoError(t, err)
	assert.Nil(t, sessions)
}

// TestChannel_ResolveSpeakerEchoesConstructorSpeaker — when the
// Channel was constructed with a non-empty Speaker, ResolveSpeaker
// returns it verbatim regardless of the channel-side id.
func TestChannel_ResolveSpeakerEchoesConstructorSpeaker(t *testing.T) {
	known := conversation.Speaker{ID: "web-chat:u1", DisplayName: "Alice"}
	c := New("p1", known)
	got, err := c.ResolveSpeaker(context.Background(), "anything")
	assert.NoError(t, err)
	assert.Equal(t, known, got)
}

// TestChannel_ResolveSpeakerAnonymous — an empty constructor
// Speaker yields a synthetic anonymous identity. The dispatcher
// still runs (ACL is project-scoped, not speaker-scoped, on this
// channel).
func TestChannel_ResolveSpeakerAnonymous(t *testing.T) {
	c := New("p1", conversation.Speaker{})
	got, err := c.ResolveSpeaker(context.Background(), "sess-42")
	assert.NoError(t, err)
	assert.Equal(t, "web-chat:sess-42", got.ID)
	assert.NotEmpty(t, got.DisplayName)
}

// TestSessionStore_LoadEmptyHistory — fresh store yields an
// empty-history Session for a previously-unseen SessionID. The
// dispatcher gets a clean slate.
func TestSessionStore_LoadEmptyHistory(t *testing.T) {
	store := NewSessionStore(nil, "p1")
	sess, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "browser-1"})
	assert.NoError(t, err)
	assert.Empty(t, sess.History)
	assert.Equal(t, "p1", sess.ActiveProject)
}

// TestSessionStore_AppendThenLoadRoundtrips — Append stores the
// dispatcher's Messages; Load reads a copy back. Two SessionIDs
// stay isolated so a chatty session doesn't leak history into
// another browser.
func TestSessionStore_AppendThenLoadRoundtrips(t *testing.T) {
	store := NewSessionStore(nil, "p1")
	messages := []chat.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	err := store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "browser-1"},
		dispatcher.Result{Messages: messages})
	assert.NoError(t, err)

	sess, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "browser-1"})
	assert.NoError(t, err)
	assert.Equal(t, messages, sess.History)

	// Different session id sees an empty history.
	other, _ := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "browser-2"})
	assert.Empty(t, other.History)
}

// TestSessionStore_AppendEmptyDoesNotWipe — an Append with an
// empty Messages slice on an error path must NOT clobber the
// existing history. Mirrors the GitHub channel's defensive guard.
func TestSessionStore_AppendEmptyDoesNotWipe(t *testing.T) {
	store := NewSessionStore(nil, "p1")
	original := []chat.Message{{Role: "user", Content: "saved"}}
	_ = store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "b"},
		dispatcher.Result{Messages: original})

	_ = store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "b"},
		dispatcher.Result{Messages: nil})

	got := store.History("b")
	assert.Equal(t, original, got, "empty Append must not clobber existing history")
}

// TestSessionStore_HistoryCapTrims — when the cap is set, the
// oldest turns are discarded keeping the most recent. Guards
// against process-memory bloat for chatty sessions.
func TestSessionStore_HistoryCapTrims(t *testing.T) {
	store := NewSessionStore(nil, "p1")
	store.HistoryCap = 2

	messages := []chat.Message{
		{Role: "user", Content: "1"},
		{Role: "assistant", Content: "1r"},
		{Role: "user", Content: "2"},
		{Role: "assistant", Content: "2r"},
	}
	_ = store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "b"},
		dispatcher.Result{Messages: messages})

	got := store.History("b")
	assert.Len(t, got, 2)
	// Oldest two dropped; the most recent two survive.
	assert.Equal(t, "2", got[0].Content)
	assert.Equal(t, "2r", got[1].Content)
}

// TestSessionStore_Reset — clears history for one session
// without touching others.
func TestSessionStore_Reset(t *testing.T) {
	store := NewSessionStore(nil, "p1")
	_ = store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "a"},
		dispatcher.Result{Messages: []chat.Message{{Content: "x"}}})
	_ = store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "b"},
		dispatcher.Result{Messages: []chat.Message{{Content: "y"}}})

	store.Reset("a")
	assert.Empty(t, store.History("a"))
	assert.NotEmpty(t, store.History("b"))
}

// TestWebChatSentID_Format — the synthetic sent-id is a stable
// decimal string of the slice index, suitable for log
// correlation. Edge cases: 0, single-digit, double-digit.
func TestWebChatSentID_Format(t *testing.T) {
	assert.Equal(t, "0", webChatSentID(0))
	assert.Equal(t, "5", webChatSentID(5))
	assert.Equal(t, "10", webChatSentID(10))
	assert.Equal(t, "123", webChatSentID(123))
	assert.Equal(t, "", webChatSentID(-1))
}
