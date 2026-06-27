package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/voice"
)

// fakeSTT records every Transcribe call and returns a canned
// transcript or error. Lets the test assert "the bot routed the
// voice attachment through STT" without depending on the real
// provider.
type fakeSTT struct {
	mu        sync.Mutex
	calls     int
	lastHint  voice.Hint
	lastAudio []byte
	resp      voice.Transcript
	err       error
}

func (f *fakeSTT) Transcribe(_ context.Context, audio io.Reader, hint voice.Hint) (voice.Transcript, error) {
	b, _ := io.ReadAll(audio)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastHint = hint
	f.lastAudio = b
	if f.err != nil {
		return voice.Transcript{}, f.err
	}
	return f.resp, nil
}

// fakeTTS records every Synthesize call. Returns canned audio bytes
// + duration so tests can drive both the happy-path and oversize-cap
// branches.
type fakeTTS struct {
	mu       sync.Mutex
	calls    int
	lastText string
	resp     voice.Audio
	err      error
}

func (f *fakeTTS) Synthesize(_ context.Context, text string, opts voice.TTSOptions) (voice.Audio, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastText = text
	if f.err != nil {
		return voice.Audio{}, f.err
	}
	return f.resp, nil
}

// voiceTestServer builds a httptest server that answers Telegram's
// getFile + file-fetch + sendVoice + sendMessage endpoints. The
// rewritingTransport (already used in download_test.go) routes both
// `api.telegram.org/bot<token>/...` AND `api.telegram.org/file/bot<token>/...`
// at this single server. Records every sendVoice / sendMessage hit
// so tests assert on the outbound path.
type voiceTestServer struct {
	*httptest.Server
	getFileHits   int
	fileFetchHits int
	sendVoiceHits int
	sendVoiceBody []byte
	sendMsgHits   int
	sendMsgText   string
	audioPayload  []byte // bytes the file-fetch endpoint returns
}

func newVoiceTestServer(t *testing.T, audioPayload []byte) *voiceTestServer {
	t.Helper()
	vts := &voiceTestServer{audioPayload: audioPayload}
	// Use a catch-all handler so we don't have to mirror Telegram's
	// `/botTOKEN/...` vs `/file/botTOKEN/...` URL shapes in path
	// patterns. Routing is by endpoint name on the URL path.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/getFile"):
			vts.getFileHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"voice/voice-1.ogg"}}`))
		case strings.Contains(path, "/file/") && strings.HasSuffix(path, "/voice/voice-1.ogg"):
			vts.fileFetchHits++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(vts.audioPayload)
		case strings.HasSuffix(path, "/sendVoice"):
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			vts.sendVoiceHits++
			vts.sendVoiceBody = body
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":999}}`))
		case strings.HasSuffix(path, "/sendMessage"):
			body, _ := io.ReadAll(r.Body)
			var parsed SendMessageRequest
			_ = json.Unmarshal(body, &parsed)
			vts.sendMsgHits++
			vts.sendMsgText = parsed.Text
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":888}}`))
		case strings.HasSuffix(path, "/setMyCommands"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			http.Error(w, "unexpected request path: "+path, http.StatusNotFound)
		}
	})
	vts.Server = httptest.NewServer(handler)
	return vts
}

