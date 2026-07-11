package slack

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withTestAuthTestURL points AuthTestURL at a fake server for the
// duration of the calling test, restoring it on cleanup. Same package-var
// injection idiom as telegram.telegramAPIBaseURL / mcpProbeConnect.
func withTestAuthTestURL(t *testing.T, url string) {
	t.Helper()
	orig := AuthTestURL
	AuthTestURL = url
	t.Cleanup(func() { AuthTestURL = orig })
}

// TestProbeToken_Success — auth.test 200 with ok:true returns team/user.
func TestProbeToken_Success(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"team":"Acme","user":"botuser"}`))
	}))
	defer srv.Close()
	withTestAuthTestURL(t, srv.URL)

	team, user, err := ProbeToken(context.Background(), srv.Client(), "xoxb-faketoken")
	if err != nil {
		t.Fatalf("ProbeToken() error = %v", err)
	}
	if team != "Acme" || user != "botuser" {
		t.Errorf("team/user = %q/%q, want Acme/botuser", team, user)
	}
	if gotAuth != "Bearer xoxb-faketoken" {
		t.Errorf("Authorization header = %q, want Bearer xoxb-faketoken", gotAuth)
	}
}

// TestProbeToken_InvalidAuth — Slack returns HTTP 200 with ok:false and a
// known invalid-credential error code for a bad/revoked token. Must be
// classified OutcomeFail upstream via *AuthTestError.
func TestProbeToken_InvalidAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	}))
	defer srv.Close()
	withTestAuthTestURL(t, srv.URL)

	_, _, err := ProbeToken(context.Background(), srv.Client(), "xoxb-badtoken")
	if err == nil {
		t.Fatal("expected an error for ok:false")
	}
	var authErr *AuthTestError
	if !errors.As(err, &authErr) {
		t.Fatalf("error = %v (%T), want *AuthTestError", err, err)
	}
	if authErr.Code != "invalid_auth" {
		t.Errorf("Code = %q, want invalid_auth", authErr.Code)
	}
}

// TestProbeToken_RateLimited — Slack's "ratelimited" ok:false code, or an
// HTTP 429, must not be treated the same as invalid_auth.
func TestProbeToken_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
	}))
	defer srv.Close()
	withTestAuthTestURL(t, srv.URL)

	_, _, err := ProbeToken(context.Background(), srv.Client(), "xoxb-sometoken")
	if err == nil {
		t.Fatal("expected an error for a 429 response")
	}
	var authErr *AuthTestError
	if errors.As(err, &authErr) {
		t.Errorf("a 429/ratelimited response must not surface as *AuthTestError (would be misclassified OutcomeFail), got %v", authErr)
	}
}

// TestProbeToken_ServerError — a 5xx from Slack.
func TestProbeToken_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`internal error`))
	}))
	defer srv.Close()
	withTestAuthTestURL(t, srv.URL)

	_, _, err := ProbeToken(context.Background(), srv.Client(), "xoxb-sometoken")
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	var authErr *AuthTestError
	if errors.As(err, &authErr) {
		t.Errorf("a 5xx response must not surface as *AuthTestError, got %v", authErr)
	}
}

// TestProbeToken_Malformed — ok:true JSON body that fails to parse.
func TestProbeToken_Malformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	withTestAuthTestURL(t, srv.URL)

	_, _, err := ProbeToken(context.Background(), srv.Client(), "xoxb-sometoken")
	if err == nil {
		t.Fatal("expected an error for a malformed body")
	}
}

// TestProbeToken_Timeout — a slow server against a short client timeout.
func TestProbeToken_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"team":"Acme","user":"botuser"}`))
	}))
	defer srv.Close()
	withTestAuthTestURL(t, srv.URL)

	client := &http.Client{Timeout: 5 * time.Millisecond}
	_, _, err := ProbeToken(context.Background(), client, "xoxb-sometoken")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}

// TestProbeToken_EmptyToken.
func TestProbeToken_EmptyToken(t *testing.T) {
	_, _, err := ProbeToken(context.Background(), http.DefaultClient, "  ")
	if err == nil {
		t.Fatal("expected an error for an empty/blank token")
	}
}

// TestProbeToken_NeverLeaksTokenOnNetworkError — the token travels in the
// Authorization header (never the URL), so a dial failure's error text
// must not contain it either way; this locks that invariant explicitly.
func TestProbeToken_NeverLeaksTokenOnNetworkError(t *testing.T) {
	const distinctiveSecret = "xoxb-VeryDistinctiveSlackBotTokenXYZ"
	withTestAuthTestURL(t, "http://127.0.0.1:1")
	client := &http.Client{Timeout: 200 * time.Millisecond}
	_, _, err := ProbeToken(context.Background(), client, distinctiveSecret)
	if err == nil {
		t.Fatal("expected a dial failure against port 1")
	}
	if strings.Contains(err.Error(), distinctiveSecret) {
		t.Fatalf("ProbeToken error leaked the token: %q", err.Error())
	}
}
