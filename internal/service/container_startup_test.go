package service

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
)

// ---- startup ordering: agent unix socket before executor recovery ----

// spyAgentSocketServe records when serveAgentUnixSocket is called and at
// what sequence position. Replaces the real socket setup via
// testAgentSocketServe so the test runs without a real filesystem socket.
type spyAgentSocketServe struct {
	mu      sync.Mutex
	called  bool
	callSeq int
	counter *int
}

func (s *spyAgentSocketServe) serve(_ chan error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called = true
	*s.counter++
	s.callSeq = *s.counter
	return nil
}

// spyExecutorRecover records when Recover is called and at what sequence
// position. Satisfies the executorRecoverer interface. Also cancels the
// run context so Run() exits promptly after recovery is recorded.
type spyExecutorRecover struct {
	mu      sync.Mutex
	called  bool
	callSeq int
	counter *int
	cancel  context.CancelFunc // called after recording to let Run() exit
}

func (s *spyExecutorRecover) Recover(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called = true
	*s.counter++
	s.callSeq = *s.counter
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

// TestStartup_AgentSocketServedBeforeExecutorRecover — restart-resume-on-boot
// socket race, 2026-06-21: on daemon restart, Container.Run() called
// Executor.Recover (which resumed paused steps and started podman containers)
// BEFORE the agent unix socket was created. Resumed containers bind-mount the
// socket, so they failed immediately with "statfs …/vornik.sock: no such file
// or directory". Fix-2 moves serveAgentUnixSocket to before Recover.
//
// This test mirrors the shutdown ordering test pattern (see
// TestShutdown_ExecutorQuiesceBeforeHTTPUnlinksSocket): both spies share a
// monotonic counter and record their call position; the assertion verifies
// socket-serve callSeq < executor-recover callSeq.
//
// The test MUST FAIL against the original ordering (socket after recover) and
// PASS after the fix. This was verified manually: reverting the early call
// makes agentSpy.callSeq > recoverSpy.callSeq.
func TestStartup_AgentSocketServedBeforeExecutorRecover(t *testing.T) {
	// Zero drain grace so shutdown() returns without sleeping.
	t.Setenv("VORNIK_DRAIN_GRACE_SECONDS", "0")

	counter := 0
	agentSpy := &spyAgentSocketServe{counter: &counter}
	ctx, cancel := context.WithCancel(context.Background())
	recoverSpy := &spyExecutorRecover{counter: &counter, cancel: cancel}

	// Build a minimal Container with only the fields Run() touches up to
	// and including the Recover call. Config is required for capabilities()
	// and the c.Config.Server.Address log line.
	//
	// With RunWorkers=true (the default "all" profile), Recover runs.
	// testAgentSocketServe bypasses real socket creation (no filesystem
	// side effects); testExecutorRecover bypasses the real executor and
	// cancels the context so Run reaches its select{} quickly.
	// testHTTPShutdown bypasses the real HTTP server teardown so
	// shutdown() returns without a graceful-drain wait.
	//
	// HTTPServer is still needed because Run() calls c.HTTPServer.Addr
	// inside a goroutine and c.HTTPServer.ListenAndServe() in a goroutine
	// (both guarded only by nil checks on Observability/Scheduler, not
	// HTTPServer). Using a real *http.Server on :0 is fine; the goroutine
	// exits via ErrServerClosed when shutdown() closes it.
	httpSpy := &spyHTTPServer{counter: &counter}
	srv := &http.Server{Addr: "127.0.0.1:0"}
	c := &Container{
		Logger:               zerolog.Nop(),
		Config:               &config.Config{},
		HTTPServer:           srv,
		testAgentSocketServe: agentSpy.serve,
		testExecutorRecover:  recoverSpy,
		testHTTPShutdown:     httpSpy,
	}

	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Allow up to 5 s for both spies to fire. recoverSpy cancels ctx so
	// Run() will proceed to its select{}, then call beginDrain() (0s) and
	// shutdown(). shutdown() uses testHTTPShutdown (instant) and all other
	// fields are nil-guarded, so it returns quickly.
	select {
	case <-done:
		// Run returned; spies should have fired.
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return within 5s — test setup broken")
	}

	if !agentSpy.called {
		t.Error("serveAgentUnixSocket was not called during Container.Run() — startup ordering invariant broken")
	}
	if !recoverSpy.called {
		t.Error("Executor.Recover was not called during Container.Run() — RunWorkers guard may have fired")
	}
	// Core ordering assertion: the agent unix socket must be served (Phase 0)
	// BEFORE the executor recovers in-flight executions (Phase 1) so that
	// resumed podman containers can bind-mount the socket immediately.
	if agentSpy.callSeq >= recoverSpy.callSeq {
		t.Errorf("ordering violated: serveAgentUnixSocket (seq %d) must precede Executor.Recover (seq %d) — restart-resume-on-boot socket race fix regressed",
			agentSpy.callSeq, recoverSpy.callSeq)
	}
}
