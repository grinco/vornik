package chatorigin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// TestResolveForTurn_ReasonDiscriminatesFailureModes pins the fix for the
// 2026-08-05 misleading-Send-message defect.
//
// ResolveForTurn flattened three distinct states into a bare `false`, and its
// only consumer (ui.DeliverableSend) rendered them all as "This task wasn't
// started from a chat channel — nothing to send to." On janka-companion that
// sentence was actively wrong: the task WAS Slack-originated, but its
// chat_audit_log row had been lost to an expired-context write. The operator
// reasonably concluded parent-channel inheritance was missing and went looking
// for an absent feature instead of a lost row.
//
// A caller must be able to tell "never came from chat" (final, correct) from
// "came from chat but the origin record is gone" (a fault, and diagnosable).
func TestResolveForTurn_ReasonDiscriminatesFailureModes(t *testing.T) {
	slack := &fakeChannel{name: "slack"}
	wired := fakeResolver{byName: map[string]conversation.Channel{"slack": slack}}
	missingRow := fakeAudit{err: persistence.ErrNotFound}

	t.Run("no turn id at all is not chat-originated", func(t *testing.T) {
		res, ok := ResolveForTurn(context.Background(), "", missingRow, wired)
		require.False(t, ok)
		assert.Equal(t, ReasonNotChatOriginated, res.Reason)
	})

	t.Run("missing audit row reports a lost origin record", func(t *testing.T) {
		res, ok := ResolveForTurn(context.Background(),
			"chat_20260805170412_9dc37555d22e8c76", missingRow, wired)
		require.False(t, ok)
		assert.Equal(t, ReasonOriginRecordMissing, res.Reason,
			"a task whose chat_turn_id points at a vanished row must NOT be reported as never-chat-originated")
	})

	t.Run("undecodable chat id reports a malformed origin record", func(t *testing.T) {
		res, ok := ResolveForTurn(context.Background(), "turn-1",
			fakeAudit{row: &persistence.ChatAuditEntry{ID: "turn-1", ChatID: "garbage-no-colon"}}, wired)
		require.False(t, ok)
		assert.Equal(t, ReasonOriginRecordMalformed, res.Reason)
	})

	t.Run("unwired channel reports channel unavailable", func(t *testing.T) {
		res, ok := ResolveForTurn(context.Background(), "turn-1",
			fakeAudit{row: &persistence.ChatAuditEntry{ID: "turn-1", ChatID: "slack:T03/D0B#main"}},
			fakeResolver{}) // nothing wired
		require.False(t, ok)
		assert.Equal(t, ReasonChannelUnavailable, res.Reason)
		assert.Equal(t, "slack", res.ChannelName,
			"the channel NAME is known even when the channel isn't wired — callers report which one")
	})

	t.Run("success carries no failure reason", func(t *testing.T) {
		res, ok := ResolveForTurn(context.Background(), "turn-1",
			fakeAudit{row: &persistence.ChatAuditEntry{ID: "turn-1", ChatID: "slack:T03/D0B#main"}}, wired)
		require.True(t, ok)
		assert.Equal(t, ReasonNone, res.Reason)
		assert.Equal(t, "slack", res.ChannelName)
		assert.Equal(t, "T03/D0B#main", res.SessionID)
	})
}

// TestResolve_MissingAncestorRowReportsRecordMissing covers the exact janka
// shape end-to-end through Resolve: the child carries no ChatTurnID, the
// lineage walk finds the parent's, and THAT row is the one that vanished.
// Inheritance works; the record is gone. The reason must say so.
func TestResolve_MissingAncestorRowReportsRecordMissing(t *testing.T) {
	parentID := "task_20260805170431_955e423704153971"
	parent := &persistence.Task{
		ID:         parentID,
		ChatTurnID: strp("chat_20260805170412_9dc37555d22e8c76"),
	}
	child := &persistence.Task{
		ID:           "task_20260805170522_ab6d9a44d42779b5",
		ParentTaskID: &parentID,
	}
	getter := fakeTaskGetter{byID: map[string]*persistence.Task{parentID: parent}}

	res, ok := Resolve(context.Background(), child, getter,
		fakeAudit{err: persistence.ErrNotFound},
		fakeResolver{byName: map[string]conversation.Channel{"slack": &fakeChannel{name: "slack"}}})

	require.False(t, ok)
	assert.Equal(t, ReasonOriginRecordMissing, res.Reason)
}
