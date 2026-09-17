package authz

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Regression: audit 2026-09-15 CA-09 — "Browser Claims Bypass the Shared
// Target-Key Limit".
//
// The 10-attempts-per-key-per-hour bound of §5.4 lived in internal/api, in
// front of one of the two doors that reach ClaimKey. The browser form in
// internal/ui called the account service DIRECTLY, so a signed-in caller
// could exhaust the API bucket and keep going through the form — or skip the
// API entirely. The API handler's own comment claimed the bucket was "the
// same bucket as the operator route: the attack does not care which door the
// guesses arrive through", which was true of the intent and not of the code.
//
// The bound now lives in ClaimKey itself, the one function every door must
// call, so a third door cannot be added past it.
func TestClaimKey_TargetKeyAttemptsAreBoundedAtTheService(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	f.accounts.claims.nowFunc = func() time.Time { return now }

	target := f.key(t, "akey_target", "real-secret")
	f.key(t, "akey_other", "another-secret")

	var lastErr error
	for i := 0; i < ClaimAttemptLimit+2; i++ {
		lastErr = f.accounts.ClaimKey(ctx, f.other.ID, target, "wrong-secret", Actor{Principal: "test"})
	}
	if !errors.Is(lastErr, ErrKeyClaimRateLimited) {
		t.Fatalf("after %d attempts against one key id the service must refuse; got %v", ClaimAttemptLimit+2, lastErr)
	}

	// A DIFFERENT key id has its own bucket: one target being hammered must
	// not deny claims of unrelated keys.
	if err := f.accounts.ClaimKey(ctx, f.other.ID, "akey_other", "wrong-secret", Actor{Principal: "test"}); errors.Is(err, ErrKeyClaimRateLimited) {
		t.Fatal("the bucket must be per target key, not global")
	}

	// The window expires.
	now = now.Add(ClaimAttemptWindow + time.Minute)
	if err := f.accounts.ClaimKey(ctx, f.other.ID, target, "wrong-secret", Actor{Principal: "test"}); errors.Is(err, ErrKeyClaimRateLimited) {
		t.Fatal("the bound is a rolling window, not a permanent block")
	}
}

// The refusal must arrive BEFORE the key is looked up, or the limiter would
// only bound attempts that already got their answer.
func TestClaimKey_RateLimitRefusalPrecedesTheLookup(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	target := f.key(t, "akey_target", "real-secret")
	for i := 0; i < ClaimAttemptLimit; i++ {
		_ = f.accounts.ClaimKey(ctx, f.other.ID, target, "wrong", Actor{Principal: "test"})
	}
	before := len(f.keys.calls)
	if err := f.accounts.ClaimKey(ctx, f.other.ID, target, "wrong", Actor{Principal: "test"}); !errors.Is(err, ErrKeyClaimRateLimited) {
		t.Fatalf("want ErrKeyClaimRateLimited, got %v", err)
	}
	if len(f.keys.calls) != before {
		t.Fatal("a rate-limited attempt must not reach the key repository")
	}
}
