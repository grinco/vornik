package mcp

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for the 2026-07-15 pagedrop registry-badge incident:
// the pagedrop stdio launcher (node → tsx) forks a grandchild that
// inherits the client's stderr pipe. Close() killed only the direct
// child, then blocked in cmd.Wait() waiting for stderr EOF that never
// came (the orphaned grandchild kept the write end open). Consequences:
// Registry.refreshOne never published its successful connect (the /ui/mcp
// badge stayed "not yet refreshed"), RefreshAll's WaitGroup never
// finished, the refresh-dedup entry blocked all future refreshes, and one
// orphaned node process leaked per refresh attempt.
//
// fakeForkingMCPServer is a minimal stdio MCP server (python3 is on the
// launcher allowlist) that reproduces the tsx behaviour: it answers
// initialize + tools/list, spawns a `sleep` grandchild that inherits
// stderr, and reports that grandchild's PID as the tool description so
// tests can assert the whole process group died.
const fakeForkingMCPServer = `
import sys, json, subprocess
gc = subprocess.Popen(["sleep", "300"], stdin=subprocess.DEVNULL,
                      stdout=subprocess.DEVNULL, stderr=sys.stderr)
for line in sys.stdin:
    try:
        req = json.loads(line)
    except ValueError:
        continue
    rid = req.get("id")
    if rid is None:
        continue
    m = req.get("method")
    if m == "initialize":
        r = {"protocolVersion": "2024-11-05", "capabilities": {},
             "serverInfo": {"name": "fake", "version": "0"}}
    elif m == "tools/list":
        r = {"tools": [{"name": "fake_tool", "description": str(gc.pid),
                        "inputSchema": {"type": "object"}}]}
    else:
        r = {}
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": rid, "result": r}) + "\n")
    sys.stdout.flush()
`

func forkingServerConfig(name string) ServerConfig {
	return ServerConfig{
		Name:      name,
		Transport: "stdio",
		Command:   "python3",
		Args:      []string{"-c", fakeForkingMCPServer},
	}
}

func requirePython3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
}

// processAlive reports whether pid still exists (signal 0 probe).
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestClientClose_GrandchildHoldsStderr_CloseUnblocksAndKillsGroup(t *testing.T) {
	requirePython3(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := Connect(ctx, forkingServerConfig("forking"), zerolog.Nop())
	require.NoError(t, err)
	tools := client.Tools()
	require.Len(t, tools, 1)
	grandchildPID, err := strconv.Atoi(tools[0].Description)
	require.NoError(t, err, "fake server reports its grandchild pid as the tool description")
	require.True(t, processAlive(grandchildPID), "grandchild must be running while connected")

	done := make(chan struct{})
	go func() {
		_ = client.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("Close blocked >8s waiting on the grandchild-held stderr pipe (2026-07-15 pagedrop registry hang)")
	}

	// The whole process group must die with the client — otherwise every
	// refresh leaks one orphaned server process (4 were found leaked in
	// production on 2026-07-15).
	assert.Eventually(t, func() bool { return !processAlive(grandchildPID) },
		5*time.Second, 100*time.Millisecond,
		"grandchild %d must be killed with the process group on Close", grandchildPID)
}

func TestRegistryRefreshAll_ForkingStdioServer_PublishesReachable(t *testing.T) {
	requirePython3(t)

	reg := NewRegistry([]ServerConfig{forkingServerConfig("forking")}, 0, zerolog.Nop())

	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		reg.RefreshAll(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("RefreshAll blocked >20s — refreshOne wedged in Close (2026-07-15 pagedrop registry hang)")
	}

	snap := reg.Snapshot(context.Background())
	require.Len(t, snap, 1)
	assert.True(t, snap[0].Reachable,
		"a successful connect must be published to the catalog even though the server forks a pipe-holding grandchild")
	assert.Empty(t, snap[0].Error)
	require.Len(t, snap[0].Tools, 1)
	assert.Equal(t, "fake_tool", snap[0].Tools[0].Name)
}

// killProcessGroup must refuse degenerate PIDs: -0 targets the daemon's
// OWN process group and -(-1) targets pgid 1. The test passing at all is
// half the assertion — a missing guard would SIGKILL the test process.
func TestKillProcessGroup_DegeneratePIDsAreRefused(t *testing.T) {
	for _, pid := range []int{0, -1} {
		c := &Client{cmd: &exec.Cmd{Process: &os.Process{Pid: pid}}}
		assert.NoError(t, c.killProcessGroup(), "pid %d must be refused, not signalled", pid)
	}
	assert.NoError(t, (&Client{}).killProcessGroup(), "nil cmd is a no-op")
}

// A connected stdio client must SURVIVE cancellation of the context it
// was connected with. Connect's ctx bounds the handshake — it must not
// double as the subprocess's lifetime: the daemon connects project
// clients under a bounded startup ctx and keeps them for hours.
// Discovered deploying the group-kill fix on 2026-07-15: with
// cmd.Cancel = killProcessGroup wired to the connect ctx, every project
// pagedrop client died ~30s after boot ("mcp server pagedrop closed
// stdout before responding"). The pre-fix code had the same wiring with
// a weaker kill (direct child only) — pagedrop survived it only because
// tsx's orphaned grandchild kept serving the inherited pipes by accident.
func TestClientStdio_SurvivesConnectContextCancel(t *testing.T) {
	requirePython3(t)

	ctx, cancel := context.WithCancel(context.Background())
	client, err := Connect(ctx, forkingServerConfig("longlived"), zerolog.Nop())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	cancel() // startup/handshake ctx expires — the client must live on
	time.Sleep(500 * time.Millisecond)

	callCtx, callCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer callCancel()
	_, err = client.listTools(callCtx)
	require.NoError(t, err, "stdio subprocess must outlive the connect ctx; teardown is Close()'s job")
}