// newVoiceBot constructs a Bot with a rewritingTransport pointing at
// the given test server, the given STT/TTS providers, and a user 42
// allowlisted. The bot's baseURL is overridden so /bot/* routes hit
// the mux.
func newVoiceBot(t *testing.T, vts *voiceTestServer, providers VoiceProviders) *Bot {
	t.Helper()
	hc := &http.Client{
		Transport: &rewritingTransport{base: http.DefaultTransport, targetHost: vts.URL},
	}
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	cfg := BotConfig{
		Token: "fake-token",
		AllowedUsers: map[int64]UserAccess{
			42: {Allowed: true, Projects: []string{"*"}},
		},
	}
	bot, err := NewBot(cfg, chatClient,
		WithHTTPClient(hc),
		WithVoiceProviders(providers),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	// Telegram's URL builders use baseURL for `/bot/<X>` and the
	// rewriting transport handles `/file/bot/...`. Set baseURL to
	// the test server's URL plus `/bot` so the mux routes line up.
	bot.baseURL = vts.URL + "/bot"
	return bot
}

func TestHandleUpdate_VoiceInbound_RoutesToSTT(t *testing.T) {
	stt := &fakeSTT{
		resp: voice.Transcript{
			Text:       "hello vornik",
			Language:   "en",
			DurationMs: 1500,
			Confidence: 0.91,
		},
	}
	vts := newVoiceTestServer(t, []byte("OggS-fake-payload"))
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{STT: stt})

	rcv := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(rcv)

	upd := &Update{
		Message: &struct {
			ID              int64 `json:"message_id"`
			MessageThreadID int64 `json:"message_thread_id,omitempty"`
			Chat            struct {
				ID int64 `json:"id"`
			}
			From struct {
				ID       int64  `json:"id"`
				Username string `json:"username,omitempty"`
			}
			Text           string              `json:"text"`
			Document       *TelegramDocument   `json:"document,omitempty"`
			Photo          []TelegramPhotoSize `json:"photo,omitempty"`
			Voice          *TelegramVoice      `json:"voice,omitempty"`
			Audio          *TelegramAudio      `json:"audio,omitempty"`
			Caption        string              `json:"caption,omitempty"`
			ReplyToMessage *struct {
				ID int64 `json:"message_id"`
			} `json:"reply_to_message,omitempty"`
		}{
			ID:    7,
			Voice: &TelegramVoice{FileID: "voice-7", Duration: 2, MimeType: "audio/ogg"},
		},
	}
	upd.Message.Chat.ID = 100
	upd.Message.From.ID = 42

	if err := bot.HandleUpdate(context.Background(), upd); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if !rcv.waitReceive(t, 2*time.Second) {
		t.Fatal("Receiver did not fire for voice inbound")
	}

	stt.mu.Lock()
	defer stt.mu.Unlock()
	if stt.calls != 1 {
		t.Fatalf("STT calls = %d, want 1", stt.calls)
	}
	if stt.lastHint.MimeType != "audio/ogg" {
		t.Errorf("STT hint MimeType = %q, want audio/ogg", stt.lastHint.MimeType)
	}
	if string(stt.lastAudio) != "OggS-fake-payload" {
		t.Errorf("STT got %q, want OggS-fake-payload", stt.lastAudio)
	}
	if rcv.last.Text != "hello vornik" {
		t.Errorf("dispatcher Text = %q, want hello vornik", rcv.last.Text)
	}
	if rcv.last.ChannelSpecific["voice.inbound"] != "true" {
		t.Errorf("voice.inbound tag missing; ChannelSpecific=%v", rcv.last.ChannelSpecific)
	}
	if rcv.last.ChannelSpecific["voice.duration_ms"] != "1500" {
		t.Errorf("voice.duration_ms = %q, want 1500", rcv.last.ChannelSpecific["voice.duration_ms"])
	}
	if rcv.last.ChannelSpecific["voice.language"] != "en" {
		t.Errorf("voice.language = %q, want en", rcv.last.ChannelSpecific["voice.language"])
	}
	if !bot.voiceTracker.IsVoice(100) {
		t.Errorf("voiceTracker for chat 100 not marked")
	}
}

