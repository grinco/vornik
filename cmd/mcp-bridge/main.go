// Package main provides the mcp-bridge binary.
//
// mcp-bridge is a small helper that runs inside agent containers, giving them
// access to MCP (Model Context Protocol) servers defined in the project config.
//
// Two modes, selected automatically:
//
//  1. Daemon-proxy mode (preferred, scales to multi-tenant):
//     When VORNIK_API_URL and VORNIK_PROJECT_ID are set, the bridge forwards
//     discover and call requests to the vornik daemon's
//     /api/v1/projects/{id}/mcp/* endpoints. The daemon already holds
//     persistent per-project MCP clients with their credentials — the agent
//     piggybacks on those, so the agent image doesn't need node/npm or
//     bind-mounted secrets, and there's no per-task cold-start cost.
//
//  2. Local-subprocess mode (fallback):
//     Reads mcp.json (path: $MCP_CONFIG or /app/input/mcp.json) and spawns
//     the configured command directly. Works when the daemon isn't
//     reachable but requires the subprocess command to be runnable inside
//     the agent container. Kept for back-compat and tests.
//
// Subcommands:
//
//	mcp-bridge discover
//	    Prints a JSON array of tools in OpenAI function-calling format.
//
//	mcp-bridge call <qualifiedName> <argsJSON>
//	    Calls one tool, prints its text result. Exits non-zero on error.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/mcp"
)

const (
	defaultMCPConfigPath = "/app/input/mcp.json"

	envAPIURL    = "VORNIK_API_URL"
	envProjectID = "VORNIK_PROJECT_ID"
	// VORNIK_API_KEY is the bearer the executor injects for
	// daemon-internal callbacks. Without it, daemon auth_enabled=true
	// rejects every discover + call with 401. The executor's
	// container_scheduler sets this from agent_llm.api_key so a
	// single config knob covers chat-proxy, llm-usage, tool-audit,
	// AND mcp-bridge. Falls back to VORNIK_LLM_API_KEY (some agent
	// container shapes may inject the LLM key but not the API key)
	// for forward compat. Empty = no Authorization header sent
	// (auth_enabled=false deployments).
	envAPIKey    = "VORNIK_API_KEY"
	envLLMAPIKey = "VORNIK_LLM_API_KEY"
)

// daemonBearerToken returns the credential mcp-bridge should send
// on internal-callback requests. Empty result is valid — when
// auth_enabled=false the daemon ignores Authorization headers.
func daemonBearerToken() string {
	if v := strings.TrimSpace(os.Getenv(envAPIKey)); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(envLLMAPIKey))
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mcp-bridge: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: mcp-bridge <discover|call> [args...]")
	}

	logger := zerolog.New(os.Stderr).With().Timestamp().Str("component", "mcp-bridge").Logger()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Daemon-proxy mode takes precedence when both env vars are set.
	// The daemon owns the persistent MCP clients; the agent just forwards.
	if apiURL := os.Getenv(envAPIURL); apiURL != "" {
		projectID := os.Getenv(envProjectID)
		if projectID == "" {
			return fmt.Errorf("%s is set but %s is empty — daemon-proxy mode requires both", envAPIURL, envProjectID)
		}
		switch os.Args[1] {
		case "discover":
			return httpDiscover(ctx, apiURL, projectID)
		case "call":
			if len(os.Args) < 4 {
				return fmt.Errorf("usage: mcp-bridge call <qualifiedName> <argsJSON>")
			}
			return httpCall(ctx, apiURL, projectID, os.Args[2], os.Args[3])
		default:
			return fmt.Errorf("unknown subcommand %q — use discover or call", os.Args[1])
		}
	}

	// Fallback: local subprocess mode. Reads mcp.json and spawns.
	cfgPath := os.Getenv("MCP_CONFIG")
	if cfgPath == "" {
		cfgPath = defaultMCPConfigPath
	}
	servers, err := loadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config %s: %w", cfgPath, err)
	}

	// Local mode uses a synthetic projectID — the Manager API is
	// project-keyed, but this binary doesn't interact with cross-project
	// routing. "local" keeps the map consistent.
	const localProjectID = "local"
	switch os.Args[1] {
	case "discover":
		return cmdDiscover(ctx, localProjectID, servers, logger)
	case "call":
		if len(os.Args) < 4 {
			return fmt.Errorf("usage: mcp-bridge call <qualifiedName> <argsJSON>")
		}
		return cmdCall(ctx, localProjectID, servers, os.Args[2], os.Args[3], logger)
	default:
		return fmt.Errorf("unknown subcommand %q — use discover or call", os.Args[1])
	}
}

// --- daemon-proxy mode ---

