package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/safepath"
)

// ErrClientNotReading marks a stdio client whose response reader has exited:
// the subprocess died, closed stdout, or overran the scanner buffer. The
// connection is unrecoverable, but the SERVER usually is not — it is a
// subprocess the daemon can simply start again.
//
// It is a sentinel so the manager can tell "this connection is gone, re-dial
// it" apart from every other tool-call failure, which must NOT trigger a
// re-dial: a bad argument, a vendor 4xx, or a timeout all mean the connection
// is fine and re-dialling would just kill a working server.
//
// Before this existed the dead flag was terminal for the daemon's lifetime.
// Every later call failed in ~7µs against a pipe with nothing on the other end,
// and the only cure was a daemon restart — observed 2026-08-05, when three
// google-workspace drive_search calls failed instantly and the assistant
// concluded it had misclicked its own tool.
var ErrClientNotReading = errors.New("mcp client is no longer reading responses")

// Client is a connection to a single MCP server.
type Client struct {
	config ServerConfig
	logger zerolog.Logger

	// stdio transport
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner

	// sse transport
	httpClient *http.Client

	// streamable-http transport: the server-assigned session id, captured
	// from the Mcp-Session-Id response header on initialize and echoed on
	// every subsequent request. Empty until assigned (some servers omit it).
	sessionID atomic.Value // string

	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[int64]chan stdioResult
	reqID     atomic.Int64
	tools     []Tool // filtered by config.AllowedTools when that's non-empty

	// waitOnce serialises cmd.Wait() across the reaper goroutine and
	// any Close caller. The Wait contract is "call exactly once" —
	// double-calling on Go ≤1.19 returned a non-ErrProcessDone error
	// the second time. Even on newer Go, the error path of the
	// loser race is unspecified. sync.Once collapses both paths to
	// a single call and stores the result for the loser.
	waitOnce sync.Once
	waitErr  error

	// dead flips to true when the stdio response reader exits — for
	// any reason: ErrTooLong on an oversized response line, the
	// subprocess closing stdout, or a permanent read error. Once
	// set, callStdio short-circuits with the recorded reason instead
	// of writing into a pipe whose reader is gone, which would
	// otherwise hang every future tool call until its context
	// deadline. Close() does not need to consult this — it kills
	// the process and waits regardless.
	dead       atomic.Bool
	deadReason atomic.Value // stores error

	// allowedSet caches config.AllowedTools as a set for O(1) lookups.
	// nil means "no allowlist — everything is allowed".
	allowedSet map[string]struct{}

	// toolLimiter enforces per-tool token-bucket throttles for
	// outgoing CallTool requests (rate-limit hardening sub-item 3).
	// nil when the daemon didn't configure any limits — every call
	// passes through with zero overhead. Populated in Connect from
	// cfg.ToolRateLimits.
	toolLimiter *ToolRateLimiter
}

type stdioResult struct {
	result json.RawMessage
	err    error
}

