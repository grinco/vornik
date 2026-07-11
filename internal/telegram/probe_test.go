package telegram

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withTestBaseURL points APIBaseURL at a fake server for the
// duration of the calling test, restoring it on cleanup. Mirrors the
// package-var injection convention already used for mcpProbeConnect
// (internal/ui/admin_control_plane_mcp_probe.go) so tests never touch the
// real Telegram API.
func withTestBaseURL(t *testing.T, url string) {
	t.Helper()
	orig := APIBaseURL
	APIBaseURL = url
	t.Cleanup(func() { APIBaseURL = orig })
}

// TestProbeToken_Success — a getMe 200 with ok:true returns the bot's
// username. Guards the happy path the integrations "Connected as @bot"
// Summary is built from.
func TestProbeToken_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/getMe") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"username":"my_bot"}}`))
	}))
	defer srv.Close()
	withTestBaseURL(t, srv.URL)

	username, err := ProbeToken(context.Background(), srv.Client(), "faketoken")
	if err != nil {
		t.Fatalf("ProbeToken() error = %v", err)
	}
	if username != "my_bot" {
		t.Errorf("username = %q, want my_bot", username)
	}
}

// TestProbeToken_InvalidToken — Telegram returns HTTP 401 with
// ok:false/error_code:401 for a bad token. The caller (integrations
// telegramProber) classifies this as OutcomeFail via *ProbeError.
func TestProbeToken_InvalidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
	}))
	defer srv.Close()
	withTestBaseURL(t, srv.URL)

	_, err := ProbeToken(context.Background(), srv.Client(), "badtoken")
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
	var probeErr *ProbeError
	if !errors.As(err, &probeErr) {
		t.Fatalf("error = %v (%T), want *ProbeError", err, err)
	}
	if probeErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", probeErr.StatusCode)
	}
}

// TestProbeToken_RateLimited — HTTP 429 must be distinguishable from an
// invalid-token 401 so the integrations layer classifies it OutcomeError,
// not OutcomeFail (design §5.2: a 429 doesn't mean the credential is bad).
func TestProbeToken_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests"}`))
	}))
	defer srv.Close()
	withTestBaseURL(t, srv.URL)

	_, err := ProbeToken(context.Background(), srv.Client(), "sometoken")
	if err == nil {
		t.Fatal("expected an error for a 429 response")
	}
	var probeErr *ProbeError
	if !errors.As(err, &probeErr) {
		t.Fatalf("error = %v (%T), want *ProbeError", err, err)
	}
	if probeErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", probeErr.StatusCode)
	}
}

// TestProbeToken_ServerError — a 5xx from Telegram.
func TestProbeToken_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":500,"description":"Internal Server Error"}`))
	}))
	defer srv.Close()
	withTestBaseURL(t, srv.URL)

	_, err := ProbeToken(context.Background(), srv.Client(), "sometoken")
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	var probeErr *ProbeError
	if !errors.As(err, &probeErr) {
		t.Fatalf("error = %v (%T), want *ProbeError", err, err)
	}
	if probeErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", probeErr.StatusCode)
	}
}

// TestProbeToken_Malformed — a 200 with an unparseable body must error
// (classified OutcomeError upstream), not panic or silently return "".
func TestProbeToken_Malformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	withTestBaseURL(t, srv.URL)

	_, err := ProbeToken(context.Background(), srv.Client(), "sometoken")
	if err == nil {
		t.Fatal("expected an error for a malformed body")
	}
}

// TestProbeToken_Timeout — a slow server against a short client timeout
// must return a plain (non-*ProbeError) error, which the integrations
// layer classifies OutcomeError.
func TestProbeToken_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"username":"my_bot"}}`))
	}))
	defer srv.Close()
	withTestBaseURL(t, srv.URL)

	client := &http.Client{Timeout: 5 * time.Millisecond}
	_, err := ProbeToken(context.Background(), client, "sometoken")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	var probeErr *ProbeError
	if errors.As(err, &probeErr) {
		t.Errorf("timeout must not be classified as a *ProbeError, got %v", probeErr)
	}
}

// TestProbeToken_EmptyToken — an empty token is rejected before any
// network call (defensive; the UI's Required field validation is the
// primary guard, this is defense in depth).
func TestProbeToken_EmptyToken(t *testing.T) {
	_, err := ProbeToken(context.Background(), http.DefaultClient, "  ")
	if err == nil {
		t.Fatal("expected an error for an empty/blank token")
	}
}

// TestProbeToken_NeverLeaksTokenOnNetworkError — the getMe URL embeds the
// token in its path (https://api.telegram.org/bot<token>/getMe). A dial
// failure against an unreachable base URL must not leak the token into the
// returned error text — http.Client wraps the *full request URL* into
// transport errors by default, which would otherwise leak it. (design §8
// "log-echo assertion".)
func TestProbeToken_NeverLeaksTokenOnNetworkError(t *testing.T) {
	const distinctiveSecret = "123456:AAVeryDistinctiveTelegramBotTokenXYZ"
	withTestBaseURL(t, "http://127.0.0.1:1")
	client := &http.Client{Timeout: 200 * time.Millisecond}
	_, err := ProbeToken(context.Background(), client, distinctiveSecret)
	if err == nil {
		t.Fatal("expected a dial failure against port 1")
	}
	if strings.Contains(err.Error(), distinctiveSecret) {
		t.Fatalf("ProbeToken error leaked the token: %q", err.Error())
	}
}
