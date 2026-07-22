package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestBuildWebWriteCaption pins the summary content and the deep link, and
// asserts the caption carries NO approve/reject action — Telegram is
// notify-only (LLD I7); the binding decision happens in /inbox.
func TestBuildWebWriteCaption(t *testing.T) {
	caption := buildWebWriteCaption(
		"assistant", "claims.airline.com", "webwrite_abc123",
		"http://192.0.2.10:8080/inbox#webwrite_abc123")
	for _, want := range []string{
		"assistant", "claims.airline.com", "webwrite_abc123",
		"http://192.0.2.10:8080/inbox#webwrite_abc123",
	} {
		if !strings.Contains(caption, want) {
			t.Errorf("caption missing %q:\n%s", want, caption)
		}
	}
	// Notify-only: the summary must not invite an inline decision.
	lower := strings.ToLower(caption)
	for _, forbidden := range []string{"approve", "reject", "callback", "/approve", "/reject"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("notify-only caption must not contain %q (binding decision is inbox-only):\n%s", forbidden, caption)
		}
	}
}

// TestNotifyWebWritePending_PhotoToEveryOperator: a preview screenshot is
// delivered as a photo to each operator, and the outgoing request carries NO
// reply_markup (no inline approve/reject keyboard) — the notify-only guarantee.
func TestNotifyWebWritePending_PhotoToEveryOperator(t *testing.T) {
	var photos int32
	var sawReplyMarkup int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendPhoto") {
			atomic.AddInt32(&photos, 1)
			_ = r.ParseMultipartForm(1 << 20)
			if r.FormValue("reply_markup") != "" {
				atomic.AddInt32(&sawReplyMarkup, 1)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	b := stubBot(srv.URL, 1, 2, 3)
	err := b.NotifyWebWritePending(context.Background(), "assistant", "claims.airline.com",
		"webwrite_abc123", "http://192.0.2.10:8080/inbox#webwrite_abc123", []byte{0xff, 0xd8})
	if err != nil {
		t.Fatalf("NotifyWebWritePending: %v", err)
	}
	if got := atomic.LoadInt32(&photos); got != 3 {
		t.Fatalf("want 3 photo sends (one per operator), got %d", got)
	}
	if got := atomic.LoadInt32(&sawReplyMarkup); got != 0 {
		t.Fatalf("notify-only: no request may carry reply_markup, saw %d", got)
	}
}

// TestNotifyWebWritePending_TextWhenNoScreenshot: with no screenshot the alert
// goes out as a plain sendMessage (never a callback-bearing markup send), and
// the body contains the deep link + summary but no approve/reject action.
func TestNotifyWebWritePending_TextWhenNoScreenshot(t *testing.T) {
	var mu sync.Mutex
	paths := map[string]int{}
	var msgBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			paths["msg"]++
			b, _ := io.ReadAll(r.Body)
			msgBody = string(b)
		} else if strings.HasSuffix(r.URL.Path, "/sendPhoto") {
			paths["photo"]++
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()

	b := stubBot(srv.URL, 7)
	if err := b.NotifyWebWritePending(context.Background(), "assistant", "claims.airline.com",
		"webwrite_abc123", "http://192.0.2.10:8080/inbox#webwrite_abc123", nil); err != nil {
		t.Fatalf("NotifyWebWritePending: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if paths["photo"] != 0 || paths["msg"] != 1 {
		t.Fatalf("no-screenshot path should use sendMessage: %v", paths)
	}
	// The plain sendMessage body must not carry an inline keyboard / callback.
	if strings.Contains(msgBody, "reply_markup") || strings.Contains(msgBody, "callback_data") {
		t.Fatalf("notify-only: sendMessage body must not carry markup/callback: %s", msgBody)
	}
	if !strings.Contains(msgBody, "webwrite_abc123") {
		t.Fatalf("sendMessage body missing submission id / deep link: %s", msgBody)
	}
}
