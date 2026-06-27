package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/forge"
	_ "vornik.io/vornik/internal/forge/github" // registers the "github" forge provider via init()
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// projectGetter is the narrow project-lookup surface the resolver needs;
// *registry.Registry satisfies it. Keeps the resolver test double trivial.
type projectGetter interface {
	GetProject(id string) *registry.Project
}

// forgeProviderResolver resolves a project's configured forge.ForgeProvider,
// caching the constructed provider per project (the installation-token cache
// lives inside the provider, so reusing the instance reuses the token). Implements
// the forge-handler package's ProviderResolver. Concurrency-safe.
type forgeProviderResolver struct {
	projects projectGetter
	mu       sync.Mutex
	cache    map[string]forge.ForgeProvider
}

// ForgeProvider returns the project's provider, building it on first use.
func (r *forgeProviderResolver) ForgeProvider(_ context.Context, projectID string) (forge.ForgeProvider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.cache[projectID]; ok {
		return p, nil
	}
	if r.projects == nil {
		return nil, fmt.Errorf("forge: no project registry")
	}
	proj := r.projects.GetProject(projectID)
	if proj == nil {
		return nil, fmt.Errorf("forge: unknown project %q", projectID)
	}
	cfg, ok := proj.ResolveForge()
	if !ok {
		return nil, fmt.Errorf("forge: project %q has no forge configured", projectID)
	}
	p, err := forge.New(cfg)
	if err != nil {
		return nil, err
	}
	if r.cache == nil {
		r.cache = map[string]forge.ForgeProvider{}
	}
	r.cache[projectID] = p
	return p, nil
}

// forgePublishSource locates the daemon-side git dir + commit to publish for a
// task: the project workspace clone at its current HEAD — which, when the
// forge.open_change_request step runs, is the merged result of the child
// dev-pipeline task. Implements the forge-handler package's PublishSource.
type forgePublishSource struct {
	workspacePath string
}

// PublishSource returns the project git dir and its HEAD sha.
func (s *forgePublishSource) PublishSource(ctx context.Context, task *persistence.Task) (string, string, error) {
	if s == nil || s.workspacePath == "" || task == nil || task.ProjectID == "" {
		return "", "", fmt.Errorf("forge: cannot locate publish source (missing workspace path or task project)")
	}
	gitDir := filepath.Join(s.workspacePath, task.ProjectID)
	out, err := exec.CommandContext(ctx, "git", "-C", gitDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", "", fmt.Errorf("forge: rev-parse HEAD in %s: %w", gitDir, err)
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", "", fmt.Errorf("forge: empty HEAD sha in %s", gitDir)
	}
	return gitDir, sha, nil
}

// newForgeResolver builds the per-project forge resolver, or nil when there's no
// project registry to resolve against.
func (c *Container) newForgeResolver() *forgeProviderResolver {
	if c.Registry == nil {
		return nil
	}
	return &forgeProviderResolver{projects: c.Registry, cache: map[string]forge.ForgeProvider{}}
}

// forgeWebhookClassifier implements api.ForgeClassifier: it classifies a verified
// webhook body into the provider-neutral forge_job for the project's forge
// provider. Body-based (ClassifyEvent infers the event type from the payload
// shape) so it works on the relay-forwarded path where headers are absent.
type forgeWebhookClassifier struct {
	resolver *forgeProviderResolver
}

// ClassifyWebhook returns the marshaled forge_job and ok=true for a recognised
// forge event on a forge-configured project; ok=false otherwise (the webhook
// task is then created without a forge_job).
func (c *forgeWebhookClassifier) ClassifyWebhook(ctx context.Context, projectID string, body []byte) (json.RawMessage, string, bool) {
	if c == nil || c.resolver == nil {
		return nil, "", false
	}
	provider, err := c.resolver.ForgeProvider(ctx, projectID)
	if err != nil {
		return nil, "", false // project has no forge configured
	}
	job, ok := provider.ClassifyEvent(http.Header{}, body)
	if !ok {
		return nil, "", false
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return nil, "", false
	}
	return raw, forgeTaskPrompt(job), true
}

// forgeTaskPrompt builds a clean spec for the agent from the classified job —
// the issue/CR title + body — instead of handing it the raw webhook JSON.
func forgeTaskPrompt(job forge.ForgeJob) string {
	kind := "issue"
	if job.IsChangeRequest {
		kind = "pull request"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub %s #%d in %s", kind, job.Number, job.Repo)
	if t := strings.TrimSpace(job.Title); t != "" {
		b.WriteString(": " + t)
	}
	if body := strings.TrimSpace(job.Body); body != "" {
		b.WriteString("\n\n" + body)
	}
	return b.String()
}

// verifyForgePermissions checks, once at boot, that every forge-configured
// project's provider can push branches (the permission forge.open_change_request
// needs). Best-effort + non-fatal: a warning at boot lets an operator fix an
// under-permissioned App before the first publish fails, on BOTH the channel and
// the generic-webhook paths (the channel-start check only covers channel users).
// Provider-neutral via ForgeProvider.VerifyPushAccess.
func (c *Container) verifyForgePermissions(ctx context.Context, resolver *forgeProviderResolver) {
	if resolver == nil || c.Registry == nil {
		return
	}
	for _, p := range c.Registry.ListProjects() {
		if _, ok := p.ResolveForge(); !ok {
			continue
		}
		provider, err := resolver.ForgeProvider(ctx, p.ID)
		if err != nil {
			c.Logger.Debug().Err(err).Str("project_id", p.ID).Msg("forge: could not build provider for permission check (skipping)")
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = provider.VerifyPushAccess(cctx)
		cancel()
		if err != nil {
			c.Logger.Warn().Err(err).Str("project_id", p.ID).
				Msg("forge: push access NOT verified — forge.open_change_request will fail to push branches until fixed")
			continue
		}
		c.Logger.Info().Str("project_id", p.ID).Msg("forge: push access verified")
	}
}

// newForgeClassifier builds the webhook forge-job classifier, or nil when there's
// no project registry.
func (c *Container) newForgeClassifier() *forgeWebhookClassifier {
	r := c.newForgeResolver()
	if r == nil {
		return nil
	}
	return &forgeWebhookClassifier{resolver: r}
}
