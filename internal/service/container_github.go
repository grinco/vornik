package service

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// buildGitHubChannel scans the project registry for every project
// carrying a fully configured `github_app` block and constructs
// one *github.Channel that routes inbound webhooks across them by
// matching `payload.installation.id` against each project's
// configured `github_app.installation_id`. Returns (nil, nil) when
// no project has the channel enabled — the service container then
// leaves the API route unmounted entirely.
//
// Single-installation back-compat: when exactly one project has
// the channel enabled, the channel is built with the legacy
// top-level Config fields and behaves identically to the
// pre-multi-installation code path.
//
// Multi-installation mode: when two or more projects have the
// channel enabled, the channel is built with a non-empty
// Config.Installations slice. Every installation_id MUST be
// distinct across the enabled projects (validated by github.New);
// a collision is an operator misconfiguration that aborts boot.
//
// The second return value is the slice of enabled projects, used
// by the service container to wire one DispatcherReceiver +
// session-store project resolution per installation. In single-
// install mode the slice has exactly one entry — same behaviour
// as before.
//
// Returned errors are operator-facing — empty webhook secret env
// var, missing PEM file, malformed key — and abort daemon boot so
// the misconfig surfaces at startup rather than the first
// delivery.
func buildGitHubChannel(projects []*registry.Project) (*github.Channel, []*registry.Project, error) {
	return buildGitHubChannelWithTaskCreator(projects, nil)
}

// taskCreatorFromRepo returns a closure that builds a
// githubTaskCreator pinned to a project. Encapsulates the
// nil-checks required to keep the channel usable when the
// container hasn't yet wired a task repo (e.g. during early-boot
// tests). Returns nil from the closure when the prerequisites
// aren't met so the channel falls back to its no-op log path.
func taskCreatorFromRepo(
	taskRepo persistence.TaskRepository,
	labelMap map[string]string,
	logger zerolog.Logger,
) func(*registry.Project) github.TaskCreator {
	return func(p *registry.Project) github.TaskCreator {
		if taskRepo == nil || p == nil {
			return nil
		}
		return newGitHubTaskCreator(taskRepo, p, labelMap, logger)
	}
}

// buildGitHubChannelWithTaskCreator is the slice 4F variant of
// buildGitHubChannel that injects an optional TaskCreator factory
// into the channel's Config before construction. The
// taskCreatorFor closure is invoked per-project (once in
// single-installation mode, once per installation in
// multi-installation mode) and may return nil to fall back to the
// existing "TaskCreator not wired" log path.
//
// Kept as a separate function (rather than a single variadic) so
// the existing buildGitHubChannel tests don't need to thread an
// extra closure through every call.
func buildGitHubChannelWithTaskCreator(
	projects []*registry.Project,
	taskCreatorFor func(*registry.Project) github.TaskCreator,
) (*github.Channel, []*registry.Project, error) {
	var enabled []*registry.Project
	for _, p := range projects {
		if !p.GitHubApp.Enabled() {
			continue
		}
		enabled = append(enabled, p)
	}
	if len(enabled) == 0 {
		return nil, nil, nil
	}

	// Single-installation back-compat path. Behaviour matches the
	// pre-multi-installation code: top-level Config fields,
	// single project pinned to the channel. Test surface for this
	// path is the existing container_github_test.go suite.
	if len(enabled) == 1 {
		picked := enabled[0]
		cfg, err := resolveGitHubAppConfig(picked.GitHubApp)
		if err != nil {
			return nil, []*registry.Project{picked}, fmt.Errorf("project %q github_app: %w", picked.ID, err)
		}
		if taskCreatorFor != nil {
			if tc := taskCreatorFor(picked); tc != nil {
				cfg.TaskCreator = tc
			}
		}
		ch, err := github.New(cfg)
		if err != nil {
			return nil, []*registry.Project{picked}, fmt.Errorf("project %q github_app: %w", picked.ID, err)
		}
		return ch, []*registry.Project{picked}, nil
	}

	// Multi-installation path. Every enabled project must share
	// the same WebhookSecret + APIBaseURL (one channel = one
	// HTTP handler = one secret to verify against), so the first
	// project's values win and the rest are checked for
	// consistency. Mismatch aborts boot.
	first := enabled[0]
	baseCfg, err := resolveGitHubAppConfig(first.GitHubApp)
	if err != nil {
		return nil, enabled, fmt.Errorf("project %q github_app: %w", first.ID, err)
	}
	installs := make([]github.InstallationConfig, 0, len(enabled))
	installs = append(installs, installationConfigFromConfigWithTaskCreator(first.ID, baseCfg, taskCreatorFor, first))

	for _, p := range enabled[1:] {
		cfg, err := resolveGitHubAppConfig(p.GitHubApp)
		if err != nil {
			return nil, enabled, fmt.Errorf("project %q github_app: %w", p.ID, err)
		}
		if cfg.WebhookSecret != baseCfg.WebhookSecret {
			return nil, enabled, fmt.Errorf("project %q github_app: WebhookSecret differs from project %q — every github-app project must share the same secret (one webhook URL on the GitHub App settings page)", p.ID, first.ID)
		}
		if cfg.APIBaseURL != baseCfg.APIBaseURL {
			return nil, enabled, fmt.Errorf("project %q github_app: APIBaseURL %q differs from project %q (%q)", p.ID, cfg.APIBaseURL, first.ID, baseCfg.APIBaseURL)
		}
		installs = append(installs, installationConfigFromConfigWithTaskCreator(p.ID, cfg, taskCreatorFor, p))
	}

	multiCfg := github.Config{
		WebhookSecret: baseCfg.WebhookSecret,
		APIBaseURL:    baseCfg.APIBaseURL,
		HTTPClient:    baseCfg.HTTPClient,
		Installations: installs,
	}
	ch, err := github.New(multiCfg)
	if err != nil {
		return nil, enabled, fmt.Errorf("github-app multi-installation channel: %w", err)
	}
	fmt.Fprintf(os.Stderr, "vornik/service: github-app channel wired with %d installations: %s\n",
		len(enabled), strings.Join(projectIDs(enabled), ","))
	return ch, enabled, nil
}