// Connect creates and initializes an MCP client for the given server config.
// For stdio transport, it starts the subprocess. For SSE and
// streamable-http, it validates the URL.
func Connect(ctx context.Context, cfg ServerConfig, logger zerolog.Logger) (*Client, error) {
	// Per-request HTTP timeout for SSE / streamable-http. Default 30s;
	// a server may raise it (cfg.TimeoutSeconds) when its tools run long
	// (e.g. scraper web_fetch against slow targets).
	httpTimeout := 30 * time.Second
	if cfg.TimeoutSeconds > 0 {
		httpTimeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: httpTimeout}
	}
	c := &Client{
		config:     cfg,
		logger:     logger,
		httpClient: httpClient,
		pending:    make(map[int64]chan stdioResult),
	}

	switch cfg.Transport {
	case "stdio":
		if err := validateLauncher(cfg.Command); err != nil {
			return nil, fmt.Errorf("mcp server %s: %w", cfg.Name, err)
		}
		if err := c.startStdio(); err != nil {
			return nil, fmt.Errorf("mcp stdio start failed for %s: %w", cfg.Name, err)
		}
	case "sse":
		if cfg.URL == "" {
			return nil, fmt.Errorf("mcp sse server %s: url is required", cfg.Name)
		}
	case "streamable-http":
		if cfg.URL == "" {
			return nil, fmt.Errorf("mcp streamable-http server %s: url is required", cfg.Name)
		}
	default:
		return nil, fmt.Errorf("mcp server %s: unsupported transport %q (use stdio, sse, or streamable-http)", cfg.Name, cfg.Transport)
	}

	// Initialize the MCP session.
	if err := c.initialize(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("mcp initialize failed for %s: %w", cfg.Name, err)
	}

	// Discover available tools.
	tools, err := c.listTools(ctx)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("mcp tools/list failed for %s: %w", cfg.Name, err)
	}

	// Mutating-tool gate (Workspace LLD §10.2). A third-party server's tool
	// set is theirs to change, so an expose-everything config that was written
	// against a read-only catalogue must not silently acquire a destructive
	// tool. Refuses only where the change would actually widen what an agent
	// can reach; with an allowlist present the filter below already prevents
	// that, and we warn instead. Runs BEFORE the filter so the decision is
	// made against what the server really advertises.
	if err := c.applyMutatingToolGate(cfg, tools); err != nil {
		_ = c.Close()
		return nil, err
	}

	// Apply the allowlist filter, if any. Doing this at the Client layer
	// means everything downstream (Manager.Tools, agent-side mcp-bridge,
	// the dispatcher's tool catalog) sees only the allowed set, without
	// needing to know the allowlist exists.
	if len(cfg.AllowedTools) > 0 {
		c.allowedSet = allowedToolSet(cfg.AllowedTools)
		filtered := make([]Tool, 0, len(tools))
		skipped := make([]string, 0)
		for _, t := range tools {
			if _, ok := c.allowedSet[t.Name]; ok {
				filtered = append(filtered, t)
			} else {
				skipped = append(skipped, t.Name)
			}
		}
		tools = filtered
		c.logger.Info().
			Str("server", cfg.Name).
			Int("allowed", len(filtered)).
			Strs("filtered_out", skipped).
			Msg("mcp: applied allowed_tools filter")
	}

	c.tools = tools
	// Per-tool throttle (rate-limit hardening sub-item 3). Constructor
	// returns nil when no enabled spec exists, so Client carries
	// zero overhead for projects that haven't opted in.
	c.toolLimiter = NewToolRateLimiter(cfg.ToolRateLimits)
	c.logger.Info().Str("server", cfg.Name).Int("tools", len(tools)).Msg("mcp server connected")

	return c, nil
}

// Tools returns the discovered tools, post-allowlist filter.
func (c *Client) Tools() []Tool {
	return c.tools
}

// toolAllowed returns true when name is in the allowlist, or when no
// allowlist is configured. Callers use this to gate CallTool so a
// model-hallucinated tool name can't escape the allowlist by bypassing
// the catalog we advertised.
func (c *Client) toolAllowed(name string) bool {
	if c.allowedSet == nil {
		return true
	}
	_, ok := c.allowedSet[name]
	return ok
}

// Name returns the server name.
func (c *Client) Name() string {
	return c.config.Name
}

// CallTool invokes a tool on this MCP server. Returns an error without
// making the RPC when the tool name isn't in the allowlist — the model
// may hallucinate a tool name outside our advertised catalog, and we do
// not want that to reach the server where a broader OAuth scope might
// let it succeed.
//
// Throttle gate (rate-limit hardening sub-item 3): before the JSON-RPC
// write the client consults its per-tool token bucket. A drained
// bucket returns a *ToolRateLimitError (which agents recognise via
// the rate_limit_error prefix and the embedded Retry-After) without
// ever touching the upstream server — the whole point of the
// in-daemon ceiling is to absorb the misbehaviour ourselves.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*ToolResult, error) {
	if !c.toolAllowed(name) {
		return nil, fmt.Errorf("tool %q is not in allowed_tools for server %q", name, c.config.Name)
	}
	if blocked, retryAfter := c.toolLimiter.Allow(c.config.Name, name); blocked {
		ObserveToolRateLimited(c.config.ProjectID, c.config.Name, name)
		c.logger.Warn().
			Str("server", c.config.Name).
			Str("tool", name).
			Dur("retry_after", retryAfter).
			Msg("mcp: per-tool rate limit reached")
		return nil, &ToolRateLimitError{
			Server:     c.config.Name,
			Tool:       name,
			RetryAfter: retryAfter,
		}
	}
	params := toolCallParams{Name: name, Arguments: args}
	resp, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return nil, err
	}

	var result ToolResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse tools/call result: %w", err)
	}
	return &result, nil
}

