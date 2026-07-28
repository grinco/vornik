package dispatcher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
)

// EU AI Act Art 50(1) conformity at the ChannelReceiver chokepoint.
// Design: https://docs.vornik.io
//
// Regression context: before this landed, NONE of the five channels disclosed
// that the user was talking to an AI. The obligation binds the provider from
// 2 Aug 2026 with Art 99 exposure of EUR 15M or 3% of worldwide turnover.

// recordingChannel captures every Send in order so a test can assert not just
// THAT the disclosure went out but that it went out FIRST.
type recordingChannel struct {
	name     string
	mu       sync.Mutex
	sent     []string
	sendErr  error
	failOnce bool
}

func (c *recordingChannel) Name() string { return c.name }
func (c *recordingChannel) Start(context.Context, conversation.Receiver) error {
	return nil
}
func (c *recordingChannel) Stop() error { return nil }

func (c *recordingChannel) Send(_ context.Context, msg conversation.ChannelMessage) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		if c.failOnce {
			c.sendErr = nil
		}
		return "", c.sendErr
	}
	c.sent = append(c.sent, msg.Text)
	return "id", nil
}

func (c *recordingChannel) ListSessions(context.Context) ([]conversation.Session, error) {
	return nil, nil
}

func (c *recordingChannel) ResolveSpeaker(_ context.Context, id string) (conversation.Speaker, error) {
	return conversation.Speaker{ID: id}, nil
}

func (c *recordingChannel) texts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sent...)
}

// fixedDoer is a Doer that always returns the same reply.
type fixedDoer struct {
	reply  string
	called int
}

func (d *fixedDoer) Process(context.Context, Request) Result {
	d.called++
	return Result{Text: d.reply}
}

func (d *fixedDoer) ProcessStreaming(_ context.Context, _ Request, _ chat.StreamCallback) Result {
	d.called++
	return Result{Text: d.reply}
}

func newDisclosureReceiver(ch conversation.Channel, doer Doer, store aidisclosure.Store) *ChannelReceiver {
	return &ChannelReceiver{
		Channel:    ch,
		Agent:      doer,
		Disclosure: aidisclosure.New(aidisclosure.Config{}, store),
	}
}

type memStore struct {
	mu     sync.Mutex
	served map[string]bool
	putErr error
}

func newMemStore() *memStore { return &memStore{served: map[string]bool{}} }

func (m *memStore) WasServed(_ context.Context, ch, sess string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.served[ch+"|"+sess], nil
}

func (m *memStore) MarkServed(_ context.Context, ch, sess, _ string) error {
	if m.putErr != nil {
		return m.putErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.served[ch+"|"+sess] = true
	return nil
}

// TestReceive_PerSessionChannel_DisclosureIsSentBEFORETheReply pins Art 50(5):
// the notice must land at the latest at the first interaction. Asserting
// ordering, not mere presence — a disclosure that arrives after the AI has
// already spoken does not satisfy "first interaction".
func TestReceive_PerSessionChannel_DisclosureIsSentBEFORETheReply(t *testing.T) {
	ch := &recordingChannel{name: "telegram"}
	r := newDisclosureReceiver(ch, &fixedDoer{reply: "the assistant reply"}, newMemStore())

	err := r.Receive(context.Background(), conversation.ChannelMessage{
		Source: "telegram", SessionID: "chat-1", SpeakerID: "u1", Text: "hi",
	})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}

	sent := ch.texts()
	if len(sent) != 2 {
		t.Fatalf("want 2 messages (disclosure then reply), got %d: %q", len(sent), sent)
	}
	if !strings.Contains(strings.ToLower(sent[0]), "ai system") {
		t.Errorf("first message must be the disclosure, got %q", sent[0])
	}
	if sent[1] != "the assistant reply" {
		t.Errorf("second message must be the reply, got %q", sent[1])
	}
}

func TestReceive_PerSessionChannel_NotReDisclosedOnTheSecondTurn(t *testing.T) {
	ch := &recordingChannel{name: "telegram"}
	r := newDisclosureReceiver(ch, &fixedDoer{reply: "reply"}, newMemStore())
	msg := conversation.ChannelMessage{Source: "telegram", SessionID: "chat-1", SpeakerID: "u1", Text: "hi"}

	for i := range 2 {
		if err := r.Receive(context.Background(), msg); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}

	sent := ch.texts()
	var disclosures int
	for _, s := range sent {
		if strings.Contains(strings.ToLower(s), "ai system") {
			disclosures++
		}
	}
	if disclosures != 1 {
		t.Errorf("disclosure sent %d times across 2 turns, want exactly 1: %q", disclosures, sent)
	}
}

