// HandleMessage's non-slash branch routes through the wired
// ConversationChannel Receiver. This file covers that routing in
// isolation — the receiver is a stub that just records calls.
//
// Integration assertions (the receiver actually invokes the
// dispatcher, the reply rounds back through Channel.Send) live in
// the dispatcher.ChannelReceiver tests; this layer only checks
// "HandleMessage forwarded the inbound."

package telegram

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
)

// handleMessageReceiver is a conversation.Receiver test double that
// records every inbound ChannelMessage and signals through a
// channel when Receive is invoked. Lets the test wait for the
// async dispatch goroutine without sleep-and-pray. calls / last
// are guarded because triggerFollowup may invoke Receive from
// several goroutines concurrently (one per coalesced turn).
type handleMessageReceiver struct {
	mu         sync.Mutex
	calls      int
	last       conversation.ChannelMessage
	receiveErr error
	done       chan struct{}
}

func (r *handleMessageReceiver) Receive(_ context.Context, msg conversation.ChannelMessage) error {
	r.mu.Lock()
	r.calls++
	r.last = msg
	err := r.receiveErr
	r.mu.Unlock()
	if r.done != nil {
		select {
		case r.done <- struct{}{}:
		default:
		}
	}
	return err
}

func (r *handleMessageReceiver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// lastMessage returns the most recent ChannelMessage under the guard.
// Tests where a second delivery could still be in flight must read
// through this instead of touching r.last directly — the unguarded
// read raced Receive's write on CI (2026-06-04).
func (r *handleMessageReceiver) lastMessage() conversation.ChannelMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// waitReceiver blocks until the receiver fires or the timeout
// expires. Returns true on receipt, false on timeout — the
// caller decides whether absence is a failure.
func (r *handleMessageReceiver) waitReceive(t *testing.T, timeout time.Duration) bool {
	t.Helper()
	select {
	case <-r.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

type blockingReceiver struct {
	started chan string
	release chan struct{}

	mu       sync.Mutex
	inFlight int
	max      int
}

func (r *blockingReceiver) Receive(_ context.Context, msg conversation.ChannelMessage) error {
	r.mu.Lock()
	r.inFlight++
	if r.inFlight > r.max {
		r.max = r.inFlight
	}
	r.mu.Unlock()

	r.started <- msg.Text
	<-r.release

	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
	return nil
}

func (r *blockingReceiver) maxInFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.max
}

// TestHandleMessage_ReceiverPath_FiresReceive — when a Receiver
// is wired and the message is a normal (non-slash) user turn,
// HandleMessage spawns a goroutine that calls Receiver.Receive
// with the translated ChannelMessage. The legacy inbox path is
// not exercised.
func TestHandleMessage_ReceiverPath_FiresReceive(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	r := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(r)

	err := bot.HandleMessage(context.Background(), &Message{
		ID: 1, ChatID: 100, UserID: 42, Text: "hello dispatcher",
	})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if !r.waitReceive(t, 2*time.Second) {
		t.Fatal("Receiver.Receive was not called within 2s")
	}
	if r.calls != 1 {
		t.Errorf("Receive calls = %d, want 1", r.calls)
	}
	if r.last.SessionID != "100" {
		t.Errorf("ChannelMessage.SessionID = %q, want 100 (chat_id encoded)", r.last.SessionID)
	}
	if r.last.SpeakerID != "42" {
		t.Errorf("ChannelMessage.SpeakerID = %q, want 42 (user_id encoded)", r.last.SpeakerID)
	}
	if r.last.Text != "hello dispatcher" {
		t.Errorf("ChannelMessage.Text = %q, want 'hello dispatcher'", r.last.Text)
	}
}

// TestHandleMessage_ReceiverPath_SerializesSameChat ensures two
// quick messages from the same chat do not dispatch concurrently.
// The receiver replaces the whole conversation history after each
// turn, so concurrent same-chat turns can lose or reorder history.
func TestHandleMessage_ReceiverPath_SerializesSameChat(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	r := &blockingReceiver{
		started: make(chan string, 2),
		release: make(chan struct{}),
	}
	bot.SetReceiver(r)

	for _, text := range []string{"first", "second"} {
		if err := bot.HandleMessage(context.Background(), &Message{
			ID: 1, ChatID: 100, UserID: 42, Text: text,
		}); err != nil {
			t.Fatalf("HandleMessage(%q): %v", text, err)
		}
	}

	select {
	case got := <-r.started:
		if got != "first" && got != "second" {
			t.Fatalf("unexpected first receiver text %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first receiver call did not start")
	}

	select {
	case got := <-r.started:
		t.Fatalf("second same-chat receiver call started before first released: %q", got)
	case <-time.After(100 * time.Millisecond):
	}

	r.release <- struct{}{}
	select {
	case <-r.started:
	case <-time.After(2 * time.Second):
		t.Fatal("second receiver call did not start after first released")
	}
	r.release <- struct{}{}

	if got := r.maxInFlight(); got != 1 {
		t.Errorf("max in-flight receiver calls = %d, want 1", got)
	}
}

// TestHandleMessage_ReceiverPath_ErrorLoggedNotReturned — the
// goroutine that calls Receive is fire-and-forget; a receiver
// error must not propagate back to HandleMessage's caller (the
// poll loop). Errors are observable via the bot's logger; the
// HandleMessage return stays nil so the poll loop keeps draining
// updates.
func TestHandleMessage_ReceiverPath_ErrorLoggedNotReturned(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	bot.SetReceiver(&handleMessageReceiver{
		done:       make(chan struct{}, 1),
		receiveErr: errors.New("downstream blew up"),
	})

	err := bot.HandleMessage(context.Background(), &Message{
		ID: 3, ChatID: 100, UserID: 42, Text: "boom",
	})
	if err != nil {
		t.Errorf("HandleMessage returned %v, want nil (receiver errors are fire-and-forget)", err)
	}
}

// TestHandleMessage_ReceiverPath_StillRunsSlashCommands — slash
// commands MUST take precedence over the receiver path. /help,
// /project, etc. handle locally and should NOT route through the
// dispatcher (which would burn LLM budget on every slash command).
func TestHandleMessage_ReceiverPath_StillRunsSlashCommands(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	r := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(r)

	// /help is handled in-bot via the commands map; the receiver
	// path must not see it.
	_ = bot.HandleMessage(context.Background(), &Message{
		ID: 4, ChatID: 100, UserID: 42, Text: "/help",
	})

	// Wait briefly to ensure no late goroutine fires.
	if r.waitReceive(t, 200*time.Millisecond) {
		t.Errorf("Receiver fired for /help — slash commands must stay in-bot")
	}
}

// TestHandleMessage_ReceiverPath_ProjectRevocationStaysInline —
// when the operator has narrowed the user's allowlist mid-flight
// and the chat had an active project the user can no longer
// access, the revocation message + pin clear fires inline BEFORE
// invoking the receiver. Otherwise the dispatcher's tools would
// see a project_id the auth layer disallows.
func TestHandleMessage_ReceiverPath_ProjectRevocationStaysInline(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{
		Token: "t",
		AllowedUsers: map[int64]UserAccess{
			42: {Allowed: true, Projects: []string{"alpha"}},
		},
	})
	bot.setActiveProject(100, "now-revoked")
	r := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(r)

	_ = bot.HandleMessage(context.Background(), &Message{
		ID: 5, ChatID: 100, UserID: 42, Text: "anything",
	})

	// Receiver must NOT fire — the revocation flow short-circuits.
	if r.waitReceive(t, 200*time.Millisecond) {
		t.Errorf("Receiver fired despite project-access revocation")
	}
	if got := bot.getActiveProject(100); got != "" {
		t.Errorf("active project = %q, want empty (revocation should clear pin)", got)
	}
}
