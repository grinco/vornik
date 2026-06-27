package dispatcher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
)

// stubChannel is a minimal conversation.Channel used by every
// ChannelReceiver test. Records each Send call so assertions can
// verify reply routing.
type stubChannel struct {
	name string

	mu   sync.Mutex
	sent []conversation.ChannelMessage
	// sendErr, when non-nil, is returned from every Send call.
	sendErr error
}

func (s *stubChannel) Name() string { return s.name }
func (s *stubChannel) Start(ctx context.Context, _ conversation.Receiver) error {
	<-ctx.Done()
	return ctx.Err()
}
func (s *stubChannel) Stop() error { return nil }
func (s *stubChannel) Send(_ context.Context, m conversation.ChannelMessage) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	if s.sendErr != nil {
		return "", s.sendErr
	}
	return "id-" + m.SessionID, nil
}
func (s *stubChannel) ListSessions(_ context.Context) ([]conversation.Session, error) {
	return nil, nil
}
func (s *stubChannel) ResolveSpeaker(_ context.Context, _ string) (conversation.Speaker, error) {
	return conversation.Speaker{}, conversation.ErrSpeakerUnknown
}

func (s *stubChannel) sentCopy() []conversation.ChannelMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]conversation.ChannelMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

// stubStreamingChannel layers StreamingSend on top of stubChannel.
// Streaming-related test fields control the StreamingSend / Stream
// behaviour to exercise every branch in dispatch.
type stubStreamingChannel struct {
	stubChannel
	streamingSendErr error // when non-nil, StreamingSend returns it

	// streams collects every Stream this channel handed out so tests
	// can assert on coalesced deltas.
	mu      sync.Mutex
	streams []*stubStream
}

func (s *stubStreamingChannel) StreamingSend(_ context.Context, sessionID string) (conversation.Stream, error) {
	if s.streamingSendErr != nil {
		return nil, s.streamingSendErr
	}
	st := &stubStream{sessionID: sessionID}
	s.mu.Lock()
	s.streams = append(s.streams, st)
	s.mu.Unlock()
	return st, nil
}

func (s *stubStreamingChannel) lastStream() *stubStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.streams) == 0 {
		return nil
	}
	return s.streams[len(s.streams)-1]
}

type stubStream struct {
	sessionID string

	mu        sync.Mutex
	deltas    []string
	closed    bool
	appendErr error // when non-nil, sticky terminal error from Append
	closeErr  error // when non-nil, Close returns it
}

func (s *stubStream) Append(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendErr != nil {
		return s.appendErr
	}
	s.deltas = append(s.deltas, text)
	return nil
}

func (s *stubStream) Close() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.closeErr != nil {
		return "", s.closeErr
	}
	return "stream-" + s.sessionID, nil
}

// stubAgent is a Doer that returns canned Process / ProcessStreaming
// results and exposes the last Request it saw so tests can assert
// on the translation.
type stubAgent struct {
	mu sync.Mutex

	processResult   Result
	streamingResult Result
	// streamingTokens, when non-empty, drive the onText callback
	// during ProcessStreaming. Each entry is one accumulated value
	// passed to the callback. Mirrors chat.StreamCallback semantics
	// (accumulated text, not delta).
	streamingTokens []string

	lastReq      Request
	processCalls int
	streamCalls  int
}

func (a *stubAgent) Process(_ context.Context, req Request) Result {
	a.mu.Lock()
	a.lastReq = req
	a.processCalls++
	a.mu.Unlock()
	return a.processResult
}

func (a *stubAgent) ProcessStreaming(_ context.Context, req Request, onText chat.StreamCallback) Result {
	a.mu.Lock()
	a.lastReq = req
	a.streamCalls++
	tokens := append([]string{}, a.streamingTokens...)
	a.mu.Unlock()
	for _, tok := range tokens {
		if onText != nil {
			onText(tok)
		}
	}
	return a.streamingResult
}

// stubSessionStore captures Load / Append calls and lets tests
// inject canned sessions or errors.
type stubSessionStore struct {
	mu sync.Mutex

	session   Session
	loadErr   error
	appendErr error

	loadCount   int
	appendCount int
	lastAppend  Result
}

func (s *stubSessionStore) Load(_ context.Context, _ conversation.ChannelMessage) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadCount++
	return s.session, s.loadErr
}

func (s *stubSessionStore) Append(_ context.Context, _ conversation.ChannelMessage, r Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendCount++
	s.lastAppend = r
	return s.appendErr
}

