package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
)

// recordingReceiver captures every Receive call so tests can assert
// on what the channel forwarded to the dispatcher.
type recordingReceiver struct {
	mu  sync.Mutex
	got []conversation.ChannelMessage
	err error // optional error to return from Receive
}

func (r *recordingReceiver) Receive(_ context.Context, msg conversation.ChannelMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, msg)
	return r.err
}

func (r *recordingReceiver) snapshot() []conversation.ChannelMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]conversation.ChannelMessage, len(r.got))
	copy(out, r.got)
	return out
}

// bindReceiver wires a recordingReceiver into the channel via the
// recv field directly. Tests use this rather than Start to avoid the
// goroutine + context plumbing — Start would block on ctx.Done.
func bindReceiver(ch *Channel, recv conversation.Receiver) {
	ch.recvMu.Lock()
	ch.recv = recv
	ch.recvMu.Unlock()
}

// postSignedJSON encodes the payload, signs it under cfg.SigningSecret
// at `now`, and POSTs through the channel's HandleWebhook. Returns
// the captured ResponseRecorder for assertions.
func postSignedJSON(t *testing.T, ch *Channel, secret string, now time.Time, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := signedRequest(t, secret, now.Unix(), body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, req)
	return w
}

// makeChannel constructs a channel pinned to a fixed clock for
// deterministic replay-window behaviour.
func makeChannel(t *testing.T, cfg Config, now time.Time) *Channel {
	t.Helper()
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch.clock = func() time.Time { return now }
	return ch
}

// TestChannel_SatisfiesInterface — compile-time guard.
func TestChannel_SatisfiesInterface(t *testing.T) {
	ch, _ := New(validConfig())
	var _ conversation.Channel = ch
}

// TestSend_NoBotTokenReturnsSentinel — a channel with no BotToken
// surfaces ErrOutboundNotConfigured so callers branch via errors.Is.
// SessionID has to be valid so we get past the parse gate; this test
// exercises the post-parse "no credentials" path.
func TestSend_NoBotTokenReturnsSentinel(t *testing.T) {
	cfg := validConfig()
	cfg.BotToken = ""
	ch, _ := New(cfg)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "hi",
	})
	if !errors.Is(err, ErrOutboundNotConfigured) {
		t.Errorf("Send err = %v, want ErrOutboundNotConfigured", err)
	}
}

// TestHandleWebhook_URLVerification — Slack's one-shot endpoint-
// registration handshake. The handler must echo `challenge` back in
// the response body so the Slack admin UI can confirm the URL is
// reachable.
func TestHandleWebhook_URLVerification(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)

	w := postSignedJSON(t, ch, cfg.SigningSecret, now, map[string]any{
		"type":      "url_verification",
		"challenge": "abc123",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "abc123" {
		t.Errorf("body = %q, want %q", got, "abc123")
	}
}

// TestHandleWebhook_BadSignature_Returns401 — the signature gate
// is the first line of defence; a tampered body must fail before
// any payload-parse logic runs.
func TestHandleWebhook_BadSignature_Returns401(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)

	body := []byte(`{"type":"event_callback"}`)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set("X-Slack-Request-Timestamp", fmt.Sprintf("%d", now.Unix()))
	req.Header.Set("X-Slack-Signature", "v0=deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// TestHandleWebhook_WrongMethod — Slack only ever POSTs; reject
// other verbs so a misconfigured proxy doesn't surface as a 200.
func TestHandleWebhook_WrongMethod(t *testing.T) {
	cfg := validConfig()
	ch, _ := New(cfg)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

// TestHandleWebhook_OversizedBody — defensive cap.
func TestHandleWebhook_OversizedBody(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	// We don't need to sign — the size check runs before signature
	// verification (the size check protects the verifier from a
	// memory exhaustion attack).
	body := strings.Repeat("a", maxWebhookBodyBytes+10)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

// TestHandleWebhook_AppMention_Dispatches — happy path: an
// app_mention event from a known team and a known user produces
// exactly one ChannelMessage forwarded to the receiver, with the
// right Source/SessionID shape and ChannelSpecific metadata.
func TestHandleWebhook_AppMention_Dispatches(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev0001",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "U_alice",
			"text":         "<@U_bot> hello",
			"channel":      "C_general",
			"ts":           "1700000001.000100",
			"channel_type": "channel",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("Receive call count = %d, want 1", len(got))
	}
	msg := got[0]
	if msg.Source != channelName {
		t.Errorf("Source = %q, want %q", msg.Source, channelName)
	}
	if msg.SessionID != "T123/C_general#1700000001.000100" {
		t.Errorf("SessionID = %q, want T123/C_general#1700000001.000100", msg.SessionID)
	}
	if msg.SpeakerID != "U_alice" {
		t.Errorf("SpeakerID = %q, want U_alice", msg.SpeakerID)
	}
	if msg.Text != "<@U_bot> hello" {
		t.Errorf("Text = %q, want %q", msg.Text, "<@U_bot> hello")
	}
	if got := msg.ChannelSpecific["team_id"]; got != "T123" {
		t.Errorf("ChannelSpecific[team_id] = %q, want T123", got)
	}
	if got := msg.ChannelSpecific["channel_id"]; got != "C_general" {
		t.Errorf("ChannelSpecific[channel_id] = %q, want C_general", got)
	}
}

// TestHandleWebhook_MessageIM_Dispatches — DM messages bypass the
// @mention requirement; every message in a DM is for the bot.
func TestHandleWebhook_MessageIM_Dispatches(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev0002",
		"event": map[string]any{
			"type":         "message",
			"user":         "U_alice",
			"text":         "hi without any mention",
			"channel":      "D_alice",
			"ts":           "1700000002.000100",
			"channel_type": "im",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("Receive call count = %d, want 1 (DMs dispatch without mention)", len(got))
	}
}

// TestHandleWebhook_MessageChannels_RequiresMention — public channel
// messages without @vornik drop silently; same channel with mention
// dispatches.
func TestHandleWebhook_MessageChannels_RequiresMention(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	// First: no mention → drop.
	noMention := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev0003",
		"event": map[string]any{
			"type":         "message",
			"user":         "U_alice",
			"text":         "just chatting",
			"channel":      "C_general",
			"ts":           "1700000003.000100",
			"channel_type": "channel",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, noMention)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("Receive count after no-mention = %d, want 0", len(got))
	}

	// Second: with @vornik → dispatch.
	withMention := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev0004",
		"event": map[string]any{
			"type":         "message",
			"user":         "U_alice",
			"text":         "@vornik please look at this",
			"channel":      "C_general",
			"ts":           "1700000004.000100",
			"channel_type": "channel",
		},
	}
	w = postSignedJSON(t, ch, cfg.SigningSecret, now, withMention)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("Receive count after mention = %d, want 1", len(got))
	}
}

