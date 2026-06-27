package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// TestAPIKeyLimiter_WarnTier_FiresAt80PctConsumed — the two-tier
// guard added in rate-limit hardening item (6). Once a bucket's
// remaining tokens drop below WarnThresholdFrac × burst, Allow
// returns Warn=true on the SAME call that would otherwise just
// be "ok". AuthMiddleware uses this to emit a warning header so
// operators see degradation coming before the 429 fires.
func TestAPIKeyLimiter_WarnTier_FiresAt80PctConsumed(t *testing.T) {
	l := NewAPIKeyLimiter()
	now := time.Now()
	burst := 10
	// First 7 calls: bucket goes 10→3 remaining. Threshold is
	// 20% (= 2 tokens for burst=10); 3 is still above so no warn.
	for i := 0; i < 7; i++ {
		d := l.Allow("k-warn", 100, burst, now)
		if d.Warn {
			t.Errorf("call %d should NOT warn (remaining=%.2f, threshold=%.2f)", i, d.RemainingTokens, float64(burst)*WarnThresholdFrac)
		}
	}
	// 8th call: 3→2 remaining. 2 ≤ 2 (threshold) → warn.
	d := l.Allow("k-warn", 100, burst, now)
	if !d.Warn {
		t.Errorf("8th call should warn at 80%% consumed: %+v (threshold=%.2f)", d, float64(burst)*WarnThresholdFrac)
	}
	if d.Blocked {
		t.Errorf("8th call must warn, NOT block: %+v", d)
	}
}

// TestAPIKeyLimiter_WarnTier_NotFiredWhenAmpleHeadroom — defensive:
// when the bucket starts full and only one token is consumed, Warn
// must be false. Catches an inverted threshold check.
func TestAPIKeyLimiter_WarnTier_NotFiredWhenAmpleHeadroom(t *testing.T) {
	l := NewAPIKeyLimiter()
	d := l.Allow("k-fresh", 100, 100, time.Now())
	if d.Warn {
		t.Errorf("first call against burst=100 should not warn: %+v", d)
	}
}

// TestAPIKeyLimiter_WarnTier_BlockedAlsoWarns — once the bucket is
// drained the request is blocked, but Warn must ALSO be true so the
// upstream metric counts both "warn" and "block" outcomes without
// having to bucket by remaining tokens again.
func TestAPIKeyLimiter_WarnTier_BlockedAlsoWarns(t *testing.T) {
	l := NewAPIKeyLimiter()
	now := time.Now()
	for i := 0; i < 5; i++ {
		l.Allow("k-drain", 1, 5, now)
	}
	d := l.Allow("k-drain", 1, 5, now)
	if !d.Blocked {
		t.Fatalf("6th call must be blocked: %+v", d)
	}
	if !d.Warn {
		t.Errorf("blocked call must also carry Warn=true so /metrics can pick it up: %+v", d)
	}
}

// TestAPIKeyLimiter_AllowsUpToBurst — the headline property: a
// fresh bucket starts at `burst` tokens and the first `burst`
// calls all pass without blocking.
func TestAPIKeyLimiter_AllowsUpToBurst(t *testing.T) {
	l := NewAPIKeyLimiter()
	now := time.Now()
	for i := 0; i < 5; i++ {
		d := l.Allow("akey-1", 10, 5, now)
		if d.Blocked {
			t.Errorf("call %d blocked unexpectedly: %+v", i, d)
		}
	}
	// Next call must block — bucket drained.
	d := l.Allow("akey-1", 10, 5, now)
	if !d.Blocked {
		t.Errorf("post-burst call should block: %+v", d)
	}
	if d.RetryAfter <= 0 {
		t.Errorf("retry_after should be positive when blocked: %+v", d)
	}
}

// TestAPIKeyLimiter_RefillsLinearly — after some elapsed time
// at rate=rps, the bucket refills proportionally. Pin the
// arithmetic so a future "use ticker instead of lazy refill"
// refactor doesn't silently change the calibration.
func TestAPIKeyLimiter_RefillsLinearly(t *testing.T) {
	l := NewAPIKeyLimiter()
	now := time.Now()
	// Drain a 5-token bucket at rps=10.
	for i := 0; i < 5; i++ {
		l.Allow("k", 10, 5, now)
	}
	// 200ms later → 2 tokens refilled (10 rps × 0.2 s = 2.0).
	later := now.Add(200 * time.Millisecond)
	d := l.Allow("k", 10, 5, later)
	if d.Blocked {
		t.Fatalf("after 200ms refill should allow: %+v", d)
	}
	// After 5 drains + 1 allow (post-refill), bucket has ~1.0 tokens.
	if d.RemainingTokens < 0.9 || d.RemainingTokens > 1.1 {
		t.Errorf("remaining_tokens = %.2f, want ≈1.0", d.RemainingTokens)
	}
}

