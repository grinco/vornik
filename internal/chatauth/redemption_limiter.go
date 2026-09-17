package chatauth

import (
	"sync"
	"time"
)

// RedemptionLimiter bounds link-code attempts per speaker.
//
// It exists because redemption MUST be reachable before authorization —
// oidc-identity-permissions-design §5.1: "`/link <code>` is processed from
// unknown senders — that is how a new binding is created." A speaker who is
// not yet linked is by definition not yet authorized, so gating redemption on
// authorization makes the feature unreachable for exactly the population it
// exists for.
//
// That reachability is also the reason for this type. An endpoint anyone can
// reach is a guessing surface, and the chat path has no equivalent of the
// HTTP claim limiter. The code space (28^8 over a ten-minute TTL) makes a
// blind search hopeless on its own, but "hopeless" is an arithmetic argument
// and a bound is a mechanism — the operator-profile OTP flow already carries
// a lockout for the same reason, and two link paths with one lockout between
// them would be the asymmetry that gets found later.
//
// Keyed by (channel, external_id): the speaker, not the chat, so moving to
// another group does not reset the count.
type RedemptionLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	limit    int
	window   time.Duration
	now      func() time.Time
}

// Defaults: generous for a typo, useless for a search.
const (
	redemptionLimit  = 10
	redemptionWindow = 10 * time.Minute
)

// NewRedemptionLimiter builds a limiter with the defaults above. One per
// channel is fine — the key includes the channel, so a shared instance and
// per-channel instances count the same speaker the same way.
func NewRedemptionLimiter() *RedemptionLimiter {
	return &RedemptionLimiter{
		attempts: map[string][]time.Time{},
		limit:    redemptionLimit,
		window:   redemptionWindow,
		now:      time.Now,
	}
}

// Allow records an attempt and reports whether it may proceed.
//
// It counts the attempt BEFORE the outcome is known, deliberately. A limiter
// that only counted failures could never bound the attempts it exists to
// bound — the same defect a design review caught in the HTTP claim limiter,
// where the bucket could not increment on the attempts it was written for.
func (l *RedemptionLimiter) Allow(channel, externalID string) bool {
	if l == nil {
		return true
	}
	key := channel + ":" + externalID
	now := l.now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.attempts[key][:0]
	for _, t := range l.attempts[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.attempts[key] = kept
		return false
	}
	l.attempts[key] = append(kept, now)

	// Drop other speakers' windows once they have emptied. Without this the
	// map keeps one key per distinct speaker for the life of the process —
	// the slices are trimmed, but the keys are not, and a bot that sees many
	// speakers accumulates them forever (review-20260915-8284).
	//
	// Swept here, on the write that already holds the lock, rather than on a
	// timer: the sweep costs nothing when the map is small, and a map that is
	// large is exactly one being written to.
	if len(l.attempts) > l.limit {
		for k, ts := range l.attempts {
			if k == key {
				continue
			}
			if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
				delete(l.attempts, k)
			}
		}
	}
	return true
}
