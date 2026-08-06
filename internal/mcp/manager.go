package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
)

// Manager owns MCP server connections scoped per project. Every project
// that declares `mcp.servers` gets its own independent set of Client
// connections — the same server name (e.g. "gmail") in two different
// projects yields two isolated MCP processes with their own credentials
// and their own tool catalogs. That's what lets multiple operators run
// their personal assistants on the same daemon without cross-contamination.
//
// Callers must always pass a projectID. The zero string is treated as a
// lookup miss (returns no tools, rejects execute).
type Manager struct {
	mu      sync.RWMutex
	clients map[string]map[string]*Client // projectID → serverName → client
	logger  zerolog.Logger
	// redialLocks holds one mutex per (projectID, serverName) so a burst of
	// calls against a server that just died re-dials it once, not once per
	// caller. Keyed independently of `clients` because it must outlive the
	// entry it guards (the re-dial replaces that entry).
	redialLocks sync.Map
	// blockNotifier, when set, gets a post-hook on every successful tool
	// result to push an operator Telegram alert for a solvable scraper block
	// on a curated portal. Nil (default) → no notification. See block_notify.go.
	blockNotifier *BlockNotifier
}

// SetBlockNotifier wires the scraper-block → Telegram notify hook. Nil-safe;
// pass nil (or never call) to leave notification off.
func (m *Manager) SetBlockNotifier(bn *BlockNotifier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blockNotifier = bn
}

// NewManager creates an MCP manager. Call StartForProject once per project.
func NewManager(logger zerolog.Logger) *Manager {
	return &Manager{
		clients: make(map[string]map[string]*Client),
		logger:  logger,
	}
}

// connectFn is the dial seam used by StartForProject / SyncProjects —
// overridden in tests to inject fake clients without a real transport.
var connectFn = Connect

// closeFn is the teardown seam for displaced clients — overridden in tests to
// simulate a Close() that blocks (a stdio subprocess that won't reap, an SSE
// stream that won't terminate) without a real transport.
var closeFn = (*Client).Close

