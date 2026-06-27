package mcp

import (
	"bufio"
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clientLifecycleSleeper starts a long-lived child process the test can
// kill via Client.Close. It bypasses Connect (and therefore the MCP
// handshake) so the test can exercise the subprocess lifecycle —
// startStdio's pipe wiring, the reaper goroutine, waitForSubprocess's
// once-only semantics, and Close's kill+wait — without standing up a
// real MCP server. Returns nil when no suitable launcher is on the box.
func clientLifecycleSleeper(t *testing.T) *Client {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep binary available: %v", err)
	}
	c := &Client{
		config: ServerConfig{Name: "sleeper", Transport: "stdio"},
		logger: zerolog.Nop(),
	}
	c.cmd = exec.Command(sleep, "30")
	stdin, err := c.cmd.StdinPipe()
	require.NoError(t, err)
	c.stdin = stdin
	stdoutPipe, err := c.cmd.StdoutPipe()
	require.NoError(t, err)
	c.stdout = bufio.NewScanner(stdoutPipe)
	require.NoError(t, c.cmd.Start())
	return c
}

// TestClientClose_KillsSubprocessAndIsIdempotent verifies Close kills the
// child and that calling it twice does not error or double-Wait (the
// sync.Once contract in waitForSubprocess collapses both callers to one
// underlying cmd.Wait).
func TestClientClose_KillsSubprocessAndIsIdempotent(t *testing.T) {
	c := clientLifecycleSleeper(t)

	require.NoError(t, c.Close(), "first Close should succeed")
	// The process must actually be reaped — a second Close is a no-op that
	// must not panic or return a fresh Wait error.
	require.NoError(t, c.Close(), "second Close must be idempotent")

	// waitForSubprocess called any number of additional times returns the
	// stored result, never a new (racy) Wait error.
	first := c.waitForSubprocess()
	second := c.waitForSubprocess()
	assert.Equal(t, first, second, "waitForSubprocess must memoise its result")
}

// TestWaitForSubprocess_NilCmdIsNoOp covers the nil-cmd guard (SSE /
// streamable-http clients never spawn a process).
func TestWaitForSubprocess_NilCmdIsNoOp(t *testing.T) {
	c := &Client{config: ServerConfig{Name: "http", Transport: "sse"}, logger: zerolog.Nop()}
	require.NoError(t, c.waitForSubprocess())
	require.NoError(t, c.Close(), "Close on a transport with no subprocess is a no-op")
}

// TestReapSubprocess_NilCmdReturns covers reapSubprocess's nil-cmd guard.
func TestReapSubprocess_NilCmdReturns(t *testing.T) {
	c := &Client{config: ServerConfig{Name: "http", Transport: "sse"}, logger: zerolog.Nop()}
	c.reapSubprocess() // must return immediately, not panic.
}

// TestReapAndCloseRace_ShareOneWait runs the reaper goroutine concurrently
// with Close, the exact production scenario waitForSubprocess's sync.Once
// guards: the reaper and the Close caller both reach cmd.Wait, and only one
// underlying call may run. With -race this asserts there is no data race
// and neither caller observes a spurious error.
func TestReapAndCloseRace_ShareOneWait(t *testing.T) {
	c := clientLifecycleSleeper(t)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.reapSubprocess() // races with Close below.
	}()

	// Give the reaper a beat to enter Wait, then kill+wait from Close.
	require.NoError(t, c.Close())
	wg.Wait()
}

// TestStartStdio_RejectsBadStdinPipe is a thin smoke test that startStdio
// wires the command + cwd pin + env without error for an allowed launcher,
// then leaves the process killable. We stop short of initialize (which
// needs a real MCP peer) and just confirm the transport plumbing succeeds
// and the child is reaped cleanly on Close.
func TestStartStdio_WiresTransportAndCwd(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("no sleep binary: %v", err)
	}
	c := &Client{
		config: ServerConfig{
			Name:      "sleeper",
			Transport: "stdio",
			Command:   "sleep",
			Args:      []string{"30"},
			Env:       map[string]string{"FOO": "$BAR_UNSET"},
		},
		logger:  zerolog.Nop(),
		pending: make(map[int64]chan stdioResult),
	}
	// Allow the bare "sleep" launcher to resolve via PATH for exec.Command.
	require.NoError(t, c.startStdio(context.Background()))
	require.NotNil(t, c.cmd)
	assert.Equal(t, "/", c.cmd.Dir, "subprocess cwd must be pinned to / per the audit")
	require.NotNil(t, c.stdin)
	require.NotNil(t, c.stdout)

	// Tear down: Close must kill+reap the child without error.
	require.NoError(t, c.Close())
	// Bound the test so a leaked child can't hang the suite.
	done := make(chan struct{})
	go func() { _ = c.waitForSubprocess(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("subprocess not reaped after Close")
	}
}
