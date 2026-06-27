package api

import (
	"sync"
	"time"
)

// authFailureLimiter is a per-client-IP brute-force lockout for API-key auth.
// The throughput limiters (PerIPLimiter / APIKeyLimiter) bound request RATE
// but don't react to repeated AUTH FAILURES — so an attacker rotating guesses
// under the rate cap could grind the static admin key. This limiter counts
// consecutive auth failures per IP within a sliding window and locks the IP
// out for a cooldown once the threshold is crossed.
//
// Window-based (no success-reset needed): a legitimate client that fat-fingers
// a few times within the window stays well under the threshold, so it's never
// locked; only a sustained guessing run trips it. Nil-safe — a nil limiter
// disables the lockout (tests / deployments that opt out).
type authFailureLimiter struct {
	mu          sync.Mutex
	buckets     map[string]*authFailureBucket
	maxFailures int
	window      time.Duration
	lockout     time.Duration
	now         func() time.Time
}

type authFailureBucket struct {
	count       int
	windowStart time.Time
	lockedUntil time.Time
}

// newAuthFailureLimiter builds a limiter. Sensible defaults if args are
// non-positive: 15 failures / 5 min → 15 min lockout.
func newAuthFailureLimiter(maxFailures int, window, lockout time.Duration) *authFailureLimiter {
	if maxFailures <= 0 {
		maxFailures = 15
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	if lockout <= 0 {
		lockout = 15 * time.Minute
	}
	return &authFailureLimiter{
		buckets:     map[string]*authFailureBucket{},
		maxFailures: maxFailures,
		window:      window,
		lockout:     lockout,
		now:         time.Now,
	}
}

// Allowed reports whether the IP may attempt auth (false ⇒ currently locked
// out). retryAfter is the remaining lockout when blocked.
func (l *authFailureLimiter) Allowed(ip string) (allowed bool, retryAfter time.Duration) {
	if l == nil || ip == "" {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[ip]
	if b == nil {
		return true, 0
	}
	now := l.now()
	if now.Before(b.lockedUntil) {
		return false, b.lockedUntil.Sub(now)
	}
	return true, 0
}

// RecordFailure registers one auth failure for the IP and locks the IP out
// when the per-window threshold is crossed.
func (l *authFailureLimiter) RecordFailure(ip string) {
	if l == nil || ip == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[ip]
	if b == nil {
		b = &authFailureBucket{windowStart: now}
		l.buckets[ip] = b
	}
	// Already locked — extend nothing; the lockout runs to completion.
	if now.Before(b.lockedUntil) {
		return
	}
	// New window?
	if now.Sub(b.windowStart) > l.window {
		b.count = 0
		b.windowStart = now
	}
	b.count++
	if b.count >= l.maxFailures {
		b.lockedUntil = now.Add(l.lockout)
		b.count = 0
		b.windowStart = now
	}
	// Opportunistic prune so the map can't grow unbounded.
	if len(l.buckets) > 10000 {
		for k, v := range l.buckets {
			if now.After(v.lockedUntil) && now.Sub(v.windowStart) > l.window {
				delete(l.buckets, k)
			}
		}
	}
}
