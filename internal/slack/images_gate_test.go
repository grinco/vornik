package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// imageGateStub serves files.info (with a configurable shares block) +
// conversations.history (with a configurable message text) + the download.
func imageGateStub(t *testing.T, threadTs, msgText, msgUser string, payload []byte) *httptest.Server {
	t.Helper()
	const msgTs = "1785370000.000100"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/files.info"):
			share := map[string]any{"ts": msgTs}
			if threadTs != "" {
				share["thread_ts"] = threadTs
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"file": map[string]any{
					"id":                   "F_IMG",
					"name":                 "screenshot.png",
					"mimetype":             "image/png",
					"size":                 len(payload),
					"url_private_download": srv.URL + "/download",
					"shares": map[string]any{
						"public": map[string]any{"C_general": []any{share}},
					},
				},
			})
		case strings.Contains(r.URL.Path, "/conversations.history"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []any{map[string]any{"text": msgText, "user": msgUser}},
			})
		case r.URL.Path == "/download":
			_, _ = w.Write(payload)
		default:
			_, _ = w.Write([]byte(`{"ok":true,"ts":"1.1"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func imageEventPayload(withBotAuth bool) map[string]any {
	p := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev_img_gate",
		"event": map[string]any{
			"type":         "file_shared",
			"user":         "U_alice",
			"channel":      "C_general",
			"channel_type": "channel",
			"file":         map[string]any{"id": "F_IMG"},
		},
	}
	if withBotAuth {
		p["authorizations"] = []any{map[string]any{"is_bot": true, "user_id": "U_bot"}}
	}
	return p
}

func TestImageGate_ThreadNotEngaged_Dropped(t *testing.T) {
	srv := imageGateStub(t, "1785367141.211839", "", "", []byte("\x89PNGfake"))
	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	ch := makeChannel(t, cfg, time.Now())
	ch.SetThreadEngagementChecker(&engagementStub{engaged: map[string]bool{}}) // nothing engaged
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), imageEventPayload(false))

	if n := len(rec.snapshot()); n != 0 {
		t.Fatalf("dispatch = %d, want 0 — an image in a thread we're not engaged in must be ignored", n)
	}
}

func TestImageGate_ThreadEngaged_RepliesInThread(t *testing.T) {
	const threadTs = "1785367141.211839"
	srv := imageGateStub(t, threadTs, "", "", []byte("\x89PNGfake"))
	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	ch := makeChannel(t, cfg, time.Now())
	ch.SetThreadEngagementChecker(&engagementStub{engaged: map[string]bool{
		"T123/C_general#" + threadTs: true,
	}})
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), imageEventPayload(false))

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatch = %d, want 1 — an image in an engaged thread is ours", len(got))
	}
	if got[0].SessionID != "T123/C_general#"+threadTs {
		t.Fatalf("session = %q, want the thread session (reply must thread, not go to #main)", got[0].SessionID)
	}
}

func TestImageGate_TopLevelTagged_RepliesInChannel(t *testing.T) {
	srv := imageGateStub(t, "", "hey <@U_bot> what is this?", "", []byte("\x89PNGfake"))
	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	ch := makeChannel(t, cfg, time.Now())
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), imageEventPayload(true))

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatch = %d, want 1 — a tagged top-level image is ours", len(got))
	}
	if got[0].SessionID != "T123/C_general#main" {
		t.Fatalf("session = %q, want the channel session #main", got[0].SessionID)
	}
}

func TestImageGate_TopLevelUntagged_Dropped(t *testing.T) {
	srv := imageGateStub(t, "", "cute cat photo", "", []byte("\x89PNGfake")) // no mention
	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	ch := makeChannel(t, cfg, time.Now())
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), imageEventPayload(true))

	if n := len(rec.snapshot()); n != 0 {
		t.Fatalf("dispatch = %d, want 0 — an untagged top-level image must be ignored", n)
	}
}

// A thread the bot STARTED (bot authored the root) is ours even before any
// thread history exists — an image as the first reply must be handled.
func TestImageGate_ThreadRootedOnBot_Replies(t *testing.T) {
	const threadTs = "1785367141.211839"
	srv := imageGateStub(t, threadTs, "", "U_bot", []byte("\x89PNGfake")) // thread root authored by the bot
	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	ch := makeChannel(t, cfg, time.Now())
	ch.SetThreadEngagementChecker(&engagementStub{engaged: map[string]bool{}}) // no stored history yet
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), imageEventPayload(true))

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatch = %d, want 1 — an image in a bot-started thread is ours", len(got))
	}
	if got[0].SessionID != "T123/C_general#"+threadTs {
		t.Fatalf("session = %q, want the thread session", got[0].SessionID)
	}
}
