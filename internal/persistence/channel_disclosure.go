package persistence

import (
	"context"
	"time"
)

// ChannelDisclosure is one row in `channel_disclosure_log` — the record that
// a given conversation session was told it is talking to an AI system.
//
// The table serves two purposes at once, deliberately: it is the per-session
// state that stops the disclosure repeating on every turn, AND it is the
// EU AI Act Art 99 evidence trail. An obligation that was met but cannot be
// proven is worth very little when a supervisory authority asks.
//
// Design: https://docs.vornik.io
type ChannelDisclosure struct {
	// Channel is the conversation.Channel Name() ("telegram", "email", …).
	Channel string

	// SessionID is the channel-native conversation identifier.
	SessionID string

	// ServedAt is when the disclosure was delivered (UTC).
	ServedAt time.Time

	// TextHash is the SHA-256 hex of the exact notice text served. Storing
	// the hash rather than the prose keeps rows small while still answering
	// "which wording did this session see?" after the operator edits the
	// disclosure — which is precisely what an enforcement request asks.
	TextHash string
}

// ChannelDisclosureRepository persists the Art 50 disclosure record.
//
// Unlike ChannelSessionRepository — whose SQLite implementation is a
// deliberate no-op because an in-memory cache is authoritative there — this
// repository MUST be durable on every backend. A no-op stub would leave
// sqlite deployments with no evidence trail at all, silently converting a met
// obligation into an unprovable one.
type ChannelDisclosureRepository interface {
	// WasServed reports whether this (channel, session) has already been
	// disclosed to. Callers treat an error as "not served" and disclose
	// anyway — failing toward disclosure, never away from it.
	WasServed(ctx context.Context, channel, sessionID string) (bool, error)

	// MarkServed records the disclosure. Idempotent: a repeat call for the
	// same (channel, session) is not an error and does not create a second
	// row, so concurrent first turns cannot double-record.
	MarkServed(ctx context.Context, channel, sessionID, textHash string) error

	// ServedBetween returns every disclosure served in [from, to], oldest
	// first. This is the enforcement-response query — see the Art 99
	// runbook in docs/operator/.
	ServedBetween(ctx context.Context, from, to time.Time) ([]ChannelDisclosure, error)
}
