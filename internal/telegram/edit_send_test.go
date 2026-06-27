// Coverage for the small edit / send-getID / getFile error branches
// the existing tests leave at 80%. Each test runs against a fake
// server so the test stays isolated.

package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

func newBotWithCustomServer(t *testing.T, handler http.HandlerFunc) *Bot {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	chatClient := chat.NewClient("https://example.com", "k", "m")
	b, err := NewBot(BotConfig{Token: "tok"}, chatClient,
		WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	b.baseURL = srv.URL
	return b
}

// --- sendMessageGetID ----------------------------------------------------

func TestSendMessageGetID_EmptyText(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t"})
	_, err := b.sendMessageGetID(context.Background(), 100, "")
	if err == nil {
		t.Error("empty text: expected error, got nil")
	}
}

func TestSendMessageGetID_HappyPath(t *testing.T) {
	b := newBotWithCustomServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	})
	id, err := b.sendMessageGetID(context.Background(), 100, "hi")
	if err != nil {
		t.Fatalf("sendMessageGetID: %v", err)
	}
	if id != 7 {
		t.Errorf("id: got %d, want 7", id)
	}
}

func TestSendMessageGetID_ServerNotOK(t *testing.T) {
	b := newBotWithCustomServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"description":"chat not found"}`))
	})
	_, err := b.sendMessageGetID(context.Background(), 100, "hi")
	if err == nil {
		t.Error("not OK: expected error, got nil")
	}
}

func TestSendMessageGetID_BadJSON(t *testing.T) {
	b := newBotWithCustomServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	})
	_, err := b.sendMessageGetID(context.Background(), 100, "hi")
	if err == nil {
		t.Error("bad JSON: expected error, got nil")
	}
}

// --- editMessageText -----------------------------------------------------

func TestEditMessageText_EmptyTextNoOp(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t"})
	if err := b.editMessageText(context.Background(), 100, 1, ""); err != nil {
		t.Errorf("empty text: got %v, want nil", err)
	}
}

func TestEditMessageText_HappyPath(t *testing.T) {
	b := newBotWithCustomServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	if err := b.editMessageText(context.Background(), 100, 1, "new text"); err != nil {
		t.Errorf("editMessageText: %v", err)
	}
}

func TestEditMessageText_LongTextTruncated(t *testing.T) {
	long := strings.Repeat("x", 5000)
	b := newBotWithCustomServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Decoded body should have <=4096 chars in text.
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	if err := b.editMessageText(context.Background(), 100, 1, long); err != nil {
		t.Errorf("editMessageText long: %v", err)
	}
}

// --- getFile -------------------------------------------------------------

func TestGetFile_HappyPath(t *testing.T) {
	b := newBotWithCustomServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"docs/abc.pdf"}}`))
	})
	path, err := b.getFile(context.Background(), "f-1")
	if err != nil {
		t.Fatalf("getFile: %v", err)
	}
	if path != "docs/abc.pdf" {
		t.Errorf("path: got %q, want docs/abc.pdf", path)
	}
}

func TestGetFile_ServerNotOK(t *testing.T) {
	b := newBotWithCustomServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false}`))
	})
	_, err := b.getFile(context.Background(), "f-1")
	if err == nil {
		t.Error("not OK: expected error, got nil")
	}
}

func TestGetFile_BadJSON(t *testing.T) {
	b := newBotWithCustomServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	})
	_, err := b.getFile(context.Background(), "f-1")
	if err == nil {
		t.Error("bad JSON: expected error, got nil")
	}
}

func TestGetFile_HTTPFailure(t *testing.T) {
	chatClient := chat.NewClient("https://example.com", "k", "m")
	b, _ := NewBot(BotConfig{Token: "tok"}, chatClient)
	b.baseURL = "http://127.0.0.1:0"
	_, err := b.getFile(context.Background(), "f-1")
	if err == nil {
		t.Error("unreachable server: expected error, got nil")
	}
}
