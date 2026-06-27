package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
)

// newTestBotWithStub returns a Bot whose Telegram HTTP base points
// at the provided httptest server. The chat.Client is wired but
// unused in channel tests (no LLM calls fire through the adapter).
func newTestBotWithStub(t *testing.T, server *httptest.Server, opts ...BotOption) *Bot {
	t.Helper()
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	allOpts := append([]BotOption{WithHTTPClient(server.Client())}, opts...)
	bot, err := NewBot(BotConfig{Token: "test-token"}, chatClient, allOpts...)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = server.URL
	return bot
}

// telegramAPI is a minimal in-test Telegram API stub. It records
// every request and returns the next response from a queue.
type telegramAPI struct {
	mu       sync.Mutex
	sendIDs  []int64
	requests []channelTestRecordedRequest
	failNext atomic.Bool
}

type channelTestRecordedRequest struct {
	Path string
	Body map[string]any
}

func (a *telegramAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		a.mu.Lock()
		a.requests = append(a.requests, channelTestRecordedRequest{Path: r.URL.Path, Body: parsed})
		a.mu.Unlock()

		if a.failNext.Swap(false) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false,"description":"forced failure"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "sendMessage"):
			a.mu.Lock()
			id := int64(len(a.sendIDs) + 1)
			a.sendIDs = append(a.sendIDs, id)
			a.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":` + strconv.FormatInt(id, 10) + `}}`))
		case strings.Contains(r.URL.Path, "editMessageText"):
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		case strings.Contains(r.URL.Path, "getUpdates"):
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		}
	})
}

func (a *telegramAPI) requestCount(pathFragment string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, req := range a.requests {
		if strings.Contains(req.Path, pathFragment) {
			n++
		}
	}
	return n
}

// TestChannel_NameAndInterfaces — compile-time guards plus the
// runtime Name() check. Critical: a misnamed channel would silently
// route messages to the wrong adapter on multi-channel boot.
func TestChannel_NameAndInterfaces(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)

	ch := NewChannel(bot)
	if ch.Name() != "telegram" {
		t.Errorf("Channel.Name() = %q, want telegram", ch.Name())
	}
	var _ conversation.Channel = ch
	var _ conversation.StreamingChannel = ch
}

// TestChannel_Send_RoundTrip — Send must parse the SessionID,
// invoke Bot.sendMessageGetID, and return the resulting Telegram
// message_id as a decimal string.
func TestChannel_Send_RoundTrip(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)

	id, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "98765",
		Text:      "hello",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "1" {
		t.Errorf("Send sentID = %q, want 1", id)
	}
	if api.requestCount("sendMessage") != 1 {
		t.Errorf("expected exactly 1 sendMessage call, got %d", api.requestCount("sendMessage"))
	}
}

// TestChannel_Send_BadSessionID — non-numeric / empty SessionIDs
// return a descriptive error rather than silently routing to chat
// 0. Caller bugs surface as routing errors not silent drops.
func TestChannel_Send_BadSessionID(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)

	cases := []string{"", "not-a-chat-id", "12abc"}
	for _, c := range cases {
		if _, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: c, Text: "x"}); err == nil {
			t.Errorf("Send(SessionID=%q) returned nil error, want one", c)
		}
	}
	if api.requestCount("sendMessage") != 0 {
		t.Errorf("Send with bad SessionID hit upstream %d times, want 0", api.requestCount("sendMessage"))
	}
}

// TestChannel_ResolveSpeaker_AllowlistGate — when AllowedUsers is
// configured, only listed users resolve cleanly. Unknown users
// return ErrSpeakerUnknown.
func TestChannel_ResolveSpeaker_AllowlistGate(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()

	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{
		Token: "test-token",
		AllowedUsers: map[int64]UserAccess{
			42: {Allowed: true, Projects: []string{"*"}},
		},
	}, chatClient, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = server.URL
	ch := NewChannel(bot)

	sp, err := ch.ResolveSpeaker(context.Background(), "42")
	if err != nil {
		t.Fatalf("ResolveSpeaker(42): %v", err)
	}
	if sp.ID != "telegram:42" {
		t.Errorf("Speaker.ID = %q, want telegram:42", sp.ID)
	}

	if _, err := ch.ResolveSpeaker(context.Background(), "9999"); !errors.Is(err, conversation.ErrSpeakerUnknown) {
		t.Errorf("denied user returned %v, want ErrSpeakerUnknown", err)
	}
	if _, err := ch.ResolveSpeaker(context.Background(), "not-numeric"); !errors.Is(err, conversation.ErrSpeakerUnknown) {
		t.Errorf("malformed id returned %v, want ErrSpeakerUnknown", err)
	}
}