// SyncProjects reconciles the manager to exactly the desired
// per-project server sets. It replaces the previous reload pattern of
// Close()-then-StartForProject, which wiped every client and then
// re-dialled with nothing connected in between — so for the duration
// of the reconnects (up to 30s per server) every in-flight and
// incoming Execute/Tools call failed with "not connected", on EVERY
// config reload, for tasks in unrelated projects too (bug-sweep
// follow-up 2026-06-04).
//
// Sequence: dial every desired client with NO lock held, swap the
// whole catalog under one write-lock acquisition (which inherently
// waits for in-flight Execute RLock holders to drain), then close the
// displaced clients. A consumer therefore always observes either the
// complete old catalog or the complete new one — never an empty
// window. Projects absent from desired are dropped; per the existing
// partial-success convention, a server that fails to dial is logged
// and skipped.
func (m *Manager) SyncProjects(ctx context.Context, desired map[string][]ServerConfig) {
	// One dial result per server. client==nil means the dial failed (or was
	// abandoned) — it's simply omitted from the new catalog.
	type dialResult struct {
		projectID string
		name      string
		client    *Client
	}

	fresh := make(map[string]map[string]*Client, len(desired))
	total := 0
	for projectID, servers := range desired {
		if projectID == "" {
			m.logger.Error().Msg("mcp: SyncProjects given empty projectID — ignored")
			continue
		}
		fresh[projectID] = make(map[string]*Client, len(servers))
		total += len(servers)
	}

	// Dial every server CONCURRENTLY, each bounded at 30s. Results flow back on
	// a BUFFERED channel (cap = total) so a straggler's late send never blocks
	// and never touches a shared map — only this function's goroutine writes
	// `fresh`, so there is no data race with the swap below. Collection stops at
	// the first of: every dial reported, or `ctx` (the overall reconnect
	// budget from initMCP) expiring. That second arm is the critical one:
	// SyncProjects runs INSIDE the config-reload activator holding the
	// reloader's reloadMu, so a single dial that hangs past its own 30s ctx
	// (a transport that ignores cancellation) must not stall the whole cycle —
	// it would wedge live config reloads until a restart. We proceed with
	// whatever connected and abandon the straggler. 2026-07-08 watcher-wedge
	// root-cause fix (the initMCP-no-timeout follow-up deferred on 2026-07-06).
	results := make(chan dialResult, total)
	for projectID, servers := range desired {
		if projectID == "" {
			continue
		}
		for _, cfg := range servers {
			go func(projectID string, cfg ServerConfig) {
				connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				client, err := connectFn(connectCtx, cfg, m.logger.With().Str("project", projectID).Logger())
				if err != nil {
					m.logger.Error().
						Err(err).
						Str("project", projectID).
						Str("server", cfg.Name).
						Msg("mcp: failed to connect")
					results <- dialResult{projectID, cfg.Name, nil}
					return
				}
				results <- dialResult{projectID, cfg.Name, client}
			}(projectID, cfg)
		}
	}

	pending := total
collect:
	for pending > 0 {
		select {
		case r := <-results:
			if r.client != nil {
				fresh[r.projectID][r.name] = r.client
			}
			pending--
		case <-ctx.Done():
			m.logger.Warn().
				Int("connected", total-pending).
				Int("total", total).
				Msg("mcp: reconnect budget exceeded; proceeding with connected servers, slow dials abandoned")
			break collect
		}
	}

	m.mu.Lock()
	displaced := m.clients
	m.clients = fresh
	m.mu.Unlock()

	// Close the displaced (old-catalog) clients OUT OF BAND. The new catalog is
	// already live after the swap above, so nothing needs these closes to
	// finish before SyncProjects returns — and Client.Close() CAN block (stdio
	// Close does Process.Kill + cmd.Wait; a subprocess that won't reap, or an
	// SSE stream that won't terminate, hangs it). SyncProjects runs inside the
	// config-reload activator holding the reloader's reloadMu, so a blocking
	// Close here would wedge the whole reload cycle — this, not the connect,
	// was the actual 2026-07-08 activator hang. Detach so a slow Close can't
	// hold up the reload; a pathological Close leaks one goroutine at worst.
	// Pinned on this goroutine for the same reason as in redial: these closes are
	// fire-and-forget, so reading the package seam inside them is an
	// unsynchronised read against any test that restores it.
	closer := closeFn
	for projectID, byServer := range displaced {
		for name, client := range byServer {
			go func(projectID, name string, client *Client) {
				if err := closer(client); err != nil {
					m.logger.Warn().
						Err(err).
						Str("project", projectID).
						Str("server", name).
						Msg("mcp: close displaced client")
				}
			}(projectID, name, client)
		}
	}
}

// StartForProject connects to all MCP servers declared by a single project
// and records the resulting clients under that project's ID. Safe to call
// multiple times for the same projectID — new servers are added; existing
// server names are re-dialled (the old client is closed first).
//
// Servers that fail to connect are logged and skipped (partial success);
// the rest of the project's servers still come up.
func (m *Manager) StartForProject(ctx context.Context, projectID string, servers []ServerConfig) {
	if projectID == "" {
		m.logger.Error().Msg("mcp: StartForProject called with empty projectID — ignored")
		return
	}
	for _, cfg := range servers {
		connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		client, err := connectFn(connectCtx, cfg, m.logger.With().Str("project", projectID).Logger())
		cancel()
		if err != nil {
			m.logger.Error().
				Err(err).
				Str("project", projectID).
				Str("server", cfg.Name).
				Msg("mcp: failed to connect")
			continue
		}
		m.mu.Lock()
		if _, ok := m.clients[projectID]; !ok {
			m.clients[projectID] = make(map[string]*Client)
		}
		// If a client with this server name already exists, close it first.
		// Otherwise the old process would leak while the new one replaces
		// its entry in the map.
		if old, exists := m.clients[projectID][cfg.Name]; exists {
			_ = old.Close()
		}
		m.clients[projectID][cfg.Name] = client
		m.mu.Unlock()
	}
}

// Tools returns the discovered tools for one project, in OpenAI
// function-calling format. Tool names are namespaced as
// mcp__{serverName}__{toolName} — the project is NOT in the qualified
// name because each project has its own independent catalog (which is
// the whole point of per-project scoping).
func (m *Manager) Tools(projectID string) []chat.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	byServer := m.clients[projectID]
	if len(byServer) == 0 {
		return nil
	}
	var tools []chat.Tool
	for serverName, client := range byServer {
		for _, t := range client.Tools() {
			qualifiedName := fmt.Sprintf("mcp__%s__%s", serverName, t.Name)

			params := t.InputSchema
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			} else if stripped, ok := hideDaemonSuppliedArgs(params); ok {
				params = stripped
			}
			tools = append(tools, chat.Tool{
				Type: "function",
				Function: chat.ToolFunction{
					Name:        qualifiedName,
					Description: fmt.Sprintf("[%s] %s", serverName, t.Description),
					Parameters:  params,
				},
			})
		}
	}
	return tools
}

