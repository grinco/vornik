package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
)

// slackAPIStub records the Web API calls a turn makes, so the tests can assert on the
// placeholder lifecycle without reaching Slack.
type slackAPIStub struct {
	mu    sync.Mutex
	calls []string
	texts map[string]string // ts -> text of posted messages
	seq   int
}

func newSlackAPIStub() (*slackAPIStub, *httptest.Server) {
	s := &slackAPIStub{texts: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
			Ts   string `json:"ts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		s.mu.Lock()
		s.calls = append(s.calls, r.URL.Path)
		ts := ""
		switch r.URL.Path {
		case "/chat.postMessage":
			s.seq++
			ts = "ts-" + string(rune('0'+s.seq))
			s.texts[ts] = body.Text
		case "/chat.delete":
			delete(s.texts, body.Ts)
		}
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"` + ts + `"}`))
	}))
	return s, srv
}

func (s *slackAPIStub) callsTo(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c == path {
			n++
		}
	}
	return n
}

func (s *slackAPIStub) liveTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.texts))
	for _, t := range s.texts {
		out = append(out, t)
	}
	return out
}

// progressChannel builds a channel wired to the stub API with a short signal delay so
// the tests do not have to wait two real seconds.
func progressChannel(t *testing.T, srvURL string, delay time.Duration) *Channel {
	t.Helper()
	cfg := validConfig()
	cfg.APIBaseURL = srvURL
	cfg.ProgressSignalDelay = delay
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ch
}

// awaitPlaceholder blocks until the placeholder's ts has been recorded, which is a later
// moment than the POST being sent. Polling the request count instead would race the
// assignment and make these tests flaky.
func awaitPlaceholder(t *testing.T, ch *Channel, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ch.placeholderTs(sessionID) != "" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("placeholder for %s was never posted", sessionID)
}

// BACKLOG 2026-07-30, the highest-value Slack item: a turn takes 10-60s and Slack shows
// nothing, so a working bot is indistinguishable from a broken one. That is how the
// thread-reply bug hid — a 2.9 KB answer was generated and silently lost, and the only
// way anyone noticed was to ask.
//
// A turn that runs long enough to look dead posts a placeholder, which is removed when
// the real reply lands.
func TestProgressSignal_SlowTurnPostsThenRemovesPlaceholder(t *testing.T) {
	stub, srv := newSlackAPIStub()
	defer srv.Close()
	ch := progressChannel(t, srv.URL, 10*time.Millisecond)
	const session = "T123/C_general#main"

	ch.beginProgressSignal(context.Background(), session)

	// Wait until the placeholder's ts has been RECORDED, not merely POSTed. This
	// loop used to poll stub.callsTo("/chat.postMessage"), which is the earlier of
	// the two moments: Send's delete only fires if it can see a stored
	// placeholder ts, so winning that race left the placeholder undeleted and
	// failed at "placeholder deletes = 0, want 1" — observed on the parent repo's
	// slower CI runner (grinco/vornik-ee PR #62, 2026-08-06), never locally.
	// awaitPlaceholder exists for exactly this and says so; the other two
	// progress tests already used it.
	awaitPlaceholder(t, ch, session)
	if got := stub.callsTo("/chat.postMessage"); got != 1 {
		t.Fatalf("placeholder posts = %d, want 1 after the delay elapsed", got)
	}

	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: session,
		Text:      "here is the answer",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := stub.callsTo("/chat.delete"); got != 1 {
		t.Fatalf("placeholder deletes = %d, want 1 once the reply landed", got)
	}
	live := stub.liveTexts()
	if len(live) != 1 || live[0] != "here is the answer" {
		t.Fatalf("surviving messages = %v, want only the answer", live)
	}
}

// A fast turn must leave no trace. Posting then deleting a placeholder for a reply that
// arrives in under a second is pure flicker, and it doubles the outbound message rate
// against Slack's 1-msg/sec/channel Tier-3 cap for no benefit.
func TestProgressSignal_FastTurnPostsNoPlaceholder(t *testing.T) {
	stub, srv := newSlackAPIStub()
	defer srv.Close()
	ch := progressChannel(t, srv.URL, time.Hour) // never fires within the test
	const session = "T123/C_general#main"

	ch.beginProgressSignal(context.Background(), session)
	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: session,
		Text:      "quick answer",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := stub.callsTo("/chat.delete"); got != 0 {
		t.Fatalf("chat.delete calls = %d, want 0 — nothing was ever posted", got)
	}
	if got := stub.callsTo("/chat.postMessage"); got != 1 {
		t.Fatalf("postMessage calls = %d, want 1 (the answer only)", got)
	}
}