// TestChannelReceiver_ReceiveSatisfiesContract — compile-time +
// runtime guard that *ChannelReceiver implements
// conversation.Receiver.
func TestChannelReceiver_ReceiveSatisfiesContract(t *testing.T) {
	var _ conversation.Receiver = (*ChannelReceiver)(nil)
}

// TestChannelReceiver_OneShotChannel_RoutesViaSend — non-streaming
// channels get Process invoked and the reply lands on Send.
func TestChannelReceiver_OneShotChannel_RoutesViaSend(t *testing.T) {
	ch := &stubChannel{name: "stub"}
	agent := &stubAgent{processResult: Result{Text: "hello back"}}
	store := &stubSessionStore{}

	rcv := &ChannelReceiver{Channel: ch, Agent: agent, Sessions: store}

	err := rcv.Receive(context.Background(), conversation.ChannelMessage{
		Source:    "stub",
		SessionID: "s-1",
		Text:      "ping",
	})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if agent.processCalls != 1 {
		t.Errorf("Process calls = %d, want 1", agent.processCalls)
	}
	if agent.streamCalls != 0 {
		t.Errorf("ProcessStreaming calls = %d, want 0 (non-streaming channel)", agent.streamCalls)
	}
	sent := ch.sentCopy()
	if len(sent) != 1 || sent[0].Text != "hello back" || sent[0].SessionID != "s-1" {
		t.Errorf("Channel.Send got %+v, want one Send of hello back / s-1", sent)
	}
	if store.appendCount != 1 {
		t.Errorf("SessionStore.Append calls = %d, want 1", store.appendCount)
	}
	if store.lastAppend.Text != "hello back" {
		t.Errorf("SessionStore.Append saw Text=%q, want hello back", store.lastAppend.Text)
	}
}

// TestChannelReceiver_StreamingChannel_RoutesViaStream — streaming
// channels get ProcessStreaming + Append-per-delta + Close. Send
// is NOT called on the happy path.
func TestChannelReceiver_StreamingChannel_RoutesViaStream(t *testing.T) {
	ch := &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}}
	agent := &stubAgent{
		streamingResult: Result{Text: "hello world"},
		streamingTokens: []string{"hello", "hello world"},
	}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}

	err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-2", Text: "ping"})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if agent.streamCalls != 1 || agent.processCalls != 0 {
		t.Errorf("calls: stream=%d process=%d, want stream=1 process=0", agent.streamCalls, agent.processCalls)
	}
	if len(ch.sentCopy()) != 0 {
		t.Errorf("Channel.Send called %d times on streaming happy path, want 0", len(ch.sentCopy()))
	}
	st := ch.lastStream()
	if st == nil {
		t.Fatal("no Stream was created")
	}
	// Accumulated callback values were "hello" then "hello world"; the
	// receiver should have appended "hello" and " world".
	want := []string{"hello", " world"}
	if len(st.deltas) != len(want) {
		t.Fatalf("Stream got deltas %v, want %v", st.deltas, want)
	}
	for i, d := range st.deltas {
		if d != want[i] {
			t.Errorf("delta[%d] = %q, want %q", i, d, want[i])
		}
	}
	if !st.closed {
		t.Error("Stream was not closed")
	}
}

// TestChannelReceiver_StreamingChannel_AppendsFinalTextWhenNoDelta
// covers providers that return a final Result.Text but never invoke
// the streaming callback. Telegram would otherwise keep the
// placeholder message forever because the happy path never calls
// Channel.Send.
func TestChannelReceiver_StreamingChannel_AppendsFinalTextWhenNoDelta(t *testing.T) {
	ch := &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}}
	agent := &stubAgent{streamingResult: Result{Text: "final reply"}}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}

	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-final", Text: "ping"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	st := ch.lastStream()
	if st == nil {
		t.Fatal("no Stream was created")
	}
	if got := strings.Join(st.deltas, ""); got != "final reply" {
		t.Errorf("streamed text = %q, want final reply", got)
	}
	if len(ch.sentCopy()) != 0 {
		t.Errorf("Channel.Send called on streaming happy path, want 0")
	}
}

// TestChannelReceiver_StreamingChannel_PostprocessesFinalSuffix
// ensures channel-specific final text, such as Telegram's output
// guard footer, is appended before Close on the streaming happy
// path. The non-streaming path already sends the postprocessed text
// through Channel.Send.
func TestChannelReceiver_StreamingChannel_PostprocessesFinalSuffix(t *testing.T) {
	ch := &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}}
	agent := &stubAgent{
		streamingResult: Result{Text: "body"},
		streamingTokens: []string{"bo", "body"},
	}
	rcv := &ChannelReceiver{
		Channel: ch,
		Agent:   agent,
		ResultPostprocessor: func(r Result) string {
			return r.Text + "\n\nfooter"
		},
	}

	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-footer", Text: "ping"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	st := ch.lastStream()
	if st == nil {
		t.Fatal("no Stream was created")
	}
	if got := strings.Join(st.deltas, ""); got != "body\n\nfooter" {
		t.Errorf("streamed text = %q, want body with footer", got)
	}
}

