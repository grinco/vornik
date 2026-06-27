package slack

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/conversation"
)

// TestResolveSpeakerForInstallation_EmptyUserRejected — covers the
// per-installation enforcement helper directly. The HTTP path drops
// the event before this method runs (the channel.handleMessageEvent
// caller short-circuits on `ev.User == ""`), so coverage here came in
// at 83% with the empty-user defensive branch missing.
func TestResolveSpeakerForInstallation_EmptyUserRejected(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	inst := ch.installations[0]
	if _, err := ch.resolveSpeakerForInstallation(inst, ""); !errors.Is(err, conversation.ErrSpeakerUnknown) {
		t.Errorf("resolveSpeakerForInstallation('') err = %v, want ErrSpeakerUnknown", err)
	}
	// Whitespace-only is equivalent to empty after TrimSpace.
	if _, err := ch.resolveSpeakerForInstallation(inst, "   "); !errors.Is(err, conversation.ErrSpeakerUnknown) {
		t.Errorf("resolveSpeakerForInstallation('   ') err = %v, want ErrSpeakerUnknown", err)
	}
}

// TestAnyInstallationAllowsSpeaker_HitOnPopulatedAllowlist — the
// existing tests only cover the empty-allowlist pass-through branch.
// Pin the hit-on-populated path so a future "always pass" regression
// surfaces in coverage.
func TestAnyInstallationAllowsSpeaker_HitOnPopulatedAllowlist(t *testing.T) {
	cfg := validConfig()
	cfg.SenderAllowlist = []string{"U_alice"}
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !ch.anyInstallationAllowsSpeaker("U_alice") {
		t.Error("populated allowlist with matching id should admit")
	}
	if ch.anyInstallationAllowsSpeaker("U_bob") {
		t.Error("populated allowlist with non-matching id should reject")
	}
}

// TestHandleWebhook_ReadBodyFailure — io.ReadAll on a body that
// errors mid-read returns 400 rather than panicking. We model this
// with a Reader that returns the first byte then errors; the size
// check is irrelevant since the read fails before length is known.
func TestHandleWebhook_ReadBodyFailure(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	body := &erroringReader{err: errors.New("read boom"), data: []byte("partial")}
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.Header.Set("X-Slack-Request-Timestamp", "1700000000")
	req.Header.Set("X-Slack-Signature", "v0=deadbeef")
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on body read failure", w.Code)
	}
}

// TestHandleWebhook_EmptyTeamID_Acks — Slack envelopes without
// team_id can't be routed; the channel must ack with 200 + audit
// log rather than 400 (Slack retries on non-200).
func TestHandleWebhook_EmptyTeamID_Acks(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "   ", // whitespace trims to empty
		"event_id": "Ev_nilteam",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "U_alice",
			"text":         "<@bot> hi",
			"channel":      "C_general",
			"ts":           "1700000010.000100",
			"channel_type": "channel",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("Receive count = %d, want 0 (no team_id ⇒ drop)", len(got))
	}
}

// TestHandleWebhook_UnknownChannelType_Drops — message events with
// a channel_type the handler doesn't recognise (e.g. "mpim") fall
// through to the debug-log branch. Asserts no dispatcher fan-out
// without coupling to the log content.
func TestHandleWebhook_UnknownChannelType_Drops(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev_mpim",
		"event": map[string]any{
			"type":         "message",
			"user":         "U_alice",
			"text":         "@vornik hi",
			"channel":      "G_private",
			"ts":           "1700000010.000100",
			"channel_type": "mpim", // not "im"/"channel"/"group"
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("Receive count = %d, want 0 for unknown channel_type", len(got))
	}
}

// TestHandleWebhook_UnknownEventType_Acks — a non-message,
// non-app_mention event_type (e.g. "reaction_added") falls through
// to the default debug-log branch.
func TestHandleWebhook_UnknownEventType_Acks(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev_reaction",
		"event": map[string]any{
			"type": "reaction_added",
			"user": "U_alice",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("Receive count = %d, want 0 for non-message event type", len(got))
	}
}