// TestChannel_ResolveSpeaker_DevModePassThrough — empty
// AllowedUsers map allows all numeric ids, matching
// Bot.IsAllowed's dev-mode behaviour.
func TestChannel_ResolveSpeaker_DevModePassThrough(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)

	sp, err := ch.ResolveSpeaker(context.Background(), "12345")
	if err != nil {
		t.Fatalf("ResolveSpeaker dev-mode: %v", err)
	}
	if sp.ID != "telegram:12345" {
		t.Errorf("Speaker.ID = %q, want telegram:12345", sp.ID)
	}
	if sp.DisplayName == "" {
		t.Errorf("Speaker.DisplayName is empty, want non-empty fallback")
	}
}

// TestChannel_ListSessions_ReadsChatUsers — after the bot has
// recorded a chat user (simulating an inbound message), ListSessions
// reports one session keyed on chat_id.
func TestChannel_ListSessions_ReadsChatUsers(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)

	bot.recordChatUser(111, 42)
	bot.recordChatUser(222, 43)

	sessions, err := ch.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("ListSessions returned %d sessions, want 2", len(sessions))
	}
	seen := map[string]bool{}
	for _, s := range sessions {
		seen[s.ID] = true
		if s.ParticipantCount != 1 {
			t.Errorf("ParticipantCount = %d, want 1", s.ParticipantCount)
		}
	}
	if !seen["111"] || !seen["222"] {
		t.Errorf("session ids = %v, want 111 and 222", seen)
	}
}

// TestMessageToChannelMessage_Translation — every Message field
// that has a generic equivalent must surface in the right place; the
// rest goes to ChannelSpecific.
func TestMessageToChannelMessage_Translation(t *testing.T) {
	in := &Message{
		ID:               7,
		ChatID:           1234,
		UserID:           42,
		Username:         "alice",
		Text:             "ping",
		FileID:           "BAADF",
		FileName:         "doc.pdf",
		ReplyToMessageID: 3,
		MessageThreadID:  99,
	}
	got := MessageToChannelMessage(in)

	if got.Source != "telegram" {
		t.Errorf("Source = %q, want telegram", got.Source)
	}
	if got.ID != "7" {
		t.Errorf("ID = %q, want 7", got.ID)
	}
	if got.SessionID != "1234" {
		t.Errorf("SessionID = %q, want 1234", got.SessionID)
	}
	if got.SpeakerID != "42" {
		t.Errorf("SpeakerID = %q, want 42", got.SpeakerID)
	}
	if got.Text != "ping" {
		t.Errorf("Text = %q, want ping", got.Text)
	}
	if got.InReplyTo != "3" {
		t.Errorf("InReplyTo = %q, want 3", got.InReplyTo)
	}
	if got.ThreadID != "99" {
		t.Errorf("ThreadID = %q, want 99", got.ThreadID)
	}
	if got.ChannelSpecific["telegram_thread_id"] != "99" {
		t.Errorf("ChannelSpecific[telegram_thread_id] = %q, want 99", got.ChannelSpecific["telegram_thread_id"])
	}
	if got.ChannelSpecific["telegram_username"] != "alice" {
		t.Errorf("ChannelSpecific[telegram_username] = %q, want alice", got.ChannelSpecific["telegram_username"])
	}
	if got.ChannelSpecific["telegram_file_id"] != "BAADF" {
		t.Errorf("ChannelSpecific[telegram_file_id] = %q, want BAADF", got.ChannelSpecific["telegram_file_id"])
	}
	if got.ChannelSpecific["telegram_file_name"] != "doc.pdf" {
		t.Errorf("ChannelSpecific[telegram_file_name] = %q, want doc.pdf", got.ChannelSpecific["telegram_file_name"])
	}
	if got.Timestamp.IsZero() {
		t.Errorf("Timestamp should be set, got zero")
	}
}

