package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestMuxHandler_VornikSlashCommandDispatches(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)
	mux := NewMuxHandler([]*Channel{ch}, zerolog.Nop())

	form := url.Values{
		"team_id":    {"T123"},
		"channel_id": {"C_general"},
		"user_id":    {"U_alice"},
		"command":    {"/vornik"},
		"text":       {"summarize the incident"},
		"trigger_id": {"trigger-123"},
	}.Encode()
	req := signedRequest(t, cfg.SigningSecret, now.Unix(), []byte(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for len(rec.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("Receive call count = %d, want 1", len(got))
	}
	if got[0].Text != "summarize the incident" {
		t.Fatalf("Text = %q, want slash-command prompt", got[0].Text)
	}
	if got[0].SessionID != "T123/C_general#slash:U_alice" {
		t.Fatalf("SessionID = %q", got[0].SessionID)
	}
}

func TestMuxHandler_DuplicateSlashTriggerDispatchesOnce(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)
	mux := NewMuxHandler([]*Channel{ch}, zerolog.Nop())
	form := url.Values{
		"team_id":    {"T123"},
		"channel_id": {"C_general"},
		"user_id":    {"U_alice"},
		"command":    {"/vornik"},
		"text":       {"hello"},
		"trigger_id": {"trigger-retry"},
	}.Encode()

	for range 2 {
		req := signedRequest(t, cfg.SigningSecret, now.Unix(), []byte(form))
		req = req.WithContext(context.Background())
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	}
	deadline := time.Now().Add(time.Second)
	for len(rec.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("Receive call count = %d, want 1 for duplicate trigger: %+v", len(got), got)
	}
}

func TestMuxHandler_SlashCommandRequiresValidSignature(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)
	mux := NewMuxHandler([]*Channel{ch}, zerolog.Nop())
	form := url.Values{
		"team_id":    {"T123"},
		"channel_id": {"C_general"},
		"user_id":    {"U_alice"},
		"command":    {"/vornik"},
		"text":       {"forged prompt"},
		"trigger_id": {"trigger-forged"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/slack/webhook", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", "1700000000")
	req.Header.Set("X-Slack-Signature", "v0=forged")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("forged slash command dispatched: %+v", got)
	}
}

func TestParseSlackSessionID_SlashSessionHasNoThreadTimestamp(t *testing.T) {
	team, channel, thread, err := parseSlackSessionID("T123/C_general#slash:U_alice")
	if err != nil {
		t.Fatalf("parseSlackSessionID: %v", err)
	}
	if team != "T123" || channel != "C_general" || !strings.HasPrefix(thread, "slash:") {
		t.Fatalf("unexpected parse: team=%q channel=%q thread=%q", team, channel, thread)
	}
}