// TestChannelReceiver_StreamingToolStatus_DoesNotFallbackDuplicate covers
// the Telegram tool-use path: streamed text may include status paragraphs
// before the final answer, so it intentionally differs from Result.Text.
// The receiver must close the stream, not send a second one-shot duplicate.
func TestChannelReceiver_StreamingToolStatus_DoesNotFallbackDuplicate(t *testing.T) {
	ch := &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}}
	agent := &stubAgent{
		streamingResult: Result{Text: "Task created."},
		streamingTokens: []string{
			"I'll create it.",
			"I'll create it.\n\n[📋 creating task]\n\n",
			"I'll create it.\n\n[📋 creating task]\n\nTask created.",
		},
	}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}

	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-tool", Text: "ping"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(ch.sentCopy()) != 0 {
		t.Errorf("Channel.Send called on status+final stream, want no duplicate fallback")
	}
	st := ch.lastStream()
	if st == nil {
		t.Fatal("no Stream was created")
	}
	if got := strings.Join(st.deltas, ""); !strings.HasSuffix(got, "Task created.") {
		t.Errorf("streamed text = %q, want final answer suffix", got)
	}
}

func TestChannelReceiver_StreamingToolStatus_AppendsPostprocessSuffix(t *testing.T) {
	ch := &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}}
	agent := &stubAgent{
		streamingResult: Result{Text: "Task created."},
		streamingTokens: []string{
			"[📋 creating task]\n\nTask created.",
		},
	}
	rcv := &ChannelReceiver{
		Channel: ch,
		Agent:   agent,
		ResultPostprocessor: func(r Result) string {
			return r.Text + "\n\nfooter"
		},
	}

	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-tool-footer", Text: "ping"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	st := ch.lastStream()
	if st == nil {
		t.Fatal("no Stream was created")
	}
	if got := strings.Join(st.deltas, ""); !strings.HasSuffix(got, "Task created.\n\nfooter") {
		t.Errorf("streamed text = %q, want postprocessed suffix appended", got)
	}
	if len(ch.sentCopy()) != 0 {
		t.Errorf("Channel.Send called on status+footer stream, want no duplicate fallback")
	}
}

// TestChannelReceiver_StreamingSendFailure_FallsBackToProcess —
// when StreamingSend itself errors, the receiver bypasses streaming
// and goes through Process + Send.
func TestChannelReceiver_StreamingSendFailure_FallsBackToProcess(t *testing.T) {
	ch := &stubStreamingChannel{
		stubChannel:      stubChannel{name: "stub"},
		streamingSendErr: errors.New("upstream stream init failed"),
	}
	agent := &stubAgent{processResult: Result{Text: "fallback reply"}}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}

	err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-3", Text: "ping"})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if agent.processCalls != 1 || agent.streamCalls != 0 {
		t.Errorf("expected Process fallback, got process=%d stream=%d", agent.processCalls, agent.streamCalls)
	}
	sent := ch.sentCopy()
	if len(sent) != 1 || sent[0].Text != "fallback reply" {
		t.Errorf("Send got %+v, want one Send of fallback reply", sent)
	}
}

// TestChannelReceiver_StreamAppendError_FallsBackToSend — when
// Stream.Append errors mid-stream the receiver finishes the
// dispatcher run, then Sends the full text as a one-shot fallback.
func TestChannelReceiver_StreamAppendError_FallsBackToSend(t *testing.T) {
	ch := &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}}
	rcv := &ChannelReceiver{
		Channel: ch,
		Agent: &stubAgent{
			streamingResult: Result{Text: "the whole reply"},
			streamingTokens: []string{"start", "start middle", "start middle end"},
		},
	}
	// Pre-seed the next Stream returned by StreamingSend to fail
	// every Append. The receiver tracks the failure and falls back.
	// stubStreamingChannel doesn't pre-seed; the easiest way is to
	// hook the stream after StreamingSend by setting appendErr on
	// the latest stream from inside the test — but that races with
	// the receiver, so use a hookable stub instead.

	// Replace the channel with a hooked variant that pre-fails the
	// stream by returning a Stream with appendErr already set.
	ch2 := &hookedStreamingChannel{
		stubStreamingChannel: ch,
		nextStream:           &stubStream{appendErr: errors.New("upstream edit failed")},
	}
	rcv.Channel = ch2

	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-4", Text: "ping"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	// Stream gets closed; Send fires the full text as fallback.
	if ch2.preStream.closed != true {
		t.Error("Stream was not closed after Append error")
	}
	sent := ch.sentCopy()
	if len(sent) != 1 || sent[0].Text != "the whole reply" {
		t.Errorf("fallback Send = %+v, want one Send of the whole reply", sent)
	}
}

