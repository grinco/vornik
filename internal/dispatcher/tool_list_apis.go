package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/registry"
)

// maxListAPIsResults caps list_apis output (design §5.3 step 7):
// bounds the response even when a large project allowlist (or an
// unfiltered global registry) would otherwise return everything.
const maxListAPIsResults = 50

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

// listAPIs returns the active project's allowed third-party API
// providers so the agent can discover what query_api can call
// without guessing provider names. Gate order mirrors queryAPI
// (design §5.3, §6): nil-dep/capability → active-project →
// ownership → resolve allowlist (nil-safe, warns on drift) →
// keep-filter → optional query filter → cap → render.
func (te *ToolExecutor) listAPIs(_ context.Context, argsJSON, activeProject string, allowedProjects []string) ToolResult {
	pl, ok := te.apiClient.(apigateway.ProviderLister)
	if te.apiClient == nil || !ok {
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
	// single warning regardless of provider count rather than one per
	// provider inside the filter loop below.
	if te.registry == nil {
		te.logger.Warn().Str("project", activeProject).
			Msg("list_apis: registry not wired; treating project as all-providers-allowed")
	} else if te.registry.GetProject(activeProject) == nil {
		te.logger.Warn().Str("project", activeProject).
			Msg("list_apis: session-permitted project absent from registry")
	}

	providers := pl.ListProviders()
	kept := make([]apigateway.ProviderInfo, 0, len(providers))
	for _, p := range providers {
		if providerAllowedForProject(te.registry, activeProject, p.Name) {
			kept = append(kept, p)
		}
	}

	if q := strings.ToLower(strings.TrimSpace(args.Query)); q != "" {
		filtered := make([]apigateway.ProviderInfo, 0, len(kept))
		for _, p := range kept {
			if strings.Contains(strings.ToLower(p.Name), q) || strings.Contains(strings.ToLower(p.Description), q) {
				filtered = append(filtered, p)
			}
		}
		kept = filtered
	}

	truncated := false
	if len(kept) > maxListAPIsResults {
		kept = kept[:maxListAPIsResults]
		truncated = true
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

// providerAllowedForProject reports whether provider is enabled for
// project under the registry's permissions.api_providers allowlist
// (design §4.2). A nil registry, a project absent from the registry,
// or an empty/nil allowlist all mean "all providers allowed" — the
// same empty-means-all convention as AllowedTools/AllowedProjects
// elsewhere; the caller is responsible for surfacing the nil-registry/
// missing-project drift via a warning (listAPIs does this once,
// up front, rather than per provider here). Membership is
// case-sensitive, matching Phase-A Registry.Lookup.
//
// Shared by list_apis (keep-filter, here) and query_api (per-call
// gate before apiClient.Call — design §5.4).
func providerAllowedForProject(reg *registry.Registry, project, provider string) bool {
	if reg == nil {
		return true
	}
	p := reg.GetProject(project)
	if p == nil {
		return true
	}
	allow := p.Permissions.APIProviders
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
