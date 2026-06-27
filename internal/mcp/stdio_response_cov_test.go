package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadStdioResponses_DeliversMatchingResponse exercises the stdio
// happy path: the reader unmarshals a framed JSON-RPC response, finds the
// matching pending channel by id, and delivers the result so the blocked
// callStdio returns. This is the core request/response correlation that
// the dead-flag tests deliberately don't cover.
func TestReadStdioResponses_DeliversMatchingResponse(t *testing.T) {
	stdinReader, stdinWriter := io.Pipe()
	defer func() { _ = stdinWriter.Close() }()

	stdoutReader, stdoutWriter := io.Pipe()
	defer func() { _ = stdoutWriter.Close() }()

	c := &Client{
		config:  ServerConfig{Name: "echo", Transport: "stdio"},
		logger:  zerolog.Nop(),
		stdin:   stdinWriter,
		stdout:  bufio.NewScanner(stdoutReader),
		pending: make(map[int64]chan stdioResult),
	}
	go c.readStdioResponses()

	// Fake server: read each request frame from stdin and write back a
	// JSON-RPC reply with the SAME id, plus an unrelated notification
	// (id 0) and a non-JSON log line — both must be skipped by the reader.
	go func() {
		sc := bufio.NewScanner(stdinReader)
		for sc.Scan() {
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			_ = json.Unmarshal(sc.Bytes(), &req)
			_, _ = io.WriteString(stdoutWriter, "this is a plain log line, not JSON\n")
			_, _ = io.WriteString(stdoutWriter, `{"jsonrpc":"2.0","method":"notifications/progress"}`+"\n")
			reply, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []map[string]any{{"name": "ping"}}},
			})
			_, _ = stdoutWriter.Write(append(reply, '\n'))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := c.callStdio(ctx, "tools/list", nil)
	require.NoError(t, err)

	var res toolsListResult
	require.NoError(t, json.Unmarshal(raw, &res))
	require.Len(t, res.Tools, 1)
	assert.Equal(t, "ping", res.Tools[0].Name)
}

// TestReadStdioResponses_PropagatesJSONRPCError verifies that an error
// object in the framed response is delivered as the call's error.
func TestReadStdioResponses_PropagatesJSONRPCError(t *testing.T) {
	stdinReader, stdinWriter := io.Pipe()
	defer func() { _ = stdinWriter.Close() }()
	stdoutReader, stdoutWriter := io.Pipe()
	defer func() { _ = stdoutWriter.Close() }()

	c := &Client{
		config:  ServerConfig{Name: "echo", Transport: "stdio"},
		logger:  zerolog.Nop(),
		stdin:   stdinWriter,
		stdout:  bufio.NewScanner(stdoutReader),
		pending: make(map[int64]chan stdioResult),
	}
	go c.readStdioResponses()

	go func() {
		sc := bufio.NewScanner(stdinReader)
		for sc.Scan() {
			var req struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(sc.Bytes(), &req)
			reply, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
			_, _ = stdoutWriter.Write(append(reply, '\n'))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.callStdio(ctx, "tools/call", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "method not found")
}

// TestReadStdioResponses_DrainsPendingOnReaderExit confirms that when the
// stdout stream closes with a request still in flight, the reader's
// post-loop drain delivers the close error to the waiting caller (so it
// doesn't block until its context deadline). This is the in-flight
// counterpart to the dead-flag fast-fail.
func TestReadStdioResponses_DrainsPendingOnReaderExit(t *testing.T) {
	stdinReader, stdinWriter := io.Pipe()
	defer func() { _ = stdinReader.Close() }()
	defer func() { _ = stdinWriter.Close() }()
	go func() { _, _ = io.Copy(io.Discard, stdinReader) }()

	stdoutReader, stdoutWriter := io.Pipe()

	c := &Client{
		config:  ServerConfig{Name: "echo", Transport: "stdio"},
		logger:  zerolog.Nop(),
		stdin:   stdinWriter,
		stdout:  bufio.NewScanner(stdoutReader),
		pending: make(map[int64]chan stdioResult),
	}
	go c.readStdioResponses()

	// Register an in-flight call, then close stdout before any reply.
	errCh := make(chan error, 1)
	go func() {
		_, err := c.callStdio(context.Background(), "tools/list", nil)
		errCh <- err
	}()

	// Give callStdio a moment to register its pending channel, then close.
	time.Sleep(20 * time.Millisecond)
	_ = stdoutWriter.Close()

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "closed stdout")
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight call was not drained on reader exit")
	}
}

// TestStartForProject_SkipsFailedDialAndReplacesExisting drives the
// connectFn seam to cover StartForProject's partial-success branch (a
// failed dial is logged and skipped) and the replace-existing-client
// branch (a second successful dial for the same server name closes the
// previous client).
func TestStartForProject_SkipsFailedDialAndReplacesExisting(t *testing.T) {
	orig := connectFn
	defer func() { connectFn = orig }()

	var attempt int
	connectFn = func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		attempt++
		if cfg.Name == "broken" {
			return nil, errors.New("dial refused")
		}
		return newFakeClient(cfg, []Tool{{Name: "ping"}}), nil
	}

	mgr := NewManager(zerolog.Nop())

	// Empty project ID is ignored (guard branch).
	mgr.StartForProject(context.Background(), "", []ServerConfig{{Name: "x"}})
	require.Equal(t, 0, mgr.ServerCount())

	// One good + one broken server: partial success keeps the good one.
	mgr.StartForProject(context.Background(), "p1", []ServerConfig{
		{Name: "ok"},
		{Name: "broken"},
	})
	require.Equal(t, 1, mgr.ServerCount(), "broken dial must be skipped, ok must connect")
	require.Equal(t, 1, mgr.ProjectCount())

	// Re-dialling the same server name replaces (and closes) the prior
	// client without leaking a second map entry.
	mgr.StartForProject(context.Background(), "p1", []ServerConfig{{Name: "ok"}})
	require.Equal(t, 1, mgr.ServerCount(), "re-dial must replace, not duplicate")
}