func TestHandleUpdate_VoiceInbound_STTFailReplies(t *testing.T) {
	stt := &fakeSTT{err: errors.New("model unavailable")}
	vts := newVoiceTestServer(t, []byte("OggS-fake"))
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{STT: stt})

	rcv := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(rcv)

	upd := &Update{
		Message: &struct {
			ID              int64 `json:"message_id"`
			MessageThreadID int64 `json:"message_thread_id,omitempty"`
			Chat            struct {
				ID int64 `json:"id"`
			}
			From struct {
				ID       int64  `json:"id"`
				Username string `json:"username,omitempty"`
			}
			Text           string              `json:"text"`
			Document       *TelegramDocument   `json:"document,omitempty"`
			Photo          []TelegramPhotoSize `json:"photo,omitempty"`
			Voice          *TelegramVoice      `json:"voice,omitempty"`
			Audio          *TelegramAudio      `json:"audio,omitempty"`
			Caption        string              `json:"caption,omitempty"`
			ReplyToMessage *struct {
				ID int64 `json:"message_id"`
			} `json:"reply_to_message,omitempty"`
		}{
			ID:    8,
			Voice: &TelegramVoice{FileID: "voice-8", Duration: 2},
		},
	}
	upd.Message.Chat.ID = 100
	upd.Message.From.ID = 42

	if err := bot.HandleUpdate(context.Background(), upd); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	// Dispatcher must NOT have fired.
	if rcv.waitReceive(t, 200*time.Millisecond) {
		t.Errorf("Receiver fired despite STT failure")
	}
	// Bot must have sent a humane error via sendMessage.
	if vts.sendMsgHits != 1 {
		t.Errorf("sendMessage hits = %d, want 1 humane-error reply", vts.sendMsgHits)
	}
	if !strings.Contains(vts.sendMsgText, "couldn't make out") && !strings.Contains(vts.sendMsgText, "couldn't fetch") {
		t.Errorf("humane error text = %q", vts.sendMsgText)
	}
}

func TestHandleUpdate_AudioFile_AlsoTranscribed(t *testing.T) {
	stt := &fakeSTT{resp: voice.Transcript{Text: "audio file said this", DurationMs: 800}}
	vts := newVoiceTestServer(t, []byte("ID3-fake-mp3"))
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{STT: stt})

	rcv := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(rcv)

	upd := &Update{
		Message: &struct {
			ID              int64 `json:"message_id"`
			MessageThreadID int64 `json:"message_thread_id,omitempty"`
			Chat            struct {
				ID int64 `json:"id"`
			}
			From struct {
				ID       int64  `json:"id"`
				Username string `json:"username,omitempty"`
			}
			Text           string              `json:"text"`
			Document       *TelegramDocument   `json:"document,omitempty"`
			Photo          []TelegramPhotoSize `json:"photo,omitempty"`
			Voice          *TelegramVoice      `json:"voice,omitempty"`
			Audio          *TelegramAudio      `json:"audio,omitempty"`
			Caption        string              `json:"caption,omitempty"`
			ReplyToMessage *struct {
				ID int64 `json:"message_id"`
			} `json:"reply_to_message,omitempty"`
		}{
			ID:    9,
			Audio: &TelegramAudio{FileID: "audio-9", Duration: 3, MimeType: "audio/mpeg", FileName: "song.mp3"},
		},
	}
	upd.Message.Chat.ID = 100
	upd.Message.From.ID = 42

	if err := bot.HandleUpdate(context.Background(), upd); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if !rcv.waitReceive(t, 2*time.Second) {
		t.Fatal("Receiver did not fire for audio file")
	}
	if stt.lastHint.MimeType != "audio/mpeg" {
		t.Errorf("hint MimeType = %q, want audio/mpeg", stt.lastHint.MimeType)
	}
	if rcv.last.Text != "audio file said this" {
		t.Errorf("Text = %q, want audio file said this", rcv.last.Text)
	}
}

