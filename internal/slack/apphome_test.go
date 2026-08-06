package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// viewsPublishStub captures the views.publish payload the channel sends.
type viewsPublishStub struct {
	mu    sync.Mutex
	calls int
	users []string
	views []string
}

func (s *viewsPublishStub) snapshot() (int, []string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, append([]string(nil), s.users...), append([]string(nil), s.views...)
}

func newViewsPublishStub(t *testing.T) (*viewsPublishStub, *httptest.Server) {
	t.Helper()
	stub := &viewsPublishStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/views.publish") {
			var body struct {
				UserID string          `json:"user_id"`
				View   json.RawMessage `json:"view"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			stub.mu.Lock()
			stub.calls++
			stub.users = append(stub.users, body.UserID)
			stub.views = append(stub.views, string(body.View))
			stub.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1.1"}`))
	}))
	t.Cleanup(srv.Close)
	return stub, srv
}

func appHomeOpenedPayload(userID string) map[string]any {
	return map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev_home_" + userID,
		"event": map[string]any{
			"type": "app_home_opened",
			"user": userID,
			"tab":  "home",
		},
	}
}

// BACKLOG 2026-07-30: users opening the app's Home tab saw Slack's stock "This is still
// a work in progress… visit the About tab" text. That is what Slack renders when
// home_tab_enabled is true and the app never calls views.publish — Vornik published no
// home view at all, so the surface read as broken.
func TestAppHomeOpened_PublishesAUsefulHomeView(t *testing.T) {
	stub, srv := newViewsPublishStub(t)

	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	cfg.ChannelAllowlist = []string{"C03HTMUL2S1"}
	cfg.SenderAllowlist = []string{"U_alice"}
	cfg.SlashCommand = "/holly"
	ch := makeChannel(t, cfg, time.Now())

	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), appHomeOpenedPayload("U_alice"))

	calls, users, views := stub.snapshot()
	if calls != 1 {
		t.Fatalf("views.publish calls = %d, want 1", calls)
	}
	if users[0] != "U_alice" {
		t.Errorf("user_id = %q, want U_alice", users[0])
	}

	view := views[0]
	if !strings.Contains(view, `"type":"home"`) {
		t.Errorf("view is not a home view: %s", view)
	}
	// The point of the surface is telling someone what this bot serves and how to reach
	// it. A view that renders but says nothing useful is the placeholder with extra
	// steps.
	for _, want := range []string{"/holly", "C03HTMUL2S1"} {
		if !strings.Contains(view, want) {
			t.Errorf("home view does not mention %q: %s", want, view)
		}
	}
}

// The `tab` field distinguishes Home from Messages. Publishing on a messages-tab open
// would overwrite the home view every time someone opens a DM with the bot.
func TestAppHomeOpened_IgnoresTheMessagesTab(t *testing.T) {
	stub, srv := newViewsPublishStub(t)

	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	ch := makeChannel(t, cfg, time.Now())

	payload := appHomeOpenedPayload("U_alice")
	payload["event"].(map[string]any)["tab"] = "messages"
	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), payload)

	if calls, _, _ := stub.snapshot(); calls != 0 {
		t.Fatalf("views.publish calls = %d, want 0 for the messages tab", calls)
	}
}

// A workspace member who is not allow-listed cannot use the bot, so the home view must
// not imply otherwise — and publishing to them spends an API call for nothing.
func TestAppHomeOpened_RespectsTheSenderAllowlist(t *testing.T) {
	stub, srv := newViewsPublishStub(t)

	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	cfg.SenderAllowlist = []string{"U_alice"}
	ch := makeChannel(t, cfg, time.Now())

	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), appHomeOpenedPayload("U_stranger"))

	if calls, _, _ := stub.snapshot(); calls != 0 {
		t.Fatalf("views.publish calls = %d, want 0 for a non-allow-listed user", calls)
	}
}

// The view is JSON we assemble, and the channel allowlist is operator-supplied text.
// Anything interpolated has to survive being encoded rather than being pasted into a
// string, or a stray quote produces a view Slack rejects wholesale.
func TestAppHomeOpened_ViewIsValidJSONWithAwkwardConfig(t *testing.T) {
	stub, srv := newViewsPublishStub(t)

	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	cfg.ChannelAllowlist = []string{`C1", "injected":"`}
	cfg.SlashCommand = `/od"d`
	ch := makeChannel(t, cfg, time.Now())

	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), appHomeOpenedPayload("U_alice"))

	calls, _, views := stub.snapshot()
	if calls != 1 {
		t.Fatalf("views.publish calls = %d, want 1", calls)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(views[0]), &decoded); err != nil {
		t.Fatalf("published view is not valid JSON: %v\n%s", err, views[0])
	}
	if decoded["type"] != "home" {
		t.Errorf("view type = %v, want home", decoded["type"])
	}
}

// An app_home_opened delivery must be acked inside Slack's three-second budget like
// every other event, with the publish running on the detached context.
func TestAppHomeOpened_AcksImmediately(t *testing.T) {
	_, srv := newViewsPublishStub(t)

	cfg := validConfig()
	cfg.APIBaseURL = srv.URL
	ch := makeChannel(t, cfg, time.Now())

	body, err := json.Marshal(appHomeOpenedPayload("U_alice"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := signedRequest(t, cfg.SigningSecret, time.Now().Unix(), body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ch.waitInFlight()
}

// The "Where I answer" block is a claim the operator's users read and act on, so
// it has to track the fail-closed default 2026.8.0 introduced. Before this test
// an installation with no channel_allowlist advertised "Any channel I have been
// added to" — the exact opposite of what the gate does, since an empty list now
// denies every channel. Same root as the 2026-08-06 DM outage: a default flipped
// and the surfaces describing it were not swept.
func TestBuildHomeView_EmptyChannelAllowlistDoesNotPromiseAnyChannel(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Run("fail-closed: DMs only", func(t *testing.T) {
		view := ch.buildHomeView(&installation{projectID: "p"})
		body := flattenHomeView(t, view)
		if strings.Contains(body, "Any channel I have been added to") {
			t.Errorf("empty channel_allowlist must NOT advertise any-channel access; got:\n%s", body)
		}
		if !strings.Contains(body, "Direct messages only") {
			t.Errorf("empty channel_allowlist should say DMs only; got:\n%s", body)
		}
	})

	t.Run("dev mode opted open: any channel is honest", func(t *testing.T) {
		view := ch.buildHomeView(&installation{projectID: "p", allowUnlisted: true})
		if body := flattenHomeView(t, view); !strings.Contains(body, "Any channel I have been added to") {
			t.Errorf("allow_unlisted_senders DOES admit any channel; got:\n%s", body)
		}
	})
}

// Group DMs are dropped as an unhandled conversation type and mpim:history is
// deliberately absent from the manifest, so a user who adds the bot to one gets
// silence. Say so in the surface they will look at.
func TestBuildHomeView_StatesGroupDMsAreUnsupported(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := flattenHomeView(t, ch.buildHomeView(&installation{projectID: "p"}))
	if !strings.Contains(strings.ToLower(body), "group dm") {
		t.Errorf("home view should state group DMs are unsupported; got:\n%s", body)
	}
}

// flattenHomeView flattens every block's text so a test can assert on the view's
// prose without coupling to block ordering.
func flattenHomeView(t *testing.T, v homeView) string {
	t.Helper()
	var sb strings.Builder
	for _, b := range v.Blocks {
		if b.Text != nil {
			sb.WriteString(b.Text.Text)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}