// The disclosure notice is sent through the same Send path and arrives BEFORE the
// answer on a session's first turn. It must consume the placeholder like any other
// real message — otherwise the placeholder outlives the turn and sits there forever.
func TestProgressSignal_FirstRealMessageConsumesIt(t *testing.T) {
	stub, srv := newSlackAPIStub()
	defer srv.Close()
	ch := progressChannel(t, srv.URL, 10*time.Millisecond)
	const session = "T123/C_general#main"

	ch.beginProgressSignal(context.Background(), session)
	deadline := time.Now().Add(2 * time.Second)
	for stub.callsTo("/chat.postMessage") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// The two real messages are a second apart in production (the disclosure precedes
	// the model call). Drive the clock rather than sleeping, so the per-channel Tier-3
	// bucket — which is orthogonal to what this test asserts — refills between them.
	now := time.Now()
	ch.clock = func() time.Time { return now }
	for _, text := range []string{"You are interacting with an AI system.", "the answer"} {
		if _, err := ch.Send(context.Background(), conversation.ChannelMessage{
			SessionID: session, Text: text,
		}); err != nil {
			t.Fatalf("Send(%q): %v", text, err)
		}
		now = now.Add(2 * time.Second)
	}

	if got := stub.callsTo("/chat.delete"); got != 1 {
		t.Fatalf("chat.delete calls = %d, want exactly 1 — the placeholder is removed "+
			"once, by whichever real message lands first", got)
	}
}

// Signals are per session: a placeholder armed for a DM must not be cleared by a reply
// posted into a channel thread.
func TestProgressSignal_IsScopedPerSession(t *testing.T) {
	stub, srv := newSlackAPIStub()
	defer srv.Close()
	ch := progressChannel(t, srv.URL, 10*time.Millisecond)

	ch.beginProgressSignal(context.Background(), "T123/D0BLA9ZRDFH#main")
	deadline := time.Now().Add(2 * time.Second)
	for stub.callsTo("/chat.postMessage") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#main", Text: "unrelated channel reply",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := stub.callsTo("/chat.delete"); got != 0 {
		t.Fatalf("chat.delete calls = %d, want 0 — the DM's placeholder is not this "+
			"session's to clear", got)
	}
}

// A placeholder must never outlive the daemon's own bookkeeping: Stop drains in-flight
// turns, so an armed-but-unfired signal has to be disarmed rather than firing into a
// channel after shutdown.
func TestProgressSignal_StopDisarmsPendingSignals(t *testing.T) {
	stub, srv := newSlackAPIStub()
	defer srv.Close()
	ch := progressChannel(t, srv.URL, 30*time.Millisecond)

	ch.beginProgressSignal(context.Background(), "T123/C_general#main")
	if err := ch.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	time.Sleep(120 * time.Millisecond) // well past the delay
	if got := stub.callsTo("/chat.postMessage"); got != 0 {
		t.Fatalf("postMessage calls after Stop = %d, want 0", got)
	}
}

// The placeholder must not compete with the reply for the per-(team, channel) Tier-3
// token bucket. acquireOutboundToken returns an ERROR rather than waiting when the
// bucket is empty, so a placeholder that consumed the token would make a turn finishing
// shortly after the delay fail to deliver its answer — converting a cosmetic feature
// into the very silent-loss bug it exists to expose.
func TestProgressSignal_DoesNotConsumeTheReplyRateLimitToken(t *testing.T) {
	stub, srv := newSlackAPIStub()
	defer srv.Close()
	ch := progressChannel(t, srv.URL, 5*time.Millisecond)
	const session = "T123/C_general#main"

	ch.beginProgressSignal(context.Background(), session)
	deadline := time.Now().Add(2 * time.Second)
	for stub.callsTo("/chat.postMessage") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stub.callsTo("/chat.postMessage") != 1 {
		t.Fatal("placeholder never posted")
	}

	// Immediately after the placeholder — the worst case for the bucket.
	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: session,
		Text:      "the answer, delivered despite the placeholder",
	}); err != nil {
		t.Fatalf("reply Send failed right after a placeholder: %v", err)
	}
}

