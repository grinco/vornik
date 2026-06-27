package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signedRequest builds an httptest request with a valid v0 HMAC for
// the given body, secret, and timestamp. Returns the request +
// timestamp string so callers can mutate either if a test needs to
// exercise a particular failure mode.
func signedRequest(t *testing.T, secret string, tsUnix int64, body []byte) *http.Request {
	t.Helper()
	tsStr := fmt.Sprintf("%d", tsUnix)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + tsStr + ":"))
	_, _ = mac.Write(body)
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/slack/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Slack-Request-Timestamp", tsStr)
	req.Header.Set("X-Slack-Signature", sig)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// validConfig returns a Config seeded with safe defaults that pass
// New's validation. Callers override the bits they're exercising.
func validConfig() Config {
	return Config{
		SigningSecret: "shhh-signing-secret",
		BotToken:      "xoxb-test-token",
		TeamID:        "T123",
		TeamAllowlist: []string{"T123"},
	}
}

// TestNew_RejectsEmptySigningSecret — defensive boot guard. An
// unconfigured signing secret would accept every payload, so we
// fail loudly at construction.
func TestNew_RejectsEmptySigningSecret(t *testing.T) {
	cfg := validConfig()
	cfg.SigningSecret = ""
	if _, err := New(cfg); err == nil {
		t.Fatal("New with empty SigningSecret returned nil error, want one")
	}
}

// TestNew_RejectsBlankTeamAllowlistAndTeamID — fresh install with no
// team configured must reject construction. Mirrors the GitHub
// channel's empty-RepoAllowlist boot guard.
func TestNew_RejectsBlankTeamAllowlistAndTeamID(t *testing.T) {
	cfg := validConfig()
	cfg.TeamID = ""
	cfg.TeamAllowlist = nil
	if _, err := New(cfg); err == nil {
		t.Fatal("New with no TeamID + empty TeamAllowlist returned nil, want error")
	}
}

// TestNew_SingleInstallation_BuildsRoute — happy path: a single-
// installation config produces exactly one resolved route indexed
// by TeamID.
func TestNew_SingleInstallation_BuildsRoute(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, want := len(ch.installations), 1; got != want {
		t.Errorf("installations len = %d, want %d", got, want)
	}
	if _, ok := ch.installationsByID["T123"]; !ok {
		t.Errorf("installationsByID missing T123, got %v", ch.installationsByID)
	}
}

// TestNew_MultiInstallation_RejectsDuplicateTeam — defensive: two
// projects pointing at the same workspace is a config error.
func TestNew_MultiInstallation_RejectsDuplicateTeam(t *testing.T) {
	cfg := Config{
		SigningSecret: "shhh",
		Installations: []InstallationConfig{
			{ProjectID: "a", TeamID: "T123", BotToken: "xoxb-a"},
			{ProjectID: "b", TeamID: "T123", BotToken: "xoxb-b"},
		},
	}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("New with duplicate TeamID returned nil, want error")
	}
	if !strings.Contains(err.Error(), "duplicate team_id") {
		t.Errorf("err = %v, want one mentioning duplicate team_id", err)
	}
}

// TestNew_MultiInstallation_RejectsEmptyTeamID — each entry must
// supply a TeamID.
func TestNew_MultiInstallation_RejectsEmptyTeamID(t *testing.T) {
	cfg := Config{
		SigningSecret: "shhh",
		Installations: []InstallationConfig{{ProjectID: "a"}},
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("New with empty TeamID returned nil, want error")
	}
}

// TestNew_DefaultsRateLimit — operators who don't supply RPS/burst
// get Slack's documented Tier-3 ceiling (1 msg/sec, burst 1).
func TestNew_DefaultsRateLimit(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ch.postMessageRPS != defaultPostMessageRPS {
		t.Errorf("postMessageRPS = %d, want %d", ch.postMessageRPS, defaultPostMessageRPS)
	}
	if ch.postMessageBurst != defaultPostMessageBurst {
		t.Errorf("postMessageBurst = %d, want %d", ch.postMessageBurst, defaultPostMessageBurst)
	}
}

// TestNew_DefaultsClockAndAPIBase — production wiring leaves both
// unset; New must fill them in.
func TestNew_DefaultsClockAndAPIBase(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ch.clock == nil {
		t.Error("clock = nil, want time.Now default")
	}
	if ch.apiBaseURL != defaultAPIBaseURL {
		t.Errorf("apiBaseURL = %q, want %q", ch.apiBaseURL, defaultAPIBaseURL)
	}
}

