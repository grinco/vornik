package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
)

// stubDoer is a minimal dispatcher.Doer that captures every Process
// call and returns a canned reply. Lets us run the channel ←→
// dispatcher receiver ←→ outbound chat.postMessage pipeline end-to-
// end without standing up an actual LLM client.
type stubDoer struct {
	reply string
	calls atomic.Int64
}

func (s *stubDoer) Process(_ context.Context, req dispatcher.Request) dispatcher.Result {
	s.calls.Add(1)
	msgs := append([]chat.Message(nil), req.Messages...)
	msgs = append(msgs, chat.Message{Role: "assistant", Content: s.reply})
	return dispatcher.Result{
		Text:     s.reply,
		Messages: msgs,
	}
}

func (s *stubDoer) ProcessStreaming(ctx context.Context, req dispatcher.Request, _ chat.StreamCallback) dispatcher.Result {
	return s.Process(ctx, req)
}

// stubSessionStore is the minimal SessionStore for the E2E test.
// Returns empty history per Load; ignores Append.
type stubSessionStore struct {
	projectID string
}

func (s *stubSessionStore) Load(_ context.Context, _ conversation.ChannelMessage) (dispatcher.Session, error) {
	return dispatcher.Session{ActiveProject: s.projectID}, nil
}
func (s *stubSessionStore) Append(_ context.Context, _ conversation.ChannelMessage, _ dispatcher.Result) error {
	return nil
}

// TestE2E_AppMentionRoundTrip — slice-5 end-to-end smoke test. A
// signed app_mention webhook arrives, the channel translates it
// into a ChannelMessage, the ChannelReceiver invokes the stub
// dispatcher, the dispatcher's reply is posted back via the
// chat.postMessage stub server. Verifies every link in the chain
// the operator-visible "@vornik hi → bot replies" workflow.
func TestE2E_AppMentionRoundTrip(t *testing.T) {
	now := time.Unix(1700000000, 0)

	// 1. Stub Slack Web API for chat.postMessage.
	var (
		postedBody    atomic.Value // string of the last posted body
		postedCount   atomic.Int64
		gotAuthHeader atomic.Value // string of Authorization header
	)
	apiStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		postedCount.Add(1)
		body, _ := io.ReadAll(r.Body)
		postedBody.Store(string(body))
		gotAuthHeader.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1700000099.000200","channel":"C_ops"}`))
	}))
	t.Cleanup(apiStub.Close)

	// 2. Construct the channel pointed at the stub.
	ch, err := New(Config{
		SigningSecret: "shh",
		BotToken:      "xoxb-bot",
		TeamID:        "T_E2E",
		TeamAllowlist: []string{"T_E2E"},
		APIBaseURL:    apiStub.URL,
		HTTPClient:    apiStub.Client(),
		// PostMessageRPS=0 → defaults to 1/sec; one round-trip in the
		// test never trips it. Tests that exercise the rate limiter
		// override directly.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch.clock = func() time.Time { return now }

	// 3. Wire a stub dispatcher receiver.
	doer := &stubDoer{reply: "hi from vornik"}
	receiver := &dispatcher.ChannelReceiver{
		Channel:  ch,
		Agent:    doer,
		Sessions: &stubSessionStore{projectID: "proj-e2e"},
	}
	bindReceiver(ch, receiver)

	// 4. Mount the channel via the MuxHandler the daemon would use.
	mux := NewMuxHandler([]*Channel{ch}, zerolog.Nop())
	router := httptest.NewServer(mux)
	t.Cleanup(router.Close)

	// 5. POST a signed app_mention delivery.
	payload, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T_E2E",
		"event_id": "Ev_E2E_1",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "U_alice",
			"text":         "<@bot> hello vornik",
			"channel":      "C_ops",
			"ts":           "1700000010.000100",
			"channel_type": "channel",
		},
	})
	req := signedRequest(t, "shh", now.Unix(), payload)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// 6. Verify the dispatcher fired.
	if got := doer.calls.Load(); got != 1 {
		t.Errorf("dispatcher call count = %d, want 1", got)
	}

	// 7. Verify chat.postMessage fired with the canned reply.
	if got := postedCount.Load(); got != 1 {
		t.Errorf("chat.postMessage call count = %d, want 1", got)
	}
	bodyStr, _ := postedBody.Load().(string)
	var postedReq chatPostMessageRequest
	if err := json.Unmarshal([]byte(bodyStr), &postedReq); err != nil {
		t.Fatalf("decode posted body: %v\nbody=%s", err, bodyStr)
	}
	if postedReq.Channel != "C_ops" {
		t.Errorf("posted Channel = %q, want C_ops", postedReq.Channel)
	}
	if postedReq.Text != "hi from vornik" {
		t.Errorf("posted Text = %q, want %q", postedReq.Text, "hi from vornik")
	}
	if postedReq.ThreadTs != "1700000010.000100" {
		t.Errorf("posted ThreadTs = %q, want %q (the inbound ts as the new thread root)", postedReq.ThreadTs, "1700000010.000100")
	}
	if got := gotAuthHeader.Load(); got != "Bearer xoxb-bot" {
		t.Errorf("Authorization header = %q, want Bearer xoxb-bot", got)
	}

	// 8. Verify the channel recorded a session that ListSessions can
	//    surface to the operator UI.
	sessions, err := ch.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListSessions = %d sessions, want 1", len(sessions))
	}
	if sessions[0].ID != "T_E2E/C_ops#1700000010.000100" {
		t.Errorf("session ID = %q", sessions[0].ID)
	}
}