// installationConfigFromConfigWithTaskCreator combines the multi-
// installation translation with the slice-4F TaskCreator factory.
// When taskCreatorFor is nil the resulting InstallationConfig
// keeps TaskCreator unset (same as the pre-4F shape).
func installationConfigFromConfigWithTaskCreator(
	projectID string,
	cfg github.Config,
	taskCreatorFor func(*registry.Project) github.TaskCreator,
	project *registry.Project,
) github.InstallationConfig {
	out := installationConfigFromConfig(projectID, cfg)
	if taskCreatorFor != nil && project != nil {
		if tc := taskCreatorFor(project); tc != nil {
			out.TaskCreator = tc
		}
	}
	return out
}

// installationConfigFromConfig translates a resolved github.Config
// (single-installation shape) into one entry in the
// multi-installation Installations slice. ProjectID is the vornik
// project ID; the rest mirror the per-project github_app block.
func installationConfigFromConfig(projectID string, cfg github.Config) github.InstallationConfig {
	return github.InstallationConfig{
		ProjectID:       projectID,
		InstallationID:  cfg.InstallationID,
		AppID:           cfg.AppID,
		PrivateKey:      cfg.PrivateKey,
		RepoAllowlist:   cfg.RepoAllowlist,
		TaskLabels:      cfg.TaskLabels,
		PRReviewLabels:  cfg.PRReviewLabels,
		SenderAllowlist: cfg.SenderAllowlist,
		// TaskCreator wired separately by the service container —
		// each installation gets its own per-project adapter.
		TaskCreator: nil,
	}
}

// projectIDs is a small helper for the boot log line.
func projectIDs(ps []*registry.Project) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

// resolveGitHubAppConfig translates a ProjectGitHubApp YAML block
// into the github.Config the channel constructor consumes:
// secrets are read from env vars (mirrors ProjectWebhookSource.SecretEnv),
// the private key is loaded from disk and parsed, and string
// allowlists pass through verbatim.
func resolveGitHubAppConfig(p registry.ProjectGitHubApp) (github.Config, error) {
	secret := os.Getenv(p.WebhookSecretEnv)
	if strings.TrimSpace(secret) == "" {
		return github.Config{}, fmt.Errorf("webhook_secret_env %q is unset or empty", p.WebhookSecretEnv)
	}

	cfg := github.Config{
		AppID:           p.AppID,
		InstallationID:  p.InstallationID,
		APIBaseURL:      p.APIBaseURL,
		WebhookSecret:   secret,
		RepoAllowlist:   p.RepoAllowlist,
		TaskLabels:      p.TaskLabels,
		PRReviewLabels:  p.PRReviewLabels,
		SenderAllowlist: p.SenderAllowlist,
	}

	if strings.TrimSpace(p.PrivateKeyPath) != "" {
		pemBytes, err := os.ReadFile(p.PrivateKeyPath)
		if err != nil {
			return github.Config{}, fmt.Errorf("read private_key_path %q: %w", p.PrivateKeyPath, err)
		}
		key, err := github.LoadPrivateKeyPEM(pemBytes)
		if err != nil {
			return github.Config{}, err
		}
		cfg.PrivateKey = key
	} else if p.AppID != 0 || p.InstallationID != 0 {
		// Validate at the registry layer should have caught this
		// (all-or-nothing), but belt-and-braces: refuse to construct
		// an outbound-half-configured channel.
		return github.Config{}, errors.New("app_id / installation_id set but private_key_path is empty")
	}

	return cfg, nil
}
