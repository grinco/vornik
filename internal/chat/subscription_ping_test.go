// Coverage for the Ping / Complete*Tools delegators on
// ClaudeSubscriptionClient and CodexSubscriptionClient. These
// wrappers each have one observable behaviour worth pinning:
// they reject a missing-auth configuration with a clear error
// rather than panicking, and a healthy on-disk credentials.json
// makes Ping succeed without any network round-trip.

package chat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- ClaudeSubscriptionClient.Ping --------------------------------------

// writeClaudeCreds drops a minimal valid .credentials.json into dir,
// with the token already expiring far in the future so Ping doesn't
// need to refresh. Returns the file path.
func writeClaudeCreds(t *testing.T, dir string, expiresAtMillis int64) string {
	t.Helper()
	creds := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "access-token-xxx",
			"refreshToken": "refresh-token-yyy",
			"expiresAt":    expiresAtMillis,
		},
	}
	raw, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		t.Fatalf("marshal creds: %v", err)
	}
	path := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	return path
}

func TestClaudeSubscription_Ping_HappyPath(t *testing.T) {
	dir := t.TempDir()
	// Expiry far in the future so the refresh path is not triggered.
	path := writeClaudeCreds(t, dir, time.Now().Add(time.Hour).UnixMilli())
	c := NewClaudeSubscriptionClient("claude-opus-4-7",
		WithClaudeSubscriptionAuthPath(path))
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping(valid creds): got %v, want nil", err)
	}
}

func TestClaudeSubscription_Ping_NilAuthRejected(t *testing.T) {
	c := &ClaudeSubscriptionClient{} // direct construction sidesteps NewClaudeSubscriptionClient defaulting
	if err := c.Ping(context.Background()); err == nil {
		t.Error("Ping(nil auth manager): expected error, got nil")
	}
}

func TestClaudeSubscription_Ping_MissingCreds(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-opus-4-7",
		WithClaudeSubscriptionAuthPath("/no/such/.credentials.json"))
	// Make sure the env-override fast path doesn't mask the error.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	if err := c.Ping(context.Background()); err == nil {
		t.Error("Ping(missing creds): expected error, got nil")
	}
}

// Token's env-override branch — when CLAUDE_CODE_OAUTH_TOKEN is set,
// the manager skips the file entirely and returns the env value.
func TestClaudeAuth_Token_EnvOverride(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "env-token-zzz")
	mgr := newClaudeAuthManager("/no/such/path/.credentials.json")
	tok, err := mgr.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "env-token-zzz" {
		t.Errorf("env override: got %q, want env-token-zzz", tok)
	}
}

// --- CodexSubscriptionClient.Ping ---------------------------------------

func TestCodexSubscription_Ping_NilAuthRejected(t *testing.T) {
	c := &CodexSubscriptionClient{}
	if err := c.Ping(context.Background()); err == nil {
		t.Error("Ping(nil auth manager): expected error, got nil")
	}
}

func TestCodexSubscription_Ping_MissingCreds(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4",
		WithCodexSubscriptionAuthPath("/no/such/auth.json"))
	if err := c.Ping(context.Background()); err == nil {
		t.Error("Ping(missing auth.json): expected error, got nil")
	}
}

func TestCodexSubscription_Ping_HappyPath(t *testing.T) {
	dir := t.TempDir()
	jwt := buildCodexJWT(t, "acct-1", time.Now().Add(time.Hour))
	path := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken: jwt, AccessToken: "a", RefreshToken: "r", AccountID: "acct-1",
	})
	c := NewCodexSubscriptionClient("gpt-5.4",
		WithCodexSubscriptionAuthPath(path))
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping(valid auth.json): got %v, want nil", err)
	}
}

// --- CodexSubscription Complete / CompleteWithTools / Stream
// All three end at call() which then makes an HTTP request. With no
// auth file we expect a clean error rather than a panic. With a
// valid auth file but no test HTTP server we still expect an error.

func TestCodexSubscription_Complete_MissingCreds(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4",
		WithCodexSubscriptionAuthPath("/no/such/auth.json"),
		WithCodexSubscriptionTimeout(2*time.Second))
	_, err := c.Complete(context.Background(),
		[]Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Error("Complete with missing creds: expected error, got nil")
	}
}

func TestCodexSubscription_CompleteWithTools_MissingCreds(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4",
		WithCodexSubscriptionAuthPath("/no/such/auth.json"),
		WithCodexSubscriptionTimeout(2*time.Second))
	_, err := c.CompleteWithTools(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Error("CompleteWithTools with missing creds: expected error, got nil")
	}
}

func TestCodexSubscription_CompleteWithToolsStream_MissingCreds(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4",
		WithCodexSubscriptionAuthPath("/no/such/auth.json"),
		WithCodexSubscriptionTimeout(2*time.Second))
	_, err := c.CompleteWithToolsStream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil {
		t.Error("CompleteWithToolsStream with missing creds: expected error, got nil")
	}
}

// --- ClaudeSubscription Complete / CompleteWithTools / Stream
// Same shape — exercise the wrapper + the auth-failure branch in
// call().

func TestClaudeSubscription_Complete_MissingCreds(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	c := NewClaudeSubscriptionClient("claude-opus-4-7",
		WithClaudeSubscriptionAuthPath("/no/such/.credentials.json"),
		WithClaudeSubscriptionTimeout(2*time.Second))
	_, err := c.Complete(context.Background(),
		[]Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Error("Complete with missing creds: expected error, got nil")
	}
}

func TestClaudeSubscription_CompleteWithTools_MissingCreds(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	c := NewClaudeSubscriptionClient("claude-opus-4-7",
		WithClaudeSubscriptionAuthPath("/no/such/.credentials.json"),
		WithClaudeSubscriptionTimeout(2*time.Second))
	_, err := c.CompleteWithTools(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Error("CompleteWithTools with missing creds: expected error, got nil")
	}
}

func TestClaudeSubscription_CompleteWithToolsStream_MissingCreds(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	c := NewClaudeSubscriptionClient("claude-opus-4-7",
		WithClaudeSubscriptionAuthPath("/no/such/.credentials.json"),
		WithClaudeSubscriptionTimeout(2*time.Second))
	_, err := c.CompleteWithToolsStream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil {
		t.Error("CompleteWithToolsStream with missing creds: expected error, got nil")
	}
}

// --- Client.Ping coverage -----------------------------------------------

// Client.Ping short-circuits when WithStaticModelList was set —
// covers the static-only path with no network call.
func TestClient_Ping_StaticModelsShortCircuits(t *testing.T) {
	c := NewClient("https://api.example.com", "k", "gpt-stub",
		WithStaticModelList([]ModelInfo{{ID: "m1"}}))
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping with static list: got %v, want nil", err)
	}
}