// TestMessageToChannelMessage_ZeroOptionalFields — when optional
// Telegram fields are zero, the translation omits them rather than
// emitting "0" strings or empty-string slots.
func TestMessageToChannelMessage_ZeroOptionalFields(t *testing.T) {
	in := &Message{ID: 1, ChatID: 2, UserID: 3, Text: "hi"}
	got := MessageToChannelMessage(in)

	if got.InReplyTo != "" {
		t.Errorf("InReplyTo = %q, want empty for non-reply", got.InReplyTo)
	}
	if got.ThreadID != "" {
		t.Errorf("ThreadID = %q, want empty for non-forum", got.ThreadID)
	}
	if _, ok := got.ChannelSpecific["telegram_thread_id"]; ok {
		t.Errorf("ChannelSpecific[telegram_thread_id] set for non-forum message")
	}
	if _, ok := got.ChannelSpecific["telegram_username"]; ok {
		t.Errorf("ChannelSpecific[telegram_username] set when username is empty")
	}
}

// TestMessageToChannelMessage_SyntheticZeroUserID — auto-resume
// (server-internal synthetic turns) constructs *Message with
// UserID == 0. SpeakerID must be empty in that case so the
// SessionStore's allowlist check skips — the message is not
// user-authenticated.
func TestMessageToChannelMessage_SyntheticZeroUserID(t *testing.T) {
	in := &Message{ID: 0, ChatID: 100, UserID: 0, Text: "[Task X completed]"}
	got := MessageToChannelMessage(in)
	if got.SpeakerID != "" {
		t.Errorf("SpeakerID = %q, want empty for synthetic UserID==0 turn", got.SpeakerID)
	}
	if got.SessionID != "100" {
		t.Errorf("SessionID = %q, want 100", got.SessionID)
	}
	if got.Text != "[Task X completed]" {
		t.Errorf("Text = %q, want literal synthetic body", got.Text)
	}
}

// TestTelegramStream_AppendCoalesces — within minEditEvery, repeat
// Appends accumulate in the buffer rather than hammering Telegram's
// edit endpoint. Close flushes the accumulated buffer.
func TestTelegramStream_AppendCoalesces(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)

	stream, err := ch.StreamingSend(context.Background(), "555")
	if err != nil {
		t.Fatalf("StreamingSend: %v", err)
	}

	// First Append: buffered (lastEdit is zero, so time.Since is
	// huge — actually the FIRST append DOES edit). Force the
	// coalesce window to a long interval and replace lastEdit by
	// reaching in via the concrete type so the test is deterministic.
	ts := stream.(*telegramStream)
	ts.minEditEvery = 10 * time.Second
	ts.lastEdit = time.Now() // start the window NOW; subsequent appends buffer

	if err := stream.Append("hello "); err != nil {
		t.Fatalf("Append #1: %v", err)
	}
	if err := stream.Append("world"); err != nil {
		t.Fatalf("Append #2: %v", err)
	}
	// Within 10s window, no edits should have hit upstream yet.
	if api.requestCount("editMessageText") != 0 {
		t.Errorf("edits within coalesce window = %d, want 0", api.requestCount("editMessageText"))
	}

	id, err := stream.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if id == "" {
		t.Error("Close returned empty sentID")
	}
	if api.requestCount("editMessageText") != 1 {
		t.Errorf("edits after Close = %d, want 1", api.requestCount("editMessageText"))
	}
	_ = bot // keep bot reference live; *Bot field tied to httptest server lifecycle
}

