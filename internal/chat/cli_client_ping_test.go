package chat

import (
	"context"
	"testing"
)

// TestCLIClient_Ping uses /bin/true to exercise the happy path and
// a known-bad binary to exercise the error path. The Ping wrapper
// itself doesn't care which binary it spawns — only that --version
// returns exit 0. Skips on non-unix.
func TestCLIClient_Ping(t *testing.T) {
	c := NewCLIClient("claude-3-5-sonnet", WithCLIBinary("/bin/true"))
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping(/bin/true): %v", err)
	}

	// Known-bad binary.
	cBad := NewCLIClient("claude-3-5-sonnet", WithCLIBinary("/nonexistent-binary-xyz"))
	if err := cBad.Ping(context.Background()); err == nil {
		t.Error("Ping(bogus) should error")
	}
}
