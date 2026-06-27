package persistence

import (
	"context"
	"time"
)

// OperatorIdentityLink is one row in operator_identity_link.
// Maps a channel-specific speaker id (e.g. "tg:12345",
// "web:abc-hash", "slack:U01234") onto a canonical operator id
// so the dispatcher's profile lookup sees a single operator
// across every channel they've linked.
//
// The canonical operator id is itself a channel-speaker-id —
// the one that was the "primary" at link time. There is no
// separate per-operator GUID; we keep the model deliberately
// flat so an operator who only ever uses one channel still gets
// rows that match exactly.
type OperatorIdentityLink struct {
	// ChannelSpeakerID is the per-channel id (PK). Format is
	// `<channel>:<id>` so the channel surfaces are unambiguous.
	ChannelSpeakerID string
	// OperatorID is the canonical id rows resolve to. May
	// equal ChannelSpeakerID when this row is the primary
	// (a link's anchor); otherwise it's a different speaker id
	// that all rows in the link share.
	OperatorID string
	// LinkedAt is when this row was last written. Idempotent
	// upserts don't bump it — the timestamp tracks human-
	// observable "when did this operator confirm the link",
	// not the most recent write.
	LinkedAt time.Time
	// LinkedBy records the surface that authorised the link.
	// "self" — operator typed /link on both channels.
	// "cli"  — operator linked via `vornikctl operator link`.
	// "auto" — future: same email across two channels with a
	//          verified inbound. Reserved.
	LinkedBy string
}

// OperatorIdentityLinkRepository persists identity-link rows.
// See https://docs.vornik.io (Phase A).
//
// All operations are operator-id-scoped; cross-tenant scoping is
// deferred to 2026.8.0+ when the tenant model lands.
type OperatorIdentityLinkRepository interface {
	// Get returns the link row for one channel speaker id.
	// ErrNotFound when the speaker isn't linked — callers
	// fall back to the speaker id as its own canonical id.
	Get(ctx context.Context, channelSpeakerID string) (*OperatorIdentityLink, error)

	// ListForOperator returns every speaker id that resolves
	// to operatorID. Used by `vornikctl operator show-links`
	// and the merge-on-link logic so the dispatcher can
	// preview what's about to consolidate.
	ListForOperator(ctx context.Context, operatorID string) ([]*OperatorIdentityLink, error)

	// Upsert inserts or refreshes a link row. LinkedAt is
	// stamped to NOW() on first insert; existing rows keep
	// their original timestamp so the audit log reflects when
	// the operator first confirmed the relationship.
	Upsert(ctx context.Context, link *OperatorIdentityLink) error

	// Delete removes one link row (operator unlinks a single
	// channel). Idempotent.
	Delete(ctx context.Context, channelSpeakerID string) error

	// DeleteAllForOperator drops every row pointing at one
	// operator. Used by `vornikctl operator forget` so the
	// canonical id doesn't outlive its profile.
	DeleteAllForOperator(ctx context.Context, operatorID string) error
}