// TestHandleWebhook_UnknownTeam_DropsWith200 — Slack retries on
// non-200, so unknown team_ids must ack and audit-log.
func TestHandleWebhook_UnknownTeam_DropsWith200(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T_OTHER",
		"event_id": "Ev0005",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "U_alice",
			"text":         "<@bot> hi",
			"channel":      "C_general",
			"ts":           "1700000005.000100",
			"channel_type": "channel",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Slack would retry indefinitely otherwise); body=%s", w.Code, w.Body.String())
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("Receive count = %d, want 0 for unknown team", len(got))
	}
}

// TestHandleWebhook_UnknownSpeaker_DropsBeforeDispatch — the
// per-installation SenderAllowlist enforces an "no LLM spend on
// unauthorised callers" guarantee documented in the conversation-
// channel design. A non-empty allowlist + a non-listed user must
// drop before the receiver is invoked.
func TestHandleWebhook_UnknownSpeaker_DropsBeforeDispatch(t *testing.T) {
	cfg := validConfig()
	cfg.SenderAllowlist = []string{"U_known"}
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev0006",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "U_strange",
			"text":         "<@bot> hi",
			"channel":      "C_general",
			"ts":           "1700000006.000100",
			"channel_type": "channel",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("Receive count = %d, want 0 for non-allowlisted user (no LLM spend)", len(got))
	}
}

// TestHandleWebhook_ChannelNotAllowlisted_Drops — pinned allowlist
// rejects messages from a channel not on it. A misclick in Slack's
// "install to channel" picker shouldn't expose the bot.
func TestHandleWebhook_ChannelNotAllowlisted_Drops(t *testing.T) {
	cfg := validConfig()
	cfg.ChannelAllowlist = []string{"C_ops"}
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev0007",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "U_alice",
			"text":         "<@bot> hi",
			"channel":      "C_random",
			"ts":           "1700000007.000100",
			"channel_type": "channel",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("Receive count = %d, want 0 for unallowed channel", len(got))
	}
}

// TestHandleWebhook_BotEcho_Drops — Slack delivers a bot's own
// outbound back to itself via message.channels with a non-empty
// bot_id. The channel must drop those to prevent feedback loops.
func TestHandleWebhook_BotEcho_Drops(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev0008",
		"event": map[string]any{
			"type":         "message",
			"user":         "U_alice",
			"text":         "@vornik echo",
			"channel":      "C_general",
			"ts":           "1700000008.000100",
			"channel_type": "channel",
			"bot_id":       "B_vornik",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("Receive count = %d, want 0 for bot-echoed message", len(got))
	}
}