// OPERATOR REQUEST 2026-07-30: a mechanism to display what the turn is doing — thinking,
// calling a tool — that "can be rewritten and doesn't have to use any of the context".
//
// The placeholder is rewritten in place via chat.update as the dispatcher reports each
// tool call, so the human watches the work progress. No new OAuth scope, unlike
// assistant.threads.setStatus.
func TestSetTurnStatus_RewritesThePostedPlaceholder(t *testing.T) {
	stub, srv := newSlackAPIStub()
	defer srv.Close()
	ch := progressChannel(t, srv.URL, 10*time.Millisecond)
	const session = "T123/C_general#main"

	ch.beginProgressSignal(context.Background(), session)
	awaitPlaceholder(t, ch, session)

	ch.SetTurnStatus(context.Background(), session, "searching memory…")
	ch.SetTurnStatus(context.Background(), session, "scheduling the job…")

	if got := stub.callsTo("/chat.update"); got != 2 {
		t.Fatalf("chat.update calls = %d, want 2 — one per reported activity", got)
	}
	// Still exactly one message in the channel: it is rewritten, not re-posted.
	if got := stub.callsTo("/chat.postMessage"); got != 1 {
		t.Fatalf("postMessage calls = %d, want 1 — the status replaces the text in place", got)
	}
}

// A status must never CREATE a placeholder. The delay exists so a turn that answers in
// under two seconds leaves no trace, and a tool call happens well inside that window.
func TestSetTurnStatus_NeverPostsWhenNoPlaceholderIsUpYet(t *testing.T) {
	stub, srv := newSlackAPIStub()
	defer srv.Close()
	ch := progressChannel(t, srv.URL, time.Hour) // the timer will not fire
	const session = "T123/C_general#main"

	ch.beginProgressSignal(context.Background(), session)
	ch.SetTurnStatus(context.Background(), session, "searching memory…")

	if got := stub.callsTo("/chat.postMessage"); got != 0 {
		t.Fatalf("postMessage calls = %d, want 0 — a fast turn stays silent", got)
	}
	if got := stub.callsTo("/chat.update"); got != 0 {
		t.Fatalf("chat.update calls = %d, want 0 — there is nothing to update", got)
	}
}

// A status reported before the placeholder is posted is remembered, so the placeholder
// opens on what the turn is ACTUALLY doing rather than the generic line.
func TestSetTurnStatus_PlaceholderOpensOnTheLatestActivity(t *testing.T) {
	stub, srv := newSlackAPIStub()
	defer srv.Close()
	ch := progressChannel(t, srv.URL, 60*time.Millisecond)
	const session = "T123/C_general#main"

	ch.beginProgressSignal(context.Background(), session)
	ch.SetTurnStatus(context.Background(), session, "searching memory…")

	deadline := time.Now().Add(2 * time.Second)
	for stub.callsTo("/chat.postMessage") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	live := stub.liveTexts()
	if len(live) != 1 || !strings.Contains(live[0], "searching memory") {
		t.Fatalf("opening placeholder = %v, want the reported activity", live)
	}
}

// Once the reply has landed the status is finished. Updating afterwards would resurrect
// a line the person has no reason to see again — and the message may already be deleted.
func TestSetTurnStatus_IgnoredAfterTheReplyLands(t *testing.T) {
	stub, srv := newSlackAPIStub()
	defer srv.Close()
	ch := progressChannel(t, srv.URL, 10*time.Millisecond)
	const session = "T123/C_general#main"

	ch.beginProgressSignal(context.Background(), session)
	awaitPlaceholder(t, ch, session)
	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: session, Text: "the answer",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	before := stub.callsTo("/chat.update")
	ch.SetTurnStatus(context.Background(), session, "too late…")
	if got := stub.callsTo("/chat.update"); got != before {
		t.Fatalf("chat.update calls = %d, want %d — the turn is over", got, before)
	}
}

// An unknown session, or an empty status, must not reach Slack at all.
func TestSetTurnStatus_IgnoresUnknownSessionsAndEmptyText(t *testing.T) {
	stub, srv := newSlackAPIStub()
	defer srv.Close()
	ch := progressChannel(t, srv.URL, 10*time.Millisecond)

	ch.SetTurnStatus(context.Background(), "T123/C_never#main", "searching…")
	ch.SetTurnStatus(context.Background(), "", "searching…")
	ch.SetTurnStatus(context.Background(), "T123/C_general#main", "   ")

	if got := stub.callsTo("/chat.update") + stub.callsTo("/chat.postMessage"); got != 0 {
		t.Fatalf("API calls = %d, want 0", got)
	}
}

// Compile-time proof the channel satisfies the interface the service layer asserts on.
func TestChannel_SatisfiesTurnStatusChannel(_ *testing.T) {
	var _ conversation.TurnStatusChannel = (*Channel)(nil)
}
