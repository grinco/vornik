package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestHandleMessage_EmptyText_NoAuthReply pins the silent-drop guard.
//
// Regression: Telegram emits service messages (forum_topic_created,
// forum_topic_closed, etc.) whenever the bot creates / closes a
// topic. Those arrive via getUpdates as a Message with text="" and
// from.id = the bot's own user_id. With the empty-text guard placed
// AFTER the auth check, the bot replied "You are not authorized to
// use this bot." to the supergroup once per forum-topic lifecycle
// event — every task completion produced a noisy unauthorized
// banner in the shared group channel.
//
// The fix: silent-drop empty-text messages BEFORE auth so service
// messages (and any other text-less / file-less update) never
// trigger a reply.
func TestHandleMessage_EmptyText_NoAuthReply(t *testing.T) {
	var sendCalls atomic.Int64
	var lastBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/sendMessage") {
			sendCalls.Add(1)
			lastBody, _ = io.ReadAll(r.Body)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer ts.Close()

	// AllowedUsers is intentionally NOT empty — but the user_id on
	// the service-message Message is the bot's id (999), which is
	// not in the allowlist. Pre-fix this would have produced the
	// "You are not authorized" reply.
	bot, err := NewBot(BotConfig{
		Token:        "test",
		AllowedUsers: map[int64]UserAccess{1: {Allowed: true, Projects: []string{"*"}}},
	}, nil)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = ts.URL

	// Service message shape: from a non-allowed UserID (the bot,
	// in production), with an empty Text — exactly what
	// forum_topic_created looks like after Update→Message
	// conversion in HandleUpdate.
	msg := &Message{
		ChatID:          -1001234567890,
		UserID:          999, // bot's own user_id; not in allowlist
		Text:            "",
		MessageThreadID: 42,
	}

	if err := bot.HandleMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if sendCalls.Load() != 0 {
		t.Errorf("HandleMessage must not send any reply for an empty-text update; got %d calls, body=%s",
			sendCalls.Load(), string(lastBody))
	}
}

// TestHandleMessage_WhitespaceOnlyText_NoAuthReply covers the
// "operator typed only whitespace and hit send" edge that the same
// guard handles for free — also no auth reply.
func TestHandleMessage_WhitespaceOnlyText_NoAuthReply(t *testing.T) {
	var sendCalls atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/sendMessage") {
			sendCalls.Add(1)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer ts.Close()

	bot, err := NewBot(BotConfig{
		Token:        "test",
		AllowedUsers: map[int64]UserAccess{1: {Allowed: true, Projects: []string{"*"}}},
	}, nil)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = ts.URL

	msg := &Message{ChatID: 1, UserID: 999, Text: "   \n\t  "}
	if err := bot.HandleMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if sendCalls.Load() != 0 {
		t.Errorf("whitespace-only text must drop silently; got %d send calls", sendCalls.Load())
	}
}

// TestHandleMessage_UnauthorizedWithText_StillRejects sanity-checks
// that we didn't accidentally relax the auth check for actual
// inbound chat: a non-allowed user sending real text still gets the
// "not authorized" reply. The guard only fires for empty content.
func TestHandleMessage_UnauthorizedWithText_StillRejects(t *testing.T) {
	var sentBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/sendMessage") {
			sentBody, _ = io.ReadAll(r.Body)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer ts.Close()

	bot, err := NewBot(BotConfig{
		Token:        "test",
		AllowedUsers: map[int64]UserAccess{1: {Allowed: true, Projects: []string{"*"}}},
	}, nil)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = ts.URL

	msg := &Message{ChatID: 1, UserID: 999, Text: "hello bot let me in"}
	if err := bot.HandleMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if !strings.Contains(string(sentBody), "not authorized") {
		t.Errorf("expected auth-rejection reply for real text from non-allowed user; body=%s", string(sentBody))
	}
}
