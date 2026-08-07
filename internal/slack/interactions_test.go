package slack

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func signedInteraction(t *testing.T, secret, payloadJSON string, ts time.Time) *http.Request {
	t.Helper()
	form := url.Values{"payload": {payloadJSON}}
	body := []byte(form.Encode())
	req := signedRequest(t, secret, ts.Unix(), body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

const okPayload = `{"type":"block_actions","team":{"id":"T123"},"user":{"id":"U_alice"},
	"channel":{"id":"C_general"},"message":{"ts":"1700000002.000100"},
	"response_url":"https://hooks.slack.test/actions/abc",
	"actions":[{"action_id":"steer:c:task_1:0","value":"steer:c:task_1:0"}]}`

// The signature gate is the whole security boundary between an unauthenticated
// internet POST and a handler that resolves checkpoints. It must run before any
// parsing, exactly as the events path does.
func TestParseInteraction_RejectsBadSignature(t *testing.T) {
	cfg := validConfig()
	ch := makeChannel(t, cfg, time.Now())

	req := signedInteraction(t, "the-wrong-secret", okPayload, time.Now())
	if _, err := ch.ParseInteraction(req, time.Now()); err == nil {
		t.Fatal("a payload signed with the wrong secret must be refused")
	}
}

// Slack's replay window. An old-but-correctly-signed POST is a replay.
func TestParseInteraction_RejectsStaleTimestamp(t *testing.T) {
	cfg := validConfig()
	now := time.Now()
	ch := makeChannel(t, cfg, now)

	req := signedInteraction(t, cfg.SigningSecret, okPayload, now.Add(-30*time.Minute))
	if _, err := ch.ParseInteraction(req, now); err == nil {
		t.Fatal("a payload outside the replay window must be refused")
	}
}

func TestParseInteraction_ExtractsWhatTheHandlerNeeds(t *testing.T) {
	cfg := validConfig()
	now := time.Now()
	ch := makeChannel(t, cfg, now)

	got, err := ch.ParseInteraction(signedInteraction(t, cfg.SigningSecret, okPayload, now), now)
	if err != nil {
		t.Fatalf("ParseInteraction: %v", err)
	}
	if got.UserID != "U_alice" {
		t.Errorf("UserID = %q — this is the §3a identity, it must come from the SIGNED payload", got.UserID)
	}
	if got.TeamID != "T123" {
		t.Errorf("TeamID = %q, want T123", got.TeamID)
	}
	if got.ActionValue != "steer:c:task_1:0" {
		t.Errorf("ActionValue = %q", got.ActionValue)
	}
	if got.ResponseURL != "https://hooks.slack.test/actions/abc" {
		t.Errorf("ResponseURL = %q — needed to replace the prompt", got.ResponseURL)
	}
}

// An unknown team must not be served: the signing secret is per-installation,
// so a payload from a workspace this deployment does not serve is not ours.
func TestParseInteraction_RejectsUnknownTeam(t *testing.T) {
	cfg := validConfig()
	now := time.Now()
	ch := makeChannel(t, cfg, now)

	payload := strings.Replace(okPayload, `"id":"T123"`, `"id":"T_OTHER"`, 1)
	if _, err := ch.ParseInteraction(signedInteraction(t, cfg.SigningSecret, payload, now), now); err == nil {
		t.Fatal("an interaction from an unconfigured team must be refused")
	}
}

// A non-block_actions interaction (view_submission, shortcut) is not ours.
func TestParseInteraction_IgnoresOtherInteractionTypes(t *testing.T) {
	cfg := validConfig()
	now := time.Now()
	ch := makeChannel(t, cfg, now)

	payload := strings.Replace(okPayload, `"type":"block_actions"`, `"type":"view_submission"`, 1)
	if _, err := ch.ParseInteraction(signedInteraction(t, cfg.SigningSecret, payload, now), now); err == nil {
		t.Fatal("a non-block_actions interaction must be refused")
	}
}

// Guard the ack contract at the transport layer: the handler writes 200 with an
// empty body, so Slack never shows the operator a timeout.
func TestInteractionAck_IsEmpty200(t *testing.T) {
	w := httptest.NewRecorder()
	WriteInteractionAck(w)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("ack body = %q, want empty — the confirmation rides response_url", body)
	}
}