func TestHandleUpdate_TextAfterVoice_ClearsTracker(t *testing.T) {
	stt := &fakeSTT{resp: voice.Transcript{Text: "first voice"}}
	vts := newVoiceTestServer(t, []byte("OggS"))
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{STT: stt})
	rcv := &handleMessageReceiver{done: make(chan struct{}, 4)}
	bot.SetReceiver(rcv)

	// First: voice inbound marks tracker.
	upd := &Update{Message: &struct {
		ID              int64 `json:"message_id"`
		MessageThreadID int64 `json:"message_thread_id,omitempty"`
		Chat            struct {
			ID int64 `json:"id"`
		}
		From struct {
			ID       int64  `json:"id"`
			Username string `json:"username,omitempty"`
		}
		Text           string              `json:"text"`
		Document       *TelegramDocument   `json:"document,omitempty"`
		Photo          []TelegramPhotoSize `json:"photo,omitempty"`
		Voice          *TelegramVoice      `json:"voice,omitempty"`
		Audio          *TelegramAudio      `json:"audio,omitempty"`
		Caption        string              `json:"caption,omitempty"`
		ReplyToMessage *struct {
			ID int64 `json:"message_id"`
		} `json:"reply_to_message,omitempty"`
	}{ID: 11, Voice: &TelegramVoice{FileID: "v-11"}}}
	upd.Message.Chat.ID = 100
	upd.Message.From.ID = 42
	if err := bot.HandleUpdate(context.Background(), upd); err != nil {
		t.Fatalf("HandleUpdate voice: %v", err)
	}
	_ = rcv.waitReceive(t, time.Second)
	if !bot.voiceTracker.IsVoice(100) {
		t.Fatal("tracker not set after voice inbound")
	}

	// Second: plain text. Tracker should clear.
	if err := bot.HandleMessage(context.Background(), &Message{
		ID: 12, ChatID: 100, UserID: 42, Text: "typed correction",
	}); err != nil {
		t.Fatalf("HandleMessage text: %v", err)
	}
	if bot.voiceTracker.IsVoice(100) {
		t.Error("tracker not cleared by plain-text inbound")
	}
}

func TestChannelSend_VoiceInbound_RoutesThroughTTS(t *testing.T) {
	tts := &fakeTTS{
		resp: voice.Audio{
			Bytes:        []byte("OggS-synth"),
			MimeType:     "audio/ogg",
			DurationMs:   3000,
			SampleRateHz: 48000,
		},
	}
	vts := newVoiceTestServer(t, nil)
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{TTS: tts})
	bot.voiceTracker = newVoiceInboundTracker()
	bot.voiceTracker.MarkVoice(100)

	ch := NewChannel(bot)
	sentID, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "100",
		Text:      "hello back",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sentID != "999" {
		t.Errorf("sentID = %q, want 999 (sendVoice mock)", sentID)
	}
	if tts.calls != 1 {
		t.Errorf("TTS calls = %d, want 1", tts.calls)
	}
	if tts.lastText != "hello back" {
		t.Errorf("TTS text = %q", tts.lastText)
	}
	if vts.sendVoiceHits != 1 {
		t.Errorf("sendVoice hits = %d, want 1", vts.sendVoiceHits)
	}
	if vts.sendMsgHits != 0 {
		t.Errorf("sendMessage hits = %d, want 0 (voice path used)", vts.sendMsgHits)
	}
	if !strings.Contains(string(vts.sendVoiceBody), "OggS-synth") {
		t.Errorf("sendVoice multipart body missing synthesised payload")
	}
	if !strings.Contains(string(vts.sendVoiceBody), "100") {
		t.Errorf("sendVoice body missing chat_id 100")
	}
}

func TestChannelSend_VoiceInbound_OversizeAudioFallsBackToText(t *testing.T) {
	tts := &fakeTTS{
		resp: voice.Audio{
			Bytes:      []byte("OggS-too-long"),
			DurationMs: 120_000, // 2 min — exceeds Telegram's 60 s cap
		},
	}
	vts := newVoiceTestServer(t, nil)
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{TTS: tts})
	bot.voiceTracker = newVoiceInboundTracker()
	bot.voiceTracker.MarkVoice(100)

	ch := NewChannel(bot)
	sentID, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "100",
		Text:      "oversize reply",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sentID != "888" {
		t.Errorf("sentID = %q, want 888 (sendMessage mock)", sentID)
	}
	if vts.sendVoiceHits != 0 {
		t.Errorf("sendVoice hits = %d, want 0 (oversize must fall back to text)", vts.sendVoiceHits)
	}
	if vts.sendMsgHits != 1 {
		t.Errorf("sendMessage hits = %d, want 1", vts.sendMsgHits)
	}
}

