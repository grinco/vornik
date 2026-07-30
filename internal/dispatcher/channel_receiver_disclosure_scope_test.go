package dispatcher

import (
	"context"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/conversation"
)

// scopingChannel is a recordingChannel that declares a disclosure scope coarser than
// its session id — the Slack shape, where every thread is its own session but all of
// them are the same person in the same channel.
type scopingChannel struct {
	recordingChannel
	scope string

	mu     sync.Mutex
	sentTo []string
}

func (c *scopingChannel) DisclosureScope(conversation.ChannelMessage) string { return c.scope }

func (c *scopingChannel) Send(ctx context.Context, msg conversation.ChannelMessage) (string, error) {
	c.mu.Lock()
	c.sentTo = append(c.sentTo, msg.SessionID)
	c.mu.Unlock()
	return c.recordingChannel.Send(ctx, msg)
}

func (c *scopingChannel) destinations() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sentTo...)
}

// OPERATOR REPORT 2026-07-30: "the you are interacting with an AI system is sent
// multiple times — when the robot replies first time, and then when I ask a question in
// a thread."
//
// Cause: the Art 50 notice is tracked per SESSION, and Slack's session id encodes the
// thread (`<team>/<channel>#<thread_ts>`), so every thread was a first interaction.
//
// Art 50(1) obliges the provider to ensure the natural persons CONCERNED are informed
// that they are interacting with an AI system, at the time of first interaction. It does
// not require the notice to be repeated once that person knows. A channel that can say
// "this is the same person I already told" is therefore allowed to say so — and the
// notice still has to be DELIVERED into the thread the person is actually reading.
func TestServeDisclosure_HonoursACoarserChannelScope(t *testing.T) {
	ch := &scopingChannel{recordingChannel: recordingChannel{name: "slack"}, scope: "T1/C1|U_alice"}
	store := newMemStore()
	r := newDisclosureReceiver(ch, &fixedDoer{reply: "answer"}, store)

	// Same person, same channel, three different Slack sessions: the channel-level
	// conversation and two threads under it.
	for _, session := range []string{"T1/C1#main", "T1/C1#1785367141.211839", "T1/C1#1785999999.000100"} {
		if err := r.Receive(context.Background(), conversation.ChannelMessage{
			Source: "slack", SessionID: session, SpeakerID: "U_alice", Text: "hello",
		}); err != nil {
			t.Fatalf("Receive(%s): %v", session, err)
		}
	}

	var notices int
	for _, text := range ch.texts() {
		if strings.Contains(text, "interacting with an AI") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("disclosure notices = %d, want 1 — the same person in the same channel "+
			"is one first interaction, not one per thread", notices)
	}
}

// The scope is bookkeeping only. The notice itself must still be delivered to the
// session the person is reading, or it would be disclosed somewhere they never look.
func TestServeDisclosure_NoticeGoesToTheSessionNotTheScope(t *testing.T) {
	ch := &scopingChannel{recordingChannel: recordingChannel{name: "slack"}, scope: "T1/C1|U_alice"}
	r := newDisclosureReceiver(ch, &fixedDoer{reply: "answer"}, newMemStore())

	const session = "T1/C1#1785367141.211839"
	if err := r.Receive(context.Background(), conversation.ChannelMessage{
		Source: "slack", SessionID: session, SpeakerID: "U_alice", Text: "hello",
	}); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	dests := ch.destinations()
	if len(dests) == 0 {
		t.Fatal("nothing was sent")
	}
	if dests[0] != session {
		t.Errorf("notice delivered to %q, want the reader's session %q", dests[0], session)
	}
}

// A DIFFERENT person is a different first interaction and must get their own notice —
// this is why the scope is per-person and not simply the channel. A pure channel key
// would silently deny the notice to everyone who arrives after the first speaker.
func TestServeDisclosure_EachPersonIsStillDisclosedTo(t *testing.T) {
	store := newMemStore()
	var notices int

	for _, who := range []string{"U_alice", "U_bob"} {
		ch := &scopingChannel{
			recordingChannel: recordingChannel{name: "slack"},
			scope:            "T1/C1|" + who,
		}
		r := newDisclosureReceiver(ch, &fixedDoer{reply: "answer"}, store)
		if err := r.Receive(context.Background(), conversation.ChannelMessage{
			Source: "slack", SessionID: "T1/C1#main", SpeakerID: who, Text: "hello",
		}); err != nil {
			t.Fatalf("Receive(%s): %v", who, err)
		}
		for _, text := range ch.texts() {
			if strings.Contains(text, "interacting with an AI") {
				notices++
			}
		}
	}

	if notices != 2 {
		t.Fatalf("notices across two people = %d, want 2 — Art 50(1) is owed to each "+
			"natural person concerned", notices)
	}
}

// A channel that does NOT implement the interface keeps the previous behaviour exactly.
// The fallback is what keeps email, github-app and web-chat unaffected.
func TestServeDisclosure_FallsBackToSessionIDWithoutAScoper(t *testing.T) {
	ch := &recordingChannel{name: "slack"}
	store := newMemStore()
	r := newDisclosureReceiver(ch, &fixedDoer{reply: "answer"}, store)

	for _, session := range []string{"T1/C1#main", "T1/C1#thread"} {
		if err := r.Receive(context.Background(), conversation.ChannelMessage{
			Source: "slack", SessionID: session, SpeakerID: "U_alice", Text: "hi",
		}); err != nil {
			t.Fatalf("Receive: %v", err)
		}
	}

	var notices int
	for _, text := range ch.texts() {
		if strings.Contains(text, "interacting with an AI") {
			notices++
		}
	}
	if notices != 2 {
		t.Fatalf("notices = %d, want 2 — without a declared scope the session id is "+
			"still the key", notices)
	}
}
