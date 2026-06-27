package ratelimit

import (
	"sync"
	"time"
)

// APIKeyLimiter is the per-API-key request-rate gate enforced by
// AuthMiddleware after a successful DB-backed key lookup. Each
// bound key with a non-NULL rate_limit_rps gets a private token
// bucket; calls beyond the bucket capacity surface as 429 with a
// Retry-After header derived from the bucket's refill time.
//
// In-memory only — like the project-task limiter above, the
// counter resets on daemon restart. The 8-gap rate-limit
// hardening item in BACKLOG.md tracks the move to durable
// distributed state for multi-daemon SaaS deployments.
//
// Concurrency: each key's bucket has its own mutex so high-RPS
// keys don't contend with low-RPS keys. The outer map is
// protected by APIKeyLimiter.mu only for inserts; reads are
// lock-free against the map (sync.Map would also work but the
// concrete map+mutex is easier to reason about).
type APIKeyLimiter struct {
	mu      sync.RWMutex
	buckets map[string]*keyBucket // keyID → bucket
}

// NewAPIKeyLimiter constructs an empty limiter. Buckets are
// lazily allocated on first request per key.
func NewAPIKeyLimiter() *APIKeyLimiter {
	return &APIKeyLimiter{buckets: make(map[string]*keyBucket)}
}

// WarnThresholdFrac is the bucket-headroom fraction below which Allow
// flags the decision as Warn — by default 20% remaining, i.e. once
// 80% of burst is consumed. AuthMiddleware surfaces the warning via
// a non-blocking response header so operators (and HA clients)
// back off before the bucket actually empties.
const WarnThresholdFrac = 0.20

// KeyDecision is the outcome of one Allow call.
type KeyDecision struct {
	// Blocked is true when the bucket is empty.
	Blocked bool
	// Warn is true when the bucket is past the WarnThresholdFrac
	// headroom — operators get a heads-up via a response header
	// before the 429 fires. Always true when Blocked is true so a
	// single counter increment ("decision outcome") captures the
	// worst-case classification.
	Warn bool
	// RetryAfter is the time until ≥1 token will be available
	// again. Surfaced in the HTTP Retry-After header so the
	// caller can back off precisely instead of guessing.
	// Zero when Blocked == false.
	RetryAfter time.Duration
	// RemainingTokens is the bucket level AFTER this call —
	// useful for "X-RateLimit-Remaining" response headers and
	// the operator UI panel.
	RemainingTokens float64
}

// Allow consumes one token from the bucket for keyID. rps is the
// long-term sustained rate (tokens added per second); burst is
// the maximum number of tokens the bucket can hold (max
// instantaneous in-flight requests). rps≤0 or burst≤0 means "no
// limit" — Allow returns an un-blocked decision without
// allocating a bucket.
func (l *APIKeyLimiter) Allow(keyID string, rps, burst int, now time.Time) KeyDecision {
	if l == nil || keyID == "" || rps <= 0 || burst <= 0 {
		return KeyDecision{}
	}
	b := l.bucketFor(keyID, rps, burst, now)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(rps, burst, now)
	warnLine := float64(burst) * WarnThresholdFrac
	if b.tokens >= 1 {
		b.tokens--
		return KeyDecision{
			RemainingTokens: b.tokens,
			Warn:            b.tokens <= warnLine,
		}
	}
	needed := 1 - b.tokens
	retryAfter := time.Duration(needed/float64(rps)*1e9) * time.Nanosecond
	return KeyDecision{
		Blocked:         true,
		Warn:            true, // blocked implies warn — single counter outcome
		RetryAfter:      retryAfter,
		RemainingTokens: b.tokens,
	}
}