// daemonHTTP returns the base URL and HTTP client to reach the daemon.
// When VORNIK_API_URL uses the unix:// scheme — the daemon-only network
// policy, where the container has no network device and reaches the
// daemon over a bind-mounted unix socket (DaemonOnlySocketContainerPath)
// — it returns a client that dials the socket plus a synthetic
// http://unix base. Otherwise it returns the (trimmed) apiURL and a
// plain client. timeout==0 leaves the client without one.
func daemonHTTP(apiURL string, timeout time.Duration) (string, *http.Client) {
	if strings.HasPrefix(apiURL, "unix://") {
		sock := strings.TrimPrefix(apiURL, "unix://")
		dialer := &net.Dialer{}
		return "http://unix", &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", sock)
				},
			},
		}
	}
	return strings.TrimRight(apiURL, "/"), &http.Client{Timeout: timeout}
}

func httpDiscover(ctx context.Context, apiURL, projectID string) error {
	base, client := daemonHTTP(apiURL, 30*time.Second)
	u := base + "/api/v1/projects/" + projectID + "/mcp/tools"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if tok := daemonBearerToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("daemon call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var wrap struct {
		Tools []chat.Tool `json:"tools"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return fmt.Errorf("parse daemon response: %w", err)
	}
	if wrap.Tools == nil {
		wrap.Tools = []chat.Tool{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(wrap.Tools)
}

func httpCall(ctx context.Context, apiURL, projectID, qualifiedName, argsJSON string) error {
	// Per-call timeout distinct from the session-level ctx (see below).
	base, callClient := daemonHTTP(apiURL, 60*time.Second)
	u := base + "/api/v1/projects/" + projectID + "/mcp/tools/call"
	// Validate argsJSON BEFORE marshalling so a hallucinated /
	// truncated argument from the LLM produces a typed error
	// instead of an empty POST body. Pre-fix the marshal error
	// was discarded (`body, _ := json.Marshal(...)`) which sent
	// nil bytes when json.RawMessage was invalid — daemon would
	// 400 and the LLM would retry into a tight loop.
	if !json.Valid([]byte(argsJSON)) {
		preview := argsJSON
		if len(preview) > 200 {
			preview = preview[:200] + "…(truncated)"
		}
		return fmt.Errorf("argsJSON is not valid JSON (likely a hallucinated/truncated tool call): %s", preview)
	}
	body, err := json.Marshal(map[string]any{
		"name":      qualifiedName,
		"arguments": json.RawMessage(argsJSON),
	})
	if err != nil {
		return fmt.Errorf("build request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := daemonBearerToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	// Forward the agent container's task / execution origin so
	// the daemon can plumb them through to broker audit rows
	// (trading_orders.task_id / execution_id). Pre-2026-05-11
	// every trading_orders row had NULL task_id, blocking
	// task↔order correlation for investigations. These env vars
	// are stamped on the container by the executor; an
	// operator-driven CLI run won't have them set, which is
	// fine — the broker writes NULL columns.
	if v := os.Getenv("VORNIK_TASK_ID"); v != "" {
		req.Header.Set("X-Task-ID", v)
	}
	if v := os.Getenv("VORNIK_EXECUTION_ID"); v != "" {
		req.Header.Set("X-Execution-ID", v)
	}

	// callClient carries a per-call timeout distinct from the
	// session-level ctx (5min). A hung daemon-side handler would
	// otherwise pin every tool call to the full 5-minute budget — one
	// stalled call burns the entire session, cascading into executor
	// step timeouts and warm-container health releases.
	resp, err := callClient.Do(req)
	if err != nil {
		return fmt.Errorf("daemon call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var wrap struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &wrap); err != nil {
		return fmt.Errorf("parse daemon response: %w", err)
	}
	fmt.Print(wrap.Text)
	return nil
}

// --- local subprocess mode ---

func cmdDiscover(ctx context.Context, projectID string, servers []mcp.ServerConfig, logger zerolog.Logger) error {
	mgr := mcp.NewManager(logger)
	mgr.StartForProject(ctx, projectID, servers)
	defer mgr.Close()

	tools := mgr.Tools(projectID)
	if tools == nil {
		tools = []chat.Tool{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(tools)
}

func cmdCall(ctx context.Context, projectID string, servers []mcp.ServerConfig, qualifiedName, argsJSON string, logger zerolog.Logger) error {
	mgr := mcp.NewManager(logger)
	mgr.StartForProject(ctx, projectID, servers)
	defer mgr.Close()

	result, err := mgr.Execute(ctx, projectID, qualifiedName, argsJSON)
	if err != nil {
		return err
	}
	fmt.Print(result)
	return nil
}

// loadConfig reads an mcp.json file (array of ServerConfig).
func loadConfig(path string) ([]mcp.ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var servers []mcp.ServerConfig
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil, fmt.Errorf("parse mcp.json: %w", err)
	}
	return servers, nil
}
