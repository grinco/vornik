package email

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

// IMAPReconnector is the optional reconnect seam an IMAPClient may
// implement. The slice-2 channel detects a transport-level fetch
// error (EOF, closed network connection, broken pipe) and calls
// Reconnect on the same client before retrying the fetch. Clients
// that don't implement IMAPReconnector are tolerated — the channel
// simply logs the transport drop and waits for the next poll cycle,
// matching slice-1 behaviour.
//
// The interface intentionally stays a single method so test fakes
// can wire it without dragging in dial-config knowledge — the
// implementation holds onto the original IMAPDialConfig it was
// passed at Connect time and reuses it.
type IMAPReconnector interface {
	// Reconnect tears down the existing IMAP connection (if any)
	// and dials a fresh one using the credentials supplied at the
	// initial Connect call. Returns the wrapped transport error
	// when the redial fails; callers log + continue.
	Reconnect(ctx context.Context) error
}

// isTransportError reports whether err indicates a TCP/TLS-level
// drop that warrants a reconnect attempt, vs. an application-level
// error (bad search criteria, mailbox not found) where a reconnect
// won't help.
//
// The classifier is intentionally permissive on the transport side:
// false negatives (missing a real drop) keep the channel stuck for
// one extra poll cycle; false positives (calling Reconnect on a
// non-drop) just burn one entry of the rate-limit budget. Erring
// toward false positives keeps the slice-2 hardening robust against
// the long tail of IMAP server error shapes.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// Network-error matching by message substring. Go's net package
	// returns concrete *net.OpError / *os.SyscallError types but the
	// inner text is the only thing stable across versions for the
	// "use of closed network connection" idiom (net.ErrClosed in
	// Go 1.16+ is type-checkable but only when the error wasn't
	// wrapped through an intermediate layer that flattens the chain
	// — the IMAP client library does flatten, so string matching
	// is the pragmatic call).
	msg := strings.ToLower(err.Error())
	transportNeedles := []string{
		"use of closed network connection",
		"broken pipe",
		"connection reset",
		"connection refused",
		"connection closed",
		"i/o timeout",
		"eof",
		"network is unreachable",
		"no route to host",
	}
	for _, needle := range transportNeedles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// reconnectLimiter is a tiny token bucket holding the timestamps of
// the most recent N reconnects. tryAcquire returns true when a new
// reconnect can fire (i.e. the oldest entry in the window is older
// than `window` ago, OR the buffer hasn't yet filled to N entries).
//
// Design choice: ring buffer of timestamps rather than a leaky
// bucket counter so the cap is exactly N-per-window without the
// fractional-token rounding errors a bucket has at low rates. The
// buffer is sized once at construction; tryAcquire is the only
// mutator.
type reconnectLimiter struct {
	mu         sync.Mutex
	window     time.Duration
	limit      int
	timestamps []time.Time
	clock      func() time.Time
}

// newReconnectLimiter constructs a rate limiter that admits at most
// `limit` events per `window`. A zero limit denies every call —
// useful for tests asserting the deny-all branch but not a
// configuration the channel itself selects.
func newReconnectLimiter(limit int, window time.Duration, clock func() time.Time) *reconnectLimiter {
	if clock == nil {
		clock = time.Now
	}
	return &reconnectLimiter{
		window: window,
		limit:  limit,
		clock:  clock,
		// Pre-allocate the slice to avoid grow allocations under
		// load; capacity = limit means tryAcquire can always do
		// the trim-and-append in one pass.
		timestamps: make([]time.Time, 0, limit),
	}
}

// tryAcquire admits the call (returning true) when the count of
// timestamps within `window` of now is below the limit. Otherwise
// returns false without modifying state.
func (r *reconnectLimiter) tryAcquire() bool {
	if r.limit <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	cutoff := now.Add(-r.window)
	// Drop stale entries (older than the window). Linear scan is
	// fine — the buffer is at most `limit` entries, which is single
	// digit in practice.
	kept := r.timestamps[:0]
	for _, t := range r.timestamps {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	r.timestamps = kept
	if len(r.timestamps) >= r.limit {
		return false
	}
	r.timestamps = append(r.timestamps, now)
	return true
}