// Close shuts down the connection.
//
// Stdio teardown kills the whole PROCESS GROUP, not just the direct
// child. Launchers like tsx (pagedrop) and npx fork a grandchild that
// inherits our stdio pipes; killing only the wrapper left that
// grandchild alive holding the stderr write end, so cmd.Wait() blocked
// on pipe EOF indefinitely — the 2026-07-15 pagedrop registry hang
// (refreshOne wedged in Close, /ui/mcp badge stuck at "not yet
// refreshed", one orphaned node process leaked per refresh attempt).
// startStdio puts the child in its own process group (Setpgid) so the
// group kill reaches every descendant, and sets cmd.WaitDelay as a
// backstop for descendants that escape the group (setsid).
func (c *Client) Close() error {
	if c.cmd != nil && c.cmd.Process != nil {
		if err := c.killProcessGroup(); err != nil {
			c.logger.Debug().Err(err).Str("server", c.config.Name).Msg("mcp: process kill error")
		}
		if err := c.waitForSubprocess(); err != nil {
			c.logger.Debug().Err(err).Str("server", c.config.Name).Msg("mcp: wait error on close")
		}
	}
	return nil
}

// killProcessGroup SIGKILLs the stdio subprocess's process group
// (startStdio ran it with Setpgid, so pgid == child pid), falling back
// to a single-process kill when the group signal fails (e.g. the group
// is already gone, or the process was started without Setpgid).
func (c *Client) killProcessGroup() error {
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	// Refuse degenerate PIDs: negating Pid 0 signals the DAEMON'S OWN
	// process group (kill(0, SIGKILL) = self-DoS) and a released
	// process's Pid -1 would negate to kill(1). Neither can follow a
	// successful Start, but the blast radius of getting this wrong is
	// the whole daemon — guard unconditionally.
	if c.cmd.Process.Pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return c.cmd.Process.Kill()
	}
	return nil
}

// waitForSubprocess collapses concurrent cmd.Wait() callers to a
// single underlying call. The reaper goroutine launched in
// startStdio AND any Close() caller both used to invoke Wait
// unsynchronised — a documented race that returned an unspecified
// non-ErrProcessDone error to whichever goroutine lost. With
// sync.Once the actual Wait runs once; the loser sees the same
// error the winner stored.
func (c *Client) waitForSubprocess() error {
	if c.cmd == nil {
		return nil
	}
	c.waitOnce.Do(func() {
		c.waitErr = c.cmd.Wait()
	})
	return c.waitErr
}

// expandSafe expands ${VAR} and $VAR references in s, but refuses to
// substitute variables prefixed with VORNIK_ — those hold daemon secrets
// (database password, API keys) that must not leak into subprocess args
// or environment. Unknown variables expand to the empty string.
func expandSafe(s string) string {
	return os.Expand(s, func(name string) string {
		if strings.HasPrefix(name, "VORNIK_") {
			return ""
		}
		return os.Getenv(name)
	})
}

func baseMCPEnv() []string {
	allow := []string{
		"PATH",
		"HOME",
		"USER",
		"LOGNAME",
		"TMPDIR",
		"TEMP",
		"TMP",
		"LANG",
		"LC_ALL",
		"SSL_CERT_FILE",
		"SSL_CERT_DIR",
	}
	env := make([]string, 0, len(allow))
	for _, key := range allow {
		if val, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+val)
		}
	}
	return env
}

// --- stdio transport ---

// buildStdioEnv composes the environment for a stdio MCP subprocess.
//
// Three layers, in order, because later entries win in exec's env:
//  1. baseMCPEnv() — the minimal inherited environment.
//  2. cfg.Env — the legacy operator map, ${VAR}-expanded through a restricted
//     mapper that refuses to leak the daemon's own secrets (anything prefixed
//     VORNIK_). Callers needing a project-scoped variable name it something else.
//  3. cfg.AuthEnv — credentials resolved from the server's `auth: {mode: env}`
//     block, passed VERBATIM. No expansion: these are literal secret values,
//     and expanding one would mangle any credential containing '$' (base64 and
//     JWT-ish values routinely do) or, worse, substitute another variable's
//     value into it.
func buildStdioEnv(cfg ServerConfig) []string {
	env := baseMCPEnv()
	for k, v := range cfg.Env {
		env = append(env, k+"="+expandSafe(v))
	}
	for k, v := range cfg.AuthEnv {
		env = append(env, k+"="+v)
	}
	return env
}

