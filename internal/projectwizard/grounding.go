package projectwizard

import (
	"context"
	"strings"
)

// GroundingServer describes an MCP server available to the wizard
// with its list of exposed tools.
type GroundingServer struct {
	Name  string
	Tools []string
}

// MCPGroundingSource provides the current snapshot of available MCP servers
// and their tools for the wizard to ground its reasoning.
type MCPGroundingSource interface {
	Servers(ctx context.Context) []GroundingServer
}

// ModelLister provides the list of available models to the wizard.
type ModelLister interface {
	Models(ctx context.Context) []string
}

// buildMCPSection renders the available MCP servers and tools.
func buildMCPSection(ctx context.Context, mcp MCPGroundingSource) string {
	var b strings.Builder
	b.WriteString("Available MCP servers and tools:\n")
	if mcp == nil {
		b.WriteString("- No MCP servers configured.\n")
		return b.String()
	}

	servers := mcp.Servers(ctx)
	if len(servers) == 0 {
		b.WriteString("- No MCP servers configured.\n")
		return b.String()
	}

	for _, srv := range servers {
		b.WriteString("- `")
		b.WriteString(srv.Name)
		b.WriteString("`:")
		if len(srv.Tools) == 0 {
			b.WriteString(" (no tools exposed)\n")
		} else {
			for i, tool := range srv.Tools {
				if i == 0 {
					b.WriteString(" ")
				} else {
					b.WriteString(", ")
				}
				b.WriteString("`")
				b.WriteString(tool)
				b.WriteString("`")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// buildModelsSection renders the available models.
func buildModelsSection(ctx context.Context, models ModelLister) string {
	var b strings.Builder
	b.WriteString("Available models:\n")
	if models == nil {
		b.WriteString("- Default model only.\n")
		return b.String()
	}

	modelList := models.Models(ctx)
	if len(modelList) == 0 {
		b.WriteString("- Default model only.\n")
		return b.String()
	}

	for _, m := range modelList {
		b.WriteString("- `")
		b.WriteString(m)
		b.WriteString("`\n")
	}
	return b.String()
}

// buildTemplatesSection renders the available base templates.
func buildTemplatesSection(priors []TemplatePrior) string {
	var b strings.Builder
	b.WriteString("Available base templates:\n")
	if len(priors) == 0 {
		b.WriteString("- (None configured; see custom-base below.)\n")
		return b.String()
	}

	for _, p := range priors {
		b.WriteString("- `")
		b.WriteString(p.Slug)
		b.WriteString("` — ")
		b.WriteString(p.DisplayName)
		if p.Description != "" {
			b.WriteString(": ")
			b.WriteString(p.Description)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// buildAddonVocabularySection renders the addon vocabulary documentation.
// Each addon is a JSON object with a "type" field plus the fields below;
// the exact field shapes MUST match the appliers in appliers.go or the
// composition engine will reject the emitted addon.
func buildAddonVocabularySection() string {
	var b strings.Builder
	b.WriteString("Addon vocabulary — every addon is a JSON object with a `type` field plus:\n\n")
	b.WriteString("- `mcp_server`: {`name`: string (an MCP server from the list above), " +
		"`allowed_tools`: array of strings (optional — tool names from that server)}\n")
	b.WriteString("- `schedule`: {`interval`: string (a Go duration like \"168h\" or \"24h\", NOT a cron expression), " +
		"`goal`: string (required — fired verbatim each tick), `task_type`: string (optional)}\n")
	b.WriteString("- `rag_source`: {`source`: string (a doc URL/repo/description to track), " +
		"`cadence`: string (a Go duration like \"24h\")}\n")
	b.WriteString("- `chat_tools`: {`allowed_tools`: array of strings " +
		"(added to the project's PROJECT-level tool allowlist, not a role's)}\n")
	b.WriteString("- `role_prompt_append`: {`role`: string (must be an existing swarm role, e.g. \"lead\" for custom-base), " +
		"`text`: string (appended to that role's system prompt)}\n")
	b.WriteString("- `secret_requirement`: {`name`: string (env var name), `label`: string (optional human label)}\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("- `schedule` and `rag_source` are mutually exclusive on one project; the engine errors if both are set.\n")
	b.WriteString("- `chat_tools` grants tools at the project scope, not a single role.\n")
	b.WriteString("- If no base template matches the project intent, use `custom-base` (role: \"lead\").\n")
	return b.String()
}

// BuildGrounding constructs a compact text block for the wizard's system
// prompt, grounding it in the live state of available MCP servers, models,
// base templates, and the addon vocabulary (six types, mutual-exclusion
// rules, and custom-base fallback).
func BuildGrounding(ctx context.Context, mcp MCPGroundingSource, models ModelLister, priors []TemplatePrior) string {
	var b strings.Builder
	b.WriteString("Live project state:\n\n")
	b.WriteString(buildMCPSection(ctx, mcp))
	b.WriteString("\n")
	b.WriteString(buildModelsSection(ctx, models))
	b.WriteString("\n")
	b.WriteString(buildTemplatesSection(priors))
	b.WriteString("\n")
	b.WriteString(buildAddonVocabularySection())
	return b.String()
}