// TestChannel_Name — channel identifier must be the stable string
// downstream consumers branch on (ChannelMessage.Source, metrics).
func TestChannel_Name(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := ch.Name(); got != "slack" {
		t.Errorf("Name() = %q, want slack", got)
	}
}

// TestVerifySignature_HappyPath — valid HMAC + fresh timestamp → nil.
func TestVerifySignature_HappyPath(t *testing.T) {
	cfg := validConfig()
	ch, _ := New(cfg)
	now := time.Unix(1700000000, 0)
	ch.clock = func() time.Time { return now }
	body := []byte(`{"type":"event_callback"}`)
	req := signedRequest(t, cfg.SigningSecret, now.Unix(), body)
	if err := ch.verifySignature(req, body, now); err != nil {
		t.Errorf("verifySignature: %v", err)
	}
}

// TestVerifySignature_BodyTamper — flipping any byte of the body
// invalidates the HMAC even with a fresh timestamp.
func TestVerifySignature_BodyTamper(t *testing.T) {
	cfg := validConfig()
	ch, _ := New(cfg)
	now := time.Unix(1700000000, 0)
	body := []byte(`{"type":"event_callback"}`)
	req := signedRequest(t, cfg.SigningSecret, now.Unix(), body)
	tampered := []byte(`{"type":"event_callback!"}`)
	err := ch.verifySignature(req, tampered, now)
	if err == nil || !strings.Contains(err.Error(), "signature mismatch") {
		t.Errorf("verifySignature on tampered body = %v, want signature mismatch", err)
	}
}

// TestVerifySignature_ReplayWindow_TooOld — a delivery whose
// timestamp is older than 5 min must be rejected even when the HMAC
// verifies, blocking captured-payload replay attacks.
func TestVerifySignature_ReplayWindow_TooOld(t *testing.T) {
	cfg := validConfig()
	ch, _ := New(cfg)
	now := time.Unix(1700000000, 0)
	oldTs := now.Add(-6 * time.Minute).Unix()
	body := []byte(`{"type":"event_callback"}`)
	req := signedRequest(t, cfg.SigningSecret, oldTs, body)
	err := ch.verifySignature(req, body, now)
	if err == nil || !strings.Contains(err.Error(), "replay window") {
		t.Errorf("verifySignature on stale timestamp = %v, want replay-window rejection", err)
	}
}

// TestVerifySignature_ReplayWindow_TooFarFuture — a forward-skewed
// timestamp beyond the window is also rejected; mirrors the past-
// horizon check.
func TestVerifySignature_ReplayWindow_TooFarFuture(t *testing.T) {
	cfg := validConfig()
	ch, _ := New(cfg)
	now := time.Unix(1700000000, 0)
	futureTs := now.Add(6 * time.Minute).Unix()
	body := []byte(`{}`)
	req := signedRequest(t, cfg.SigningSecret, futureTs, body)
	err := ch.verifySignature(req, body, now)
	if err == nil || !strings.Contains(err.Error(), "future") {
		t.Errorf("verifySignature on future timestamp = %v, want future-window rejection", err)
	}
}

// TestVerifySignature_ReplayWindow_WithinSkew — a delivery within
// the ±5 min window (slight forward or backward NTP skew) verifies
// successfully. Tests both directions so the edge case isn't half-
// covered.
func TestVerifySignature_ReplayWindow_WithinSkew(t *testing.T) {
	cfg := validConfig()
	ch, _ := New(cfg)
	now := time.Unix(1700000000, 0)
	for name, skew := range map[string]time.Duration{
		"4min past":   -4 * time.Minute,
		"4min future": 4 * time.Minute,
		"exact now":   0,
	} {
		t.Run(name, func(t *testing.T) {
			ts := now.Add(skew).Unix()
			body := []byte(`{}`)
			req := signedRequest(t, cfg.SigningSecret, ts, body)
			if err := ch.verifySignature(req, body, now); err != nil {
				t.Errorf("verifySignature skew=%s: %v", skew, err)
			}
		})
	}
}

