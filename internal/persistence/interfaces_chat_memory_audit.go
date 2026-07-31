package persistence

import (
	"context"
	"time"
)

// SLICE 3 (part 2) of the chat memory-write design
// (https://docs.vornik.io §5.3.3), revision 8.
//
// The append-only accountability record for a shared-scope memory write. Revision 4 deleted
// the pending confirmation row on grant and kept nothing; review round 4 was right that this
// destroys the Art 5(2) answer to "prove this shared write was acknowledged". Its own
// suggested fix — retain the pending row until the chunk's TTL — breaks §5.3.1's
// one-pending-per-session primary key, so the two concerns are split into two tables:
//
//   - chat_memory_write_confirmations is transient and unique per conversation.
//   - chat_memory_write_audit (this type) is append-only evidence.
//
// On grant the audit row is written FIRST, and only then is the pending row deleted — so a
// delete that fails after the audit leaves an extra (harmless, append-only) attestation
// rather than losing the evidence, and an audit that fails leaves the pending row in place
// for retry. That ordering is the whole point of separating them (§5.3.3).

// ChatMemoryWriteAudit is one append-only attestation that a shared-scope memory write was
// authorized: this speaker acknowledged this fingerprint at this time.
//
// It stores the CONTENT FINGERPRINT, never the content, so the accountability record does not
// become a second copy of personal data outliving the chunk's TTL (Art 5(1)(e)). Once the
// chunk is deleted the row attests that a write with this fingerprint was authorized — not
// what the text said (§5.3.4).
type ChatMemoryWriteAudit struct {
	// Channel + SessionID identify the conversation the acknowledgement came from.
	Channel   string
	SessionID string

	// ContentFingerprint ties this attestation to the chunk it justifies. It is the same
	// SHA-256 the pending confirmation carried (see dispatcher.sharedWriteFingerprint).
	ContentFingerprint string

	// Scope is "shared" in v1 — stored so an audit of the table shows what was asked for.
	Scope string

	// OperatorID is "<source>:<speaker>" — the speaker who proposed AND acknowledged. This
	// column is personal data; the table is retained on erasure under Art 17(3)(b) rather
	// than deleted, and that exemption is disclosed in the retained-categories report
	// (§5.3.4). Retention is still bounded — swept at the chunk TTL plus a 7-day grace.
	OperatorID string

	// ProposedAt / AcknowledgedAt are copied from the pending row at grant time, and
	// GrantedAt is when the write was authorized. All three are populated on an audit row —
	// unlike the pending row, AcknowledgedAt here is never NULL, because a row is written
	// only on grant.
	ProposedAt     time.Time
	AcknowledgedAt time.Time
	GrantedAt      time.Time
}

// ChatMemoryWriteAuditRepository persists the append-only shared-write attestations.
//
// Separate from ChatMemoryWriteConfirmationRepository on purpose: that repo owns a transient,
// replace-in-place row; this one owns immutable evidence. There is deliberately no Update and
// no per-row Delete — retention is a horizon sweep (slice 5), and Art 17 exempts the table.
type ChatMemoryWriteAuditRepository interface {
	// Record appends one attestation. Written BEFORE the pending confirmation row is
	// deleted on grant (§5.3.3).
	Record(ctx context.Context, a *ChatMemoryWriteAudit) error

	// ListByFingerprint returns the attestations for a content fingerprint, oldest first —
	// the evidence query answering "prove this shared write was acknowledged" starting from
	// the chunk in question.
	ListByFingerprint(ctx context.Context, fingerprint string) ([]ChatMemoryWriteAudit, error)
}