// Execute routes a tool call to the correct MCP server within a project.
// qualifiedName must be in mcp__{serverName}__{toolName} form. The project
// scope is enforced: a call with a server name not present in the given
// project's catalog returns an error even if another project happens to
// have a server with the same name.
func (m *Manager) Execute(ctx context.Context, projectID, qualifiedName, argsJSON string) (string, error) {
	serverName, toolName, ok := parseQualifiedName(qualifiedName)
	if !ok {
		return "", fmt.Errorf("invalid MCP tool name: %s", qualifiedName)
	}

	// The daemon owns the caller's identity, so it supplies it rather than
	// trusting whatever the model put in the arguments.
	argsJSON = injectDaemonSuppliedArgs(argsJSON, projectID)

	start := time.Now()
	result, err := m.callOnce(ctx, projectID, serverName, toolName, argsJSON)

	// A dead stdio connection is the one failure worth retrying, because the
	// server behind it is a subprocess we can simply start again. Everything
	// else (bad argument, vendor 4xx, timeout) means the connection is healthy
	// and re-dialling would kill a working server, so the sentinel check has to
	// stay narrow.
	if errors.Is(err, ErrClientNotReading) {
		m.logger.Warn().
			Err(err).
			Str("project", projectID).
			Str("server", serverName).
			Msg("mcp: connection is dead — re-dialling before failing the call")
		if redialErr := m.redial(projectID, serverName); redialErr != nil {
			m.logger.Error().
				Err(redialErr).
				Str("project", projectID).
				Str("server", serverName).
				Msg("mcp: re-dial failed; the server is down")
			// Say this in terms the MODEL can act on. The raw wording ("no
			// longer reading responses", "closed stdout before responding")
			// reads like a malformed call, and on 2026-08-05 the assistant
			// duly blamed its own arguments for an MCP subprocess that had
			// exited.
			return "", fmt.Errorf("MCP server %q is not running and could not be restarted (%v) — "+
				"this is a server fault, not a problem with the arguments, so retrying "+
				"this call or varying its arguments will not help", serverName, redialErr)
		}
		m.logger.Info().
			Str("project", projectID).
			Str("server", serverName).
			Msg("mcp: re-dial succeeded — retrying the tool call")
		result, err = m.callOnce(ctx, projectID, serverName, toolName, argsJSON)
	}
	duration := time.Since(start)

	if err != nil {
		m.logger.Warn().
			Err(err).
			Str("project", projectID).
			Str("server", serverName).
			Str("tool", toolName).
			Dur("duration", duration).
			Msg("mcp: tool call failed")
		return "", fmt.Errorf("MCP tool %s failed: %w", qualifiedName, err)
	}

	m.logger.Info().
		Str("project", projectID).
		Str("server", serverName).
		Str("tool", toolName).
		Dur("duration", duration).
		Bool("is_error", result.IsError).
		Msg("mcp: tool call completed")

	text := result.Text()
	if result.IsError {
		return fmt.Sprintf("MCP error: %s", text), nil
	}
	// Post-hook: a solvable scraper block on a curated portal fires an
	// operator notification. Nil-safe, non-blocking, all work deferred to a
	// background worker — this must never affect the result just returned.
	m.blockNotifier.MaybeNotify(projectID, toolName, argsJSON, text)
	return text, nil
}

// callOnce resolves the client and makes exactly one tool call.
//
// The read lock is held across the whole CallTool so Close()/StartForProject
// cannot free the client mid-call. Multiple callers still run concurrently
// (RLock is shared); only shutdown and re-dial wait for in-flight calls to
// drain — which is precisely why the re-dial happens BETWEEN two calls to this
// helper rather than inside it. Taking the write lock while holding the read
// lock would deadlock.
func (m *Manager) callOnce(ctx context.Context, projectID, serverName, toolName, argsJSON string) (*ToolResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byServer := m.clients[projectID]
	var client *Client
	if byServer != nil {
		client = byServer[serverName]
	}
	if client == nil {
		return nil, fmt.Errorf("MCP server %q not connected for project %q", serverName, projectID)
	}
	return client.CallTool(ctx, toolName, json.RawMessage(argsJSON))
}

