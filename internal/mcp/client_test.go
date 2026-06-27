package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientCallStdioRespectsContextCancellation(t *testing.T) {
	stdinReader, stdinWriter := io.Pipe()
	defer func() { _ = stdinReader.Close() }()
	defer func() { _ = stdinWriter.Close() }()
	go func() {
		_, _ = io.Copy(io.Discard, stdinReader)
	}()

	stdoutReader, stdoutWriter := io.Pipe()
	defer func() { _ = stdoutWriter.Close() }()

	client := &Client{
		config:  ServerConfig{Name: "test", Transport: "stdio"},
		logger:  zerolog.Nop(),
		stdin:   stdinWriter,
		stdout:  bufio.NewScanner(stdoutReader),
		pending: make(map[int64]chan stdioResult),
	}
	go client.readStdioResponses()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.callStdio(ctx, "tools/list", nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestClient_StdioReader_DeadFlagShortCircuits verifies that once the
// stdout reader exits (here simulated by closing the pipe), subsequent
// callStdio invocations fail fast with a meaningful error rather than
// hanging until their context deadline. Without the dead-flag fix the
// scanner could exit on bufio.ErrTooLong (a >10 MiB single-line
// response from a buggy MCP server) and every future call to that
// client would silently wait out its full timeout, wedging the
// dispatcher.
func TestClient_StdioReader_DeadFlagShortCircuits(t *testing.T) {
	stdinReader, stdinWriter := io.Pipe()
	defer func() { _ = stdinReader.Close() }()
	defer func() { _ = stdinWriter.Close() }()
	go func() { _, _ = io.Copy(io.Discard, stdinReader) }()

	stdoutReader, stdoutWriter := io.Pipe()

	client := &Client{
		config:  ServerConfig{Name: "test", Transport: "stdio"},
		logger:  zerolog.Nop(),
		stdin:   stdinWriter,
		stdout:  bufio.NewScanner(stdoutReader),
		pending: make(map[int64]chan stdioResult),
	}
	readerDone := make(chan struct{})
	go func() {
		client.readStdioResponses()
		close(readerDone)
	}()

	// Close stdout to make the reader exit cleanly. In production an
	// ErrTooLong from the scanner buffer cap would land in the same
	// post-loop cleanup path.
	_ = stdoutWriter.Close()

	select {
	case <-readerDone:
	case <-time.After(time.Second):
		t.Fatal("reader did not exit after stdout close")
	}

	// Now the client should refuse new calls immediately. The
	// pre-fix behavior was to hang on the response channel until ctx
	// expired; the test fails (times out) without the dead-flag.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.callStdio(ctx, "tools/list", nil)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.NotErrorIs(t, err, context.DeadlineExceeded, "should fail fast, not wait for ctx timeout")
	assert.Less(t, elapsed, 25*time.Millisecond, "callStdio must short-circuit, not block")
	assert.Contains(t, err.Error(), "no longer reading responses")
}

func TestValidateLauncherRejectsAllowedPrefixTraversal(t *testing.T) {
	require.Error(t, validateLauncher("/usr/bin/../../tmp/evil"))
	require.Error(t, validateLauncher("/usr/local/bin/../../../tmp/evil"))
}

// TestClient_AllowedTools_FiltersCatalog locks down the allowlist semantics
// that shrink the advertised tool catalog for a project. The gmail MCP
// server advertises 19 tools but only 6 work under a gmail.readonly +
// gmail.compose token — the remaining 13 fail at the Google API but still
// bloat the LLM tool catalog enough to push BAG past its silent-timeout
// threshold. Filtering at the Client layer ensures every downstream
// consumer (Manager.Tools, the dispatcher, the agent-side bridge) sees
// only the allowed set without each having to know the allowlist exists.
func TestClient_AllowedTools_FiltersCatalog(t *testing.T) {
	allTools := []Tool{
		{Name: "search_emails"},
		{Name: "read_email"},
		{Name: "send_email"},
		{Name: "delete_email"},
		{Name: "download_attachment"},
	}

	t.Run("no allowlist — all tools exposed", func(t *testing.T) {
		c := &Client{config: ServerConfig{Name: "gmail"}, logger: zerolog.Nop()}
		applyAllowlistForTest(c, allTools)
		require.Len(t, c.Tools(), 5)
		require.True(t, c.toolAllowed("send_email"))
	})

	t.Run("allowlist reduces catalog and gates calls", func(t *testing.T) {
		c := &Client{
			config: ServerConfig{
				Name:         "gmail",
				AllowedTools: []string{"search_emails", "read_email", "download_attachment"},
			},
			logger: zerolog.Nop(),
		}
		applyAllowlistForTest(c, allTools)

		require.ElementsMatch(t,
			[]string{"search_emails", "read_email", "download_attachment"},
			toolNames(c.Tools()),
		)

		// Gating: toolAllowed is what CallTool consults before sending
		// an RPC. A hallucinated tool name cannot escape the allowlist
		// even if the server would have accepted it.
		require.True(t, c.toolAllowed("search_emails"))
		require.False(t, c.toolAllowed("send_email"))
		require.False(t, c.toolAllowed("delete_email"))
	})

	t.Run("stale allowlist entries are silently ignored", func(t *testing.T) {
		// Server tool sets change between versions; an allowlist entry
		// that no longer matches any advertised tool should not crash
		// the connection or leak into Tools(). It's just a no-op.
		c := &Client{
			config: ServerConfig{
				Name:         "gmail",
				AllowedTools: []string{"search_emails", "does_not_exist"},
			},
			logger: zerolog.Nop(),
		}
		applyAllowlistForTest(c, allTools)

		require.Len(t, c.Tools(), 1)
		require.Equal(t, "search_emails", c.Tools()[0].Name)
	})
}

// applyAllowlistForTest mirrors the filter logic Connect() applies so
// tests can exercise Client.Tools() and Client.toolAllowed() without
// standing up a real MCP transport.
func applyAllowlistForTest(c *Client, discovered []Tool) {
	if len(c.config.AllowedTools) == 0 {
		c.tools = discovered
		return
	}
	c.allowedSet = make(map[string]struct{}, len(c.config.AllowedTools))
	for _, n := range c.config.AllowedTools {
		c.allowedSet[n] = struct{}{}
	}
	for _, t := range discovered {
		if _, ok := c.allowedSet[t.Name]; ok {
			c.tools = append(c.tools, t)
		}
	}
}

// TestClient_SSE_AttachesConfiguredHeaders pins the SSE-transport
// header injection that the daemon uses to scope per-project
// state at MCP servers (X-Project-ID for the broker, the broker's
// per-call cap overlay in X-Project-Caps). The Client populates
// every JSON-RPC HTTP request with c.config.Headers so the daemon
// doesn't have to thread a per-call argument through every tool
// signature — server side just reads what it needs from the
// request envelope.
func TestClient_SSE_AttachesConfiguredHeaders(t *testing.T) {
	var (
		mu          sync.Mutex
		seenHeaders []http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		// Clone — the request goes away after this handler returns.
		seenHeaders = append(seenHeaders, r.Header.Clone())
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		// Minimal JSON-RPC reply: tools/list returns empty,
		// initialize returns the protocol bits the client expects.
		_, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]any{"tools": []any{}},
		})
	}))
	defer srv.Close()

	cfg := ServerConfig{
		Name:      "broker-fake",
		Transport: "sse",
		URL:       srv.URL,
		Headers: map[string]string{
			"X-Project-ID":   "ibkr-trader",
			"X-Project-Caps": `{"max_position_usd":1000}`,
		},
	}
	c, err := Connect(context.Background(), cfg, zerolog.Nop())
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, seenHeaders, "no requests reached the test server")
	for i, h := range seenHeaders {
		assert.Equal(t, "ibkr-trader", h.Get("X-Project-ID"), "request %d missing X-Project-ID", i)
		assert.Equal(t, `{"max_position_usd":1000}`, h.Get("X-Project-Caps"), "request %d missing X-Project-Caps", i)
		assert.Equal(t, "application/json", h.Get("Content-Type"), "Content-Type still applied alongside extras")
	}
}

func toolNames(ts []Tool) []string {
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.Name
	}
	return names
}