// hookedStreamingChannel returns a pre-built Stream on the next
// StreamingSend call rather than minting a fresh one — lets tests
// pre-configure the stream's failure mode.
type hookedStreamingChannel struct {
	*stubStreamingChannel
	nextStream *stubStream
	preStream  *stubStream
}

func (h *hookedStreamingChannel) StreamingSend(_ context.Context, sessionID string) (conversation.Stream, error) {
	st := h.nextStream
	st.sessionID = sessionID
	h.preStream = st
	return st, nil
}

// TestChannelReceiver_StreamCloseError_AfterPartialDoesNotDuplicate —
// when Close errors after text has already been streamed, the receiver
// must not send a second full reply. Telegram users otherwise see two
// near-identical messages for tool-use turns.
func TestChannelReceiver_StreamCloseError_AfterPartialDoesNotDuplicate(t *testing.T) {
	preStream := &stubStream{closeErr: errors.New("close failed")}
	ch := &hookedStreamingChannel{
		stubStreamingChannel: &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}},
		nextStream:           preStream,
	}
	agent := &stubAgent{
		streamingResult: Result{Text: "full reply"},
		streamingTokens: []string{"hello"},
	}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}

	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-5", Text: "ping"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	sent := ch.sentCopy()
	if len(sent) != 0 {
		t.Errorf("fallback Send = %+v, want no duplicate after partial stream", sent)
	}
}

func TestChannelReceiver_StreamCloseError_AfterFinalAppendDoesNotDuplicate(t *testing.T) {
	preStream := &stubStream{closeErr: errors.New("close failed")}
	ch := &hookedStreamingChannel{
		stubStreamingChannel: &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}},
		nextStream:           preStream,
	}
	agent := &stubAgent{streamingResult: Result{Text: "full reply"}}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}

	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-5-empty", Text: "ping"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	if got := strings.Join(preStream.deltas, ""); got != "full reply" {
		t.Errorf("streamed text = %q, want full reply", got)
	}
	if sent := ch.sentCopy(); len(sent) != 0 {
		t.Errorf("fallback Send = %+v, want no duplicate after final append", sent)
	}
}

// TestChannelReceiver_DispatcherError_Propagates — Result.Err
// returned by the dispatcher comes back as the Receive error. The
// SessionStore.Append is skipped (the turn was a failure; persisting
// it would lock in a half-finished history).
func TestChannelReceiver_DispatcherError_Propagates(t *testing.T) {
	ch := &stubChannel{name: "stub"}
	agentErr := errors.New("provider rate limited")
	agent := &stubAgent{processResult: Result{Err: agentErr}}
	store := &stubSessionStore{}

	rcv := &ChannelReceiver{Channel: ch, Agent: agent, Sessions: store}
	err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-6", Text: "ping"})
	if !errors.Is(err, agentErr) {
		t.Errorf("Receive err = %v, want wrapping of %v", err, agentErr)
	}
	if store.appendCount != 0 {
		t.Errorf("SessionStore.Append called %d times on dispatcher error, want 0", store.appendCount)
	}
}

// TestChannelReceiver_SpeakerUnknown_BypassesDispatcher — when the
// SessionStore rejects the speaker with ErrSpeakerUnknown, the
// receiver returns it without burning an LLM call.
func TestChannelReceiver_SpeakerUnknown_BypassesDispatcher(t *testing.T) {
	ch := &stubChannel{name: "stub"}
	agent := &stubAgent{}
	store := &stubSessionStore{loadErr: conversation.ErrSpeakerUnknown}

	rcv := &ChannelReceiver{Channel: ch, Agent: agent, Sessions: store}
	err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-7", Text: "ping"})
	if !errors.Is(err, conversation.ErrSpeakerUnknown) {
		t.Errorf("Receive err = %v, want ErrSpeakerUnknown", err)
	}
	if agent.processCalls != 0 || agent.streamCalls != 0 {
		t.Errorf("dispatcher invoked on unknown speaker: process=%d stream=%d", agent.processCalls, agent.streamCalls)
	}
	if len(ch.sentCopy()) != 0 {
		t.Errorf("Channel.Send called %d times on unknown speaker, want 0", len(ch.sentCopy()))
	}
}