// redial replaces one project's dead client for one server with a fresh
// connection, reusing the config the dead client was built from.
//
// Serialised per (project, server) and double-checked, so a burst of concurrent
// calls against a server that just died produces ONE new subprocess rather than
// one per caller — the failure mode that makes naive reconnect logic worse than
// none. Losers of the race find a live client on the re-check and return
// success without dialling.
//
// The dial deliberately does NOT inherit the caller's context. A tool call may
// be moments from its deadline, and the connection should be healed for
// everyone who comes next even if THIS request gives up; the 30s bound matches
// the other dial sites, so a hung dial cannot leak indefinitely.
func (m *Manager) redial(projectID, serverName string) error {
	key := projectID + "\x00" + serverName
	lockAny, _ := m.redialLocks.LoadOrStore(key, &sync.Mutex{})
	lock, ok := lockAny.(*sync.Mutex)
	if !ok {
		return fmt.Errorf("mcp: internal: bad redial lock type for %q", key)
	}
	lock.Lock()
	defer lock.Unlock()

	m.mu.RLock()
	current := m.clients[projectID][serverName]
	m.mu.RUnlock()
	if current == nil {
		return fmt.Errorf("server %q is no longer configured for project %q", serverName, projectID)
	}
	if !current.dead.Load() {
		// Somebody else already replaced it while we waited for the lock.
		return nil
	}
	cfg := current.config

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fresh, err := connectFn(ctx, cfg, m.logger.With().Str("project", projectID).Logger())
	if err != nil {
		return err
	}

	// Pin the closer HERE, on the calling goroutine, rather than letting the
	// out-of-band goroutines below read the package seam whenever they happen to
	// run. Two reasons, and the second is why this is production code and not a
	// test tweak:
	//
	//  1. Those goroutines are fire-and-forget and outlive this call, so reading
	//     a package-level var inside them is a read with no ordering against
	//     anything. `go test -race` reports it against the seam restore in
	//     swapDialSeams' t.Cleanup (manager_redial_test.go) — which is a real
	//     unsynchronised read, not a test artefact.
	//  2. Semantically the closer that runs should be the one in force when we
	//     DECIDED to close, not whatever is installed by the time the goroutine
	//     is scheduled.
	closer := closeFn

	m.mu.Lock()
	byServer, stillThere := m.clients[projectID]
	if !stillThere {
		// The project was dropped by a reload while we dialled. Don't
		// resurrect it — close what we just built.
		m.mu.Unlock()
		go func() { _ = closer(fresh) }()
		return fmt.Errorf("project %q was removed while re-dialling %q", projectID, serverName)
	}
	displaced := byServer[serverName]
	byServer[serverName] = fresh
	m.mu.Unlock()

	// Out of band: stdio Close does Kill + Wait and can block on a subprocess
	// that will not reap, and this runs on a live tool-call path.
	if displaced != nil {
		go func() { _ = closer(displaced) }()
	}
	return nil
}

// ServerCount returns the total number of connected clients across all
// projects. Used by init logs and readiness checks; not for routing.
func (m *Manager) ServerCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, byServer := range m.clients {
		n += len(byServer)
	}
	return n
}

// ProjectCount returns the number of projects with at least one
// successfully-connected MCP client.
func (m *Manager) ProjectCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, byServer := range m.clients {
		if len(byServer) > 0 {
			n++
		}
	}
	return n
}

// Close shuts down every MCP client across every project.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for projectID, byServer := range m.clients {
		for name, client := range byServer {
			if err := client.Close(); err != nil {
				m.logger.Warn().
					Err(err).
					Str("project", projectID).
					Str("server", name).
					Msg("mcp: close error")
			}
		}
	}
	m.clients = make(map[string]map[string]*Client)
}

// parseQualifiedName splits mcp__{server}__{tool} into server and tool names.
func parseQualifiedName(name string) (server, tool string, ok bool) {
	if !strings.HasPrefix(name, "mcp__") {
		return "", "", false
	}
	rest := name[len("mcp__"):]
	parts := strings.SplitN(rest, "__", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