// TestHandleMessageEvent_EmptyUser_Drops — an app_mention without
// user (a malformed Slack delivery) drops before dispatch. The
// HandleWebhook path already covers the happy case; this one
// exercises the defensive guard.
func TestHandleMessageEvent_EmptyUser_Drops(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev_nouser",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "", // empty
			"text":         "<@bot> hi",
			"channel":      "C_general",
			"ts":           "1700000010.000100",
			"channel_type": "channel",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("Receive count = %d, want 0 for empty-user event", len(got))
	}
}

// TestHandleMessageEvent_NoReceiverBound_Drops — events that pass
// every allowlist but arrive before Start has wired a Receiver are
// dropped with a warning log. Operator sees the warn; no panic.
func TestHandleMessageEvent_NoReceiverBound_Drops(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	// Deliberately do NOT call bindReceiver — c.recv stays nil.

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev_norecv",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "U_alice",
			"text":         "<@bot> hi",
			"channel":      "C_general",
			"ts":           "1700000010.000100",
			"channel_type": "channel",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no receiver still acks)", w.Code)
	}
}

// TestHandleMessageEvent_NonReceiverBound_Drops — the recv field is
// typed `any` (so types.go doesn't import conversation). A future
// refactor that binds the wrong concrete type into recv would surface
// at runtime; the channel logs + drops rather than panicking.
func TestHandleMessageEvent_NonReceiverBound_Drops(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	// Set recv to a type that doesn't satisfy conversation.Receiver.
	ch.recvMu.Lock()
	ch.recv = "not-a-receiver"
	ch.recvMu.Unlock()

	payload := map[string]any{
		"type":     "event_callback",
		"team_id":  "T123",
		"event_id": "Ev_wrongtype",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "U_alice",
			"text":         "<@bot> hi",
			"channel":      "C_general",
			"ts":           "1700000010.000100",
			"channel_type": "channel",
		},
	}
	w := postSignedJSON(t, ch, cfg.SigningSecret, now, payload)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (bad-type recv still acks)", w.Code)
	}
}

// TestChannelTitleFromPayload_NilEvent — defensive guard against a
// future refactor that lets a nil Event reach the title helper.
// Currently HandleWebhook short-circuits earlier; this pins the
// behaviour so a refactor can't silently produce a panic.
func TestChannelTitleFromPayload_NilEvent(t *testing.T) {
	if got := channelTitleFromPayload(eventPayload{Event: nil}); got != "" {
		t.Errorf("channelTitleFromPayload(nil event) = %q, want empty", got)
	}
}

// TestIsMentionWordChar_DigitsAndUnderscores — the mention parser
// treats digits, '-', and '_' as word characters so "@vornik123" or
// "@vornik_bot" don't false-positive as a mention. The
// TestMentionsVornik table exercises letters; this one pins the
// digit branch.
func TestIsMentionWordChar_DigitsAndUnderscores(t *testing.T) {
	for _, b := range []byte{'0', '5', '9'} {
		if !isMentionWordChar(b) {
			t.Errorf("isMentionWordChar(%q) = false, want true (digit)", b)
		}
	}
	for _, b := range []byte{'_', '-'} {
		if !isMentionWordChar(b) {
			t.Errorf("isMentionWordChar(%q) = false, want true", b)
		}
	}
	for _, b := range []byte{' ', '!', '@', '/', '.'} {
		if isMentionWordChar(b) {
			t.Errorf("isMentionWordChar(%q) = true, want false", b)
		}
	}
}

// TestMentionsVornik_DigitSuffix — pinned through the public surface
// so the digit branch in isMentionWordChar is exercised end-to-end:
// "@vornik0" should NOT count as a mention.
func TestMentionsVornik_DigitSuffix(t *testing.T) {
	if mentionsVornik("hello @vornik0") {
		t.Error("mentionsVornik should reject digit-suffixed name")
	}
}