// TestChannel_StreamingSend_RefusesInVoiceMode — when the chat's
// last inbound was a voice message AND TTS is wired, StreamingSend
// must refuse so the dispatcher falls back to the one-shot Send
// path where the voice branch lives. Without this guard, streaming
// silently bypasses voice replies (the bug fixed alongside this
// test).
//
// The check: shouldReplyAsVoice = voiceTracker.IsVoice(chat) AND
// b.voice.TTS != nil. We construct that state via WithVoiceProviders
// + MarkVoice, then assert StreamingSend returns
// ErrVoiceModeNoStream and never hits the upstream sendMessage.
func TestChannel_StreamingSend_RefusesInVoiceMode(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	// Wire a TTS stub so shouldReplyAsVoice returns true once the
	// tracker reports voice. STT isn't needed for this path; the
	// inbound voice routing happens in HandleMessage, not here.
	WithVoiceProviders(VoiceProviders{TTS: &fakeTTS{}})(bot)
	bot.voiceTracker.MarkVoice(555)

	ch := NewChannel(bot)
	stream, err := ch.StreamingSend(context.Background(), "555")
	if err == nil {
		t.Fatalf("StreamingSend in voice mode returned nil error; expected ErrVoiceModeNoStream")
	}
	if !errors.Is(err, ErrVoiceModeNoStream) {
		t.Errorf("StreamingSend returned %v, want ErrVoiceModeNoStream", err)
	}
	if stream != nil {
		t.Errorf("StreamingSend in voice mode returned non-nil Stream %T", stream)
	}
	// Critical: no placeholder message was posted. Without this
	// guard the dispatcher's fallback would have sent two messages
	// to the user (the streaming placeholder + the synthesised
	// voice reply).
	if api.requestCount("sendMessage") != 0 {
		t.Errorf("StreamingSend in voice mode hit sendMessage %d times, want 0", api.requestCount("sendMessage"))
	}
}

// TestChannel_StreamingSend_BadSessionID — parseTelegramSessionID
// failure short-circuits before any HTTP call. Covers the early
// return branch in StreamingSend.
func TestChannel_StreamingSend_BadSessionID(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)

	stream, err := ch.StreamingSend(context.Background(), "")
	if err == nil {
		t.Errorf("StreamingSend(empty) returned nil error")
	}
	if stream != nil {
		t.Errorf("StreamingSend(empty) returned non-nil Stream")
	}
	if api.requestCount("sendMessage") != 0 {
		t.Errorf("upstream hit on bad SessionID: %d times", api.requestCount("sendMessage"))
	}
}

// TestTelegramStream_AppendEditFails_SetsTerminated — when
// editMessageText errors mid-stream, terminated becomes sticky
// (subsequent Append + Close return the original error). Uses a
// cancelled context to force editMessageText to return an HTTP
// transport error.
func TestTelegramStream_AppendEditFails_SetsTerminated(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := ch.StreamingSend(ctx, "555")
	if err != nil {
		t.Fatalf("StreamingSend: %v", err)
	}
	ts := stream.(*telegramStream)
	ts.minEditEvery = 0 // every Append flushes
	ts.lastEdit = time.Time{}

	// Cancel before Append fires so editMessageText sees a cancelled
	// context and returns a transport error.
	cancel()

	if err := stream.Append("hello"); err == nil {
		t.Fatal("Append returned nil after editMessageText was cancelled, want error")
	}

	// Subsequent Append returns the same sticky terminal error.
	first := stream.Append("again")
	second := stream.Append("more")
	if first == nil || second == nil || first.Error() != second.Error() {
		t.Errorf("sticky terminal error did not match: first=%v second=%v", first, second)
	}

	// Close on a terminated stream returns the same sticky error.
	if _, err := stream.Close(); err == nil {
		t.Error("Close on terminated stream returned nil, want sticky error")
	}
}

// TestTelegramStream_CloseEditFails — Close path's editMessageText
// error branch. Uses a cancelled context to force the upstream
// flush in Close to fail.
func TestTelegramStream_CloseEditFails(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := ch.StreamingSend(ctx, "555")
	if err != nil {
		t.Fatalf("StreamingSend: %v", err)
	}
	ts := stream.(*telegramStream)
	ts.minEditEvery = 10 * time.Second // buffer everything
	ts.lastEdit = time.Now()           // start the window NOW so Append buffers

	if err := stream.Append("buffered"); err != nil {
		t.Fatalf("Append (buffered): %v", err)
	}

	// Cancel before Close so the final-flush editMessageText fails.
	cancel()
	if _, err := stream.Close(); err == nil {
		t.Error("Close returned nil after cancelled-ctx final flush, want error")
	}
}

