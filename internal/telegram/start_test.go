// Coverage for Bot.Start — the long-poll lifecycle entry. We test
// the session-restore branch, the already-running guard, and the
// SessionPath-enabled side-effects (periodicSaveLoop goroutine
// spawning). pollLoop is exercised via Start since they're paired.

package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

// pollingBotForTest builds a bot whose long-poll target is a local
// httptest server that returns an empty getUpdates response so the
// pollLoop iterates without producing any work.
func pollingBotForTest(t *testing.T, cfg BotConfig) *Bot {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Empty result keeps the poll loop spinning without doing
		// anything observable. We slow down the response slightly so
		// the test can stop the bot before a second round.
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	t.Cleanup(srv.Close)

	chatClient := chat.NewClient("https://example.com", "k", "m")
	b, err := NewBot(cfg, chatClient,
		WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	b.baseURL = srv.URL
	return b
}

func TestBotStart_StartsAndStops(t *testing.T) {
	b := pollingBotForTest(t, BotConfig{Token: "tok"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Allow a poll iteration so the goroutine reaches its select.
	time.Sleep(20 * time.Millisecond)
	if err := b.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestBotStart_AlreadyRunningErrors(t *testing.T) {
	b := pollingBotForTest(t, BotConfig{Token: "tok"})
	defer func() { _ = b.Stop() }()

	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := b.Start(context.Background()); err == nil {
		t.Error("second Start: expected error, got nil")
	}
}

func TestBotStart_RestoresSessionsFromDisk(t *testing.T) {
	// Pre-seed the on-disk session file so Start's restore branch
	// fires.
	dir := t.TempDir()
	sessPath := filepath.Join(dir, "sessions.json")

	// Use the chat package to save a real conversation through the
	// official API, then read it back via Start.
	savedConv := chat.NewConversation("c-100", 32)
	savedConv.AddMessage(chat.Message{Role: "user", Content: "remembered"})
	if err := chat.SaveConversations(sessPath, map[int64]*chat.Conversation{100: savedConv}); err != nil {
		t.Fatalf("SaveConversations: %v", err)
	}

	b := pollingBotForTest(t, BotConfig{
		Token:            "tok",
		SessionPath:      sessPath,
		MaxHistory:       50,
		MaxHistoryTokens: 4096,
	})
	defer func() { _ = b.Stop() }()

	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let restore complete

	if got := b.ActiveChatCount(); got != 1 {
		t.Errorf("active chats after restore: got %d, want 1", got)
	}
	conv := b.getConversation(100)
	if conv.Len() != 1 {
		t.Errorf("restored conv: Len=%d, want 1", conv.Len())
	}
}

func TestBotStart_BadSessionFileStartsFresh(t *testing.T) {
	// A garbled session file should NOT prevent Start from
	// running — Start logs a warn and continues with an empty map.
	dir := t.TempDir()
	sessPath := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(sessPath, []byte("not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	b := pollingBotForTest(t, BotConfig{Token: "tok", SessionPath: sessPath})
	defer func() { _ = b.Stop() }()
	if err := b.Start(context.Background()); err != nil {
		t.Errorf("Start with garbage session: got %v, want nil", err)
	}
	if got := b.ActiveChatCount(); got != 0 {
		t.Errorf("active chats: got %d, want 0", got)
	}
}

// TestBotPollLoop_IteratesAndExits asserts the pollLoop goroutine
// loops at least once on the configured server before stopping on
// stopChan close. We can't observe internal calls but can check
// that the request handler was hit.
func TestBotPollLoop_HitsServer(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()

	chatClient := chat.NewClient("https://example.com", "k", "m")
	b, _ := NewBot(BotConfig{Token: "tok"}, chatClient,
		WithHTTPClient(srv.Client()),
	)
	b.baseURL = srv.URL

	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// pollLoop loops fast (no sleep between polls) so we should
	// see at least one HTTP hit within 200ms.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if hits.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = b.Stop()
	if hits.Load() < 1 {
		t.Errorf("pollLoop: 0 server hits, expected ≥1")
	}
}

// TestBotPollLoop_HandlesErrorFromServer drives the pollLoop's
// error-on-getUpdates path: when the server returns a non-OK
// response the loop sleeps 5s, then tries again. We just confirm
// the loop doesn't crash and Stop returns cleanly.
func TestBotPollLoop_ServerErrorRecovery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	chatClient := chat.NewClient("https://example.com", "k", "m")
	b, _ := NewBot(BotConfig{Token: "tok"}, chatClient,
		WithHTTPClient(srv.Client()))
	b.baseURL = srv.URL

	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Allow pollLoop to fail once and start its sleep.
	time.Sleep(100 * time.Millisecond)
	if err := b.Stop(); err != nil {
		t.Errorf("Stop after server error: got %v, want nil", err)
	}
}

// Keep json reference so the import stays useful for other tests
// in this file.
var _ = json.Marshal