func TestChannelSend_VoiceInbound_TTSErrorFallsBackToText(t *testing.T) {
	tts := &fakeTTS{err: voice.ErrOversizeText}
	vts := newVoiceTestServer(t, nil)
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{TTS: tts})
	bot.voiceTracker = newVoiceInboundTracker()
	bot.voiceTracker.MarkVoice(100)

	ch := NewChannel(bot)
	sentID, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "100",
		Text:      "synth-fail reply",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if vts.sendMsgHits != 1 {
		t.Errorf("sendMessage hits = %d, want 1 fallback", vts.sendMsgHits)
	}
	if vts.sendVoiceHits != 0 {
		t.Errorf("sendVoice hits = %d, want 0", vts.sendVoiceHits)
	}
	if sentID != "888" {
		t.Errorf("sentID = %q, want 888", sentID)
	}
}

func TestChannelSend_NoVoiceTracker_TextOnly(t *testing.T) {
	tts := &fakeTTS{}
	vts := newVoiceTestServer(t, nil)
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{TTS: tts})
	// Don't mark the tracker — chat 100 is text-only.
	ch := NewChannel(bot)
	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "100",
		Text:      "text only",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if tts.calls != 0 {
		t.Errorf("TTS calls = %d, want 0 (no voice tracker)", tts.calls)
	}
	if vts.sendMsgHits != 1 {
		t.Errorf("sendMessage hits = %d, want 1", vts.sendMsgHits)
	}
}

func TestVoiceInboundTracker_MarkAndClear(t *testing.T) {
	tr := newVoiceInboundTracker()
	if tr.IsVoice(1) {
		t.Errorf("zero tracker reports voice for 1")
	}
	tr.MarkVoice(1)
	if !tr.IsVoice(1) {
		t.Errorf("MarkVoice(1) didn't take")
	}
	tr.MarkText(1)
	if tr.IsVoice(1) {
		t.Errorf("MarkText(1) didn't clear")
	}
}

func TestDetectVoiceAttachment(t *testing.T) {
	cases := []struct {
		name     string
		voice    *TelegramVoice
		audio    *TelegramAudio
		wantFID  string
		wantName string
		wantMime string
	}{
		{"voice present", &TelegramVoice{FileID: "v", MimeType: "audio/ogg"}, nil, "v", "voice.ogg", "audio/ogg"},
		{"voice default mime", &TelegramVoice{FileID: "v"}, nil, "v", "voice.ogg", "audio/ogg"},
		{"audio present", nil, &TelegramAudio{FileID: "a", MimeType: "audio/mp4", FileName: "song.m4a"}, "a", "song.m4a", "audio/mp4"},
		{"audio default mime", nil, &TelegramAudio{FileID: "a"}, "a", "audio", "audio/mpeg"},
		{"voice takes precedence", &TelegramVoice{FileID: "v"}, &TelegramAudio{FileID: "a"}, "v", "voice.ogg", "audio/ogg"},
		{"neither present", nil, nil, "", "", ""},
		{"empty voice id ignored", &TelegramVoice{}, nil, "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotID, gotName, gotHint := detectVoiceAttachment(c.voice, c.audio)
			if gotID != c.wantFID || gotName != c.wantName || gotHint.MimeType != c.wantMime {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)", gotID, gotName, gotHint.MimeType, c.wantFID, c.wantName, c.wantMime)
			}
		})
	}
}