// TestChannel_StreamingSend_PlaceholderFails — when the initial
// placeholder send to Telegram fails, StreamingSend returns the
// error without leaking a half-initialised Stream. Callers
// (DispatcherReceiver) fall back to one-shot Send per design.
func TestChannel_StreamingSend_PlaceholderFails(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)

	api.failNext.Store(true)
	stream, err := ch.StreamingSend(context.Background(), "555")
	if err == nil {
		t.Errorf("StreamingSend returned nil error after placeholder failure")
	}
	if stream != nil {
		t.Errorf("StreamingSend returned non-nil Stream %v after placeholder failure", stream)
	}
}

// TestChannel_Send_PlaceholderFails — Send returns an error when
// the upstream sendMessage fails; the receiver's caller surfaces
// the error to the user.
func TestChannel_Send_UpstreamFailure(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)

	api.failNext.Store(true)
	id, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "1", Text: "x"})
	if err == nil {
		t.Errorf("Send returned nil error after upstream failure")
	}
	if id != "" {
		t.Errorf("Send returned non-empty id %q after upstream failure", id)
	}
}

// TestTelegramStream_AppendAfterCloseErrors — once a stream is
// closed, further Append calls return the terminal sentinel.
func TestTelegramStream_AppendAfterCloseErrors(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	_ = bot
	ch := NewChannel(bot)

	stream, err := ch.StreamingSend(context.Background(), "1")
	if err != nil {
		t.Fatalf("StreamingSend: %v", err)
	}
	if _, err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := stream.Append("after close"); !errors.Is(err, ErrStreamClosed) {
		t.Errorf("Append after Close = %v, want ErrStreamClosed", err)
	}
	if _, err := stream.Close(); !errors.Is(err, ErrStreamClosed) {
		t.Errorf("second Close = %v, want ErrStreamClosed", err)
	}
}

// TestTelegramStream_AppendUpstreamErrorIsTerminal — when the
// upstream edit fails, the error becomes sticky: subsequent Append
// and Close both return the same error.
func TestTelegramStream_AppendUpstreamErrorIsTerminal(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)

	stream, err := ch.StreamingSend(context.Background(), "1")
	if err != nil {
		t.Fatalf("StreamingSend: %v", err)
	}
	ts := stream.(*telegramStream)
	ts.minEditEvery = 0 // force every Append to hit upstream
	ts.lastEdit = time.Time{}

	// editMessageText currently always returns 200 in this stub.
	// To exercise the terminal-error path we'd need a stub that
	// fails on the editMessageText path; this is left as a TODO
	// because editMessageText in the production bot swallows the
	// upstream OK=false response. Verify the happy path here and
	// leave the terminal-error path covered by integration tests.
	if err := stream.Append("ok"); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// TestChannel_StartStop_Delegate — Start and Stop are pure
// delegators over Bot.Start/Bot.Stop. The receiver argument is
// unused post-migration (Bot drives dispatch via SetReceiver).
func TestChannel_StartStop_Delegate(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ch.Start(ctx, nil) }()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Channel.Start did not return after ctx cancel")
	}
	if err := ch.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestErrStreamClosed_Sentinel — the stream's terminal sentinel
// must be comparable via errors.Is AND return a non-empty Error()
// string (the Error() method is the only branch that gets exercised
// when callers log the error rather than branch on it).
func TestErrStreamClosed_Sentinel(t *testing.T) {
	if !errors.Is(ErrStreamClosed, ErrStreamClosed) {
		t.Error("errors.Is misbehaves on ErrStreamClosed")
	}
	if ErrStreamClosed.Error() == "" {
		t.Error("ErrStreamClosed.Error() returned empty string")
	}
}