// collidingAuthEnvKeys returns the variable names an `auth: {mode: env}` block
// overwrites in the legacy Env map. Auth winning is the intent, but it must not
// be silent — the header path Warns on the same collision, and an operator
// debugging "my Env value isn't reaching the server" deserves the same clue
// (review-20260804-350e finding 4). Kept separate from buildStdioEnv so that
// stays a pure function.
func collidingAuthEnvKeys(cfg ServerConfig) []string {
	if len(cfg.AuthEnv) == 0 || len(cfg.Env) == 0 {
		return nil
	}
	var out []string
	for k := range cfg.AuthEnv {
		if _, ok := cfg.Env[k]; ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// startStdio launches the subprocess. It deliberately takes NO context: see
// the exec.Command note below — the caller's ctx bounds the connect handshake
// and must not bound the process, so a ctx parameter here would be an
// invitation to wire it into exec.CommandContext and reintroduce the 2026-07-15
// regression.
func (c *Client) startStdio() error {
	env := buildStdioEnv(c.config)
	if collisions := collidingAuthEnvKeys(c.config); len(collisions) > 0 {
		c.logger.Warn().
			Str("server", c.config.Name).
			Strs("vars", collisions).
			Msg("mcp: auth credentials overwriting env values of the same name")
	}

	args := make([]string, len(c.config.Args))
	for i, a := range c.config.Args {
		args[i] = expandSafe(a)
	}

	// Deliberately NOT exec.CommandContext: the caller's ctx bounds
	// the CONNECT handshake, but the subprocess must live as long as the
	// client — project clients are connected under a bounded startup ctx
	// and then held for hours. Binding the process to that ctx killed
	// every long-lived stdio client ~30s after boot (2026-07-15 deploy;
	// the pre-group-kill code had the same wiring and "worked" only
	// because tsx's orphaned grandchild kept serving the inherited pipes
	// after the wrapper died). Teardown is exclusively Close()'s job.
	c.cmd = exec.Command(c.config.Command, args...)
	c.cmd.Env = env
	// Own process group + bounded Wait: Close()'s group kill reaches
	// every descendant the launcher forks (tsx, npx — see the Close()
	// doc comment), and WaitDelay releases cmd.Wait (with ErrWaitDelay)
	// if some descendant that escaped the group still holds the stderr
	// pipe open after the process itself is gone.
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.cmd.WaitDelay = 10 * time.Second
	// Pin cwd to "/" so the MCP subprocess can't read files via
	// relative paths against the daemon's working directory. The
	// daemon may be running from a config tree that contains
	// secrets (e.g. vornik.yaml with DB credentials); inheriting
	// that CWD would let a poorly-written MCP server expose them
	// through a relative-path bug. Per audit recommendation.
	c.cmd.Dir = "/"
	c.cmd.Stderr = &logWriter{logger: c.logger, server: c.config.Name}

	var err error
	c.stdin, err = c.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdoutPipe, err := c.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	c.stdout = bufio.NewScanner(stdoutPipe)
	c.stdout.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024) // 10MB line buffer

	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("start command %q: %w", c.config.Command, err)
	}

	go c.readStdioResponses()

	// Reaper goroutine: some MCP servers (notably `npx -y <pkg>`) fork a
	// node process and then exit the wrapper, which would leave the
	// wrapper <defunct> until someone calls Wait. This goroutine does
	// that and logs the exit. When Close() runs first, Kill()+Wait()
	// happen there and this goroutine returns via ErrProcessDone.
	go c.reapSubprocess()

	return nil
}

// reapSubprocess calls cmd.Wait() so the kernel can fully release the
// child when it exits. Without this, a long-lived daemon accumulates
// <defunct> process entries for each MCP server subprocess it spawned.
// No-op when cmd is nil. Routes through waitForSubprocess so the
// reaper and any concurrent Close() share one Wait call.
func (c *Client) reapSubprocess() {
	if c.cmd == nil {
		return
	}
	if err := c.waitForSubprocess(); err != nil {
		// ProcessDone is the expected signal when Close() already
		// called Wait. Other errors (non-zero exit, signal) are
		// worth logging but not fatal.
		c.logger.Debug().
			Err(err).
			Str("server", c.config.Name).
			Msg("mcp: subprocess exited")
		return
	}
	c.logger.Debug().
		Str("server", c.config.Name).
		Msg("mcp: subprocess exited cleanly")
}

func (c *Client) initialize(ctx context.Context) error {
	params := initializeParams{
		ProtocolVersion: "2024-11-05",
		ClientInfo: clientInfo{
			Name:    "vornik",
			Version: "1.0.0",
		},
	}
	_, err := c.call(ctx, "initialize", params)
	if err != nil {
		return err
	}
	// Send initialized notification (no response expected).
	_ = c.notify("notifications/initialized", nil)
	return nil
}