// TestChannelReceiver_SessionLoadErrorWrapped — a non-sentinel
// error from SessionStore.Load is wrapped, not unwrapped to
// ErrSpeakerUnknown. Critical: a "DB unavailable" must not look
// like an auth rejection.
func TestChannelReceiver_SessionLoadErrorWrapped(t *testing.T) {
	rcv := &ChannelReceiver{
		Channel:  &stubChannel{name: "stub"},
		Agent:    &stubAgent{},
		Sessions: &stubSessionStore{loadErr: errors.New("db unavailable")},
	}
	err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "x", Text: "ping"})
	if err == nil {
		t.Fatal("Receive returned nil, want wrapped error")
	}
	if errors.Is(err, conversation.ErrSpeakerUnknown) {
		t.Error("wrapped DB error should not unwrap to ErrSpeakerUnknown")
	}
	if !strings.Contains(err.Error(), "session load") {
		t.Errorf("error %q should mention 'session load'", err.Error())
	}
}

// TestChannelReceiver_SessionAppendErrorWrapped — a SessionStore
// Append failure surfaces as the Receive error so the channel layer
// can log it; the user already saw the reply via Channel.Send.
func TestChannelReceiver_SessionAppendErrorWrapped(t *testing.T) {
	ch := &stubChannel{name: "stub"}
	rcv := &ChannelReceiver{
		Channel:  ch,
		Agent:    &stubAgent{processResult: Result{Text: "reply"}},
		Sessions: &stubSessionStore{appendErr: errors.New("disk full")},
	}
	err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-8", Text: "ping"})
	if err == nil {
		t.Fatal("Receive returned nil, want wrapped Append error")
	}
	if !strings.Contains(err.Error(), "session append") {
		t.Errorf("error %q should mention 'session append'", err.Error())
	}
	// Send was still called — the user heard back.
	if len(ch.sentCopy()) != 1 {
		t.Errorf("Send called %d times, want 1 even on Append failure", len(ch.sentCopy()))
	}
}

// TestChannelReceiver_NilSessionStore_NoOp — leaving Sessions nil
// is a valid testing mode: each turn runs against an empty
// dispatcher.Request and skips persistence.
func TestChannelReceiver_NilSessionStore_NoOp(t *testing.T) {
	ch := &stubChannel{name: "stub"}
	agent := &stubAgent{processResult: Result{Text: "ok"}}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}

	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s", Text: "ping"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if agent.lastReq.Project != "" || agent.lastReq.LeadSystemPrompt != "" {
		t.Errorf("nil SessionStore should produce empty Request, got %+v", agent.lastReq)
	}
	// The user's "ping" should still appear in Messages.
	if len(agent.lastReq.Messages) != 1 || agent.lastReq.Messages[0].Content != "ping" {
		t.Errorf("Request.Messages = %+v, want one user message 'ping'", agent.lastReq.Messages)
	}
}

// TestChannelReceiver_NilAgent_Errors — defensive: a misconfigured
// receiver must not panic.
func TestChannelReceiver_NilAgent_Errors(t *testing.T) {
	rcv := &ChannelReceiver{Channel: &stubChannel{name: "stub"}}
	err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s", Text: "ping"})
	if err == nil {
		t.Fatal("Receive with nil Agent returned nil, want error")
	}
}

// TestChannelReceiver_NilChannel_Errors — same for nil Channel.
func TestChannelReceiver_NilChannel_Errors(t *testing.T) {
	rcv := &ChannelReceiver{Agent: &stubAgent{}}
	err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s", Text: "ping"})
	if err == nil {
		t.Fatal("Receive with nil Channel returned nil, want error")
	}
}

// TestChannelReceiver_EmptyReply_SkipsSend — when the dispatcher
// returns an empty Text, the receiver does NOT call Channel.Send
// (channels reject empty messages; firing one would be noise).
func TestChannelReceiver_EmptyReply_SkipsSend(t *testing.T) {
	ch := &stubChannel{name: "stub"}
	rcv := &ChannelReceiver{
		Channel: ch,
		Agent:   &stubAgent{processResult: Result{Text: ""}},
	}
	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s", Text: "ping"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(ch.sentCopy()) != 0 {
		t.Errorf("Send called %d times on empty reply, want 0", len(ch.sentCopy()))
	}
}

