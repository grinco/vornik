package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestDrainGracePeriod_Default confirms the silent default when the
// env var isn't set. Production deploys want a short grace by default
// so k8s rolling restarts don't drag.
func TestDrainGracePeriod_Default(t *testing.T) {
	t.Setenv("VORNIK_DRAIN_GRACE_SECONDS", "")
	got := drainGracePeriod()
	if got != 5*time.Second {
		t.Errorf("default drain = %v, want 5s", got)
	}
}

// TestDrainGracePeriod_ExplicitZero gives single-node deploys + unit
// tests a way to skip the wait entirely. The handler shouldn't sleep
// when the operator asks for 0.
func TestDrainGracePeriod_ExplicitZero(t *testing.T) {
	t.Setenv("VORNIK_DRAIN_GRACE_SECONDS", "0")
	if got := drainGracePeriod(); got != 0 {
		t.Errorf("zero override → %v, want 0", got)
	}
}

// TestDrainGracePeriod_HonoursValidInteger: operator-supplied integer
// becomes a Duration of that many seconds.
func TestDrainGracePeriod_HonoursValidInteger(t *testing.T) {
	t.Setenv("VORNIK_DRAIN_GRACE_SECONDS", "12")
	if got := drainGracePeriod(); got != 12*time.Second {
		t.Errorf("got %v, want 12s", got)
	}
}

// TestDrainGracePeriod_CapsAt30s prevents a typo (or an operator
// who confuses seconds with milliseconds) from blowing through
// systemd's TimeoutStopSec=90s and forcing a SIGKILL.
func TestDrainGracePeriod_CapsAt30s(t *testing.T) {
	t.Setenv("VORNIK_DRAIN_GRACE_SECONDS", "600")
	if got := drainGracePeriod(); got != 30*time.Second {
		t.Errorf("got %v, want capped 30s", got)
	}
}

// TestDrainGracePeriod_GarbageFallsBackToDefault: parsing failure
// shouldn't drop the daemon to 0s drain — that would silently regress
// the graceful-shutdown behaviour for misconfigured deploys.
func TestDrainGracePeriod_GarbageFallsBackToDefault(t *testing.T) {
	t.Setenv("VORNIK_DRAIN_GRACE_SECONDS", "abc")
	if got := drainGracePeriod(); got != 5*time.Second {
		t.Errorf("garbage → %v, want default 5s", got)
	}
}

// TestDrainGracePeriod_NegativeTreatedAsZero: a negative number is
// nonsensical but clearly intentional ("disable drain"). Treat as 0
// rather than absolute-valuing — silent abs() would surprise.
func TestDrainGracePeriod_NegativeTreatedAsZero(t *testing.T) {
	t.Setenv("VORNIK_DRAIN_GRACE_SECONDS", "-5")
	if got := drainGracePeriod(); got != 0 {
		t.Errorf("negative → %v, want 0", got)
	}
}

// ---- shutdown ordering: executor before HTTP server ----

// spyExecutorShutdown records whether Shutdown was called and at what
// sequence position. Satisfies the executorShutdowner interface.
type spyExecutorShutdown struct {
	mu      sync.Mutex
	called  bool
	callSeq int // order index, filled from a shared counter
	counter *int
}

func (s *spyExecutorShutdown) Shutdown(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called = true
	*s.counter++
	s.callSeq = *s.counter
	return nil
}

// spyHTTPServer records when Close/Shutdown are called.
type spyHTTPServer struct {
	mu      sync.Mutex
	seq     int
	counter *int
}

func (s *spyHTTPServer) Shutdown(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.counter++
	s.seq = *s.counter
	return nil
}
func (s *spyHTTPServer) Close() error { return nil }

// TestShutdown_ExecutorQuiesceBeforeHTTPUnlinksSocket — restart-induced
// in-flight FAILED, 2026-06-21, Signature A: Container.shutdown() was
// closing the HTTP server (which unlinks the unix agent socket) BEFORE
// calling Executor.Shutdown, so in-flight podman-run bind-mounts found
// the socket deleted and errored "statfs …/vornik.sock: no such file or
// directory". Fix-1 reorders the phases so the executor quiesces first.
//
// The test injects spy implementations via the testExecShutdown hook
// (executorShutdowner) and testHTTPShutdown (httpShutdowner) to record
// call order and assert the executor runs BEFORE HTTP teardown.
func TestShutdown_ExecutorQuiesceBeforeHTTPUnlinksSocket(t *testing.T) {
	// Skip the drain grace period so the test is instant.
	t.Setenv("VORNIK_DRAIN_GRACE_SECONDS", "0")

	counter := 0
	execSpy := &spyExecutorShutdown{counter: &counter}
	httpSpy := &spyHTTPServer{counter: &counter}

	// Build a minimal Container with only the fields shutdown() touches.
	// Inject both spies: testExecShutdown for the executor phase and
	// testHTTPShutdown for the HTTP phase. All other fields are nil
	// (shutdown is nil-guarded throughout).
	c := &Container{
		Logger:           zerolog.Nop(),
		testExecShutdown: execSpy,
		testHTTPShutdown: httpSpy,
	}

	_ = c.shutdown()

	if !execSpy.called {
		t.Error("executor Shutdown was not called during Container.shutdown() — Fix-1 ordering invariant broken")
	}
	if httpSpy.seq == 0 {
		t.Error("HTTP Shutdown was not called during Container.shutdown()")
	}
	// Core ordering assertion: the executor must quiesce (Phase 2) BEFORE
	// the HTTP server is torn down (Phase 3) so the agent socket stays
	// open for any in-flight podman-run bind-mounts.
	if execSpy.callSeq >= httpSpy.seq {
		t.Errorf("ordering violated: executor Shutdown (seq %d) must precede HTTP Shutdown (seq %d)",
			execSpy.callSeq, httpSpy.seq)
	}
}

// TestShutdown_ExecutorCalledEvenWhenHTTPServerNil ensures the executor
// pause still runs on a minimal Container where HTTPServer is nil (e.g.
// worker-only nodes that never opened an HTTP listener).
func TestShutdown_ExecutorCalledEvenWhenHTTPServerNil(t *testing.T) {
	t.Setenv("VORNIK_DRAIN_GRACE_SECONDS", "0")
	counter := 0
	execSpy := &spyExecutorShutdown{counter: &counter}
	c := &Container{
		Logger:           zerolog.Nop(),
		testExecShutdown: execSpy,
		HTTPServer:       nil,
	}
	_ = c.shutdown()
	if !execSpy.called {
		t.Error("executor Shutdown must be called even when HTTPServer is nil")
	}
}