func (c *Client) listTools(ctx context.Context) ([]Tool, error) {
	resp, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}

	var result toolsListResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse tools/list: %w", err)
	}
	return result.Tools, nil
}

// call sends a JSON-RPC request and waits for the response.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	switch c.config.Transport {
	case "stdio":
		return c.callStdio(ctx, method, params)
	case "sse":
		return c.callSSE(ctx, method, params)
	case "streamable-http":
		return c.callStreamableHTTP(ctx, method, params)
	default:
		return nil, fmt.Errorf("unsupported transport: %s", c.config.Transport)
	}
}

func (c *Client) callStdio(ctx context.Context, method string, params any) (json.RawMessage, error) {
	// Fail fast when the response reader has exited — without this,
	// callStdio would write the request to stdin (which is still open
	// since we never close it on reader exit) and then block on respCh
	// forever, since nobody is left to deliver responses. Every
	// subsequent call to a wedged client would burn its full per-call
	// timeout before the dispatcher noticed the failure.
	if c.dead.Load() {
		if reason, ok := c.deadReason.Load().(error); ok && reason != nil {
			return nil, fmt.Errorf("%w: mcp client %s: %w", ErrClientNotReading, c.config.Name, reason)
		}
		return nil, fmt.Errorf("%w: mcp client %s", ErrClientNotReading, c.config.Name)
	}
	id := c.reqID.Add(1)
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	data = append(data, '\n')

	respCh := make(chan stdioResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = respCh
	c.pendingMu.Unlock()

	c.writeMu.Lock()
	_, err = c.stdin.Write(data)
	c.writeMu.Unlock()
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("write to stdin: %w", err)
	}

	select {
	case resp := <-respCh:
		return resp.result, resp.err
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	}
}

// notify sends a JSON-RPC notification (no id, no response expected).
func (c *Client) notify(method string, params any) error {
	switch c.config.Transport {
	case "stdio":
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		req := jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params}
		data, err := json.Marshal(req)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		_, err = c.stdin.Write(data)
		return err
	case "streamable-http":
		// JSON-RPC notification: jsonrpc + method (+ params), NO id.
		notif := map[string]any{"jsonrpc": "2.0", "method": method}
		if params != nil {
			notif["params"] = params
		}
		data, err := json.Marshal(notif)
		if err != nil {
			return err
		}
		// Detached context: a lifecycle notification must not be cancelled
		// with the caller's request scope mid-handshake.
		ctx := context.Background()
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.URL, bytes.NewReader(data))
		if err != nil {
			return err
		}
		c.setStreamableHeaders(ctx, httpReq)
		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			// Fire-and-forget: a missing ack must not fail Connect.
			c.logger.Debug().Str("server", c.config.Name).Err(err).Msg("mcp: streamable-http notify failed")
			return nil
		}
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			c.sessionID.Store(sid)
		}
		_ = resp.Body.Close()
		return nil
	default: // sse and any other transport: notifications are a no-op
		return nil
	}
}

func (c *Client) readStdioResponses() {
	for c.stdout.Scan() {
		line := append([]byte(nil), c.stdout.Bytes()...)
		if len(line) == 0 {
			continue
		}

		var resp jsonRPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// Could be a notification or a non-JSON log line.
			continue
		}

		c.pendingMu.Lock()
		respCh, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.pendingMu.Unlock()
		if !ok {
			continue
		}

		result := stdioResult{result: resp.Result}
		if resp.Error != nil {
			result.err = resp.Error
		}
		respCh <- result
		close(respCh)
	}

	err := c.stdout.Err()
	if err == nil {
		err = fmt.Errorf("mcp server %s closed stdout before responding", c.config.Name)
	}
	// Wrap with the sentinel so the calls ALREADY BLOCKED here get the same
	// re-dial treatment as ones arriving later. Without this the in-flight
	// callers — the ones that noticed first — would be the only ones not healed.
	drainErr := fmt.Errorf("%w: %w", ErrClientNotReading, err)

	// Mark the client dead BEFORE draining pending — so any concurrent
	// callStdio that registers between the drain and now sees the flag
	// and returns early instead of attaching a respCh that nobody will
	// ever read from. Once set, all future calls short-circuit. The
	// most common trigger here is bufio.Scanner returning ErrTooLong
	// on a >10 MiB single-line response from a buggy MCP server, but
	// any reader exit (subprocess crash, pipe close) lands in the
	// same code path and benefits from the same fast-fail.
	c.deadReason.Store(err)
	c.dead.Store(true)
	if errors.Is(err, bufio.ErrTooLong) {
		c.logger.Error().
			Str("server", c.config.Name).
			Msg("mcp: stdout response exceeded 10 MiB scanner buffer; client wedged, future calls will fail fast")
	}

	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, respCh := range c.pending {
		delete(c.pending, id)
		respCh <- stdioResult{err: drainErr}
		close(respCh)
	}
}

