package ratelimit

import "time"

// tradingKeyPrefix namespaces the trading sliding-window counters
// inside the same backing map as the task-creation counters. A
// project ID can never contain this NUL-byte sequence, so the two
// dimensions (task creation keyed by raw project ID via Record, and
// trading orders keyed by RecordKey) share the Limiter struct without
// ever colliding. Keeping one struct avoids a second mutex + map for
// what is structurally the same sliding-window algorithm.
const tradingKeyPrefix = "\x00trading:"

// RateLimit is a generic per-window cap used by the key-scoped
// CheckKey path. Zero in a field disables that window. Distinct from
// registry.ProjectRateLimit (which is task-creation specific and read
// directly off the Project struct) so the trading surface can carry
// its own caps without overloading the task-creation fields.
type RateLimit struct {
	PerMinute int
	PerHour   int
}

// CheckKey is the key-scoped, explicit-caps variant of Check: it
// inspects whether one more event for key would exceed caps, using a
// trading-namespaced sliding window so it never shares a counter with
// task creation. Read-only (does not consume a slot). The returned
// Decision carries a RetryAfter hint: the time until the oldest event
// in the binding window ages out, so the caller can emit an accurate
// HTTP Retry-After. Safe under concurrent calls.
func (l *Limiter) CheckKey(key string, now time.Time, caps RateLimit) Decision {
	if l == nil || key == "" {
		return Decision{}
	}
	if caps.PerMinute == 0 && caps.PerHour == 0 {
		return Decision{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	mk := tradingKeyPrefix + key
	ts := l.log[mk]

	minuteCutoff := now.Add(-1 * time.Minute)
	hourCutoff := now.Add(-1 * time.Hour)
	ts = pruneOlderThan(ts, hourCutoff)
	l.log[mk] = ts

	var minuteCount, hourCount int
	var oldestMinute, oldestHour time.Time
	for _, t := range ts {
		if t.After(hourCutoff) {
			hourCount++
			if oldestHour.IsZero() || t.Before(oldestHour) {
				oldestHour = t
			}
		}
		if t.After(minuteCutoff) {
			minuteCount++
			if oldestMinute.IsZero() || t.Before(oldestMinute) {
				oldestMinute = t
			}
		}
	}

	d := Decision{MinuteCount: minuteCount, HourCount: hourCount}
	if caps.PerMinute > 0 && minuteCount >= caps.PerMinute {
		d.Blocked = true
		d.Reason = "per-minute trading rate limit reached"
		// Retry once the oldest minute-window event ages past 1m.
		d.RetryAfter = retryUntil(oldestMinute.Add(time.Minute), now)
		return d
	}
	if caps.PerHour > 0 && hourCount >= caps.PerHour {
		d.Blocked = true
		d.Reason = "per-hour trading rate limit reached"
		d.RetryAfter = retryUntil(oldestHour.Add(time.Hour), now)
		return d
	}
	return d
}

// RecordKey marks one accepted event for key in the trading-namespaced
// window. Mirrors Record but for the key-scoped CheckKey surface.
func (l *Limiter) RecordKey(key string, now time.Time) {
	if l == nil || key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	mk := tradingKeyPrefix + key
	l.log[mk] = append(l.log[mk], now)
}

// retryUntil returns the positive duration from now until t. A
// non-positive result is floored to one second so callers always emit
// a Retry-After >= 1 (the window has effectively just freed, but the
// client should still pause briefly).
func retryUntil(t, now time.Time) time.Duration {
	d := t.Sub(now)
	if d <= 0 {
		return time.Second
	}
	return d
}
