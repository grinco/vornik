package authz

import (
	"context"
	"sync"
	"time"

	"vornik.io/vornik/internal/persistence"
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
// The bucket is per process, and since 2026-09-19 that is the FALLBACK rather
// than the mechanism: when a KeyClaimAttemptRepository is wired, the bound is
// counted in the database and holds across replicas and restarts (§5.4
// follow-up). This in-memory bucket remains for a deployment with no repository
// — one process, where per-process IS per cluster — so the bound never
// disappears because a store is missing.
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

// allowClaimAttempt records the attempt and reports whether it may proceed,
// preferring the DURABLE bound when one is wired.
//
// §5.4's bound held per process: N replicas allowed N times the attempts and a
// restart cleared the count. That was equally true of the HTTP limiter it
// replaced, so it was written down as remaining rather than treated as a
// regression — this closes it.
//
// A store ERROR refuses. The bound exists to bound a guesser, so an unreadable
// store is not permission — and the alternative, silently falling back to the
// in-memory bucket, would mean a deployment whose database is flapping quietly
// loses the property it thinks it has.
func (a *Accounts) allowClaimAttempt(ctx context.Context, keyID string) bool {
	if a == nil {
		return false
	}
	if a.claimAttempts == nil {
		return a.claims.allow(keyID)
	}
	count, err := a.claimAttempts.RecordClaimAttempt(ctx, keyID, time.Now().UTC(), ClaimAttemptWindow)
	if err != nil {
		return false
	}
	return count <= ClaimAttemptLimit
}

// WithClaimAttempts wires the durable attempt store. Without it the in-memory
// bucket stands, which is correct for a single-process deployment and stated
// rather than assumed.
func (a *Accounts) WithClaimAttempts(r persistence.KeyClaimAttemptRepository) *Accounts {
	if a == nil {
		return nil
	}
	a.claimAttempts = r
	return a
}
