package chat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCodexSubscriptionClient_Ping_Errors drives the readiness gate's
// error paths (nil client, missing file, malformed JSON, wrong auth_mode,
// missing tokens). The happy path requires a valid signed JWT id_token
// and is left to the integration suite.
func TestCodexSubscriptionClient_Ping_Errors(t *testing.T) {
	var nilClient *CodexSubscriptionClient
	if err := nilClient.Ping(context.Background()); err == nil {
		t.Error("nil client should error")
	}

	dir := t.TempDir()

	// Missing file.
	missing := filepath.Join(dir, "no-such.json")
	c := NewCodexSubscriptionClient("gpt-5.4-mini", WithCodexSubscriptionAuthPath(missing))
	if err := c.Ping(context.Background()); err == nil {
		t.Error("missing auth.json should error")
	}

	// Bad JSON.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`not-json`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c2 := NewCodexSubscriptionClient("gpt-5.4-mini", WithCodexSubscriptionAuthPath(bad))
	if err := c2.Ping(context.Background()); err == nil {
		t.Error("bad json should error")
	}

	// Wrong auth_mode.
	wrong := filepath.Join(dir, "wrong-mode.json")
	if err := os.WriteFile(wrong, []byte(`{"auth_mode":"api","tokens":{}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c3 := NewCodexSubscriptionClient("gpt-5.4-mini", WithCodexSubscriptionAuthPath(wrong))
	if err := c3.Ping(context.Background()); err == nil {
		t.Error("wrong auth_mode should error")
	}

	// Missing tokens.
	missingTokens := filepath.Join(dir, "no-tokens.json")
	if err := os.WriteFile(missingTokens, []byte(`{"auth_mode":"chatgpt","tokens":{}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c4 := NewCodexSubscriptionClient("gpt-5.4-mini", WithCodexSubscriptionAuthPath(missingTokens))
	if err := c4.Ping(context.Background()); err == nil {
		t.Error("missing tokens should error")
	}
}