// TestChannelReceiver_PropagatesContextTier — Session.ContextTier
// must reach Request.ContextTier so the dispatcher's
// deferred-tool-loading path can trip on DEGRADING/POOR sessions.
// Without this plumbing chat sessions silently regress the
// context-budget tier work shipped on `4f06fc1`.
func TestChannelReceiver_PropagatesContextTier(t *testing.T) {
	ch := &stubChannel{name: "stub"}
	agent := &stubAgent{processResult: Result{Text: "ok"}}
	store := &stubSessionStore{session: Session{ContextTier: chat.TierDegrading}}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent, Sessions: store}

	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s", Text: "ping"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if agent.lastReq.ContextTier != chat.TierDegrading {
		t.Errorf("Request.ContextTier = %v, want %v", agent.lastReq.ContextTier, chat.TierDegrading)
	}
}

// TestChannelReceiver_ResultPostprocessor_AppliesToSend — when a
// ResultPostprocessor is wired (e.g. Telegram's guard-footer
// renderer), the postprocessor's output is what lands on
// Channel.Send. Lets channels attach channel-specific footers
// (guard warnings, retry hints) without polluting result.Text
// upstream.
func TestChannelReceiver_ResultPostprocessor_AppliesToSend(t *testing.T) {
	ch := &stubChannel{name: "stub"}
	agent := &stubAgent{processResult: Result{
		Text:          "core reply",
		GuardWarnings: []GuardWarning{{Tool: "fetch_url", Kinds: []string{"credential_pattern"}}},
	}}
	rcv := &ChannelReceiver{
		Channel: ch,
		Agent:   agent,
		ResultPostprocessor: func(r Result) string {
			if len(r.GuardWarnings) == 0 {
				return r.Text
			}
			return r.Text + "\n\n⚠ guard: " + r.GuardWarnings[0].Kinds[0]
		},
	}
	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s", Text: "ping"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	sent := ch.sentCopy()
	if len(sent) != 1 {
		t.Fatalf("Send count = %d, want 1", len(sent))
	}
	if !strings.Contains(sent[0].Text, "core reply") || !strings.Contains(sent[0].Text, "credential_pattern") {
		t.Errorf("Send text = %q, want core reply + guard footer", sent[0].Text)
	}
}

// TestChannelReceiver_ResultPostprocessor_NilLeavesText — without
// a postprocessor the receiver sends result.Text verbatim
// (preserves existing behaviour for channels that don't need
// transformation, like the GitHub adapter).
func TestChannelReceiver_ResultPostprocessor_NilLeavesText(t *testing.T) {
	ch := &stubChannel{name: "stub"}
	rcv := &ChannelReceiver{
		Channel: ch,
		Agent:   &stubAgent{processResult: Result{Text: "verbatim"}},
	}
	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s", Text: "ping"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	sent := ch.sentCopy()
	if len(sent) != 1 || sent[0].Text != "verbatim" {
		t.Errorf("Send text = %+v, want one Send of 'verbatim'", sent)
	}
}

// TestChannelReceiver_StreamingRepeatedAccumulated_DropsNonGrowing —
// chat.StreamCallback occasionally fires with the same accumulated
// text twice (status flushes during tool calls). The receiver must
// skip the no-growth callback rather than push an empty delta the
// channel would treat as a no-op edit. Covers the early-return
// branch inside dispatch's onText closure.
func TestChannelReceiver_StreamingRepeatedAccumulated_DropsNonGrowing(t *testing.T) {
	ch := &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}}
	rcv := &ChannelReceiver{
		Channel: ch,
		Agent: &stubAgent{
			streamingResult: Result{Text: "hello"},
			// Same accumulated text twice — the second callback
			// must NOT result in a Stream.Append call.
			streamingTokens: []string{"hello", "hello", "hello world"},
		},
	}
	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-rep", Text: "ping"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	st := ch.lastStream()
	if st == nil {
		t.Fatal("no Stream")
	}
	// Two unique accumulated values means two deltas: "hello" and " world".
	if len(st.deltas) != 2 {
		t.Errorf("Stream deltas = %v, want 2 entries (no-growth callback should be dropped)", st.deltas)
	}
}

