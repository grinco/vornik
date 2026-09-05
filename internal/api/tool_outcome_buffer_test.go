package api

import (
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/mcp"
)

func TestBufferClaimsWhatTheDaemonObserved(t *testing.T) {
	b := newToolOutcomeBuffer()
	b.Record("exec-1", "searchJira", &mcp.CallError{Server: "atlassian", Tool: "searchJira", Status: 401, Class: mcp.FailureAuth})

	outcome, class, ok := b.Claim("exec-1", "searchJira")
	if !ok {
		t.Fatal("the daemon's own observation must be claimable")
	}
	if outcome != "error" || class != mcp.FailureAuth {
		t.Fatalf("got outcome=%q class=%q", outcome, class)
	}
}

// An observation is consumed once. A second audit row for the same tool
// belongs to a different call and must not inherit the first one's class.
func TestClaimConsumes(t *testing.T) {
	b := newToolOutcomeBuffer()
	b.Record("exec-1", "searchJira", nil)

	if _, _, ok := b.Claim("exec-1", "searchJira"); !ok {
		t.Fatal("first claim should succeed")
	}
	if _, _, ok := b.Claim("exec-1", "searchJira"); ok {
		t.Fatal("an observation must be claimable exactly once")
	}
}

// Rows arrive in call order, so claims are served oldest-first.
func TestClaimsAreOldestFirst(t *testing.T) {
	b := newToolOutcomeBuffer()
	b.Record("exec-1", "searchJira", nil)
	b.Record("exec-1", "searchJira", &mcp.CallError{Class: mcp.FailureAuth, Status: 401})

	if outcome, _, _ := b.Claim("exec-1", "searchJira"); outcome != "ok" {
		t.Fatalf("first claim should be the first call (ok), got %q", outcome)
	}
	if _, class, _ := b.Claim("exec-1", "searchJira"); class != mcp.FailureAuth {
		t.Fatalf("second claim should be the auth failure, got %q", class)
	}
}

// A container-local tool the daemon never executed has no observation. Saying
// so is the point: the daemon must not claim an authority it does not have.
func TestUnobservedToolIsNotClaimable(t *testing.T) {
	b := newToolOutcomeBuffer()
	if _, _, ok := b.Claim("exec-1", "file_read"); ok {
		t.Fatal("a tool the daemon never executed must have no observation")
	}
}

// An unclassified error stays unclassified. Guessing a class from message text
// is the lossiness this design removes, not relocates.
func TestUnclassifiedErrorGetsNoClass(t *testing.T) {
	b := newToolOutcomeBuffer()
	b.Record("exec-1", "t", errors.New("something went wrong: 401 unauthorized"))

	outcome, class, ok := b.Claim("exec-1", "t")
	if !ok {
		t.Fatal("expected an observation")
	}
	if outcome != "error" {
		t.Fatalf("outcome = %q", outcome)
	}
	if class != "" {
		t.Fatalf("a class must never be guessed from message text, got %q", class)
	}
}

// Stale observations are dropped: a row that never arrived must not be stamped
// onto an unrelated later call of the same tool.
func TestStaleObservationsExpire(t *testing.T) {
	now := time.Now()
	b := newToolOutcomeBuffer()
	b.now = func() time.Time { return now }
	b.Record("exec-1", "searchJira", &mcp.CallError{Class: mcp.FailureAuth})

	now = now.Add(defaultToolOutcomeMaxAge + time.Second)
	if _, _, ok := b.Claim("exec-1", "searchJira"); ok {
		t.Fatal("an observation older than maxAge must not be claimable")
	}
}

func TestBufferIsBounded(t *testing.T) {
	b := newToolOutcomeBuffer()
	for i := 0; i < defaultToolOutcomeMaxPerKey*3; i++ {
		b.Record("exec-1", "t", nil)
	}
	b.mu.Lock()
	n := len(b.entries[toolOutcomeKey{executionID: "exec-1", toolName: "t"}])
	b.mu.Unlock()
	if n > defaultToolOutcomeMaxPerKey {
		t.Fatalf("buffer grew unbounded: %d entries", n)
	}
}

// TestStaleKeysAreSweptNotLeaked — the leak the 2026-09-03 audit found.
//
// pruneLocked only runs on a Record or Claim touching the SAME key, so an
// observation whose matching agent audit row never arrives (agent crash
// mid-step, audit POST failure, task killed between the MCP call and the audit
// flush) was never revisited: correctness was unaffected — a stale entry is
// age-discarded at Claim time — but the map grew one key per such tool per
// execution, unbounded in key count, on a daemon that starts an execution
// every ~20 minutes.
//
// Eviction is AGE-driven rather than terminal-driven: the buffer's own maxAge
// already defines when an observation is dead (5 minutes; an agent posts its
// audit row within milliseconds), and the API layer does not observe execution
// terminality. The sweep rides Record, so a daemon that has stopped making
// tool calls has also stopped growing and needs no goroutine.
func TestStaleKeysAreSweptNotLeaked(t *testing.T) {
	now := time.Now()
	b := newToolOutcomeBuffer()
	b.now = func() time.Time { return now }

	// Two executions whose audit rows never arrive.
	b.Record("exec-gone-1", "a", nil)
	b.Record("exec-gone-2", "b", nil)

	// Long enough that every observation above is past maxAge…
	now = now.Add(defaultToolOutcomeMaxAge + toolOutcomeSweepInterval + time.Second)
	// …and one unrelated call arrives, which is the only event the buffer
	// gets. It must take the rest of the map with it.
	b.Record("exec-live", "c", nil)

	b.mu.Lock()
	keys := len(b.entries)
	b.mu.Unlock()
	if keys != 1 {
		t.Errorf("stale keys leaked: %d entries remain, want 1 (only the live one)", keys)
	}
	if _, _, ok := b.Claim("exec-live", "c"); !ok {
		t.Error("the sweep must not drop a live observation")
	}
}

// The sweep must not walk the map on every Record — that would make a hot path
// O(n) in the size of the map for no benefit, since nothing can expire faster
// than maxAge. The consequence, stated because it is a real property rather
// than an accident: an entry can outlive its maxAge by up to one sweep
// interval before its key is freed. It is unclaimable throughout — Claim
// discards by age — so the delay costs a map entry, not correctness.
func TestSweepIsRateLimited(t *testing.T) {
	now := time.Now()
	b := newToolOutcomeBuffer()
	b.now = func() time.Time { return now }

	b.Record("exec-1", "a", nil) // first Record sweeps (nothing to sweep yet)
	b.mu.Lock()
	first := b.lastSweep
	b.mu.Unlock()

	now = now.Add(time.Second) // well inside the interval
	b.Record("exec-2", "b", nil)

	b.mu.Lock()
	second := b.lastSweep
	keys := len(b.entries)
	b.mu.Unlock()

	if !second.Equal(first) {
		t.Errorf("the map was walked again %v after the last sweep; interval is %v",
			second.Sub(first), toolOutcomeSweepInterval)
	}
	if keys != 2 {
		t.Errorf("entries = %d, want both (neither is expired)", keys)
	}
}

// An operator-driven call from the console has no execution to correlate
// against; filing it under an empty key would make it unclaimable anyway.
func TestNoExecutionIsDropped(t *testing.T) {
	b := newToolOutcomeBuffer()
	b.Record("", "t", nil)
	b.mu.Lock()
	n := len(b.entries)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("an observation with no execution must not be stored, got %d keys", n)
	}
}