// TestHandleWebhook_Subtype_Drops — message_changed / message_deleted
// / etc. are not user-authored content; ignore them so edit-storms
// don't trigger N dispatcher turns.
func TestHandleWebhook_Subtype_Drops(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev0009",
		"event": map[string]any{
			"type":         "message",
			"user":         "U_alice",
			"text":         "@vornik revised",
			"channel":      "C_general",
			"ts":           "1700000009.000100",
			"channel_type": "channel",
			"subtype":      "message_changed",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("Receive count = %d, want 0 for subtyped message", len(got))
	}
}

// TestHandleWebhook_ThreadReply_PinsToRoot — a reply in a thread
// (ThreadTs ≠ Ts) uses ThreadTs as the SessionID anchor so sibling
// replies collapse into one session.
func TestHandleWebhook_ThreadReply_PinsToRoot(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev0010",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "U_alice",
			"text":         "<@bot> reply",
			"channel":      "C_general",
			"ts":           "1700000020.000100",
			"thread_ts":    "1700000010.000100",
			"channel_type": "channel",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("Receive count = %d, want 1", len(got))
	}
	if want := "T123/C_general#1700000010.000100"; got[0].SessionID != want {
		t.Errorf("SessionID = %q, want %q (thread root)", got[0].SessionID, want)
	}
	if got[0].ThreadID != "1700000010.000100" {
		t.Errorf("ThreadID = %q, want thread root ts", got[0].ThreadID)
	}
}

// TestHandleWebhook_RecordsSession — a successful inbound updates
// ListSessions so the operator UI surfaces it. Also covers the
// session-pin contract: subsequent events on the same SessionID
// keep the same installation pin.
func TestHandleWebhook_RecordsSession(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	bindReceiver(ch, &recordingReceiver{})

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev0011",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "U_alice",
			"text":         "<@bot> hi",
			"channel":      "C_general",
			"ts":           "1700000011.000100",
			"channel_type": "channel",
		},
	}
	_ = postSignedJSON(t, ch, cfg.SigningSecret, now, payload)

	sessions, err := ch.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1", len(sessions))
	}
	if sessions[0].ID != "T123/C_general#1700000011.000100" {
		t.Errorf("session ID = %q", sessions[0].ID)
	}
	if sessions[0].ParticipantCount != 1 {
		t.Errorf("ParticipantCount = %d, want 1", sessions[0].ParticipantCount)
	}
}

// TestResolveSpeaker_EmptyAllowlistAdmitsAny — dev-mode pass-through.
func TestResolveSpeaker_EmptyAllowlistAdmitsAny(t *testing.T) {
	cfg := validConfig() // SenderAllowlist nil
	ch, _ := New(cfg)
	sp, err := ch.ResolveSpeaker(context.Background(), "U_random")
	if err != nil {
		t.Fatalf("ResolveSpeaker: %v", err)
	}
	if sp.ID != "slack:U_random" {
		t.Errorf("Speaker.ID = %q", sp.ID)
	}
}

// TestResolveSpeaker_AllowlistRejectsUnknown — non-empty allowlist
// gates the speaker; an unknown user surfaces ErrSpeakerUnknown so
// the dispatcher can branch on it via errors.Is.
func TestResolveSpeaker_AllowlistRejectsUnknown(t *testing.T) {
	cfg := validConfig()
	cfg.SenderAllowlist = []string{"U_known"}
	ch, _ := New(cfg)
	_, err := ch.ResolveSpeaker(context.Background(), "U_stranger")
	if !errors.Is(err, conversation.ErrSpeakerUnknown) {
		t.Errorf("ResolveSpeaker err = %v, want ErrSpeakerUnknown", err)
	}
}

// TestResolveSpeaker_EmptyIDRejected — defensive: empty id should
// never round-trip to a valid Speaker.
func TestResolveSpeaker_EmptyIDRejected(t *testing.T) {
	ch, _ := New(validConfig())
	_, err := ch.ResolveSpeaker(context.Background(), "")
	if !errors.Is(err, conversation.ErrSpeakerUnknown) {
		t.Errorf("ResolveSpeaker(empty) err = %v, want ErrSpeakerUnknown", err)
	}
}

// TestStartStop_RequireReceiver — Start with nil Receiver returns
// immediately; Stop is idempotent.
func TestStartStop_RequireReceiver(t *testing.T) {
	ch, _ := New(validConfig())
	if err := ch.Start(context.Background(), nil); err == nil {
		t.Fatal("Start with nil Receiver returned nil, want error")
	}
	if err := ch.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := ch.Stop(); err != nil {
		t.Errorf("Stop (second call): %v", err)
	}
}