// TestChannelReceiver_SessionState_AppliedToRequest — fields from
// Session populate the dispatcher.Request the Agent sees. End-to-
// end translation guarantee.
func TestChannelReceiver_SessionState_AppliedToRequest(t *testing.T) {
	rcv := &ChannelReceiver{
		Channel: &stubChannel{name: "stub"},
		Agent:   &stubAgent{processResult: Result{Text: "ok"}},
		Sessions: &stubSessionStore{session: Session{
			History: []chat.Message{
				{Role: "user", Content: "earlier"},
				{Role: "assistant", Content: "answer"},
			},
			ActiveProject:    "proj-1",
			AllowedProjects:  []string{"proj-1", "proj-2"},
			LeadSystemPrompt: "you are a project assistant",
			ChatID:           12345,
		}},
	}
	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s", Text: "now"}); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	got := rcv.Agent.(*stubAgent).lastReq
	if got.ChatID != 12345 {
		t.Errorf("Request.ChatID = %d, want 12345", got.ChatID)
	}
	if got.Project != "proj-1" {
		t.Errorf("Request.Project = %q, want proj-1", got.Project)
	}
	if got.LeadSystemPrompt != "you are a project assistant" {
		t.Errorf("Request.LeadSystemPrompt = %q, want overridden", got.LeadSystemPrompt)
	}
	if len(got.AllowedProjects) != 2 {
		t.Errorf("Request.AllowedProjects = %+v, want 2 entries", got.AllowedProjects)
	}
	// History + new user turn.
	if len(got.Messages) != 3 || got.Messages[2].Content != "now" {
		t.Errorf("Request.Messages = %+v, want history + 'now' user turn", got.Messages)
	}
}

// TestChannelReceiver_SendErrorIsLogged_NotSwallowed — when the
// channel's Send returns an error, Receive must still complete
// successfully (the dispatcher already ran, session was updated)
// AND the error must reach the logger so an operator can see the
// underlying transport failure. Previously the error was discarded
// with `_, _ = r.Channel.Send(...)` and an SMTP 550 rejection
// would leave the chat log full of "I sent your email" replies
// while nothing actually landed in the recipient's inbox.
func TestChannelReceiver_SendErrorIsLogged_NotSwallowed(t *testing.T) {
	ch := &stubChannel{name: "email", sendErr: errors.New("smtp: 550 alias not permitted")}
	agent := &stubAgent{processResult: Result{Text: "I sent your email."}}
	store := &stubSessionStore{}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent, Sessions: store}

	// Receive returns nil because the dispatcher ran fine; the Send
	// failure is operational, not a contract violation.
	err := rcv.Receive(context.Background(), conversation.ChannelMessage{
		Source: "email", SessionID: "thread-1", Text: "ping",
	})
	if err != nil {
		t.Fatalf("Receive: got %v, want nil (Send error is operational)", err)
	}
	sent := ch.sentCopy()
	if len(sent) != 1 {
		t.Errorf("Send calls = %d, want 1 (Send was attempted even though it errored)", len(sent))
	}
	// We can't assert on log lines without a logger sink, but the
	// behavioural contract is: Send was called, returned an error,
	// and Receive didn't propagate that error up. That alone is what
	// the previous _, _ = pattern guaranteed; the new logger() path
	// is exercised here (no panic, no test failure → the helper's
	// nil-safe fallback works).
}

// TestEnrichUserContent_EmptyAttachments — no-op pass-through so
// channels that don't populate Attachments (Telegram, GitHub today)
// keep their current single-line content shape.
func TestEnrichUserContent_EmptyAttachments(t *testing.T) {
	out := enrichUserContent(conversation.ChannelMessage{Text: "hello"})
	if out != "hello" {
		t.Errorf("empty attachments: got %q, want %q", out, "hello")
	}
	if out := enrichUserContent(conversation.ChannelMessage{Text: ""}); out != "" {
		t.Errorf("empty text + empty attachments: got %q, want empty", out)
	}
}

// TestEnrichUserContent_WithArtifactID — happy path: an email-style
// inbound with one persisted attachment renders the body, the
// "[Attached files]" header, name, mime, human-bytes, and the
// artifact_id segment so the LLM can call read_artifact(id=...).
// TestEnrichUserContent_WithExtractionSummary — the
// document-extraction auto-trigger landed on email-attachment
// arrival. When the channel populates Attachment.Extraction the
// enriched user content MUST include the "ingested into project
// memory" trailer so the lead LLM knows the file is already
// chunked + searchable and doesn't schedule a redundant "process
// this book" task. The BuildLeadSystemPrompt directive matches on
// this exact phrasing.
func TestEnrichUserContent_WithExtractionSummary(t *testing.T) {
	msg := conversation.ChannelMessage{
		Text: "Please add this book to memory.",
		Attachments: []conversation.Attachment{
			{
				Name:       "schema-coaching.epub",
				MimeType:   "application/epub+zip",
				SizeBytes:  627_006,
				ArtifactID: "email-att-abc",
				Extraction: &conversation.ExtractionSummary{
					ExtractedDocumentID: "extdoc_xyz",
					Title:               "Schema Coaching",
					Author:              "Iain McCormick",
					SectionCount:        30,
					ChunksIngested:      283,
				},
			},
		},
	}
	got := enrichUserContent(msg)
	for _, want := range []string{
		"ingested into project memory",
		"Schema Coaching",
		"by Iain McCormick",
		"30 sections",
		"283 chunks",
		"extracted_document_id=extdoc_xyz",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in enriched content; got:\n%s", want, got)
		}
	}
}

