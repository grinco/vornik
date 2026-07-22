package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/apiaccess"
	"vornik.io/vornik/internal/apigateway"
)

// queryAPIArgs is the LLM-facing shape. The schema description NEVER names
// credentials — only capability + params (design §3.1).
type queryAPIArgs struct {
	Provider string         `json:"provider"`
	Method   string         `json:"method"`
	Path     string         `json:"path"`
	Query    map[string]any `json:"query"`
	Body     map[string]any `json:"body"`
}

// queryAPI is a thin adapter over apiaccess.Service.Query (design §2 chat
// adapter). It keeps the session-level gates (nil-dep → active-project →
// ownership) and arg parsing, then delegates the capability gate
// (provider-required → per-project api_providers allowlist → write policy
// → gateway call → sentinel mapping) to the shared core. The Allowlist
// closure loads the project's api_providers so a non-empty allowlist still
// blocks non-allowlisted providers (no discovery regression), and
// AgentWrites is a role-blind closure returning true — chat defers write
// policy to the gateway's writes_enabled (design §5c), preserving chat
// write behavior. On refusal the human-readable reason is prefixed with
// "query_api: " (never a raw Go error) so the LLM can self-correct; the
// existing downstream output_guard pass still redacts and chat stays
// UNCAPPED (design §5b).
func (te *ToolExecutor) queryAPI(ctx context.Context, argsJSON, activeProject string, allowedProjects []string) ToolResult {
	if te.apiClient == nil {
		return ToolResult{Content: "query_api: third-party API gateway not configured on this daemon."}
	}
	if strings.TrimSpace(activeProject) == "" {
		return ToolResult{Content: "query_api: no active project; switch to a project first (this tool is project-scoped)."}
	}
	if !projectAllowed(activeProject, allowedProjects) {
		return ToolResult{Content: fmt.Sprintf("query_api: access to project %q is not permitted for this session.", activeProject)}
	}
	var args queryAPIArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: "query_api: invalid arguments: " + err.Error()}
	}

	svc := &apiaccess.Service{
		Client:      te.apiClient,
		Allowlist:   te.apiProvidersAllowlist,
		AgentWrites: func(_, _ string) bool { return true }, // chat defers to the gateway
	}
	outcome := svc.Query(ctx, activeProject, "", apigateway.Request{
		Provider: args.Provider, Method: args.Method, Path: args.Path,
		Query: args.Query, Body: args.Body,
	})
	if outcome.Refusal != "" {
		return ToolResult{Content: "query_api: " + outcome.Refusal}
	}
	return ToolResult{Content: outcome.Body, Provenance: outcome.Provenance}
}
