package mcp

import (
	"context"
	"encoding/json"
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
	for projectID, byServer := range displaced {
		for name, client := range byServer {
			go func(projectID, name string, client *Client) {
				if err := closeFn(client); err != nil {
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

	// Hold the read lock across the whole CallTool so Close()/StartForProject
	// can't free the client mid-call. Multiple Execute callers still run
	// concurrently (RLock is shared); only shutdown and re-dial wait for
	// in-flight tool calls to drain.
	m.mu.RLock()
	defer m.mu.RUnlock()
	byServer := m.clients[projectID]
	var client *Client
	if byServer != nil {
		client = byServer[serverName]
	}
	if client == nil {
		return "", fmt.Errorf("MCP server %q not connected for project %q", serverName, projectID)
	}

	start := time.Now()
	result, err := client.CallTool(ctx, toolName, json.RawMessage(argsJSON))
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