func (c *Client) callSSE(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.reqID.Add(1)
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// SSE transport: POST to the server URL with JSON-RPC body.
	url := strings.TrimSuffix(c.config.URL, "/sse") + "/message"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(data)))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setBaseHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	// Per-server fixed headers (set by the daemon when wiring
	// each project's clients — e.g. X-Project-ID and
	// X-Project-Caps for the broker MCP). Loop instead of
	// MaybeSetHeaders so a nil/empty map is a no-op.
	c.applyConfigHeaders(httpReq)
	// Per-call request-scoped headers forwarded from the agent
	// container via context. The daemon's tool-call HTTP handler
	// extracts X-Task-ID / X-Execution-ID from the agent's MCP
	// bridge request and stashes them on ctx; we forward them
	// here so the broker MCP can attribute each place_order /
	// cancel_order to the originating task in trading_orders.
	// Empty values are a no-op (legacy harnesses, operator-driven
	// tool calls, etc.) — header stays unset, broker writes NULL.
	if v, ok := ctx.Value(TaskIDHeaderKey{}).(string); ok && v != "" {
		httpReq.Header.Set("X-Task-ID", v)
	}
	if v, ok := ctx.Value(ExecutionIDHeaderKey{}).(string); ok && v != "" {
		httpReq.Header.Set("X-Execution-ID", v)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sse request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap MCP SSE responses. Tool outputs can be large (log tails, big file
	// reads) but never gigabytes; 32 MiB is generous and prevents OOM on a
	// buggy or hostile server.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		// Rate-limit hardening sub-item 8 step 3: when an upstream MCP
		// server returns 429, surface the Retry-After hint so the agent
		// (which already pattern-matches on rate_limit_error from the
		// in-daemon throttle) can back off precisely instead of guessing.
		// Header form wins per RFC 7231; the SSE surface today doesn't
		// publish a body-embedded form so we don't add a parser here.
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := parseRetryAfterHeaderForMCP(resp.Header.Get("Retry-After"))
			if retryAfter <= 0 {
				retryAfter = time.Second
			}
			return nil, &ToolRateLimitError{
				Server:     c.config.Name,
				Tool:       method,
				RetryAfter: retryAfter,
			}
		}
		// Log the verbatim upstream body for debugging, but do NOT embed it
		// in the returned error: this error propagates up to the external
		// /api/v1/.../mcp/tools/call response, and the body is an untrusted
		// third-party MCP server's bytes (arbitrary size + content). Return
		// a fixed-shape, status-only error to the caller.
		logBody := string(body)
		if len(logBody) > 2048 {
			logBody = logBody[:2048] + "…"
		}
		c.logger.Warn().
			Str("server", c.config.Name).
			Str("tool", method).
			Int("status", resp.StatusCode).
			Str("body", logBody).
			Msg("mcp: sse server returned error status")
		return nil, fmt.Errorf("sse server returned %d", resp.StatusCode)
	}

	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}
	return rpcResp.Result, nil
}

// reservedMCPHeaders are protocol-owned request headers an operator's
// per-server YAML `headers` map must never override: doing so would let
// a misconfigured (or malicious) config hijack the session id or break
// content negotiation. Compared case-insensitively (HTTP header names
// are case-insensitive, and http.Header canonicalises on Set).
var reservedMCPHeaders = map[string]struct{}{
	"content-type":         {},
	"accept":               {},
	"mcp-protocol-version": {},
	"mcp-session-id":       {},
}

// applyConfigHeaders sets the per-server configured headers (Bearer auth,
// X-Project-ID, etc.) on the request, skipping any key that collides with
// a protocol-owned header (reservedMCPHeaders). A collision is logged once
// per call at Warn so the operator sees the dropped override rather than a
// silent session-id hijack.
func (c *Client) applyConfigHeaders(httpReq *http.Request) {
	for k, v := range c.config.Headers {
		if _, reserved := reservedMCPHeaders[strings.ToLower(strings.TrimSpace(k))]; reserved {
			c.logger.Warn().
				Str("server", c.config.Name).
				Str("header", k).
				Msg("mcp: ignoring configured header that collides with a protocol-owned header")
			continue
		}
		httpReq.Header.Set(k, v)
	}
	c.applyAuthHeaders(httpReq)
}

