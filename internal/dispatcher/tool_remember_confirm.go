package dispatcher

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SLICE 3 of the chat memory-write design §5.3 — the confirmation check a shared-scope write
// cannot commit without. Design revision 7, GREEN at review round 6.
//
// THIS FILE IS THE ANSWER TO REVIEW ROUND 3's ONE IMPORTANT FINDING: §5.3 "correctly describes a
// state machine but does not name the code-level check". authorizeSharedWrite is that check, and
// it is deliberately a PURE FUNCTION over a stored row — no repository, no context, no model — so
// the gating test can assert the boundary directly rather than through a conversation.
//
// The state machine (design §5.3.2):
//
//	1. PROPOSED     — remember() with no acknowledged row: store one, return a confirmation
//	                  request, write NOTHING.
//	2. ACKNOWLEDGED — the human's next inbound turn, in ChannelReceiver.Receive, matched
//	                  against a closed phrase set. Never a tool argument.
//	3. AUTHORIZED   — the next remember() for the SAME fingerprint: authorizeSharedWrite
//	                  grants, and the write path may proceed.
//
// Step 2 is why this is a boundary and not a prompt: advancing the state requires an inbound
// turn the model cannot author. A model that skips asking gets PROPOSED → PROPOSED, no matter
// how many times it calls the tool.

// sharedWriteDecision is the outcome of the shared-scope authorization check.
//
// Enumerated rather than a bool so a test can assert WHY a write was refused, and so the
// refusal text can tell the user something more useful than "no". Every value except
// sharedWriteGranted is a refusal.
type sharedWriteDecision string

const (
	// sharedWriteGranted is the ONLY value that permits a write.
	sharedWriteGranted sharedWriteDecision = "granted"
	// sharedWriteNoRecord — nothing was proposed, or the proposal was superseded/swept.
	sharedWriteNoRecord sharedWriteDecision = "no_confirmation_record"
	// sharedWriteUnacknowledged — proposed, but the human has not acknowledged. This is the
	// state a model reaches by calling remember repeatedly within one turn.
	sharedWriteUnacknowledged sharedWriteDecision = "not_acknowledged"
	// sharedWriteFingerprintMismatch — acknowledged, but for different content.
	sharedWriteFingerprintMismatch sharedWriteDecision = "content_changed_since_confirmation"
	// sharedWriteExpired — acknowledged too long ago (design §5.3.3: 15 minutes).
	sharedWriteExpired sharedWriteDecision = "confirmation_expired"
	// sharedWriteWrongOperator — a different speaker than the one who proposed.
	sharedWriteWrongOperator sharedWriteDecision = "different_speaker"
)

// permits reports whether this decision allows the write to proceed.
//
// A predicate rather than `== sharedWriteGranted` at each call site: a new refusal constant
// added later is refused by default, where a scattered equality check would have to be found
// and updated in every caller.
func (d sharedWriteDecision) permits() bool { return d == sharedWriteGranted }

// sharedWriteFingerprint pins a confirmation to the exact content it was given (design §5.3.3).
//
// Normalises whitespace — trims, then collapses internal runs — so a model re-emitting the same
// fact with different spacing still matches the confirmation the human gave.
//
// DOES NOT CASE-FOLD, deliberately: "the key is ABC" and "the key is abc" are different facts,
// and a confirmation of one must not authorize writing the other. The cost is that a model
// changing capitalisation forces a re-confirmation, which is the safe direction.
//
// Empty content returns the empty string rather than the hash of "", so an empty fingerprint can
// never be mistaken for a valid one.
func sharedWriteFingerprint(content string) string {
	normalised := strings.Join(strings.Fields(content), " ")
	if normalised == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalised))
	return hex.EncodeToString(sum[:])
}

// authorizeSharedWrite decides whether a shared-scope write may commit.
//
// GRANTS ONLY WHEN ALL FIVE HOLD (design §5.3.3):
//
//  1. a confirmation row exists;
//  2. it has been acknowledged by a human;
//  3. its fingerprint matches THIS call's content;
//  4. it has not expired;
//  5. it belongs to the same operator as the caller.
//
// Session binding is not a sixth condition because it is structural: the row is looked up by
// (channel, session_id), which is its primary key, so a lookup from another conversation cannot
// find it at all (r6 review Q2).
//
// Order matters for the refusal text, not for correctness: the checks run cheapest-and-most-
// explanatory first, so a user who changed their mind mid-sentence is told the content changed
// rather than something vaguer.
func authorizeSharedWrite(
	rec *persistence.ChatMemoryWriteConfirmation,
	content, operatorID string,
	now time.Time,
) sharedWriteDecision {
	if rec == nil {
		return sharedWriteNoRecord
	}
	// An empty caller identity can never match a proposer, but say so explicitly rather than
	// relying on string inequality: this is the path a synthetic turn takes (design §5.6.1),
	// and it must be unmistakably a refusal.
	if operatorID == "" || rec.OperatorID != operatorID {
		return sharedWriteWrongOperator
	}
	if !rec.Acknowledged() {
		return sharedWriteUnacknowledged
	}
	if fp := sharedWriteFingerprint(content); fp == "" || fp != rec.ContentFingerprint {
		return sharedWriteFingerprintMismatch
	}
	if !now.Before(rec.ExpiresAt) {
		return sharedWriteExpired
	}
	return sharedWriteGranted
}
