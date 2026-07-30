package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
)

// slackFileStub serves files.info plus the private download URL for one file.
func slackFileStub(t *testing.T, mime, name string, payload []byte) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/files.info"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"file": map[string]any{
					"id":                   "F_IMG",
					"name":                 name,
					"mimetype":             mime,
					"size":                 len(payload),
					"url_private_download": srv.URL + "/download",
				},
			})
		case r.URL.Path == "/download":
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write(payload)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"ts":"1.1"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fileSharedPayload(channelType, channelID string) map[string]any {
	return map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev_img",
		"event": map[string]any{
			"type":         "file_shared",
			"user":         "U_alice",
			"channel":      channelID,
			"channel_type": channelType,
			"ts":           "1785370000.000100",
			"file":         map[string]any{"id": "F_IMG"},
		},
	}
}

// BACKLOG 2026-07-30: a photo posted to Slack was fetched and then discarded.
// handleFileSharedEvent dropped anything failing isAudioMime with "not audio; ignoring
// (v1 slice scope)", even though the project has a vision role and files:read was
// already granted. Worse, the handler returned early when no STT provider was wired, so
// a deployment without voice could not receive an image at all.
func TestFileShared_ImageReachesTheVisionPathAsAnAttachment(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake-pixels")
	srv := slackFileStub(t, "image/png", "screenshot.png", png)

	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	ch := makeChannel(t, cfg, time.Now())
	// Deliberately NO STT provider: an image must not depend on voice being configured.
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), fileSharedPayload("channel", "C_general"))

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatch count = %d, want 1 — an image must start a turn", len(got))
	}
	if len(got[0].Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(got[0].Attachments))
	}
	a := got[0].Attachments[0]
	if a.MimeType != "image/png" {
		t.Errorf("MimeType = %q, want image/png", a.MimeType)
	}
	if a.Name != "screenshot.png" {
		t.Errorf("Name = %q, want screenshot.png", a.Name)
	}
	if a.SizeBytes != int64(len(png)) {
		t.Errorf("SizeBytes = %d, want %d", a.SizeBytes, len(png))
	}
	if a.ChannelRef == "" {
		t.Error("ChannelRef is empty — the vision path has no way to fetch the bytes")
	}
	// The vision path resolves ChannelRef as a host path FIRST and only then falls
	// through to the channel's fetcher. A ref that looks like an absolute path would
	// be read off this machine's filesystem instead.
	if strings.HasPrefix(a.ChannelRef, "/") {
		t.Errorf("ChannelRef %q looks like a filesystem path", a.ChannelRef)
	}
}

// The bytes are fetched lazily through the AttachmentFetcher seam, authenticated with
// the installation's bot token — a Slack url_private is not publicly readable.
func TestFileShared_FetchAttachmentReturnsTheBytes(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake-pixels")
	srv := slackFileStub(t, "image/png", "screenshot.png", png)

	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	ch := makeChannel(t, cfg, time.Now())
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)
	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), fileSharedPayload("im", "D0BLA9ZRDFH"))

	got := rec.snapshot()
	if len(got) != 1 || len(got[0].Attachments) != 1 {
		t.Fatalf("expected one dispatch carrying one attachment, got %#v", got)
	}

	fetcher, ok := any(ch).(conversation.AttachmentFetcher)
	if !ok {
		t.Fatal("*Channel does not implement conversation.AttachmentFetcher, so the " +
			"vision path can never resolve a Slack image")
	}
	rc, err := fetcher.FetchAttachment(context.Background(), got[0].Attachments[0])
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != string(png) {
		t.Errorf("fetched %d bytes, want the %d posted", len(data), len(png))
	}
}

// A ref for an unknown workspace must not be fetched. FetchAttachment is reached from
// the dispatcher with an attachment that rode in on a ChannelMessage, so the team it
// names is the only authority for which bot token may be spent on it.
func TestFileShared_FetchAttachmentRejectsUnknownTeamAndNonSlackRefs(t *testing.T) {
	cfg := validConfig()
	ch := makeChannel(t, cfg, time.Now())

	for _, ref := range []string{
		"slack:T_OTHER:https://files.slack.com/x",
		"https://evil.example/x",
		"",
		"slack:T123:not-a-url",
		"slack:T123:file:///etc/passwd",
	} {
		if _, err := ch.FetchAttachment(context.Background(),
			conversation.Attachment{ChannelRef: ref}); err == nil {
			t.Errorf("FetchAttachment(%q) returned no error, want one", ref)
		}
	}
}

// The audio path must be untouched: a voice memo with no STT wired is still dropped
// rather than handed to the vision path as a mystery attachment.
func TestFileShared_AudioWithoutSTTIsStillDropped(t *testing.T) {
	srv := slackFileStub(t, "audio/mp4", "memo.m4a", []byte("fake-audio"))

	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	ch := makeChannel(t, cfg, time.Now())
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), fileSharedPayload("channel", "C_general"))

	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("dispatch count = %d, want 0 — audio needs STT and there is none", len(got))
	}
}

// A PDF is neither audio nor an image. It must keep its documented behaviour — dropped
// with a reason — rather than becoming an attachment the vision path has to reject.
func TestFileShared_NonMediaFileIsStillIgnored(t *testing.T) {
	srv := slackFileStub(t, "application/pdf", "contract.pdf", []byte("%PDF-1.7"))

	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	ch := makeChannel(t, cfg, time.Now())
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), fileSharedPayload("channel", "C_general"))

	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("dispatch count = %d, want 0", len(got))
	}
}

// The allowlists gate an image exactly as they gate a message — an image is a turn, and
// this project can read a shared Google Workspace account.
func TestFileShared_ImageStillHonoursTheAllowlists(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake")
	srv := slackFileStub(t, "image/png", "x.png", png)

	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	cfg.ChannelAllowlist = []string{"C_allowed"}
	cfg.SenderAllowlist = []string{"U_bob"}
	ch := makeChannel(t, cfg, time.Now())
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	// Wrong channel, and a sender who is not on the list either.
	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), fileSharedPayload("channel", "C_general"))

	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("dispatch count = %d, want 0", len(got))
	}
}
