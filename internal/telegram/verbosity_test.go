package telegram

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestVerbosity_DefaultsToShort — chats with no /verbose call get
// the one-line task notification, NOT the rich full output. The
// rich mode posts artifact uploads + per-task chatter that customers
// complain about; operators who actually want it run /verbose full.
func TestVerbosity_DefaultsToShort(t *testing.T) {
	b := &Bot{verbosity: map[int64]string{}}
	assert.Equal(t, "short", b.getVerbosity(12345))
}

// TestVerbosity_SetThenGet — basic round-trip. Demonstrates the
// per-chat scoping: chat A's preference doesn't leak to chat B.
func TestVerbosity_SetThenGet(t *testing.T) {
	b := &Bot{verbosity: map[int64]string{}}
	b.setVerbosity(1, "silent")
	b.setVerbosity(2, "full")
	assert.Equal(t, "silent", b.getVerbosity(1))
	assert.Equal(t, "full", b.getVerbosity(2))
	assert.Equal(t, "short", b.getVerbosity(3), "chat 3 has no preference set — defaults to short")
}

// TestVerbosity_NilSafe — getters and setters on a nil receiver
// must not panic; the dispatcher path can call into the bot in
// edge cases where construction failed.
func TestVerbosity_NilSafe(t *testing.T) {
	var b *Bot
	assert.Equal(t, "short", b.getVerbosity(1))
	b.setVerbosity(1, "silent") // must not panic
}
