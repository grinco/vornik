package chatauth

import (
	"strconv"
	"testing"
	"time"
)

// TestRedemptionLimiter_BoundsAttemptsNotFailures — the limiter counts every
// attempt, before the outcome is known. A limiter that counted only failures
// could never bound the attempts it exists to bound; a design review caught
// exactly that shape in the HTTP claim limiter, where the bucket could not
// increment on the attempts it was written for.
func TestRedemptionLimiter_BoundsAttemptsNotFailures(t *testing.T) {
	l := NewRedemptionLimiter()
	for i := 0; i < redemptionLimit; i++ {
		if !l.Allow("telegram", "42") {
			t.Fatalf("attempt %d refused below the limit", i+1)
		}
	}
	if l.Allow("telegram", "42") {
		t.Error("the limit did not bind")
	}
}

// TestRedemptionLimiter_IsPerSpeakerNotPerChat — keyed by the speaker, so
// moving to another group does not reset the count, and one speaker's typos
// never lock anyone else out.
func TestRedemptionLimiter_IsPerSpeakerNotPerChat(t *testing.T) {
	l := NewRedemptionLimiter()
	for i := 0; i < redemptionLimit; i++ {
		l.Allow("telegram", "42")
	}
	if l.Allow("telegram", "42") {
		t.Error("the exhausted speaker was allowed again")
	}
	if !l.Allow("telegram", "43") {
		t.Error("a different speaker was locked out by someone else's attempts")
	}
	if !l.Allow("slack", "42") {
		t.Error("the same external id on another CHANNEL was locked out; the key must include the channel")
	}
}

// TestRedemptionLimiter_WindowExpires — a person who mistyped twice this
// morning is not locked out this afternoon.
func TestRedemptionLimiter_WindowExpires(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	l := NewRedemptionLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < redemptionLimit; i++ {
		l.Allow("telegram", "42")
	}
	if l.Allow("telegram", "42") {
		t.Fatal("the limit did not bind")
	}
	now = now.Add(redemptionWindow + time.Second)
	if !l.Allow("telegram", "42") {
		t.Error("the window never expired; a typo would lock a speaker out permanently")
	}
}

// TestRedemptionLimiter_NilIsPermissive — a channel with no limiter wired
// must keep working, since the limiter is defence in depth and not the
// authorization decision.
func TestRedemptionLimiter_NilIsPermissive(t *testing.T) {
	var l *RedemptionLimiter
	if !l.Allow("telegram", "42") {
		t.Error("a nil limiter refused")
	}
}

// TestRedemptionLimiter_ForgetsIdleSpeakers — the slices are trimmed by the
// filter, but without a sweep the KEYS accumulate: one per distinct speaker
// for the life of the process, on a bot that may see many
// (review-20260915-8284).
func TestRedemptionLimiter_ForgetsIdleSpeakers(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	l := NewRedemptionLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < 200; i++ {
		l.Allow("telegram", "churn-"+strconv.Itoa(i))
	}
	now = now.Add(redemptionWindow + time.Second)
	// One more attempt, well after every earlier window closed.
	l.Allow("telegram", "current")

	l.mu.Lock()
	size := len(l.attempts)
	l.mu.Unlock()
	if size > redemptionLimit+1 {
		t.Errorf("the limiter holds %d speaker keys after all but one window expired; "+
			"idle speakers are never forgotten", size)
	}
	// And the sweep must not lock out the speaker who is still active.
	if !l.Allow("telegram", "current") {
		t.Error("the sweep dropped the active speaker's window")
	}
}
