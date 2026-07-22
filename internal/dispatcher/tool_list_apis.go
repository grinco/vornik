package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/apiaccess"
	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/registry"
)

// listAPIsArgs is the LLM-facing shape — a single optional filter.
type listAPIsArgs struct {
	Query string `json:"query"`
}

// listAPIsProvider is the compact, LLM-facing per-provider shape
// returned by list_apis (design §5.3 step 9).
type listAPIsProvider struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Methods       []string `json:"methods"`
	WritesEnabled bool     `json:"writes_enabled"`
	Examples      []string `json:"examples,omitempty"`
}

// listAPIs is a thin adapter over apiaccess.Service.ListProviders (design
// §2 chat adapter). It keeps the session-level gates (capability/nil-dep →
// active-project → ownership) and the registry-drift warning, then loads
// the project's api_providers allowlist through apiaccess (via the
// Allowlist closure) so the discovery allowlist is NOT regressed, and
// finally renders the compact catalog. Redaction stays downstream; chat
// discovery output is first-party.
func (te *ToolExecutor) listAPIs(ctx context.Context, argsJSON, activeProject string, allowedProjects []string) ToolResult {
	if te.apiClient == nil || !implementsProviderLister(te.apiClient) {
		return ToolResult{Content: "list_apis: provider discovery not available on this daemon."}
	}
	if strings.TrimSpace(activeProject) == "" {
		return ToolResult{Content: "list_apis: no active project; switch to a project first (this tool is project-scoped)."}
	}
	if !projectAllowed(activeProject, allowedProjects) {
		return ToolResult{Content: fmt.Sprintf("list_apis: access to project %q is not permitted for this session.", activeProject)}
	}

	var args listAPIsArgs
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return ToolResult{Content: "list_apis: invalid arguments: " + err.Error()}
		}
	}

	// Resolve-and-warn once up front (design §5.3 step 4 / §8): a nil
	// registry or a session-permitted project absent from the registry
	// both widen discovery to "all providers" (metadata-only, no
	// credential surface) but the drift must be visible, so log a
	// single warning regardless of provider count. apiaccess's Allowlist
	// closure returns nil in both cases (⇒ all), preserving the widen.
	if te.registry == nil {
		te.logger.Warn().Str("project", activeProject).
			Msg("list_apis: registry not wired; treating project as all-providers-allowed")
	} else if te.registry.GetProject(activeProject) == nil {
		te.logger.Warn().Str("project", activeProject).
			Msg("list_apis: session-permitted project absent from registry")
	}

	svc := &apiaccess.Service{
		Client:    te.apiClient,
		Allowlist: te.apiProvidersAllowlist,
	}
	kept, truncated, err := svc.ListProviders(ctx, activeProject, args.Query)
	if err != nil {
		return ToolResult{Content: "list_apis: could not resolve the provider catalog: " + err.Error()}
	}

	if len(kept) == 0 {
		return ToolResult{Content: fmt.Sprintf("list_apis: no APIs are enabled for project %q.", activeProject)}
	}

	out := make([]listAPIsProvider, 0, len(kept))
	for _, p := range kept {
		out = append(out, listAPIsProvider{
			Name:          p.Name,
			Description:   p.Description,
			Methods:       p.AllowedMethods,
			WritesEnabled: p.WritesEnabled,
			Examples:      p.Examples,
		})
	}
	body, err := json.Marshal(out)
	if err != nil {
		return ToolResult{Content: "list_apis: failed to render provider catalog: " + err.Error()}
	}
	content := string(body)
	// Surface the narrowing hint ONLY when ListProviders actually dropped
	// entries. A result of exactly the cap size is complete, not truncated —
	// keying off truncated (not len(kept)==cap) avoids the false-positive
	// note at exactly 50 providers.
	if truncated {
		content += "\n\nresults truncated; pass query to narrow, or reduce the project's api_providers allowlist."
	}
	return ToolResult{Content: content, Provenance: outputguard.ProvenanceFirstParty}
}

// implementsProviderLister reports whether client also satisfies the
// optional apigateway.ProviderLister capability (design §5.2). Used
// by tool_inventory.go so the admin UI reports list_apis as
// unavailable when the wired apiClient is Call-only — a stricter
// gate than query_api's plain nil check.
func implementsProviderLister(client apigateway.Client) bool {
	_, ok := client.(apigateway.ProviderLister)
	return ok
}

// apiProvidersAllowlist is the Allowlist resolver both chat adapters pass
// to apiaccess: it returns the project's permissions.api_providers from
// the registry (nil ⇒ all providers allowed). It never errors — a nil
// registry or a project absent from the registry both resolve to nil
// (the empty-means-all convention; the drift is warned about by the
// caller, e.g. listAPIs).
func (te *ToolExecutor) apiProvidersAllowlist(projectID string) ([]string, error) {
	return projectAPIProviders(te.registry, projectID), nil
}

// projectAPIProviders returns the project's permissions.api_providers
// allowlist from the registry, or nil when the registry is nil, the
// project is absent, or the allowlist is empty (all of which mean "all
// providers allowed" — the empty-means-all convention shared with
// AllowedTools/AllowedProjects). Extracted so both the chat adapters'
// Allowlist closure (apiProvidersAllowlist) and providerAllowedForProject
// share one resolution.
func projectAPIProviders(reg *registry.Registry, project string) []string {
	if reg == nil {
		return nil
	}
	p := reg.GetProject(project)
	if p == nil {
		return nil
	}
	return p.Permissions.APIProviders
}

// providerAllowedForProject reports whether provider is enabled for
// project under the registry's permissions.api_providers allowlist
// (design §4.2). A nil registry, a project absent from the registry,
// or an empty/nil allowlist all mean "all providers allowed" — the
// same empty-means-all convention as AllowedTools/AllowedProjects
// elsewhere. Membership is case-sensitive, matching Phase-A
// Registry.Lookup. Retained for direct callers/tests; the live tool
// paths now gate through apiaccess.Service.
func providerAllowedForProject(reg *registry.Registry, project, provider string) bool {
	allow := projectAPIProviders(reg, project)
	if len(allow) == 0 {
		return true
	}
	for _, a := range allow {
		if a == provider {
			return true
		}
	}
	return false
}
