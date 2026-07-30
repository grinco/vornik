package persistence

import (
	"context"
	"time"
)

// SLICE 3 of the chat memory-write design
// (https://docs.vornik.io §5.3), revision 7 — GREEN at
// review round 6 ("Implementation authorized").
//
// A shared-scope chat memory write is a two-step: the tool PROPOSES, the human acknowledges in
// their next inbound turn, and only then may the write commit. This is the state that makes the
// second step enforceable below the UI rather than a prompt instruction — review round 3's one
// Important finding was that §5.3 "describes a state machine but does not name the code-level
// check", and this type plus dispatcher.authorizeSharedWrite are that check.
//
// WHY THIS IS NOT MODELLED AS A TOOL ARGUMENT. `internal/chat/parser.go:186` already had a
// confirmation gate for cancel_task/retry_task, and it is prompt-level: `action.Confirm` is
// parsed out of the model's OWN emitted JSON (parser.go:51), so a model can set it on the first
// turn without ever asking. Storing the acknowledgement as state the write path reads — written
// only from a human-originated inbound turn — is the difference between a gate and a comment.

// ChatMemoryWriteConfirmation is the pending (or acknowledged) confirmation for ONE
// shared-scope memory write in one conversation.
//
// At most one per (Channel, SessionID) — that pair is the primary key, so a second proposal
// REPLACES the first. This is a safety property, not a storage convenience: it means an
// acknowledgement can only ever discharge the most recent proposal, and there is never
// ambiguity about which write the human just agreed to. It also binds the acknowledgement to
// the conversation, which together with OperatorID makes the grant both session-bound and
// speaker-bound (design §5.3.3, r6 review Q2).
type ChatMemoryWriteConfirmation struct {
	// Channel + SessionID identify the conversation. Primary key.
	Channel   string
	SessionID string

	// ContentFingerprint pins the confirmation to the exact content proposed, so
	// acknowledging "I prefer dark mode" cannot authorize writing something else. See
	// dispatcher.sharedWriteFingerprint for the normalisation.
	ContentFingerprint string

	// Scope is "shared" in v1. Stored rather than implied so an audit of this table shows
	// what was asked for, not just that something was.
	Scope string

	// OperatorID is "<source>:<speaker>" — the speaker who proposed. Only they may
	// acknowledge: without this the confirmation would be a channel-level ack rather than
	// the speaker's, letting Bob discharge Alice's proposal in a shared channel.
	OperatorID string

	ProposedAt time.Time
	// ExpiresAt bounds how long an AUTHORIZATION stays open (15 minutes, design §5.3.3) —
	// deliberately unrelated to how long the resulting fact survives (90 days, §5.5).
	// Conflating them would either expire the fact in minutes or leave an authorization open
	// for a quarter.
	ExpiresAt time.Time

	// AcknowledgedAt is nil until the human acknowledges. Stamped ONLY from a
	// human-originated inbound turn in ChannelReceiver.Receive — never from a tool
	// argument, which is the whole point (design §5.3.2).
	AcknowledgedAt *time.Time
}

// Acknowledged reports whether the human has acknowledged this proposal.
func (c *ChatMemoryWriteConfirmation) Acknowledged() bool {
	return c != nil && c.AcknowledgedAt != nil
}

// ChatMemoryWriteConfirmationRepository persists the pending-confirmation state.
//
// Postgres-backed rather than an in-memory map (operator decision 2026-07-30): it survives a
// daemon restart, so a human who replies after a restart is not silently ignored, and it is
// auditable after the fact.
type ChatMemoryWriteConfirmationRepository interface {
	// Propose creates or REPLACES the pending confirmation for (Channel, SessionID),
	// clearing any previous acknowledgement. Replacing rather than erroring is what makes a
	// superseded proposal unacknowledgeable rather than dormant.
	Propose(ctx context.Context, c *ChatMemoryWriteConfirmation) error

	// Get returns the pending confirmation, or nil when there is none.
	Get(ctx context.Context, channel, sessionID string) (*ChatMemoryWriteConfirmation, error)

	// Acknowledge stamps AcknowledgedAt for the given conversation, but ONLY when the row's
	// OperatorID matches — the check is in the storage layer as well as the decision
	// function so a caller that forgets to pass the operator cannot widen the grant.
	// Reports whether a row was stamped.
	Acknowledge(ctx context.Context, channel, sessionID, operatorID string, at time.Time) (bool, error)

	// Delete removes the pending confirmation. Called after a granted decision (one-shot)
	// and when a row is found expired.
	Delete(ctx context.Context, channel, sessionID string) error

	// DeleteExpired sweeps rows past ExpiresAt so a crashed conversation cannot leave one
	// indefinitely. Returns the number removed.
	DeleteExpired(ctx context.Context, now time.Time) (int64, error)
}
