// Coverage extension for forum.go's sendForumMessage early-error
// guards and sendDocumentToForum's not-enabled / invalid-thread
// branches. The existing forum_test.go covers the happy paths;
// this fills in the negative-space coverage.

package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/chat"
)

func newForumTestBot(t *testing.T, recBaseURL string, client *http.Client, opts ...BotOption) *Bot {
	t.Helper()
	chatClient := chat.NewClient("https://example.com", "k", "m")
	defaults := []BotOption{
		WithHTTPClient(client),
		WithForumChatID(999_888_777, 0),
		WithTelegramThreadRepository(newStubThreadRepo()),
	}
	defaults = append(defaults, opts...)
	b, err := NewBot(BotConfig{Token: "tok"}, chatClient, defaults...)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	b.baseURL = recBaseURL
	return b
}

func TestSendForumMessage_EmptyTextRejected(t *testing.T) {
	rec := newTelegramRecorder(t)
	b := newForumTestBot(t, rec.server.URL, rec.server.Client())
	_, err := b.sendForumMessage(context.Background(), 1, "")
	if err == nil {
		t.Error("empty text: expected error, got nil")
	}
}

func TestSendForumMessage_ZeroThreadIDRejected(t *testing.T) {
	rec := newTelegramRecorder(t)
	b := newForumTestBot(t, rec.server.URL, rec.server.Client())
	_, err := b.sendForumMessage(context.Background(), 0, "hi")
	if err == nil {
		t.Error("zero thread: expected error, got nil")
	}
}

func TestSendForumMessage_NoForumChatID(t *testing.T) {
	rec := newTelegramRecorder(t)
	chatClient := chat.NewClient("https://example.com", "k", "m")
	b, _ := NewBot(BotConfig{Token: "tok"}, chatClient,
		WithHTTPClient(rec.server.Client()))
	b.baseURL = rec.server.URL
	_, err := b.sendForumMessage(context.Background(), 100, "hi")
	if err == nil {
		t.Error("no forum chat: expected error, got nil")
	}
}

func TestSendForumMessage_ServerNotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"description":"banned"}`))
	}))
	defer srv.Close()

	b := newForumTestBot(t, srv.URL, srv.Client())
	_, err := b.sendForumMessage(context.Background(), 1, "x")
	if err == nil {
		t.Error("server ok=false: expected error, got nil")
	}
}

func TestSendForumMessage_HappyPath(t *testing.T) {
	rec := newTelegramRecorder(t)
	b := newForumTestBot(t, rec.server.URL, rec.server.Client())
	id, err := b.sendForumMessage(context.Background(), 42, "hello")
	if err != nil {
		t.Fatalf("sendForumMessage: %v", err)
	}
	if id != 1 {
		t.Errorf("message id: got %d, want 1", id)
	}
}

func TestSendDocumentToForum_NotEnabled(t *testing.T) {
	rec := newTelegramRecorder(t)
	chatClient := chat.NewClient("https://example.com", "k", "m")
	b, _ := NewBot(BotConfig{Token: "tok"}, chatClient,
		WithHTTPClient(rec.server.Client()))
	b.baseURL = rec.server.URL
	// No forum wired.
	err := b.sendDocumentToForum(context.Background(), 1, "/nowhere", "x")
	if err == nil {
		t.Error("not enabled: expected error, got nil")
	}
}

func TestSendDocumentToForum_ZeroThreadIDRejected(t *testing.T) {
	rec := newTelegramRecorder(t)
	b := newForumTestBot(t, rec.server.URL, rec.server.Client())
	err := b.sendDocumentToForum(context.Background(), 0, "/nowhere", "x")
	if err == nil {
		t.Error("zero thread: expected error, got nil")
	}
}

func TestSendDocumentToForum_FileMissing(t *testing.T) {
	rec := newTelegramRecorder(t)
	b := newForumTestBot(t, rec.server.URL, rec.server.Client())
	err := b.sendDocumentToForum(context.Background(), 1, "/no/such/file", "x")
	if err == nil {
		t.Error("missing file: expected error, got nil")
	}
}

func TestSendDocumentToForum_HappyPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.md")
	if err := os.WriteFile(p, []byte("doc"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	defer srv.Close()
	b := newForumTestBot(t, srv.URL, srv.Client())
	if err := b.sendDocumentToForum(context.Background(), 5, p, "caption"); err != nil {
		t.Errorf("sendDocumentToForum: %v", err)
	}
}

func TestCloseForumTopic_NoOpWhenNotEnabled(t *testing.T) {
	chatClient := chat.NewClient("https://example.com", "k", "m")
	b, _ := NewBot(BotConfig{Token: "tok"}, chatClient)
	// No forum wiring → must be no-op.
	if err := b.closeForumTopic(context.Background(), 1); err != nil {
		t.Errorf("disabled forum: closeForumTopic returned %v, want nil", err)
	}
}

func TestCloseForumTopic_ZeroThreadNoOp(t *testing.T) {
	rec := newTelegramRecorder(t)
	b := newForumTestBot(t, rec.server.URL, rec.server.Client())
	if err := b.closeForumTopic(context.Background(), 0); err != nil {
		t.Errorf("zero thread: closeForumTopic returned %v, want nil", err)
	}
}

func TestCloseForumTopic_HappyPath(t *testing.T) {
	rec := newTelegramRecorder(t)
	b := newForumTestBot(t, rec.server.URL, rec.server.Client())
	if err := b.closeForumTopic(context.Background(), 1); err != nil {
		t.Errorf("closeForumTopic: %v", err)
	}
}
