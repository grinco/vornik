package conversation

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestErrSpeakerUnknown_IsConst — the sentinel must be a
// comparable typed const so callers can branch on it with
// errors.Is rather than string-matching.
func TestErrSpeakerUnknown_IsConst(t *testing.T) {
	err := ErrSpeakerUnknown
	if !errors.Is(err, ErrSpeakerUnknown) {
		t.Errorf("errors.Is misbehaves on ErrSpeakerUnknown")
	}
	if err.Error() == "" {
		t.Errorf("ErrSpeakerUnknown has empty error string")
	}
	// Wrap-and-unwrap also works (caller chains: "auth failed: <err>").
	wrapped := errors.New("wrapped: " + err.Error())
	if errors.Is(wrapped, ErrSpeakerUnknown) {
		t.Errorf("errors.Is should not match across a non-wrapping format string")
	}
}

// TestChannelMessage_ZeroValueIsValid — defensive: the
// dispatcher creates ChannelMessage values mid-flow; the zero
// value must be safe to pass to Channel.Send without panics
// (channels still need to handle empty Text + empty Session,
// but the type itself shouldn't trap).
func TestChannelMessage_ZeroValueIsValid(t *testing.T) {
	var m ChannelMessage
	// Nil ChannelSpecific must be safe to read.
	if v := m.ChannelSpecific["any-key"]; v != "" {
		t.Errorf("nil ChannelSpecific read returned %q, want \"\"", v)
	}
	// Nil Attachments slice safe to range.
	for range m.Attachments {
		t.Fatal("unexpected iteration over nil Attachments")
	}
	// Empty Timestamp is the zero time, comparable.
	if !m.Timestamp.Equal(time.Time{}) {
		t.Errorf("zero ChannelMessage.Timestamp not zero time")
	}
}

// stubChannel exercises the interface compile-time contract +
// the receive-side wiring so we can guarantee a Channel
// implementation's Receive call lands at the Receiver.
type stubChannel struct {
	name  string
	rx    Receiver
	ready chan struct{}
}

func (s *stubChannel) Name() string { return s.name }
func (s *stubChannel) Start(ctx context.Context, recv Receiver) error {
	s.rx = recv
	if s.ready != nil {
		close(s.ready)
	}
	<-ctx.Done()
	return ctx.Err()
}
func (s *stubChannel) Stop() error { return nil }
func (s *stubChannel) Send(_ context.Context, m ChannelMessage) (string, error) {
	return "sent-" + m.ID, nil
}
func (s *stubChannel) ListSessions(_ context.Context) ([]Session, error) {
	return nil, nil
}
func (s *stubChannel) ResolveSpeaker(_ context.Context, id string) (Speaker, error) {
	if id == "" {
		return Speaker{}, ErrSpeakerUnknown
	}
	return Speaker{ID: "stub:" + id, DisplayName: "stub user"}, nil
}

// stubReceiver records the messages handed to it.
type stubReceiver struct {
	got []ChannelMessage
}

func (r *stubReceiver) Receive(_ context.Context, m ChannelMessage) error {
	r.got = append(r.got, m)
	return nil
}

// Compile-time guard: stubChannel satisfies Channel. If the
// interface drifts, the test build breaks first.
var _ Channel = (*stubChannel)(nil)
var _ Receiver = (*stubReceiver)(nil)

