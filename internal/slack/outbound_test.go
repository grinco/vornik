package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
)

// slackStub captures every request to chat.postMessage so tests can
// inspect what the channel sent. The handler is parameterised on a
// `respond` callback so tests vary success / 429 / non-200 / ok:false
// without writing a new stub server per case.
type slackStub struct {
	t        *testing.T
	calls    atomic.Int64
	requests []*http.Request
	bodies   []string
	respond  func(w http.ResponseWriter, r *http.Request, body []byte)
	mu       sync.Mutex
}

func newSlackStub(t *testing.T) *slackStub {
	t.Helper()
	return &slackStub{t: t}
}

func (s *slackStub) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.calls.Add(1)
	s.requests = append(s.requests, r)
	s.bodies = append(s.bodies, string(body))
	resp := s.respond
	s.mu.Unlock()
	if resp != nil {
		resp(w, r, body)
		return
	}
	// Default: success response.
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"ts":"1700000099.000200","channel":"C_general"}`))
}

func (s *slackStub) snapshotBody(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.bodies) {
		return ""
	}
	return s.bodies[i]
}

// outboundChannel constructs a channel pointed at the supplied stub
// server's URL. Single-installation mode with BotToken set so
// outbound is configured.
func outboundChannel(t *testing.T, stub *slackStub) *Channel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(srv.Close)
	cfg := Config{
		SigningSecret:    "shhh",
		BotToken:         "xoxb-test-token",
		TeamID:           "T123",
		TeamAllowlist:    []string{"T123"},
		APIBaseURL:       srv.URL,
		HTTPClient:       srv.Client(),
		PostMessageRPS:   0, // disable per-channel limiter for the happy-path tests; enabled in dedicated test
		PostMessageBurst: 0,
	}
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ch
}

// TestSendChatPostMessage_HappyPath — a configured channel sends to
// the stub and returns the upstream ts.
func TestSendChatPostMessage_HappyPath(t *testing.T) {
	stub := newSlackStub(t)
	ch := outboundChannel(t, stub)

	ts, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "hello",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ts != "1700000099.000200" {
		t.Errorf("Send returned ts = %q, want 1700000099.000200", ts)
	}
	if stub.calls.Load() != 1 {
		t.Errorf("stub call count = %d, want 1", stub.calls.Load())
	}
	body := stub.snapshotBody(0)
	var got chatPostMessageRequest
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode stub body: %v\nbody=%s", err, body)
	}
	if got.Channel != "C_general" {
		t.Errorf("Channel = %q, want C_general", got.Channel)
	}
	if got.Text != "hello" {
		t.Errorf("Text = %q, want hello", got.Text)
	}
	if got.ThreadTs != "1700000010.000100" {
		t.Errorf("ThreadTs = %q, want 1700000010.000100", got.ThreadTs)
	}
}

// TestSendChatPostMessage_SendsBearerAuth — the bot token rides in
// the Authorization header per Slack's docs.
func TestSendChatPostMessage_SendsBearerAuth(t *testing.T) {
	stub := newSlackStub(t)
	ch := outboundChannel(t, stub)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "hello",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.requests) == 0 {
		t.Fatal("no requests captured")
	}
	got := stub.requests[0].Header.Get("Authorization")
	if got != "Bearer xoxb-test-token" {
		t.Errorf("Authorization = %q, want Bearer xoxb-test-token", got)
	}
}

// TestSendChatPostMessage_UnconfiguredReturnsSentinel — a channel
// with no bot token surfaces ErrOutboundNotConfigured so callers
// can branch via errors.Is.
func TestSendChatPostMessage_UnconfiguredReturnsSentinel(t *testing.T) {
	cfg := validConfig()
	cfg.BotToken = ""
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "hi",
	})
	if !errors.Is(err, ErrOutboundNotConfigured) {
		t.Errorf("Send err = %v, want ErrOutboundNotConfigured", err)
	}
}

// TestSendChatPostMessage_EmptyTextRejected — defensive.
func TestSendChatPostMessage_EmptyTextRejected(t *testing.T) {
	stub := newSlackStub(t)
	ch := outboundChannel(t, stub)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
	})
	if err == nil {
		t.Fatal("Send with empty text returned nil, want error")
	}
	if stub.calls.Load() != 0 {
		t.Errorf("empty-text Send hit the wire %d times, want 0", stub.calls.Load())
	}
}

// TestSendChatPostMessage_MalformedSessionID — bad SessionID
// surfaces as ErrUnknownSession.
func TestSendChatPostMessage_MalformedSessionID(t *testing.T) {
	stub := newSlackStub(t)
	ch := outboundChannel(t, stub)
	cases := []string{
		"no-separators",
		"only/slash",
		"#only-hash",
		"T123/C_general#",
	}
	for _, sid := range cases {
		t.Run(sid, func(t *testing.T) {
			_, err := ch.Send(context.Background(), conversation.ChannelMessage{
				SessionID: sid,
				Text:      "hi",
			})
			if !errors.Is(err, ErrUnknownSession) {
				t.Errorf("Send(%q) err = %v, want ErrUnknownSession", sid, err)
			}
		})
	}
}

// TestSendChatPostMessage_UnknownTeam — SessionID parses but team
// isn't on the installations map.
func TestSendChatPostMessage_UnknownTeam(t *testing.T) {
	stub := newSlackStub(t)
	ch := outboundChannel(t, stub)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T_unknown/C_general#1700000010.000100",
		Text:      "hi",
	})
	if !errors.Is(err, ErrUnknownSession) {
		t.Errorf("Send unknown team err = %v, want ErrUnknownSession", err)
	}
}

// TestSendChatPostMessage_429RateLimited — Slack returns 429 with a
// Retry-After header; the channel surfaces a RateLimitedError that
// callers can errors.As to read the retry hint.
func TestSendChatPostMessage_429RateLimited(t *testing.T) {
	stub := newSlackStub(t)
	stub.respond = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
	}
	ch := outboundChannel(t, stub)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "hi",
	})
	var rle *RateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("Send err = %v, want *RateLimitedError", err)
	}
	if rle.RetryAfter != 3*time.Second {
		t.Errorf("RetryAfter = %s, want 3s", rle.RetryAfter)
	}
}

// TestSendChatPostMessage_429NoRetryAfter — defensive: a 429 with
// no Retry-After (or malformed) returns RetryAfter=0 so the caller
// gets an unbounded "retry asap" signal rather than crashing.
func TestSendChatPostMessage_429NoRetryAfter(t *testing.T) {
	stub := newSlackStub(t)
	stub.respond = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
	}
	ch := outboundChannel(t, stub)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "hi",
	})
	var rle *RateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("Send err = %v, want *RateLimitedError", err)
	}
	if rle.RetryAfter != 0 {
		t.Errorf("RetryAfter = %s, want 0 (no header)", rle.RetryAfter)
	}
}

// TestSendChatPostMessage_NonOKResponse — Slack returns HTTP 200 +
// ok:false (e.g. channel_not_found). The channel surfaces the
// machine-readable error code so operators don't have to parse
// log lines.
func TestSendChatPostMessage_NonOKResponse(t *testing.T) {
	stub := newSlackStub(t)
	stub.respond = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
	}
	ch := outboundChannel(t, stub)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "hi",
	})
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("Send err = %v, want one mentioning channel_not_found", err)
	}
}

// TestSendChatPostMessage_HTTP500 — non-200 / non-429 surfaces as a
// generic error so callers can decide whether to retry.
func TestSendChatPostMessage_HTTP500(t *testing.T) {
	stub := newSlackStub(t)
	stub.respond = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream boom"))
	}
	ch := outboundChannel(t, stub)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "hi",
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("Send err = %v, want one mentioning HTTP 500", err)
	}
}

// TestSendChatPostMessage_RateLimit_Local — with PostMessageRPS=1
// burst=1 the second Send within the same second hits the local
// bucket and returns a RateLimitedError without touching the wire.
func TestSendChatPostMessage_RateLimit_Local(t *testing.T) {
	stub := newSlackStub(t)
	srv := httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(srv.Close)
	cfg := Config{
		SigningSecret:    "shhh",
		BotToken:         "xoxb-tok",
		TeamID:           "T123",
		TeamAllowlist:    []string{"T123"},
		APIBaseURL:       srv.URL,
		HTTPClient:       srv.Client(),
		PostMessageRPS:   1,
		PostMessageBurst: 1,
	}
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Pin the clock so both Send calls see the same `now` for the
	// keybucket. The first consumes the only token; the second is
	// blocked.
	now := time.Unix(1700000000, 0)
	ch.clock = func() time.Time { return now }

	msg := conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "first",
	}
	if _, err := ch.Send(context.Background(), msg); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	msg.Text = "second"
	_, err = ch.Send(context.Background(), msg)
	var rle *RateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("second Send err = %v, want *RateLimitedError", err)
	}
	// Only the first Send should have hit the wire.
	if stub.calls.Load() != 1 {
		t.Errorf("stub call count = %d, want 1 (second was rate-limited locally)", stub.calls.Load())
	}
}

// TestSendChatPostMessage_RateLimit_DisabledRPS — PostMessageRPS<=0
// disables the in-process limiter so an operator who trusts the
// upstream relay can skip it.
func TestSendChatPostMessage_RateLimit_DisabledRPS(t *testing.T) {
	stub := newSlackStub(t)
	srv := httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(srv.Close)
	cfg := Config{
		SigningSecret:    "shhh",
		BotToken:         "xoxb-tok",
		TeamID:           "T123",
		TeamAllowlist:    []string{"T123"},
		APIBaseURL:       srv.URL,
		HTTPClient:       srv.Client(),
		PostMessageRPS:   0, // 0 == use the default Tier-3 cap (1/sec)
		PostMessageBurst: 0,
	}
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Explicitly disable the per-channel limiter to exercise the
	// "operator opts out" path. Setting postMessageRPS<=0 post-New
	// avoids the constructor's defaulting; production wiring would
	// use a future explicit "Disabled" flag rather than the magic
	// non-positive value, but the runtime gate is what we're
	// exercising here.
	ch.postMessageRPS = -1
	ch.postMessageBurst = -1
	msg := conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "first",
	}
	for i := 0; i < 5; i++ {
		if _, err := ch.Send(context.Background(), msg); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	if got := stub.calls.Load(); got != 5 {
		t.Errorf("stub call count = %d, want 5 (no in-process gate)", got)
	}
}

// TestSendChatPostMessage_MissingTsInOK — defensive: an ok:true
// response that doesn't carry a ts is a Slack contract violation;
// surface it.
func TestSendChatPostMessage_MissingTsInOK(t *testing.T) {
	stub := newSlackStub(t)
	stub.respond = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
	ch := outboundChannel(t, stub)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "hi",
	})
	if err == nil || !strings.Contains(err.Error(), "missing ts") {
		t.Errorf("Send err = %v, want one mentioning missing ts", err)
	}
}

// TestSendChatPostMessage_HTTPClientFailure — network-layer error
// surfaces as a wrapped chat.postMessage request error.
func TestSendChatPostMessage_HTTPClientFailure(t *testing.T) {
	cfg := validConfig()
	cfg.APIBaseURL = "http://127.0.0.1:1" // closed port
	cfg.HTTPClient = &http.Client{Timeout: 50 * time.Millisecond}
	cfg.BotToken = "xoxb-tok"
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "hi",
	})
	if err == nil {
		t.Fatal("Send to closed port returned nil error, want one")
	}
}

// TestRateLimitedError_Error — humans read the message; lock the
// format so a future "let's emit JSON" refactor surfaces in
// reviews.
func TestRateLimitedError_Error(t *testing.T) {
	e := &RateLimitedError{RetryAfter: 5 * time.Second, Body: "bucket empty"}
	got := e.Error()
	if !strings.Contains(got, "5s") || !strings.Contains(got, "bucket empty") {
		t.Errorf("Error() = %q, want one mentioning the wait and the cause", got)
	}
}

// TestParseRetryAfter — covers the parser's edge cases in one
// table-driven pass.
func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"3", 3 * time.Second},
		{" 30 ", 30 * time.Second},
		{"-5", 0},
		{"abc", 0},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			got := parseRetryAfter(tc.in)
			if got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseSlackSessionID — happy + every error path.
func TestParseSlackSessionID(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		team, ch, ts, err := parseSlackSessionID("T1/C1#1700000010.000100")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if team != "T1" || ch != "C1" || ts != "1700000010.000100" {
			t.Errorf("parse = (%q,%q,%q)", team, ch, ts)
		}
	})
	t.Run("bad", func(t *testing.T) {
		for _, s := range []string{"no-anything", "T1/C1", "T1#ts", "/C1#ts", "T1/#ts", "T1/C1#"} {
			if _, _, _, err := parseSlackSessionID(s); !errors.Is(err, ErrUnknownSession) {
				t.Errorf("parseSlackSessionID(%q) err = %v, want ErrUnknownSession", s, err)
			}
		}
	})
}

// TestTruncateBody — defensive helper coverage.
func TestTruncateBody(t *testing.T) {
	short := "hi"
	if got := truncateBody(short); got != short {
		t.Errorf("truncateBody short = %q, want %q", got, short)
	}
	long := strings.Repeat("x", errorBodyExcerpt+50)
	got := truncateBody(long)
	if len(got) <= errorBodyExcerpt {
		t.Errorf("truncateBody long len = %d, want > %d (suffix preserved)", len(got), errorBodyExcerpt)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncateBody long missing ellipsis suffix: %q", got[len(got)-10:])
	}
}
