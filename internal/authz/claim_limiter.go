package authz

import (
	"sync"
	"time"
)

// ClaimAttemptLimit and ClaimAttemptWindow are §5.4's bound: 10 attempts per
// key id per hour. Exported because the HTTP doors document the bound they no
// longer implement, and a second literal is a number that can drift from the
// one the service enforces. A human claims a key once; ten failures in an hour against
// one key is already far outside honest use.
const (
	ClaimAttemptLimit  = 10
	ClaimAttemptWindow = time.Hour
)

// claimLimiter bounds key-claim attempts — oidc-identity-permissions-design
// §5.4, round-3 findings F3 and F4.
//
// The bucket is keyed by the DECLARED key id and is SHARED ACROSS CLAIMANTS
// AND SOURCE IPs. That is the whole point: the attack it exists to stop is
// guessing one known key's secret from many addresses, so a per-IP or
// per-claimant bucket would not bound it at all.
//
// It lives HERE, beside ClaimKey, rather than in an HTTP handler. The same
// bound previously sat in internal/api in front of the REST doors, while the
// browser form in internal/ui called the account service directly and never
// touched it — so a signed-in caller could exhaust the API bucket and carry
// on through the form (audit 2026-09-15 CA-09). A rate limit in front of one
// of several doors is not a rate limit. Placing it in the function every
// door must call is what makes the bound true rather than intended.
//
// The accepted cost, stated rather than left emergent (round-4 finding F4-5):
// one adversarial claimant can exhaust a real key's bucket and block others
// from claiming THAT key for the window. Claiming is a rare human action with
// no legitimate concurrency, and the alternative is letting the guesser spread
// across addresses, which is the attack.
//
// The bucket is per process. A replicated deployment therefore bounds
// attempts per replica, not per cluster — this is the same scope the HTTP
// limiter had, and it is written down here rather than implied.
type claimLimiter struct {
	mu      sync.Mutex
	hits    map[string][]time.Time
	limit   int
	window  time.Duration
	nowFunc func() time.Time
}

func newClaimLimiter() *claimLimiter {
	return &claimLimiter{hits: map[string][]time.Time{}, limit: ClaimAttemptLimit, window: ClaimAttemptWindow, nowFunc: time.Now}
}

// allow records an attempt against keyID and reports whether it may proceed.
//
// It is called for EVERY attempt before the outcome is known — that is what
// makes the refusal carry no information: it fires identically on a wrong
// secret, a nonexistent id and an already-claimed key, so the rate-limit
// channel cannot reopen the oracle §5.4 closed on the refusals themselves.
func (l *claimLimiter) allow(keyID string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.nowFunc()
	cutoff := now.Add(-l.window)
	kept := l.hits[keyID][:0]
	for _, t := range l.hits[keyID] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[keyID] = kept
		return false
	}
	l.hits[keyID] = append(kept, now)
	return true
}