// TestAPIKeyLimiter_RetryAfterReflectsDeficit — when a call
// blocks, RetryAfter must be the time needed to accrue 1 token
// at the configured rps. Operators rely on this for the
// HTTP Retry-After header so backoff is precise.
func TestAPIKeyLimiter_RetryAfterReflectsDeficit(t *testing.T) {
	l := NewAPIKeyLimiter()
	now := time.Now()
	// Drain 5-token bucket at rps=10.
	for i := 0; i < 5; i++ {
		l.Allow("k", 10, 5, now)
	}
	d := l.Allow("k", 10, 5, now)
	if !d.Blocked {
		t.Fatalf("expected block")
	}
	// At rps=10, 1 token = 100ms. Allow ±10ms slack for arithmetic.
	if d.RetryAfter < 90*time.Millisecond || d.RetryAfter > 110*time.Millisecond {
		t.Errorf("retry_after = %v, want ≈100ms", d.RetryAfter)
	}
}

// TestAPIKeyLimiter_NoLimitBypasses — rps≤0 OR burst≤0 means
// "no limit configured"; the call must pass un-blocked and the
// limiter MUST NOT allocate a bucket (would leak memory on
// keys that don't have a rate-limit configured).
func TestAPIKeyLimiter_NoLimitBypasses(t *testing.T) {
	l := NewAPIKeyLimiter()
	for i := 0; i < 100; i++ {
		if d := l.Allow("k", 0, 0, time.Now()); d.Blocked {
			t.Errorf("no-limit call %d blocked unexpectedly: %+v", i, d)
		}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.buckets) != 0 {
		t.Errorf("no-limit calls leaked %d buckets", len(l.buckets))
	}
}

// TestAPIKeyLimiter_BucketsAreIsolated — two different keys
// share the same APIKeyLimiter but have private buckets. One
// key blocked doesn't affect the other.
func TestAPIKeyLimiter_BucketsAreIsolated(t *testing.T) {
	l := NewAPIKeyLimiter()
	now := time.Now()
	// Drain key A.
	for i := 0; i < 5; i++ {
		l.Allow("A", 10, 5, now)
	}
	if !l.Allow("A", 10, 5, now).Blocked {
		t.Fatal("A should be drained")
	}
	// B is fresh and should pass.
	if l.Allow("B", 10, 5, now).Blocked {
		t.Error("B's bucket affected by A's drain — cross-contamination")
	}
}

// TestAPIKeyLimiter_Forget_DropsState — Forget on a revoked
// key MUST evict its bucket so the next allocation for the
// same ID gets a fresh full bucket (the revoked key won't
// authenticate, but a future rotation could reuse the ID
// shape — and we don't want phantom state hanging around).
func TestAPIKeyLimiter_Forget_DropsState(t *testing.T) {
	l := NewAPIKeyLimiter()
	now := time.Now()
	l.Allow("k", 10, 1, now) // drains the burst
	if !l.Allow("k", 10, 1, now).Blocked {
		t.Fatal("expected drain")
	}
	l.Forget("k")
	// Brand-new bucket; first call passes again.
	if l.Allow("k", 10, 1, now).Blocked {
		t.Error("Forget did not reset bucket")
	}
}

// TestAPIKeyLimiter_ConcurrentBucketAllocation — race-safety
// guard: two goroutines racing to allocate the same key must
// end up with exactly one bucket. Run under `-race` to catch
// any mu/map misuse.
func TestAPIKeyLimiter_ConcurrentBucketAllocation(t *testing.T) {
	l := NewAPIKeyLimiter()
	var wg sync.WaitGroup
	start := make(chan struct{})
	now := time.Now()
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l.Allow("shared-key", 1000, 1000, now)
		}()
	}
	close(start)
	wg.Wait()
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.buckets) != 1 {
		t.Errorf("buckets = %d, want 1 (race in bucketFor?)", len(l.buckets))
	}
}

// TestAPIKeyLimiter_ClockBackwardSafe — defensive: if the
// system clock moves backward between calls (NTP step), the
// bucket MUST NOT refund tokens. Pre-fix a naïve `elapsed *
// rps` would have credited the negative elapsed as a negative
// addition, leaking tokens.
func TestAPIKeyLimiter_ClockBackwardSafe(t *testing.T) {
	l := NewAPIKeyLimiter()
	now := time.Now()
	l.Allow("k", 10, 5, now) // tokens ≈ 4
	earlier := now.Add(-1 * time.Second)
	d := l.Allow("k", 10, 5, earlier) // clock went backward
	// Tokens unchanged (modulo the 1 we consumed): the refill
	// helper noops on non-positive elapsed.
	if d.Blocked {
		t.Errorf("backward-clock call should NOT block when bucket has tokens: %+v", d)
	}
	if d.RemainingTokens < 2.9 || d.RemainingTokens > 4.1 {
		t.Errorf("remaining_tokens drifted on backward clock: %.2f", d.RemainingTokens)
	}
}