// bucketFor returns the keyID's bucket, allocating it on first
// touch. Two readers racing to allocate the same key resolve
// under the write lock; only one bucket survives.
func (l *APIKeyLimiter) bucketFor(keyID string, rps, burst int, now time.Time) *keyBucket {
	l.mu.RLock()
	b, ok := l.buckets[keyID]
	l.mu.RUnlock()
	if ok {
		return b
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Double-check under the write lock.
	if b, ok = l.buckets[keyID]; ok {
		return b
	}
	b = &keyBucket{
		tokens:     float64(burst), // start full so the first request always passes
		lastRefill: now,
	}
	l.buckets[keyID] = b
	return b
}

// KeyBucketSnapshot is the lock-free view of one key's current
// bucket level. Used by the
// /api/v1/projects/{id}/ratelimit-status endpoint so the UI panel
// can render "tokens remaining" per key without driving an Allow
// call (which would consume a token just to read the gauge).
type KeyBucketSnapshot struct {
	KeyID  string
	Tokens float64
	// LastRefill is the timestamp of the last refill arithmetic —
	// useful for "idle since" labels on the panel. Zero on a
	// freshly-allocated bucket that hasn't yet been refilled.
	LastRefill time.Time
}

// Snapshot returns the current bucket state for every keyID the
// limiter has seen since boot or its last Forget call. Read-only
// — does NOT refill or consume; the returned tokens count is the
// post-last-Allow level, which is precisely what the operator
// panel wants (matches Prometheus RemainingTokens gauge). Returned
// slice is freshly allocated; callers may mutate it freely.
func (l *APIKeyLimiter) Snapshot() []KeyBucketSnapshot {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]KeyBucketSnapshot, 0, len(l.buckets))
	for keyID, b := range l.buckets {
		b.mu.Lock()
		out = append(out, KeyBucketSnapshot{
			KeyID:      keyID,
			Tokens:     b.tokens,
			LastRefill: b.lastRefill,
		})
		b.mu.Unlock()
	}
	return out
}

// SnapshotFor returns the current bucket state for a single key,
// or zero (Tokens=0, LastRefill=zero) when no bucket has been
// allocated for the key — that's distinct from "bucket allocated
// and empty", which returns Tokens=0 with a non-zero LastRefill.
// The boolean return discriminates.
func (l *APIKeyLimiter) SnapshotFor(keyID string) (KeyBucketSnapshot, bool) {
	if l == nil || keyID == "" {
		return KeyBucketSnapshot{}, false
	}
	l.mu.RLock()
	b, ok := l.buckets[keyID]
	l.mu.RUnlock()
	if !ok {
		return KeyBucketSnapshot{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return KeyBucketSnapshot{
		KeyID:      keyID,
		Tokens:     b.tokens,
		LastRefill: b.lastRefill,
	}, true
}

// Forget drops the bucket for keyID. Called on key revocation
// so a revoked key's state doesn't squat memory until daemon
// restart. Idempotent.
func (l *APIKeyLimiter) Forget(keyID string) {
	if l == nil || keyID == "" {
		return
	}
	l.mu.Lock()
	delete(l.buckets, keyID)
	l.mu.Unlock()
}

// keyBucket is one key's token-bucket state. Refill is
// computed lazily on each Allow — no background ticker.
type keyBucket struct {
	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
}

// refill adds tokens proportional to elapsed time since the
// previous refill, capped at burst. Caller holds b.mu.
func (b *keyBucket) refill(rps, burst int, now time.Time) {
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed <= 0 {
		// Clock moved backward (NTP adjust) or two calls in the
		// same nanosecond. No-op; never refund tokens. 2026-05-29
		// audit fix: DO NOT advance lastRefill to `now` here — if
		// we did, the next refill would compute elapsed against
		// the new (backward) timestamp, granting double tokens for
		// the same wall-clock interval once the clock recovers
		// forward. Leaving lastRefill at the previous (later)
		// timestamp means the next call computes elapsed correctly
		// from the last real forward time.
		return
	}
	added := elapsed * float64(rps)
	b.tokens += added
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.lastRefill = now
}
