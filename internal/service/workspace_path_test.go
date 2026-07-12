package service

import (
	"path/filepath"
	"testing"
)

// Pins the shared config→env fallback used by both the executor and
// the dispatcher (incident-telegram-upload-input-roots-20260712: the
// dispatcher had no workspace path at all, so its input-confinement
// gate never allowed the project uploads dir Telegram writes to).
func TestResolveProjectWorkspacePath(t *testing.T) {
	t.Run("configured value wins", func(t *testing.T) {
		t.Setenv("VORNIK_DATA_DIR", "/ignored")
		if got := resolveProjectWorkspacePath("/explicit"); got != "/explicit" {
			t.Fatalf("got %q, want /explicit", got)
		}
	})
	t.Run("falls back to VORNIK_DATA_DIR/workspaces", func(t *testing.T) {
		t.Setenv("VORNIK_DATA_DIR", "/data")
		want := filepath.Join("/data", "workspaces")
		if got := resolveProjectWorkspacePath(""); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("empty when nothing set", func(t *testing.T) {
		t.Setenv("VORNIK_DATA_DIR", "")
		if got := resolveProjectWorkspacePath(""); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}
