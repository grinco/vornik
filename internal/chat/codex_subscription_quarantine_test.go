package chat

// Coverage for the 2026-06-02 codex-auth hardening: dead-token
// quarantine, auto-recovery after a fresh `codex login`, re-read-under-
// lock adoption of a token another process rotated, and the file-lock
// helper. These guard against the single-use-refresh-token race that
// produced repeated `app_session_terminated` failures.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A terminal 4xx on refresh must quarantine the dead token: the first
// Token() surfaces ErrCodexSessionEnded, and subsequent calls return the
// same WITHOUT replaying the request (no failure flood).
func TestCodexAuth_Quarantine_NoReplayOnTerminalError(t *testing.T) {
	dir := t.TempDir()
	expired := buildCodexJWT(t, "acct-1", time.Now().Add(-time.Hour))
	path := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken: expired, AccessToken: "old", RefreshToken: "rt-dead", AccountID: "acct-1",
	})
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"app_session_terminated","message":"Your session has ended. Please log in again."}}`))
	}))
	defer ts.Close()

	mgr := newCodexAuthManager(path)
	mgr.tokenURL = ts.URL

	if _, _, err := mgr.Token(context.Background()); !errors.Is(err, ErrCodexSessionEnded) {
		t.Fatalf("first call: want ErrCodexSessionEnded, got %v", err)
	}
	if _, _, err := mgr.Token(context.Background()); !errors.Is(err, ErrCodexSessionEnded) {
		t.Fatalf("second call: want ErrCodexSessionEnded, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("token endpoint hit %d times; want 1 — quarantine must not replay a dead token", got)
	}
}

// After quarantine, a fresh `codex login` (a DIFFERENT refresh_token on
// disk) must auto-recover without a daemon restart.
func TestCodexAuth_RecoversAfterFreshLogin(t *testing.T) {
	dir := t.TempDir()
	expired := buildCodexJWT(t, "acct-1", time.Now().Add(-time.Hour))
	path := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken: expired, AccessToken: "old", RefreshToken: "rt-dead", AccountID: "acct-1",
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"app_session_terminated"}`))
	}))
	defer ts.Close()

	mgr := newCodexAuthManager(path)
	mgr.tokenURL = ts.URL
	if _, _, err := mgr.Token(context.Background()); !errors.Is(err, ErrCodexSessionEnded) {
		t.Fatalf("expected quarantine, got %v", err)
	}

	// Simulate `codex login`: a NEW refresh_token + a still-valid token.
	fresh := buildCodexJWT(t, "acct-1", time.Now().Add(time.Hour))
	writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken: fresh, AccessToken: "fresh-access", RefreshToken: "rt-new", AccountID: "acct-1",
	})

	access, accountID, err := mgr.Token(context.Background())
	if err != nil {
		t.Fatalf("after fresh login: %v", err)
	}
	if access != "fresh-access" {
		t.Errorf("access = %q, want fresh-access (should have recovered from disk)", access)
	}
	if accountID != "acct-1" {
		t.Errorf("accountID = %q", accountID)
	}
}

// When the cached token is near expiry but ANOTHER process already
// refreshed (a newer valid token is on disk), refreshLocked must re-read
// + adopt the disk token instead of POSTing with our stale single-use
// refresh_token (which would 400). No network call should happen.
func TestCodexAuth_ReReadAdoptsRotatedToken_NoNetwork(t *testing.T) {
	dir := t.TempDir()
	expired := buildCodexJWT(t, "acct-1", time.Now().Add(-time.Hour))
	path := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken: expired, AccessToken: "stale", RefreshToken: "rt-stale", AccountID: "acct-1",
	})

	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1) // any hit is a failure for this test
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	mgr := newCodexAuthManager(path)
	mgr.tokenURL = ts.URL
	// Prime the cache with the expired token (so the next Token() wants
	// to refresh), THEN simulate another process rotating the on-disk
	// token to a fresh, valid one.
	mgr.mu.Lock()
	if err := mgr.loadLocked(); err != nil {
		mgr.mu.Unlock()
		t.Fatalf("prime load: %v", err)
	}
	mgr.mu.Unlock()

	fresh := buildCodexJWT(t, "acct-1", time.Now().Add(time.Hour))
	writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken: fresh, AccessToken: "rotated-access", RefreshToken: "rt-rotated", AccountID: "acct-1",
	})

	access, _, err := mgr.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if access != "rotated-access" {
		t.Errorf("access = %q, want rotated-access (adopt the on-disk rotated token)", access)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("token endpoint hit %d times; want 0 — should adopt the disk token, not refresh", got)
	}
}

func TestCodexAuth_withFileLock_RunsFnAndCreatesLockFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	mgr := newCodexAuthManager(path)

	ran := false
	if err := mgr.withFileLock(func() error { ran = true; return nil }); err != nil {
		t.Fatalf("withFileLock: %v", err)
	}
	if !ran {
		t.Error("fn did not run under the lock")
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Errorf("lock file not created: %v", err)
	}
	// Sequential re-acquire must succeed (lock released on return).
	if err := mgr.withFileLock(func() error { return nil }); err != nil {
		t.Errorf("second acquire failed (lock not released?): %v", err)
	}
	// fn error propagates.
	sentinel := errors.New("boom")
	if err := mgr.withFileLock(func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("fn error should propagate; got %v", err)
	}
}

func TestIsTerminalAuthStatus(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden} {
		if !isTerminalAuthStatus(code) {
			t.Errorf("isTerminalAuthStatus(%d) = false; want true", code)
		}
	}
	for _, code := range []int{http.StatusOK, http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests} {
		if isTerminalAuthStatus(code) {
			t.Errorf("isTerminalAuthStatus(%d) = true; want false (transient — retry, don't quarantine)", code)
		}
	}
}