// TestReceive_DisclosureSendFails_FailsClosed pins design §4: if we cannot
// disclose, we must not converse. Silence beats an undisclosed AI exchange.
func TestReceive_DisclosureSendFails_FailsClosed(t *testing.T) {
	ch := &recordingChannel{name: "telegram", sendErr: errors.New("telegram 503")}
	doer := &fixedDoer{reply: "reply"}
	r := newDisclosureReceiver(ch, doer, newMemStore())

	err := r.Receive(context.Background(), conversation.ChannelMessage{
		Source: "telegram", SessionID: "chat-1", SpeakerID: "u1", Text: "hi",
	})

	if err == nil {
		t.Fatal("a failed disclosure send must fail the turn, not proceed undisclosed")
	}
	if doer.called != 0 {
		t.Errorf("dispatcher must not run when the disclosure could not be delivered (called=%d)", doer.called)
	}
}

// TestReceive_MarkServedFails_TurnStillCompletes pins the other half of the
// asymmetry: the human HAS been disclosed to, so the obligation is met and
// only the evidence trail degraded. Failing the turn here would punish the
// user for a DB blip after we did the right thing.
func TestReceive_MarkServedFails_TurnStillCompletes(t *testing.T) {
	ch := &recordingChannel{name: "telegram"}
	store := newMemStore()
	store.putErr = errors.New("db write failed")
	r := newDisclosureReceiver(ch, &fixedDoer{reply: "reply"}, store)

	err := r.Receive(context.Background(), conversation.ChannelMessage{
		Source: "telegram", SessionID: "chat-1", SpeakerID: "u1", Text: "hi",
	})

	if err != nil {
		t.Fatalf("a MarkServed failure must not fail the turn: %v", err)
	}
	if got := ch.texts(); len(got) != 2 {
		t.Errorf("disclosure + reply should both have been sent, got %q", got)
	}
}

// TestReceive_PerMessageChannel_FooterOnEveryOutbound — email and GitHub
// comments are standalone artifacts that get forwarded and quoted, so each
// carries the notice rather than relying on a session banner nobody sees.
func TestReceive_PerMessageChannel_FooterOnEveryOutbound(t *testing.T) {
	ch := &recordingChannel{name: "email"}
	r := newDisclosureReceiver(ch, &fixedDoer{reply: "the reply"}, newMemStore())
	msg := conversation.ChannelMessage{Source: "email", SessionID: "thread-1", SpeakerID: "u1", Text: "hi"}

	for i := range 2 {
		if err := r.Receive(context.Background(), msg); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}

	sent := ch.texts()
	if len(sent) != 2 {
		t.Fatalf("per-message channel should send one message per turn, got %d: %q", len(sent), sent)
	}
	for i, s := range sent {
		if !strings.Contains(strings.ToLower(s), "ai system") {
			t.Errorf("outbound %d missing the disclosure footer: %q", i, s)
		}
		if !strings.Contains(s, "the reply") {
			t.Errorf("outbound %d lost the actual reply: %q", i, s)
		}
	}
}

// TestReceive_PerMessageChannel_FooterSurvivesAResultPostprocessor — the
// footer is applied AFTER any channel postprocessor precisely so a
// postprocessor cannot strip the legally required notice.
func TestReceive_PerMessageChannel_FooterSurvivesAResultPostprocessor(t *testing.T) {
	ch := &recordingChannel{name: "github-app"}
	r := newDisclosureReceiver(ch, &fixedDoer{reply: "original"}, newMemStore())
	r.ResultPostprocessor = func(Result) string { return "postprocessor replaced everything" }

	if err := r.Receive(context.Background(), conversation.ChannelMessage{
		Source: "github-app", SessionID: "o/r#pulls/1", SpeakerID: "u1", Text: "hi",
	}); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	sent := ch.texts()
	if len(sent) != 1 {
		t.Fatalf("want 1 outbound, got %q", sent)
	}
	if !strings.Contains(strings.ToLower(sent[0]), "ai system") {
		t.Errorf("a ResultPostprocessor must not be able to strip the Art 50 notice: %q", sent[0])
	}
}

// TestReceive_EmptyReply_NoFooterOnlyMessage — sendReply skips empty text, and
// a footer with no content behind it helps nobody.
func TestReceive_EmptyReply_NoFooterOnlyMessage(t *testing.T) {
	ch := &recordingChannel{name: "email"}
	r := newDisclosureReceiver(ch, &fixedDoer{reply: ""}, newMemStore())

	if err := r.Receive(context.Background(), conversation.ChannelMessage{
		Source: "email", SessionID: "thread-1", SpeakerID: "u1", Text: "hi",
	}); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	if got := ch.texts(); len(got) != 0 {
		t.Errorf("an empty reply must stay empty, got %q", got)
	}
}

// TestReceive_NilDisclosure_DoesNotPanic keeps every existing construction
// site working while the wiring rolls out.
func TestReceive_NilDisclosure_DoesNotPanic(t *testing.T) {
	ch := &recordingChannel{name: "telegram"}
	r := &ChannelReceiver{Channel: ch, Agent: &fixedDoer{reply: "reply"}}

	if err := r.Receive(context.Background(), conversation.ChannelMessage{
		Source: "telegram", SessionID: "chat-1", SpeakerID: "u1", Text: "hi",
	}); err != nil {
		t.Fatalf("Receive with nil Disclosure: %v", err)
	}
}