// applyAuthHeaders applies credentials resolved from the server's `auth:` block
// LAST, so an auth-managed credential deterministically wins over an
// operator-set header of the same name (mcp-server-authentication-design.md §5).
// The reserved-header guard still applies: config validation rejects a
// protocol-owned header name up front, and this is the second gate for a block
// that reached the client without it.
//
// Never logs a header VALUE — these are real credentials, and a Warn line is
// exactly the kind of place one leaks from.
func (c *Client) applyAuthHeaders(httpReq *http.Request) {
	for k, v := range c.config.AuthHeaders {
		if _, reserved := reservedMCPHeaders[strings.ToLower(strings.TrimSpace(k))]; reserved {
			c.logger.Warn().
				Str("server", c.config.Name).
				Str("header", k).
				Msg("mcp: ignoring auth header that collides with a protocol-owned header")
			continue
		}
		if existing := httpReq.Header.Get(k); existing != "" {
			c.logger.Warn().
				Str("server", c.config.Name).
				Str("header", k).
				Msg("mcp: auth credential overwriting a configured header of the same name")
		}
		httpReq.Header.Set(k, v)
	}
}

// setStreamableHeaders applies the headers common to every streamable-http
// request: content negotiation, protocol version, the session id (when
// assigned), the per-server configured headers (Bearer auth etc.), and the
// ctx-forwarded task/execution attribution headers.
func (c *Client) setStreamableHeaders(ctx context.Context, httpReq *http.Request) {
	c.setBaseHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("MCP-Protocol-Version", "2024-11-05")
	if sid, _ := c.sessionID.Load().(string); sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}
	c.applyConfigHeaders(httpReq)
	if v, ok := ctx.Value(TaskIDHeaderKey{}).(string); ok && v != "" {
		httpReq.Header.Set("X-Task-ID", v)
	}
	if v, ok := ctx.Value(ExecutionIDHeaderKey{}).(string); ok && v != "" {
		httpReq.Header.Set("X-Execution-ID", v)
	}
}

// mcpHTTPStatusError maps a >=400 streamable-http response to an error,
// mirroring callSSE's model: 429 -> ToolRateLimitError (Retry-After), any
// other status -> a fixed-shape status-only error (the untrusted upstream
// body is logged truncated, never propagated to the caller).
func (c *Client) mcpHTTPStatusError(resp *http.Response, body []byte, method string) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := parseRetryAfterHeaderForMCP(resp.Header.Get("Retry-After"))
		if retryAfter <= 0 {
			retryAfter = time.Second
		}
		return &ToolRateLimitError{Server: c.config.Name, Tool: method, RetryAfter: retryAfter}
	}
	logBody := string(body)
	if len(logBody) > 2048 {
		logBody = logBody[:2048] + "…"
	}
	c.logger.Warn().Str("server", c.config.Name).Str("tool", method).
		Int("status", resp.StatusCode).Str("body", logBody).
		Msg("mcp: streamable-http server returned error status")
	return fmt.Errorf("streamable-http server returned %d", resp.StatusCode)
}

// callStreamableHTTP POSTs a JSON-RPC request to the single MCP endpoint and
// returns the matching response, handling both application/json and
// text/event-stream replies (MCP Streamable HTTP, request/response only).
func (c *Client) callStreamableHTTP(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.reqID.Add(1)
	data, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.URL, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setStreamableHeaders(ctx, httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("streamable-http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID.Store(sid)
	}

	limited := io.LimitReader(resp.Body, 32*1024*1024)

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(limited)
		return nil, c.mcpHTTPStatusError(resp, body, method)
	}

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		rpcResp, err := readSSEJSONRPCResponse(limited, id)
		if err != nil {
			return nil, err
		}
		if rpcResp.Error != nil {
			return nil, rpcResp.Error
		}
		return rpcResp.Result, nil
	}

	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}
	return rpcResp.Result, nil
}

// logWriter forwards MCP server stderr to the zerolog logger.
type logWriter struct {
	logger zerolog.Logger
	server string
}

func (w *logWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		w.logger.Debug().Str("server", w.server).Msg(msg)
	}
	return len(p), nil
}

