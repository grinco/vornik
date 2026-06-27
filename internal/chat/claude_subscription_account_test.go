package chat

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClaudeAccountInfoPath_ConfigDirEnv exercises the CLAUDE_CONFIG_DIR
// env-var branch.
func TestClaudeAccountInfoPath_ConfigDirEnv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/test-cfg")
	got := claudeAccountInfoPath()
	want := filepath.Join("/tmp/test-cfg", ".claude.json")
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// TestClaudeAccountResolver_Resolve walks both the happy parse path and
// the missing-file early-return.
func TestClaudeAccountResolver_Resolve(t *testing.T) {
	// Missing file → empty info (swallowed).
	r := &claudeAccountResolver{path: "/nonexistent-claude-json"}
	info := r.resolve()
	if info.UserID != "" || info.AccountUUID != "" {
		t.Errorf("missing file should yield zero info, got %+v", info)
	}

	// Happy parse.
	dir := t.TempDir()
	p := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(p, []byte(`{"userID":"u-1","oauthAccount":{"accountUuid":"u-aa"}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	r2 := &claudeAccountResolver{path: p}
	got := r2.resolve()
	if got.UserID != "u-1" || got.AccountUUID != "u-aa" {
		t.Errorf("info = %+v", got)
	}
	// Second resolve hits the once cache and returns the same data.
	got2 := r2.resolve()
	if got2 != got {
		t.Errorf("once cache mismatch: %+v / %+v", got2, got)
	}

	// Bad JSON → swallowed, empty info.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`not json`), 0o600); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	r3 := &claudeAccountResolver{path: bad}
	info3 := r3.resolve()
	if info3.UserID != "" || info3.AccountUUID != "" {
		t.Errorf("bad json should yield zero info, got %+v", info3)
	}
}

// TestDefaultClaudeCredentialsPath_ConfigDirEnv exercises the env-driven
// path resolution branch in the auth manager.
func TestDefaultClaudeCredentialsPath_ConfigDirEnv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/claude-cfg")
	got := defaultClaudeCredentialsPath()
	want := filepath.Join("/tmp/claude-cfg", ".credentials.json")
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// TestNewClaudeAuthManager_EmptyPathUsesDefault.
func TestNewClaudeAuthManager_EmptyPathUsesDefault(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/zzz")
	m := newClaudeAuthManager("")
	if m.path == "" {
		t.Error("empty path should default")
	}
	if !filepath.IsAbs(m.path) {
		t.Errorf("default path should be absolute, got %q", m.path)
	}
}