// TestStart_BlocksUntilCtxCancelled — sanity check that Start
// behaves as documented for callers that spawn it in a goroutine.
func TestStart_BlocksUntilCtxCancelled(t *testing.T) {
	ch, _ := New(validConfig())
	rec := &recordingReceiver{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ch.Start(ctx, rec) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Start err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return within 1s after cancel")
	}
}

// TestMentionsVornik — word-boundary aware matcher. Compresses the
// edge cases into one table-driven test.
func TestMentionsVornik(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"@vornik hello", true},
		{"hello @vornik", true},
		{"HEY @VORNIK", true},
		{"@vornik-deploy please", false}, // word-boundary fail
		{"see @vorniks plural", false},
		{"foo bar baz", false},
		{"", false},
		{"@swarm", false},
		{"@vornik", true}, // at end of string
		{"text @vornik!", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := mentionsVornik(tc.in); got != tc.want {
				t.Errorf("mentionsVornik(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestSlackTsToTime — parses Slack's "<sec>.<usec>" ts to time.Time.
func TestSlackTsToTime(t *testing.T) {
	fallback := time.Unix(999, 0)
	clock := func() time.Time { return fallback }
	cases := []struct {
		in     string
		expSec int64
	}{
		{"1700000000.000100", 1700000000},
		{"1700000000", 1700000000},
		{"", fallback.Unix()},
		{"not-a-ts", fallback.Unix()},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := slackTsToTime(tc.in, clock)
			if got.Unix() != tc.expSec {
				t.Errorf("slackTsToTime(%q).Unix() = %d, want %d", tc.in, got.Unix(), tc.expSec)
			}
		})
	}
}

// TestProjectForSession_UnknownReturnsEmpty — session pin defaults
// to empty for sessions the channel hasn't seen.
func TestProjectForSession_UnknownReturnsEmpty(t *testing.T) {
	ch, _ := New(validConfig())
	if got := ch.ProjectForSession("T_unknown/C_unknown#ts"); got != "" {
		t.Errorf("ProjectForSession = %q, want empty", got)
	}
}

// TestRecordSession_UpdatesPinOnFirstEvent_LeavesOnSubsequent —
// install pin is set on first event and never re-pinned. Matches
// the GitHub channel's contract.
func TestRecordSession_UpdatesPinOnFirstEvent_LeavesOnSubsequent(t *testing.T) {
	cfg := Config{
		SigningSecret: "shhh",
		Installations: []InstallationConfig{
			{ProjectID: "proj-a", TeamID: "T_A"},
			{ProjectID: "proj-b", TeamID: "T_B"},
		},
	}
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	instA := ch.installationsByID["T_A"]
	instB := ch.installationsByID["T_B"]
	now := time.Unix(1700000000, 0)
	ch.recordSession("S1", "title-a", "U1", now, instA)
	if got := ch.ProjectForSession("S1"); got != "proj-a" {
		t.Fatalf("ProjectForSession after first event = %q, want proj-a", got)
	}
	// Second event on the same session with a different installation
	// must not overwrite the pin.
	ch.recordSession("S1", "title-b", "U2", now.Add(time.Minute), instB)
	if got := ch.ProjectForSession("S1"); got != "proj-a" {
		t.Errorf("ProjectForSession after re-pin attempt = %q, want still proj-a", got)
	}
}

// TestHandleWebhook_NonEventCallback_Acks — payloads with an
// unrecognised top-level type ack silently so Slack doesn't retry.
func TestHandleWebhook_NonEventCallback_Acks(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, map[string]any{"type": "future_thing"})
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("Receive count = %d, want 0", len(got))
	}
}

// TestHandleWebhook_MalformedJSON_Returns400 — payload parse
// failure surfaces as 400 so an operator's curl test shows the
// problem immediately.
func TestHandleWebhook_MalformedJSON_Returns400(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	body := []byte("not-json")
	req := signedRequest(t, cfg.SigningSecret, now.Unix(), body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestHandleWebhook_ReceiverErr_LoggedNotPropagated — a receiver
// returning an error must not crash the handler; we always 200 on
// signature-valid deliveries so Slack doesn't retry.
func TestHandleWebhook_ReceiverErr_LoggedNotPropagated(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{err: errors.New("downstream boom")}
	bindReceiver(ch, rec)

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev0099",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "U_alice",
			"text":         "<@bot> hi",
			"channel":      "C_general",
			"ts":           "1700000099.000100",
			"channel_type": "channel",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 even on receiver err", w.Code)
	}
	if got := rec.snapshot(); len(got) != 1 {
		t.Errorf("Receive count = %d, want 1 (handler must still call Receive)", len(got))
	}
}
