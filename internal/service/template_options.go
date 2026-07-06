package service

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/templates"
)

// templateOptionsResolver adapts the daemon's live MCP registry and
// model catalog to the template engine's OptionsResolver seam
// (project-creation-e2e-design §1a). Each resolution gets its own
// short timeout so a slow upstream can't hang form render or
// submit validation.
type templateOptionsResolver struct {
	mcpServerNames func(context.Context) ([]string, error)
	modelIDs       func(context.Context) ([]string, error)
}

// NewTemplateOptionsResolver builds the resolver from two source
// functions. Either may be nil when the deployment lacks that
// subsystem; resolving the corresponding source then errors (the
// form shows the failure inline per the spec's error-handling
// contract, never an empty select).
func NewTemplateOptionsResolver(
	mcpServerNames func(context.Context) ([]string, error),
	modelIDs func(context.Context) ([]string, error),
) templates.OptionsResolver {
	return &templateOptionsResolver{mcpServerNames: mcpServerNames, modelIDs: modelIDs}
}

// buildTemplateOptionsResolver adapts the container's live MCP
// registry and chat client to templates.OptionsResolver — the same
// wiring container_http.go builds for the project-template gallery's
// optionsFrom(mcp_registry) / optionsFrom(models) select params.
// Extracted so buildProjectWizardOrNil (project_wizard_adapter.go) can
// wire the identical resolver into Wizard.Resolver for wizard v2's
// commit-time template composition, without the two call sites
// drifting on how a source closure degrades when its subsystem is
// unwired. Either closure is left nil when c.mcpRegistry / c.ChatClient
// is absent; NewTemplateOptionsResolver already turns a nil source
// into a per-call error instead of a panic, so the returned resolver
// is always non-nil (matches the gallery's existing behaviour).
func buildTemplateOptionsResolver(c *Container) templates.OptionsResolver {
	if c == nil {
		return nil
	}
	var mcpServerNamesFn func(context.Context) ([]string, error)
	if c.mcpRegistry != nil {
		mcpSource := &mcpFormRegistryAdapter{registry: c.mcpRegistry}
		mcpServerNamesFn = func(ctx context.Context) ([]string, error) {
			servers, err := mcpSource.Servers(ctx)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(servers))
			for _, srv := range servers {
				names = append(names, srv.Name)
			}
			return names, nil
		}
	}
	var modelIDsFn func(context.Context) ([]string, error)
	if c.ChatClient != nil {
		chatClient := c.ChatClient
		modelIDsFn = func(ctx context.Context) ([]string, error) {
			return templateModelIDs(ctx, chatClient)
		}
	}
	return NewTemplateOptionsResolver(mcpServerNamesFn, modelIDsFn)
}

func (r *templateOptionsResolver) ResolveOptions(source string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	switch source {
	case templates.OptionsSourceMCPRegistry:
		if r.mcpServerNames == nil {
			return nil, fmt.Errorf("mcp registry source not wired")
		}
		return r.mcpServerNames(ctx)
	case templates.OptionsSourceModels:
		if r.modelIDs == nil {
			return nil, fmt.Errorf("models source not wired")
		}
		return r.modelIDs(ctx)
	default:
		return nil, fmt.Errorf("unknown options source %q", source)
	}
}