func TestSendVoice_EmptyAudio(t *testing.T) {
	vts := newVoiceTestServer(t, nil)
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{})
	_, err := bot.sendVoice(context.Background(), 100, voice.Audio{})
	if err == nil || !strings.Contains(err.Error(), "empty audio") {
		t.Errorf("err = %v, want empty-audio error", err)
	}
}

func TestSendVoice_PostsMultipart(t *testing.T) {
	vts := newVoiceTestServer(t, nil)
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{})
	mid, err := bot.sendVoice(context.Background(), 100, voice.Audio{
		Bytes:      []byte("OggS-some-bytes"),
		DurationMs: 5000,
	})
	if err != nil {
		t.Fatalf("sendVoice: %v", err)
	}
	if mid != 999 {
		t.Errorf("message_id = %d, want 999", mid)
	}
	if !bytes.Contains(vts.sendVoiceBody, []byte("OggS-some-bytes")) {
		t.Errorf("multipart body missing audio bytes")
	}
	if !bytes.Contains(vts.sendVoiceBody, []byte("duration")) {
		t.Errorf("multipart body missing duration field")
	}
}

func TestSendVoice_ServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/bot/sendVoice", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"description":"FILE_PART_TOO_BIG"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	hc := &http.Client{Transport: &rewritingTransport{base: http.DefaultTransport, targetHost: srv.URL}}
	bot, err := NewBot(BotConfig{Token: "t"}, chatClient, WithHTTPClient(hc))
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = srv.URL + "/bot"
	_, err = bot.sendVoice(context.Background(), 100, voice.Audio{Bytes: []byte("OggS-x")})
	if err == nil || !strings.Contains(err.Error(), "sendVoice HTTP 400") {
		t.Errorf("err = %v, want sendVoice HTTP error", err)
	}
}

func TestVoiceImportHint(t *testing.T) {
	in := voiceHint{MimeType: "audio/ogg", SampleRateHz: 48000}
	out := voiceImportHint(in)
	if out.MimeType != "audio/ogg" || out.SampleRateHz != 48000 {
		t.Errorf("voiceImportHint dropped fields: %+v", out)
	}
}

func TestMessageToChannelMessage_VoiceTagsStamped(t *testing.T) {
	msg := &Message{
		ID:      7,
		ChatID:  100,
		UserID:  42,
		Text:    "transcribed body",
		IsVoice: true,
		VoiceTranscript: voiceTranscript{
			Text:       "transcribed body",
			Language:   "en",
			DurationMs: 1234,
			Confidence: 0.87,
		},
	}
	cm := MessageToChannelMessage(msg)
	if cm.ChannelSpecific["voice.inbound"] != "true" {
		t.Errorf("voice.inbound missing: %v", cm.ChannelSpecific)
	}
	if cm.ChannelSpecific["voice.language"] != "en" {
		t.Errorf("voice.language = %q", cm.ChannelSpecific["voice.language"])
	}
	if cm.ChannelSpecific["voice.duration_ms"] != "1234" {
		t.Errorf("voice.duration_ms = %q", cm.ChannelSpecific["voice.duration_ms"])
	}
	if !strings.HasPrefix(cm.ChannelSpecific["voice.transcript_confidence"], "0.87") {
		t.Errorf("voice.transcript_confidence = %q", cm.ChannelSpecific["voice.transcript_confidence"])
	}
}

func TestWithVoiceProviders_AppliesDefaultCap(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "t"}, chatClient, WithVoiceProviders(VoiceProviders{}))
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	if bot.voice.MaxOutboundDuration != telegramVoiceMaxDurationMs {
		t.Errorf("MaxOutboundDuration = %d, want %d (default)",
			bot.voice.MaxOutboundDuration, telegramVoiceMaxDurationMs)
	}
}

