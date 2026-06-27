package api

import (
	"testing"
	"time"
)

func TestAuthFailureLimiter_LocksOutAfterThreshold(t *testing.T) {
	now := time.Now()
	l := newAuthFailureLimiter(5, time.Minute, 10*time.Minute)
	l.now = func() time.Time { return now }

	ip := "203.0.113.9"
	// 4 failures — still allowed.
	for i := 0; i < 4; i++ {
		l.RecordFailure(ip)
		if ok, _ := l.Allowed(ip); !ok {
			t.Fatalf("locked out after %d failures, want still allowed", i+1)
		}
	}
	// 5th crosses the threshold → locked.
	l.RecordFailure(ip)
	ok, retry := l.Allowed(ip)
	if ok {
		t.Fatal("must be locked out after 5 failures")
	}
	if retry <= 0 || retry > 10*time.Minute {
		t.Errorf("retry-after = %v, want (0, 10m]", retry)
	}

	// A different IP is unaffected.
	if ok, _ := l.Allowed("198.51.100.1"); !ok {
		t.Error("unrelated IP must not be locked")
	}

	// After the lockout elapses, allowed again.
	now = now.Add(11 * time.Minute)
	if ok, _ := l.Allowed(ip); !ok {
		t.Error("must be allowed again after lockout elapses")
	}
}

func TestAuthFailureLimiter_WindowResets(t *testing.T) {
	now := time.Now()
	l := newAuthFailureLimiter(5, time.Minute, 10*time.Minute)
	l.now = func() time.Time { return now }
	ip := "203.0.113.10"

	// 4 failures, then the window lapses → counter resets, no lockout.
	for i := 0; i < 4; i++ {
		l.RecordFailure(ip)
	}
	now = now.Add(2 * time.Minute) // window (1m) elapsed
	l.RecordFailure(ip)            // counts as 1 in a fresh window
	if ok, _ := l.Allowed(ip); !ok {
		t.Fatal("scattered failures across windows must not lock out")
	}
}

func TestAuthFailureLimiter_NilSafe(t *testing.T) {
	var l *authFailureLimiter
	if ok, _ := l.Allowed("1.2.3.4"); !ok {
		t.Error("nil limiter must allow")
	}
	l.RecordFailure("1.2.3.4") // must not panic
	// empty IP is always allowed (unparseable RemoteAddr).
	l2 := newAuthFailureLimiter(1, time.Minute, time.Minute)
	l2.RecordFailure("")
	if ok, _ := l2.Allowed(""); !ok {
		t.Error("empty IP must be allowed (no key to track)")
	}
}
