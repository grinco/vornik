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
	fresh := make(map[string]map[string]*Client, len(desired))
	for projectID, servers := range desired {
		if projectID == "" {
			m.logger.Error().Msg("mcp: SyncProjects given empty projectID — ignored")
			continue
		}
		byServer := make(map[string]*Client, len(servers))
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
			byServer[cfg.Name] = client
		}
		fresh[projectID] = byServer
	}

	m.mu.Lock()
	displaced := m.clients
	m.clients = fresh
	m.mu.Unlock()

	for projectID, byServer := range displaced {
		for name, client := range byServer {
			if err := client.Close(); err != nil {
				m.logger.Warn().
					Err(err).
					Str("project", projectID).
					Str("server", name).
					Msg("mcp: close displaced client")
			}
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