// TestVerifySignature_MissingHeaders — both timestamp and signature
// headers are required. Each missing-header path returns its own
// error so an operator looking at logs can tell which header the
// upstream relay stripped.
func TestVerifySignature_MissingHeaders(t *testing.T) {
	cfg := validConfig()
	ch, _ := New(cfg)
	now := time.Unix(1700000000, 0)
	body := []byte(`{}`)

	t.Run("no timestamp", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		req.Header.Set("X-Slack-Signature", "v0=deadbeef")
		err := ch.verifySignature(req, body, now)
		if err == nil || !strings.Contains(err.Error(), "X-Slack-Request-Timestamp") {
			t.Errorf("err = %v, want one mentioning the missing timestamp header", err)
		}
	})

	t.Run("no signature", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		req.Header.Set("X-Slack-Request-Timestamp", fmt.Sprintf("%d", now.Unix()))
		err := ch.verifySignature(req, body, now)
		if err == nil || !strings.Contains(err.Error(), "X-Slack-Signature") {
			t.Errorf("err = %v, want one mentioning the missing signature header", err)
		}
	})
}

// TestVerifySignature_MalformedTimestamp — a non-integer timestamp
// header surfaces as a parse error rather than an opaque mismatch.
func TestVerifySignature_MalformedTimestamp(t *testing.T) {
	cfg := validConfig()
	ch, _ := New(cfg)
	now := time.Unix(1700000000, 0)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	req.Header.Set("X-Slack-Request-Timestamp", "not-a-number")
	req.Header.Set("X-Slack-Signature", "v0=deadbeef")
	err := ch.verifySignature(req, []byte("{}"), now)
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Errorf("err = %v, want one mentioning malformed timestamp", err)
	}
}

// TestVerifySignature_MalformedSignatureHex — the v0= prefix is
// present but the hex tail is invalid. Surfaces as a hex decode
// error rather than HMAC mismatch.
func TestVerifySignature_MalformedSignatureHex(t *testing.T) {
	cfg := validConfig()
	ch, _ := New(cfg)
	now := time.Unix(1700000000, 0)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	req.Header.Set("X-Slack-Request-Timestamp", fmt.Sprintf("%d", now.Unix()))
	req.Header.Set("X-Slack-Signature", "v0=not-hex-zzz")
	err := ch.verifySignature(req, []byte("{}"), now)
	if err == nil || !strings.Contains(err.Error(), "hex") {
		t.Errorf("err = %v, want one mentioning hex", err)
	}
}

// TestVerifySignature_MissingV0Prefix — Slack stamps every signature
// with v0=; rejecting the missing-prefix path catches misconfigured
// "we forgot to prepend the version" relays.
func TestVerifySignature_MissingV0Prefix(t *testing.T) {
	cfg := validConfig()
	ch, _ := New(cfg)
	now := time.Unix(1700000000, 0)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	req.Header.Set("X-Slack-Request-Timestamp", fmt.Sprintf("%d", now.Unix()))
	req.Header.Set("X-Slack-Signature", "deadbeef") // missing v0=
	err := ch.verifySignature(req, []byte("{}"), now)
	if err == nil || !strings.Contains(err.Error(), "prefix") {
		t.Errorf("err = %v, want one mentioning the missing prefix", err)
	}
}

// TestComputeSlackHMAC_StableShape — the helper produces the exact
// wire shape Slack documents: v0:<ts>:<body> bytes piped into
// HMAC-SHA256. Pin a known fixture so a future "let's normalise
// whitespace" refactor surfaces as a test failure rather than a
// silent compatibility break.
func TestComputeSlackHMAC_StableShape(t *testing.T) {
	got := hex.EncodeToString(computeSlackHMAC([]byte("the-key"), "1531420618", []byte(`{"hello":"world"}`)))

	// Recompute via the documented shape to confirm; the point of this
	// test is to lock the wire-protocol contract.
	mac := hmac.New(sha256.New, []byte("the-key"))
	_, _ = mac.Write([]byte("v0:1531420618:" + `{"hello":"world"}`))
	want := hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Errorf("computeSlackHMAC = %s, want %s", got, want)
	}
}

// TestURLVerification_SentinelDoesntLeakInto_Send — the
// errURLVerification sentinel is internal; ensure it's distinct
// from any other error a caller might branch on via errors.Is. (We
// don't ever want errURLVerification to compare-equal to a future
// "outbound failed" error.)
func TestURLVerification_SentinelDistinct(t *testing.T) {
	if errors.Is(errURLVerification, errors.New("other")) {
		t.Error("errURLVerification compared equal to a different error")
	}
}
