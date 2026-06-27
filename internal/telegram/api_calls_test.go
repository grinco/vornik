// Package telegram: tests for the bot's low-level Telegram API
// callers (getUpdates / sendChatAction / SendDocument). Uses the
// rewriting transport from download_test.go so the calls never
// leave the test binary.
package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// scriptedTelegramServer returns a server that replies with a
// per-path canned JSON. Unmatched paths return 404.
func scriptedTelegramServer(t *testing.T, responses map[string]string) (*httptest.Server, *[]apiCall) {
	t.Helper()
	mu := sync.Mutex{}
	calls := []apiCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, apiCall{Path: r.URL.Path, Body: r.URL.RawQuery})
		mu.Unlock()
		for prefix, body := range responses {
			if strings.HasSuffix(r.URL.Path, prefix) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
				return
			}
		}
		http.NotFound(w, r)
	}))
	return srv, &calls
}

// --- getUpdates ------------------------------------------------------

func TestGetUpdates_HappyPath(t *testing.T) {
	srv, _ := scriptedTelegramServer(t, map[string]string{
		"/getUpdates": `{"ok":true,"result":[{"update_id":1,"message":{"chat":{"id":111},"text":"hi"}}]}`,
	})
	defer srv.Close()
	bot := makeBotWithTransport(t, srv.URL)
	updates, err := bot.getUpdates(context.Background(), 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(updates) != 1 {
		t.Errorf("got %d updates, want 1", len(updates))
	}
	if updates[0].UpdateID != 1 {
		t.Errorf("UpdateID: got %d, want 1", updates[0].UpdateID)
	}
}

func TestGetUpdates_NotOK(t *testing.T) {
	srv, _ := scriptedTelegramServer(t, map[string]string{
		"/getUpdates": `{"ok":false,"description":"bad token"}`,
	})
	defer srv.Close()
	bot := makeBotWithTransport(t, srv.URL)
	if _, err := bot.getUpdates(context.Background(), 0); err == nil {
		t.Errorf("expected error when ok=false")
	}
}

func TestGetUpdates_InvalidJSON(t *testing.T) {
	srv, _ := scriptedTelegramServer(t, map[string]string{
		"/getUpdates": `{not-json`,
	})
	defer srv.Close()
	bot := makeBotWithTransport(t, srv.URL)
	if _, err := bot.getUpdates(context.Background(), 0); err == nil {
		t.Errorf("expected parse error")
	}
}

// --- sendChatAction --------------------------------------------------

func TestSendChatAction_DispatchesPOST(t *testing.T) {
	srv, calls := scriptedTelegramServer(t, map[string]string{
		"/sendChatAction": `{"ok":true}`,
	})
	defer srv.Close()
	bot := makeBotWithTransport(t, srv.URL)
	bot.sendChatAction(context.Background(), 111, "typing")
	if len(*calls) != 1 {
		t.Errorf("expected 1 call; got %d", len(*calls))
	}
}

// --- SendDocument ----------------------------------------------------

func TestSendDocument_FileMissing(t *testing.T) {
	srv, calls := scriptedTelegramServer(t, map[string]string{
		"/sendDocument": `{"ok":true}`,
	})
	defer srv.Close()
	bot := makeBotWithTransport(t, srv.URL)
	err := bot.SendDocument(context.Background(), 111, "/tmp/vornik-test-nope.bin", "missing.bin")
	if err == nil {
		t.Errorf("expected error when file is missing")
	}
	if len(*calls) != 0 {
		t.Errorf("expected 0 telegram calls when local file is missing; got %d", len(*calls))
	}
}

func TestSendDocument_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	srv, calls := scriptedTelegramServer(t, map[string]string{
		"/sendDocument": `{"ok":true,"result":{"message_id":1}}`,
	})
	defer srv.Close()
	bot := makeBotWithTransport(t, srv.URL)
	if err := bot.SendDocument(context.Background(), 111, path, "out.txt"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(*calls) != 1 {
		t.Errorf("expected 1 telegram call; got %d", len(*calls))
	}
}

// makeBotWithTransport builds a bot whose outbound HTTP client routes
// to the test server.
func makeBotWithTransport(t *testing.T, target string) *Bot {
	t.Helper()
	hc := &http.Client{
		Transport: &rewritingTransport{base: http.DefaultTransport, targetHost: target},
	}
	bot := makeBotForOptions(t)
	bot.httpClient = hc
	return bot
}