func TestWithVoiceProviders_RespectsCustomCap(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "t"}, chatClient,
		WithVoiceProviders(VoiceProviders{MaxOutboundDuration: 30_000}))
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	if bot.voice.MaxOutboundDuration != 30_000 {
		t.Errorf("MaxOutboundDuration = %d, want 30000", bot.voice.MaxOutboundDuration)
	}
}

func TestVoiceInboundTracker_NilSafe(t *testing.T) {
	var tr *voiceInboundTracker
	if tr.IsVoice(1) {
		t.Errorf("nil tracker returned true; want nil-safe false")
	}
}

func TestHandleVoiceAttachment_NoSTT(t *testing.T) {
	vts := newVoiceTestServer(t, nil)
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{})
	msg := &Message{ChatID: 100, FileID: "v-1", IsVoice: true}
	humane, ok := bot.handleVoiceAttachment(context.Background(), msg, voice.Hint{})
	if ok {
		t.Errorf("ok = true with no STT wired; want false")
	}
	if humane != "" {
		t.Errorf("humane = %q; want empty (no STT means caller falls through)", humane)
	}
}

func TestHandleVoiceAttachment_NoFileID(t *testing.T) {
	stt := &fakeSTT{}
	vts := newVoiceTestServer(t, nil)
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{STT: stt})
	msg := &Message{ChatID: 100, IsVoice: true}
	humane, ok := bot.handleVoiceAttachment(context.Background(), msg, voice.Hint{})
	if ok || humane != "" {
		t.Errorf("ok=%v humane=%q; want false / empty", ok, humane)
	}
	if stt.calls != 0 {
		t.Errorf("STT.calls = %d; want 0", stt.calls)
	}
}

func TestHandleVoiceAttachment_GetFileError(t *testing.T) {
	// Build a server that 500s on getFile so the bot's getFile
	// returns an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	hc := &http.Client{Transport: &rewritingTransport{base: http.DefaultTransport, targetHost: srv.URL}}
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "t"}, chatClient,
		WithHTTPClient(hc),
		WithVoiceProviders(VoiceProviders{STT: &fakeSTT{}}))
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = srv.URL + "/bot"
	msg := &Message{ChatID: 100, FileID: "v-1", IsVoice: true}
	humane, ok := bot.handleVoiceAttachment(context.Background(), msg, voice.Hint{})
	if ok {
		t.Errorf("ok = true on getFile failure; want false")
	}
	if !strings.Contains(humane, "couldn't fetch") {
		t.Errorf("humane = %q; want fetch-failure message", humane)
	}
}

func TestHandleVoiceAttachment_FetchBytesError(t *testing.T) {
	// getFile succeeds but file fetch 500s.
	mux := http.NewServeMux()
	mux.HandleFunc("/bot/getFile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"voice/x.ogg"}}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// All other paths (file fetch) → 500
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	hc := &http.Client{Transport: &rewritingTransport{base: http.DefaultTransport, targetHost: srv.URL}}
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "t"}, chatClient,
		WithHTTPClient(hc),
		WithVoiceProviders(VoiceProviders{STT: &fakeSTT{}}))
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = srv.URL + "/bot"
	msg := &Message{ChatID: 100, FileID: "v-x", IsVoice: true}
	humane, ok := bot.handleVoiceAttachment(context.Background(), msg, voice.Hint{})
	if ok {
		t.Errorf("ok = true on fetch failure; want false")
	}
	if !strings.Contains(humane, "couldn't fetch") {
		t.Errorf("humane = %q; want fetch-failure message", humane)
	}
}

func TestSendVoiceReply_NoTTS(t *testing.T) {
	vts := newVoiceTestServer(t, nil)
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{}) // no TTS
	sentID, used, err := bot.sendVoiceReply(context.Background(), 100, "hi")
	if err != nil || used || sentID != "" {
		t.Errorf("sendVoiceReply(no TTS) returned id=%q used=%v err=%v; want zero values", sentID, used, err)
	}
}

// keep io referenced (fakeSTT consumes the reader).
var _ = fmt.Sprintf
