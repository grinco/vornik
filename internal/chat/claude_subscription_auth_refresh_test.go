package chat

// Tests for the claude token-refresh hardening + subscription
// token-refresh observability (2026-06-07 architecture review of
// subscription-auth.md, findings 2 and 4 validated against code):
// the codex provider got cross-process flock + quarantine after the
// single-use-refresh-token race; claude shared the same credentials
// file with the interactive CLI but had only an in-process mutex,
// and neither provider emitted any metric on refresh outcomes.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// writeClaudeCredsTokens writes a minimal .credentials.json with the
// given token pair + expiry and returns its path. (writeClaudeCreds in
// subscription_ping_test.go fixes the token values; refresh tests need
// to control them.)
func writeClaudeCredsTokens(t *testing.T, dir, access, refresh string, expiresAt time.Time) string {
	t.Helper()
	path := filepath.Join(dir, ".credentials.json")
	body, err := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  access,
			"refreshToken": refresh,
			"expiresAt":    expiresAt.UnixMilli(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newChatTestMetrics(t *testing.T) *Metrics {
	t.Helper()
	return NewMetrics(prometheus.NewRegistry())
}

func TestClaudeAuth_RefreshSuccess_RecordsMetricAndTakesFileLock(t *testing.T) {
	dir := t.TempDir()
	path := writeClaudeCredsTokens(t, dir, "old-access", "old-refresh", time.Now().Add(-time.Hour))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		})
	}))
	defer ts.Close()

	m := newChatTestMetrics(t)
	mgr := newClaudeAuthManager(path)
	mgr.tokenURL = ts.URL
	mgr.setMetrics(m)

	access, err := mgr.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if access != "new-access" {
		t.Errorf("access: got %q, want new-access", access)
	}
	// The cross-process advisory lock leaves its sibling file behind.
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Errorf("expected %s.lock to exist after refresh: %v", path, err)
	}
	if got := testutil.ToFloat64(m.SubscriptionTokenRefreshTotal.WithLabelValues("claude", "success")); got != 1 {
		t.Errorf("refresh counter (claude, success) = %v, want 1", got)
	}
}

// A concurrent `claude` CLI session rotating the shared credentials
// file mid-refresh produces invalid_grant; the manager must adopt the
// CLI's fresh on-disk tokens and report the recovery distinctly.
func TestClaudeAuth_InvalidGrant_RecoversFromExternalRotation(t *testing.T) {
	dir := t.TempDir()
	path := writeClaudeCredsTokens(t, dir, "old-access", "old-refresh", time.Now().Add(-time.Hour))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Simulate the CLI winning the race: rotate the file, then
		// reject our (now stale) refresh token.
		writeClaudeCredsTokens(t, dir, "cli-access", "cli-refresh", time.Now().Add(time.Hour))
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer ts.Close()

	m := newChatTestMetrics(t)
	mgr := newClaudeAuthManager(path)
	mgr.tokenURL = ts.URL
	mgr.setMetrics(m)

	access, err := mgr.Token(context.Background())
	if err != nil {
		t.Fatalf("Token should recover via on-disk reload: %v", err)
	}
	if access != "cli-access" {
		t.Errorf("access: got %q, want cli-access (the CLI's rotation)", access)
	}
	if got := testutil.ToFloat64(m.SubscriptionTokenRefreshTotal.WithLabelValues("claude", "invalid_grant_recovered")); got != 1 {
		t.Errorf("refresh counter (claude, invalid_grant_recovered) = %v, want 1", got)
	}
}

func TestClaudeAuth_RefreshFailure_RecordsMetric(t *testing.T) {
	dir := t.TempDir()
	path := writeClaudeCredsTokens(t, dir, "old-access", "old-refresh", time.Now().Add(-time.Hour))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	m := newChatTestMetrics(t)
	mgr := newClaudeAuthManager(path)
	mgr.tokenURL = ts.URL
	mgr.setMetrics(m)

	if _, err := mgr.Token(context.Background()); err == nil {
		t.Fatal("expected refresh failure to surface")
	}
	if got := testutil.ToFloat64(m.SubscriptionTokenRefreshTotal.WithLabelValues("claude", "failure")); got != 1 {
		t.Errorf("refresh counter (claude, failure) = %v, want 1", got)
	}
}

// Codex quarantine transitions must be observable — a dead refresh
// token previously recorded only an internal deadReason string.
func TestCodexAuth_Quarantine_RecordsMetric(t *testing.T) {
	dir := t.TempDir()
	expiredJWT := buildCodexJWT(t, "acct-1", time.Now().Add(-time.Hour))
	path := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken: expiredJWT, AccessToken: "old", RefreshToken: "old-refresh",
		AccountID: "acct-1",
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"app_session_terminated"}`))
	}))
	defer ts.Close()

	m := newChatTestMetrics(t)
	mgr := newCodexAuthManager(path)
	mgr.tokenURL = ts.URL
	mgr.setMetrics(m)

	if _, _, err := mgr.Token(context.Background()); err == nil {
		t.Fatal("expected quarantine error")
	}
	if got := testutil.ToFloat64(m.SubscriptionTokenRefreshTotal.WithLabelValues("codex", "quarantined")); got != 1 {
		t.Errorf("refresh counter (codex, quarantined) = %v, want 1", got)
	}
}