// TestResolveInstallations_TeamAllowlistOnly_UsesFirstAsTeamID —
// single-installation back-compat: when TeamID is empty and only
// TeamAllowlist is set, the synthesised installation pins on the
// first allowlist entry. Earlier coverage left this branch at 0.
func TestResolveInstallations_TeamAllowlistOnly_UsesFirstAsTeamID(t *testing.T) {
	cfg := Config{
		SigningSecret: "shhh",
		TeamID:        "", // no top-level pin
		TeamAllowlist: []string{"T_FIRST", "T_SECOND"},
	}
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(ch.installations) != 2 {
		t.Fatalf("installations = %d, want 2 (first + second)", len(ch.installations))
	}
	if ch.installations[0].teamID != "T_FIRST" {
		t.Errorf("installations[0].teamID = %q, want T_FIRST", ch.installations[0].teamID)
	}
	// The second allowlist entry should appear as a no-token route
	// so inbound passes but outbound from that workspace fails the
	// empty-token sentinel.
	second, ok := ch.installationsByID["T_SECOND"]
	if !ok {
		t.Fatal("T_SECOND not in installationsByID")
	}
	if second.botToken != "" {
		t.Errorf("second installation botToken = %q, want empty (extra team)", second.botToken)
	}
}

// TestResolveInstallations_SkipsDuplicateAndEmptyAllowlistEntries —
// the loop appending the "extra teams" skips empty strings and the
// already-claimed primary team_id. Earlier coverage left both
// branches at 0.
func TestResolveInstallations_SkipsDuplicateAndEmptyAllowlistEntries(t *testing.T) {
	cfg := Config{
		SigningSecret: "shhh",
		TeamID:        "T_PRIMARY",
		// "" must be skipped; "T_PRIMARY" must NOT shadow the primary.
		TeamAllowlist: []string{"", "T_PRIMARY", "T_EXTRA"},
	}
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Expect exactly 2 installations: T_PRIMARY + T_EXTRA.
	if len(ch.installations) != 2 {
		t.Fatalf("installations = %d, want 2; ids=%v", len(ch.installations), installationTeamIDs(ch))
	}
	if _, ok := ch.installationsByID["T_PRIMARY"]; !ok {
		t.Error("T_PRIMARY missing from installationsByID")
	}
	if _, ok := ch.installationsByID["T_EXTRA"]; !ok {
		t.Error("T_EXTRA missing from installationsByID")
	}
}

// installationTeamIDs is a tiny helper for test diagnostics.
func installationTeamIDs(c *Channel) []string {
	out := make([]string, 0, len(c.installations))
	for _, inst := range c.installations {
		out = append(out, inst.teamID)
	}
	return out
}

// TestIndexSet_SkipsEmpty — the allowlist hot-path lookup uses
// indexSet; an entry that's empty after TrimSpace must not pollute
// the map (otherwise "" would falsely admit a sender with id "").
func TestIndexSet_SkipsEmpty(t *testing.T) {
	got := indexSet([]string{"a", "", "  ", "b"})
	if len(got) != 2 {
		t.Errorf("indexSet skipped empties incorrectly: %v", got)
	}
	if _, ok := got[""]; ok {
		t.Error("empty key leaked into indexed set")
	}
}

// TestMuxHandler_ReadBodyFailure — io.ReadAll on the mux's
// pre-dispatch body read can fail; the mux must surface 400 rather
// than panic. Models a body that yields data then errors.
func TestMuxHandler_ReadBodyFailure(t *testing.T) {
	mux := NewMuxHandler(nil, zerolog.Nop())
	req := httptest.NewRequest(http.MethodPost, "/", &erroringReader{err: errors.New("boom"), data: []byte("x")})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on body read failure", w.Code)
	}
}

// TestMuxHandler_URLVerificationNoChannels_DropsWith200 — defensive:
// a mux constructed with no channels still ack-and-drops a URL
// verification handshake rather than 404-ing. (Production wiring
// always registers at least one channel; the guard exists so an
// operator who removes their last channel doesn't see a verifier
// regression.)
func TestMuxHandler_URLVerificationNoChannels_DropsWith200(t *testing.T) {
	mux := NewMuxHandler(nil, zerolog.Nop())
	body := []byte(`{"type":"url_verification","challenge":"x"}`)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no channels ⇒ drop)", w.Code)
	}
}

