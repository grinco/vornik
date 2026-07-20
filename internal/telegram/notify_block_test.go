package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

func TestBuildBlockCaption(t *testing.T) {
	caption := buildBlockCaption("assistant", "tripadvisor.com", "captcha", "cf challenge",
		"vornikctl scraper login start -p assistant --url https://www.tripadvisor.com/x")
	for _, want := range []string{
		"assistant", "tripadvisor.com", "captcha", "cf challenge",
		"vornikctl scraper login start -p assistant --url https://www.tripadvisor.com/x",
	} {
		if !strings.Contains(caption, want) {
			t.Errorf("caption missing %q:\n%s", want, caption)
		}
	}
}

// stubBot wires just enough Bot state for the notifier HTTP paths.
func stubBot(baseURL string, operators ...int64) *Bot {
	allowed := make(map[int64]UserAccess, len(operators))
	for _, id := range operators {
		allowed[id] = UserAccess{}
	}
	return &Bot{
		baseURL:    baseURL,
		httpClient: http.DefaultClient,
		logger:     zerolog.Nop(),
		config:     BotConfig{AllowedUsers: allowed},
	}
}

func TestSendPhotoBytes(t *testing.T) {
	var gotChatID, gotPhoto bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendPhoto") {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		_ = r.ParseMultipartForm(1 << 20)
		if r.FormValue("chat_id") == "42" {
			gotChatID = true
		}
		if _, _, err := r.FormFile("photo"); err == nil {
			gotPhoto = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	b := stubBot(srv.URL, 42)
	if err := b.sendPhotoBytes(context.Background(), 42, []byte{0xff, 0xd8, 0xff, 0x00}, "cap"); err != nil {
		t.Fatalf("sendPhotoBytes: %v", err)
	}
	if !gotChatID || !gotPhoto {
		t.Fatalf("multipart missing fields: chat_id=%v photo=%v", gotChatID, gotPhoto)
	}
}

func TestSendPhotoBytes_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"description":"bad"}`))
	}))
	defer srv.Close()
	b := stubBot(srv.URL, 42)
	if err := b.sendPhotoBytes(context.Background(), 42, []byte{1}, "c"); err == nil {
		t.Fatal("expected error on non-200")
	}
}

func TestNotifyScraperBlock_PhotoToEveryOperator(t *testing.T) {
	var photos int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendPhoto") {
			atomic.AddInt32(&photos, 1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	b := stubBot(srv.URL, 1, 2, 3)
	err := b.NotifyScraperBlock(context.Background(), "assistant", "tripadvisor.com", "captcha", "", "cmd", []byte{0xff, 0xd8})
	if err != nil {
		t.Fatalf("NotifyScraperBlock: %v", err)
	}
	if got := atomic.LoadInt32(&photos); got != 3 {
		t.Fatalf("want 3 photo sends (one per operator), got %d", got)
	}
}

func TestNotifyScraperBlock_TextWhenNoScreenshot(t *testing.T) {
	var mu sync.Mutex
	paths := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			paths["msg"]++
		} else if strings.HasSuffix(r.URL.Path, "/sendPhoto") {
			paths["photo"]++
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()

	b := stubBot(srv.URL, 7)
	if err := b.NotifyScraperBlock(context.Background(), "assistant", "tripadvisor.com", "login_wall", "", "cmd", nil); err != nil {
		t.Fatalf("NotifyScraperBlock: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if paths["photo"] != 0 || paths["msg"] != 1 {
		t.Fatalf("no-screenshot path should use sendMessage: %v", paths)
	}
}
