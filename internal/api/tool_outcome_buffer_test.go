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

func TestForgetDropsAnExecution(t *testing.T) {
	b := newToolOutcomeBuffer()
	b.Record("exec-1", "a", nil)
	b.Record("exec-1", "b", nil)
	b.Record("exec-2", "a", nil)

	b.Forget("exec-1")

	if _, _, ok := b.Claim("exec-1", "a"); ok {
		t.Error("exec-1 observations should be gone")
	}
	if _, _, ok := b.Claim("exec-2", "a"); !ok {
		t.Error("Forget must not touch another execution")
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
