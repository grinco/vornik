package chat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestClaudeSubscriptionClient_Ping covers the readiness gate paths:
// nil client error, env-token fast path, missing credentials file, and
// a happy credentials.json read.
func TestClaudeSubscriptionClient_Ping(t *testing.T) {
	// Nil receiver.
	var nilClient *ClaudeSubscriptionClient
	if err := nilClient.Ping(context.Background()); err == nil {
		t.Error("nil client should error")
	}

	// Env-token fast path bypasses the file entirely.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "test-token")
	c := NewClaudeSubscriptionClient("claude-3-5-sonnet")
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping with env token: %v", err)
	}
	_ = os.Unsetenv("CLAUDE_CODE_OAUTH_TOKEN")

	// Pointed at a missing file → Token() returns an error and Ping
	// surfaces it.
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-file.json")
	c2 := NewClaudeSubscriptionClient("claude-3-5-sonnet",
		WithClaudeSubscriptionAuthPath(missing))
	if err := c2.Ping(context.Background()); err == nil {
		t.Error("Ping should fail when credentials file is missing")
	}

	// Pointed at a valid (non-expired) credentials.json.
	credPath := filepath.Join(dir, "creds.json")
	future := time.Now().Add(time.Hour).UnixMilli()
	body := `{"claudeAiOauth":{"accessToken":"access-x","refreshToken":"refresh-x","expiresAt":` +
		fmt.Sprint(future) + `}}`
	if err := os.WriteFile(credPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	c3 := NewClaudeSubscriptionClient("claude-3-5-sonnet",
		WithClaudeSubscriptionAuthPath(credPath))
	if err := c3.Ping(context.Background()); err != nil {
		t.Errorf("Ping with valid creds: %v", err)
	}
}