// TestChannel_StartReceiveSendRoundTrip — exercises the
// dispatch path end-to-end: Start binds the receiver; a manual
// Receive call (simulating what a real channel does on inbound)
// lands in the receiver's slice; Send returns a non-empty
// per-message id.
func TestChannel_StartReceiveSendRoundTrip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := &stubChannel{name: "stub", ready: make(chan struct{})}
	rx := &stubReceiver{}

	// Start runs Start in its own goroutine so the test can
	// drive Receive against the bound receiver.
	done := make(chan error, 1)
	go func() { done <- ch.Start(ctx, rx) }()

	// Wait for Start to bind rx. The channel close synchronizes
	// the goroutine's write with this read under the race detector.
	<-ch.ready
	if ch.rx == nil {
		t.Fatal("Start did not bind the receiver")
	}
	if err := ch.rx.Receive(ctx, ChannelMessage{
		ID: "m1", SessionID: "chat-1", SpeakerID: "u1", Text: "hi",
		Timestamp: time.Now(),
	}); err != nil {
		t.Errorf("Receive: %v", err)
	}
	if len(rx.got) != 1 || rx.got[0].Text != "hi" {
		t.Errorf("receiver got %+v", rx.got)
	}

	// Send round-trips an ID prefix so callers can correlate.
	id, err := ch.Send(ctx, ChannelMessage{ID: "outbound-1", Text: "ack"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "sent-outbound-1" {
		t.Errorf("sent id = %q, want sent-outbound-1", id)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Start exited with %v, want context.Canceled", err)
	}
}

// stubStreamingChannel — a Channel that ALSO implements
// StreamingChannel. Exercises the type-assertion path the
// dispatcher uses to choose between Send and StreamingSend.
type stubStreamingChannel struct {
	stubChannel
	streams []*stubStream
}

func (s *stubStreamingChannel) StreamingSend(_ context.Context, sessionID string) (Stream, error) {
	st := &stubStream{sessionID: sessionID}
	s.streams = append(s.streams, st)
	return st, nil
}

type stubStream struct {
	sessionID string
	appended  []string
	closed    bool
}

func (s *stubStream) Append(text string) error {
	if s.closed {
		return errors.New("stream closed")
	}
	s.appended = append(s.appended, text)
	return nil
}

func (s *stubStream) Close() (string, error) {
	s.closed = true
	return "stream-" + s.sessionID, nil
}

var _ StreamingChannel = (*stubStreamingChannel)(nil)
var _ Stream = (*stubStream)(nil)

// TestStreamingChannel_TypeAssertion — the dispatcher's
// branching contract: a streaming-capable Channel type-asserts
// to StreamingChannel; a plain Channel must not.
func TestStreamingChannel_TypeAssertion(t *testing.T) {
	var plain Channel = &stubChannel{name: "stub"}
	if _, ok := plain.(StreamingChannel); ok {
		t.Error("stubChannel must NOT satisfy StreamingChannel")
	}

	var streaming Channel = &stubStreamingChannel{stubChannel: stubChannel{name: "stub-stream"}}
	sc, ok := streaming.(StreamingChannel)
	if !ok {
		t.Fatal("stubStreamingChannel must satisfy StreamingChannel")
	}

	ctx := context.Background()
	stream, err := sc.StreamingSend(ctx, "session-1")
	if err != nil {
		t.Fatalf("StreamingSend: %v", err)
	}
	if err := stream.Append("hello "); err != nil {
		t.Fatalf("Append #1: %v", err)
	}
	if err := stream.Append("world"); err != nil {
		t.Fatalf("Append #2: %v", err)
	}
	id, err := stream.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if id != "stream-session-1" {
		t.Errorf("Close sentID = %q, want stream-session-1", id)
	}
	if err := stream.Append("after close"); err == nil {
		t.Error("Append after Close must return an error")
	}
}

// TestChannel_ResolveSpeaker_UnknownReturnsSentinel — the
// auth-rejection signal. ResolveSpeaker on an empty / unknown
// identifier MUST return ErrSpeakerUnknown so callers can
// branch on it via errors.Is.
func TestChannel_ResolveSpeaker_UnknownReturnsSentinel(t *testing.T) {
	ch := &stubChannel{name: "stub"}
	_, err := ch.ResolveSpeaker(context.Background(), "")
	if !errors.Is(err, ErrSpeakerUnknown) {
		t.Errorf("err = %v, want ErrSpeakerUnknown", err)
	}
	// Known speaker resolves cleanly.
	s, err := ch.ResolveSpeaker(context.Background(), "u42")
	if err != nil {
		t.Fatalf("ResolveSpeaker(u42): %v", err)
	}
	if s.ID == "" || s.DisplayName == "" {
		t.Errorf("speaker fields empty: %+v", s)
	}
}
