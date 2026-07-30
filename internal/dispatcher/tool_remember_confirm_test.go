package dispatcher

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SLICE 3 of the chat memory-write design, §5.3. THE GATING TEST, named as such by review
// round 3: "shared-scope write path verification test: write rejected without confirmation
// record, accepted with."
//
// It asserts directly on authorizeSharedWrite — a pure function over a stored row, with no
// model in the loop. That was round 3's stated bar for §5.3 describing a real enforcement
// point rather than a prompt-level claim in different words.
//
// All five conditions must hold to grant: a row exists, it is acknowledged, the fingerprint
// matches THIS call's content, it has not expired, and it belongs to the same operator.
func TestAuthorizeSharedWrite(t *testing.T) {
	const (
		content  = "the deadline moved to Friday"
		operator = "slack:U123"
	)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	acked := now.Add(-2 * time.Minute)

	// The happy-path row: acknowledged, unexpired, right operator, right content.
	granted := func() *persistence.ChatMemoryWriteConfirmation {
		return &persistence.ChatMemoryWriteConfirmation{
			Channel:            "slack",
			SessionID:          "T1/C1#main",
			ContentFingerprint: sharedWriteFingerprint(content),
			Scope:              string(memoryScopeShared),
			OperatorID:         operator,
			ProposedAt:         now.Add(-3 * time.Minute),
			ExpiresAt:          now.Add(12 * time.Minute),
			AcknowledgedAt:     &acked,
		}
	}

	for _, tc := range []struct {
		name    string
		mutate  func(*persistence.ChatMemoryWriteConfirmation)
		record  bool
		want    sharedWriteDecision
		content string
	}{
		{
			name:   "grants when all five conditions hold",
			record: true,
			want:   sharedWriteGranted,
		},
		{
			// The headline case: no confirmation record at all.
			name:   "denies with no record",
			record: false,
			want:   sharedWriteNoRecord,
		},
		{
			// A row exists because the tool PROPOSED, but the human never acknowledged.
			// This is the state a model reaches by calling remember twice in one turn.
			name:   "denies an unacknowledged row",
			record: true,
			mutate: func(r *persistence.ChatMemoryWriteConfirmation) { r.AcknowledgedAt = nil },
			want:   sharedWriteUnacknowledged,
		},
		{
			// Content-bound: confirming one fact must not authorize writing another.
			name:    "denies when the content changed after acknowledgement",
			record:  true,
			content: "the API key is AKIA-not-what-they-agreed-to",
			want:    sharedWriteFingerprintMismatch,
		},
		{
			name:   "denies past expiry",
			record: true,
			mutate: func(r *persistence.ChatMemoryWriteConfirmation) {
				r.ExpiresAt = now.Add(-1 * time.Second)
			},
			want: sharedWriteExpired,
		},
		{
			// Bob must not be able to discharge Alice's proposal in a shared channel.
			name:   "denies a different speaker",
			record: true,
			mutate: func(r *persistence.ChatMemoryWriteConfirmation) {
				r.OperatorID = "slack:UBOB"
			},
			want: sharedWriteWrongOperator,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec *persistence.ChatMemoryWriteConfirmation
			if tc.record {
				rec = granted()
				if tc.mutate != nil {
					tc.mutate(rec)
				}
			}
			body := content
			if tc.content != "" {
				body = tc.content
			}
			got := authorizeSharedWrite(rec, body, operator, now)
			if got != tc.want {
				t.Errorf("authorizeSharedWrite() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Only sharedWriteGranted may permit a write. Every other decision is a refusal, and a new
// decision constant added later must not accidentally read as permission — hence the
// assertion on the predicate rather than on the constant.
func TestSharedWriteDecision_OnlyGrantedPermits(t *testing.T) {
	for _, d := range []sharedWriteDecision{
		sharedWriteNoRecord, sharedWriteUnacknowledged, sharedWriteFingerprintMismatch,
		sharedWriteExpired, sharedWriteWrongOperator,
	} {
		if d.permits() {
			t.Errorf("decision %q permits a write; only sharedWriteGranted may", d)
		}
	}
	if !sharedWriteGranted.permits() {
		t.Error("sharedWriteGranted must permit the write")
	}
}

// §5.3.3: the fingerprint normalises whitespace so a model re-sending the same fact with
// different spacing still matches, but does NOT case-fold — "the key is ABC" and "the key is
// abc" are different facts, and a confirmation of one must not authorize the other.
func TestSharedWriteFingerprint(t *testing.T) {
	base := sharedWriteFingerprint("the deadline moved to Friday")

	for _, equivalent := range []string{
		"  the deadline moved to Friday  ",
		"the deadline  moved   to Friday",
		"the deadline\tmoved to\nFriday",
	} {
		if got := sharedWriteFingerprint(equivalent); got != base {
			t.Errorf("fingerprint(%q) = %q, want %q — whitespace must normalise", equivalent, got, base)
		}
	}

	for _, different := range []string{
		"The deadline moved to Friday",
		"the deadline moved to friday",
		"the deadline moved to Monday",
	} {
		if got := sharedWriteFingerprint(different); got == base {
			t.Errorf("fingerprint(%q) collided with %q — case and wording must be distinct",
				different, "the deadline moved to Friday")
		}
	}

	if sharedWriteFingerprint("") != "" {
		t.Error("empty content must fingerprint to the empty string, never to a hash of nothing")
	}
}