// TestSendChatPostMessage_BadJSONResponseBody — the parse stage fails
// when Slack returns ok-status but the body isn't valid JSON. Pins
// the error-wrap branch at outbound.go:147-149.
func TestSendChatPostMessage_BadJSONResponseBody(t *testing.T) {
	stub := newSlackStub(t)
	stub.respond = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not-json"))
	}
	ch := outboundChannel(t, stub)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "hi",
	})
	if err == nil || !strings.Contains(err.Error(), "parse response") {
		t.Errorf("Send err = %v, want one mentioning 'parse response'", err)
	}
}

// TestSendChatPostMessage_BodyReadFailure — when io.ReadAll on the
// inbound message Reader (via marshal) errors, the channel surfaces
// a wrapped error. We can't fail json.Marshal on a ChannelMessage,
// but we CAN test the http.NewRequest failure via a control character
// in the URL — the same return-and-wrap branch.
func TestSendChatPostMessage_BodyReadFailure(t *testing.T) {
	cfg := validConfig()
	cfg.BotToken = "xoxb-test"
	// A control character in the URL fails http.NewRequest validation;
	// the channel must surface a wrapped error rather than panic.
	cfg.APIBaseURL = "http://example.com/\x00bad"
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100",
		Text:      "hi",
	})
	if err == nil {
		t.Fatal("Send with malformed URL returned nil error, want one")
	}
	if !strings.Contains(err.Error(), "build request") {
		t.Errorf("Send err = %v, want one mentioning 'build request'", err)
	}
}

// erroringReader yields `data` once then returns `err` so we can
// simulate an io.ReadAll mid-stream failure without depending on
// httptest internals. Used by the body-read failure tests above.
type erroringReader struct {
	data []byte
	err  error
	done bool
}

func (r *erroringReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	n := copy(p, r.data)
	return n, r.err
}

// Compile-time guard so io interface coverage stays explicit.
var _ io.Reader = (*erroringReader)(nil)

// Ensure bytes.NewReader isn't accidentally optimised away by the
// linker — the marshal-side coverage relies on it being callable.
var _ = bytes.NewReader

// TestListSessions_SortsNewestFirst — the sort.Slice less-fn body
// only executes when there's more than one entry; the single-session
// test path leaves it uncovered. Two sessions with distinct
// LastActivity prove the newest-first ordering.
func TestListSessions_SortsNewestFirst(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	inst := ch.installations[0]
	old := time.Unix(1700000000, 0)
	newer := old.Add(time.Hour)
	ch.recordSession("S_old", "Old", "U1", old, inst)
	ch.recordSession("S_new", "New", "U2", newer, inst)

	sessions, err := ch.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	if sessions[0].ID != "S_new" {
		t.Errorf("sessions[0].ID = %q, want S_new (newest first)", sessions[0].ID)
	}
	if sessions[1].ID != "S_old" {
		t.Errorf("sessions[1].ID = %q, want S_old", sessions[1].ID)
	}
}

// TestHandleMessageEvent_RequireMentionWithoutMention_Drops — the
// defensive re-check at handleMessageEvent's top. HandleWebhook
// already filters at the call site, but a future refactor that adds
// a new caller must not bypass the @vornik guard. We exercise the
// guard directly by invoking handleMessageEvent.
func TestHandleMessageEvent_RequireMentionWithoutMention_Drops(t *testing.T) {
	cfg := validConfig()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	p := eventPayload{
		Type:    "event_callback",
		TeamID:  "T123",
		EventID: "Ev_inner_check",
		Event: &eventInner{
			Type:        "message",
			User:        "U_alice",
			Text:        "no mention here",
			Channel:     "C_general",
			Ts:          "1700000010.000100",
			ChannelType: "channel",
		},
	}
	inst := ch.installationsByID["T123"]
	ch.handleMessageEvent(context.Background(), p, inst, true)
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("Receive count = %d, want 0 (requireMention without mention drops)", len(got))
	}
}

// Ensure outboundLimiter idempotency: calling the helper twice in a
// row returns the same limiter without re-allocating.
func TestOutboundLimiter_Idempotent(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first := ch.outboundLimiter()
	second := ch.outboundLimiter()
	if first != second {
		t.Error("outboundLimiter returned a different instance on second call")
	}
}
