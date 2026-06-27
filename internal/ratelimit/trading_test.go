package ratelimit

import (
	"testing"
	"time"
)

// Backlog: "per-project rate-limit before a 2nd trading project"
// (batch-2 Financials PRE-LIVE blockers). The trading order-placement
// audit surface needs its OWN per-project sliding-window cap, isolated
// from the task-creation counters, reusing this primitive. These tests
// pin: caps enforced per key, distinct keys don't interfere, zero caps
// disable, and a Retry-After hint is produced once a window is full.

func TestCheckCapsZeroDisabled(t *testing.T) {
	l := New()
	now := time.Unix(1_700_000_000, 0)
	d := l.CheckKey("proj-a", now, RateLimit{})
	if d.Blocked {
		t.Fatal("zero caps must not block")
	}
}

func TestCheckCapsPerMinute(t *testing.T) {
	l := New()
	now := time.Unix(1_700_000_000, 0)
	caps := RateLimit{PerMinute: 2}
	for i := 0; i < 2; i++ {
		if d := l.CheckKey("proj-a", now, caps); d.Blocked {
			t.Fatalf("request %d should be allowed", i)
		}
		l.RecordKey("proj-a", now)
	}
	d := l.CheckKey("proj-a", now, caps)
	if !d.Blocked {
		t.Fatal("third request within the minute should be blocked")
	}
	if d.RetryAfter <= 0 {
		t.Fatalf("blocked decision must carry a positive RetryAfter, got %v", d.RetryAfter)
	}
}

func TestCheckCapsPerHour(t *testing.T) {
	l := New()
	now := time.Unix(1_700_000_000, 0)
	caps := RateLimit{PerHour: 3}
	// Spread across the hour so the per-minute window never trips.
	for i := 0; i < 3; i++ {
		ts := now.Add(time.Duration(i) * 10 * time.Minute)
		if d := l.CheckKey("proj-a", ts, caps); d.Blocked {
			t.Fatalf("request %d should be allowed", i)
		}
		l.RecordKey("proj-a", ts)
	}
	d := l.CheckKey("proj-a", now.Add(35*time.Minute), caps)
	if !d.Blocked {
		t.Fatal("fourth request within the hour should be blocked")
	}
	if d.RetryAfter <= 0 || d.RetryAfter > time.Hour {
		t.Fatalf("RetryAfter should be within the hour window, got %v", d.RetryAfter)
	}
}

func TestCheckCapsKeyIsolation(t *testing.T) {
	l := New()
	now := time.Unix(1_700_000_000, 0)
	caps := RateLimit{PerMinute: 1}
	l.RecordKey("proj-a", now)
	if d := l.CheckKey("proj-a", now, caps); !d.Blocked {
		t.Fatal("proj-a should be blocked after one record")
	}
	if d := l.CheckKey("proj-b", now, caps); d.Blocked {
		t.Fatal("proj-b must not be affected by proj-a's counter")
	}
}

func TestCheckKeyNamespaceIsolatedFromTaskCounters(t *testing.T) {
	// RecordKey must not pollute the task-creation counters keyed by
	// project ID directly (Record), and vice versa — trading orders
	// and task creation are independent rate dimensions.
	l := New()
	now := time.Unix(1_700_000_000, 0)
	l.Record("proj-a", now) // task-creation slot
	d := l.CheckKey("proj-a", now, RateLimit{PerMinute: 1})
	if d.Blocked {
		t.Fatal("task-creation Record must not consume a trading slot")
	}
}
