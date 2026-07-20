package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/outputguard"
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

// queryAPI calls a registered third-party API through the local gateway. Gate
// order mirrors tool_send_email.go (design §3.2): nil-dep → active-project →
// ownership → parse → validate (provider required → per-project api_providers
// allowlist, design §5.4) → call → result. The allowlist gate deliberately
// precedes existence resolution in apiClient.Call: for a restricted project,
// an unregistered-or-disallowed provider both yield the same "not enabled for
// project" refusal (information-hiding — design §7). Errors are
// ToolResult.Content (human-readable text, never a Go error) so the LLM can
// self-correct.
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
	if strings.TrimSpace(args.Provider) == "" {
		return ToolResult{Content: "query_api: `provider` is required."}
	}
	if !providerAllowedForProject(te.registry, activeProject, args.Provider) {
		return ToolResult{Content: fmt.Sprintf("query_api: provider %q is not enabled for project %q.", args.Provider, activeProject)}
	}
	if strings.TrimSpace(args.Method) == "" {
		args.Method = "GET"
	}

	resp, err := te.apiClient.Call(ctx, apigateway.Request{
		Provider: args.Provider, Method: args.Method, Path: args.Path,
		Query: args.Query, Body: args.Body,
	})
	if err != nil {
		return ToolResult{Content: mapQueryAPIError(err, args)}
	}
	return ToolResult{Content: resp.Body, Provenance: outputguard.ProvenanceThirdParty}
}

// mapQueryAPIError translates the gateway sentinels into clear, policy-aware
// messages (design §6.1: a boundary, not a transient failure). Credentials are
// never surfaced — the client already scrubs the token from any raw error.
func mapQueryAPIError(err error, args queryAPIArgs) string {
	switch {
	case errors.Is(err, apigateway.ErrUnknownProvider):
		return fmt.Sprintf("query_api: unknown provider %q — it is not registered.", args.Provider)
	case errors.Is(err, apigateway.ErrMethodNotAllowed), errors.Is(err, apigateway.ErrUpstreamMethod):
		return fmt.Sprintf("query_api: provider %q does not support %s on %q (read-only or route not configured).",
			args.Provider, strings.ToUpper(args.Method), args.Path)
	case errors.Is(err, apigateway.ErrGatewayAuth):
		return "query_api: gateway authentication failed (daemon↔gateway token misconfigured)."
	default:
		return "query_api: " + err.Error()
	}
}
