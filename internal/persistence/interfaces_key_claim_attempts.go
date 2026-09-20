package persistence

import (
	"context"
	"time"
)

// KeyClaimAttemptRepository bounds key-claim attempts across a cluster —
// oidc-identity-permissions-design §5.4, follow-up to CA-09.
//
// The bound already lives in the one function every claim door calls, so no
// door can be added past it. What it could not do was hold across REPLICAS: the
// bucket was process memory, so N replicas allowed N times the attempts and a
// restart cleared the count. That was equally true of the HTTP limiter it
// replaced — this is the half that was written down as remaining rather than a
// regression being fixed.
//
// The bucket is keyed by the DECLARED key id and is shared across claimants and
// source addresses, deliberately: the attack it bounds is guessing ONE key's
// secret from many addresses, which a per-IP or per-claimant bucket would not
// bound at all. The accepted cost, stated in §5.4 rather than left emergent, is
// that one adversarial claimant can exhaust a real key's bucket for the window.
type KeyClaimAttemptRepository interface {
	// RecordClaimAttempt writes one attempt and returns how many attempts
	// (including this one) fall inside the window ending now.
	//
	// Record-then-count in one call because the two must not be separable: a
	// caller that counted first and recorded later would leave the window a
	// caller-sized race, which is the defect being fixed one level up.
	RecordClaimAttempt(ctx context.Context, keyID string, now time.Time, window time.Duration) (int, error)

	// PruneClaimAttempts deletes attempts older than the window. Housekeeping,
	// not correctness: counting is already bounded by the window, so a missed
	// prune costs rows and never an admission.
	PruneClaimAttempts(ctx context.Context, before time.Time) (int64, error)
}