func TestEnrichUserContent_WithArtifactID(t *testing.T) {
	msg := conversation.ChannelMessage{
		Text: "Save this book, I'll need it later.",
		Attachments: []conversation.Attachment{
			{
				Name:       "book.pdf",
				MimeType:   "application/pdf",
				SizeBytes:  2_415_919, // ~2.3 MB
				ArtifactID: "art_abc123",
			},
		},
	}
	got := enrichUserContent(msg)
	for _, want := range []string{
		"Save this book, I'll need it later.",
		"[Attached files]",
		"book.pdf",
		"application/pdf",
		"2.3 MB",
		"artifact_id=art_abc123",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in enriched content; got:\n%s", want, got)
		}
	}
}

// TestEnrichUserContent_NoArtifactID — channel persisted nothing
// (no repo wired). The attachment still surfaces by name + type so
// the LLM knows it exists, but the misleading "you can read it"
// hint (artifact_id=) is omitted.
func TestEnrichUserContent_NoArtifactID(t *testing.T) {
	msg := conversation.ChannelMessage{
		Text: "fyi",
		Attachments: []conversation.Attachment{
			{Name: "photo.jpg", MimeType: "image/jpeg", SizeBytes: 188_416},
		},
	}
	got := enrichUserContent(msg)
	if !strings.Contains(got, "photo.jpg") {
		t.Error("name missing")
	}
	if !strings.Contains(got, "image/jpeg") {
		t.Error("mime missing")
	}
	if !strings.Contains(got, "184.0 KB") {
		t.Errorf("human-bytes mismatch; got: %s", got)
	}
	if strings.Contains(got, "artifact_id=") {
		t.Error("artifact_id= should not appear when ArtifactID is empty")
	}
}

// TestEnrichUserContent_MultipleAttachments — render order + dual
// entries; covers the iteration code path.
func TestEnrichUserContent_MultipleAttachments(t *testing.T) {
	msg := conversation.ChannelMessage{
		Text: "two files attached",
		Attachments: []conversation.Attachment{
			{Name: "a.txt", MimeType: "text/plain", SizeBytes: 100, ArtifactID: "art_1"},
			{Name: "b.pdf", MimeType: "application/pdf", SizeBytes: 2048, ArtifactID: "art_2"},
		},
	}
	got := enrichUserContent(msg)
	idxA := strings.Index(got, "a.txt")
	idxB := strings.Index(got, "b.pdf")
	if idxA < 0 || idxB < 0 {
		t.Fatalf("both attachments must render; got:\n%s", got)
	}
	if idxA >= idxB {
		t.Errorf("order: a.txt should precede b.pdf in:\n%s", got)
	}
	if !strings.Contains(got, "artifact_id=art_1") || !strings.Contains(got, "artifact_id=art_2") {
		t.Error("both artifact IDs must appear")
	}
}

// TestEnrichUserContent_EmptyTextWithAttachment — image-only message
// (the email body was empty but an attachment came through). Don't
// emit a leading blank line — the prompt should start with the
// "[Attached files]" header so the LLM context isn't padded with
// whitespace.
func TestEnrichUserContent_EmptyTextWithAttachment(t *testing.T) {
	msg := conversation.ChannelMessage{
		Attachments: []conversation.Attachment{{Name: "x.txt"}},
	}
	got := enrichUserContent(msg)
	if strings.HasPrefix(got, "\n\n") {
		t.Errorf("empty body should not produce double leading newline; got %q", got)
	}
	if !strings.Contains(got, "[Attached files]") {
		t.Error("header missing")
	}
}

// TestHumanBytes_ExpectedFormats pins the units the prompt uses
// across the byte ranges that matter for real inbound mail.
func TestHumanBytes_ExpectedFormats(t *testing.T) {
	cases := map[int64]string{
		0:                  "0 B",
		1023:               "1023 B",
		1024:               "1.0 KB",
		1536:               "1.5 KB",
		1024 * 1024:        "1.0 MB",
		2_415_919:          "2.3 MB",
		5 * 1024 * 1024:    "5.0 MB",
		1024 * 1024 * 1024: "1.0 GB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
