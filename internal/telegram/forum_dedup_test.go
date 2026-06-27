// Tests for the 2026-05-16 forum-artifact dedup fix. A task
// that hits AWAITING_INPUT and later COMPLETED triggers
// NotifyTaskCompleted twice; before the fix, each fire shipped
// every output artifact, so files were uploaded to the group
// thread twice. The dedup set keeps a per-(thread_id,
// artifact_id) record so the second call is a no-op for
// already-sent artifacts.
//
// These tests pin the primitive (forumArtifactAlreadySent /
// markForumArtifactSent) — wiring tests for sendArtifactsToForum
// live in forum_test.go.

package telegram

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestForumArtifactAlreadySent_EmptyStartFalse — the dedup
// set starts empty; the predicate must return false for any
// (thread, artifact) before mark is called.
func TestForumArtifactAlreadySent_EmptyStartFalse(t *testing.T) {
	b := &Bot{}
	assert.False(t, b.forumArtifactAlreadySent(1, "art-1"))
}

// TestForumArtifactDedup_MarkThenCheck — the canonical
// idempotent shape: after mark fires, the next check returns
// true.
func TestForumArtifactDedup_MarkThenCheck(t *testing.T) {
	b := &Bot{}
	b.markForumArtifactSent(42, "art-1")
	assert.True(t, b.forumArtifactAlreadySent(42, "art-1"))
}

// TestForumArtifactDedup_ScopedToThread — pin the
// (thread, artifact) scoping. A different thread on the same
// artifact must still register fresh — operators with multiple
// forum chats wired must each see the file once per chat.
func TestForumArtifactDedup_ScopedToThread(t *testing.T) {
	b := &Bot{}
	b.markForumArtifactSent(42, "art-1")
	assert.True(t, b.forumArtifactAlreadySent(42, "art-1"))
	assert.False(t, b.forumArtifactAlreadySent(99, "art-1"),
		"a different thread must NOT inherit the sent-status — the dedup is per-(thread, artifact)")
}

// TestForumArtifactDedup_ScopedToArtifact — pin the other
// axis. Two artifacts in the same thread are independent.
func TestForumArtifactDedup_ScopedToArtifact(t *testing.T) {
	b := &Bot{}
	b.markForumArtifactSent(42, "art-1")
	assert.False(t, b.forumArtifactAlreadySent(42, "art-2"),
		"different artifact in the same thread must NOT inherit sent-status")
}

// TestForumArtifactDedup_EmptyArtifactIDIsNoop — defensive:
// an empty artifact ID means the row has no stable identity
// to dedup on. Marking it must be a no-op rather than
// silently grouping every unstamped artifact under "".
func TestForumArtifactDedup_EmptyArtifactIDIsNoop(t *testing.T) {
	b := &Bot{}
	b.markForumArtifactSent(42, "")
	assert.False(t, b.forumArtifactAlreadySent(42, ""))
}

// TestForumArtifactDedup_NilBotSafe — defensive: every
// caller of these helpers nil-checks the bot already, but
// the helpers themselves must not panic on a nil receiver.
func TestForumArtifactDedup_NilBotSafe(t *testing.T) {
	var nilBot *Bot
	nilBot.markForumArtifactSent(1, "x")
	assert.False(t, nilBot.forumArtifactAlreadySent(1, "x"))
}

// TestForumArtifactDedup_RepeatedMarkIsIdempotent — calling
// mark twice on the same (thread, artifact) does not throw
// or corrupt the set.
func TestForumArtifactDedup_RepeatedMarkIsIdempotent(t *testing.T) {
	b := &Bot{}
	b.markForumArtifactSent(1, "art-1")
	b.markForumArtifactSent(1, "art-1")
	assert.True(t, b.forumArtifactAlreadySent(1, "art-1"))
}