func validateLauncher(cmd string) error {
	if cmd == "" {
		return fmt.Errorf("command is empty")
	}

	// 1. Allowlist of well-known safe launchers.
	allowedLaunchers := map[string]bool{
		"uvx":     true,
		"npx":     true,
		"python3": true,
		"python":  true,
		"node":    true,
		"go":      true,
		"bun":     true,
		"deno":    true,
	}
	if allowedLaunchers[cmd] {
		return nil
	}

	// 2. Allow absolute paths to standard system locations. Clean and
	// resolve symlinks before the prefix check so "/usr/bin/../../tmp/x"
	// or an allowed-dir symlink cannot escape the trusted launcher roots.
	allowedDirs := []string{
		"/usr/bin/",
		"/usr/local/bin/",
		"/bin/",
	}

	// Also allow ~/.local/bin if we can resolve the home dir.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		allowedDirs = append(allowedDirs, filepath.Join(home, ".local", "bin")+string(filepath.Separator))
	}

	candidate := filepath.Clean(cmd)
	if !filepath.IsAbs(candidate) {
		return fmt.Errorf("command %q is not in the allowlist of safe launchers (uvx, npx, python3, etc.) or standard system paths (/usr/bin, etc.)", cmd)
	}
	// safepath.AssertUnder resolves symlinks in both the allowed dir and the
	// candidate's existing prefix, so "/usr/bin/../../tmp/x" or an allowed-dir
	// symlink cannot escape the trusted launcher roots. Accept on the first
	// root that contains the candidate.
	for _, dir := range allowedDirs {
		if err := safepath.AssertUnder(dir, candidate); err == nil {
			return nil
		}
	}

	return fmt.Errorf("command %q is not in the allowlist of safe launchers (uvx, npx, python3, etc.) or standard system paths (/usr/bin, etc.)", cmd)
}

// applyMutatingToolGate enforces the Workspace LLD §10.2 gate and, regardless of
// whether the server opted in, tells the operator when the upstream tool surface
// has grown a mutating tool they did not declare.
//
// The warning is unconditional because it is pure signal: the allowlist already
// keeps the tool away from agents, but the CREDENTIAL may still permit what that
// tool would do, and a changed upstream surface is worth knowing about on any
// server — not only the ones strict enough to refuse.
func (c *Client) applyMutatingToolGate(cfg ServerConfig, tools []Tool) error {
	allowed := allowedToolSet(cfg.AllowedTools)
	if err := gateAdvertisedTools(cfg.Name, cfg.RequireDeclaredTools, allowed, tools); err != nil {
		return err
	}
	if overrides := AnnotationOverrides(tools); len(overrides) > 0 {
		// The server's own annotation is believed (it knows its semantics), but a
		// third party reclassifying a destructive-looking tool as safe should
		// leave a trace rather than pass silently.
		c.logger.Info().
			Str("server", cfg.Name).
			Strs("annotation_overrides", overrides).
			Msg("mcp: server annotations reclassified tools against the name heuristic")
	}
	// Host filesystem reach is reported separately from "mutating", and at WARN
	// even when the tool is filtered out, because it is the one hazard an operator
	// cannot infer from anything else they can see. Google's Workspace server
	// advertises five such tools (`drive_downloadFile`, `gmail_downloadAttachment`,
	// `slides_getImages`, `slides_getSlideThumbnail`, `gmail_createDraft`); four
	// begin with a read verb, none is annotated, and the subprocess runs as the
	// daemon user. Exposing one is file access as the daemon, not as the agent.
	if hostPaths := HostFilesystemTools(tools); len(hostPaths) > 0 {
		event := c.logger.Warn()
		if exposed := HostFilesystemTools(allowedSubset(allowed, tools)); len(exposed) > 0 {
			event = event.Strs("exposed_to_agents", exposed)
		}
		event.
			Str("server", cfg.Name).
			Strs("host_filesystem_tools", hostPaths).
			Msg("mcp: server advertises tools that read or write paths on THIS host, as the " +
				"daemon's OS user — keep them out of allowed_tools unless you intend that")
	}
	if undeclared := undeclaredMutatingTools(allowed, tools); len(undeclared) > 0 {
		c.logger.Warn().
			Str("server", cfg.Name).
			Strs("undeclared_mutating_tools", undeclared).
			Msg("mcp: server advertises mutating tools not in allowed_tools — filtered out, " +
				"but the upstream tool surface has changed; review whether the credential's " +
				"scopes still match what you intend to permit")
	}
	return nil
}
