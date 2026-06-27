package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/projectwizard"
	"vornik.io/vornik/internal/templates"
)

// resolveWizardModel returns the model the conversational project
// wizard should use: chat.wizard_model when set, otherwise "" so the
// chat provider falls back to its own default (chat.model / router
// fallback) — the historical behaviour.
func resolveWizardModel(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Chat.WizardModel)
}

// fsProjectWriter implements projectwizard.ProjectWriter by
// writing the proposed project.yaml under the daemon's deployed
// configs root. Same atomic-ish pattern the template gallery's
// MaterialiseFiles handler uses: stat for collision, then
// MkdirAll + WriteFile.
//
// After writing, it triggers a synchronous config reload (when wired)
// so the just-created project is registered in-memory BEFORE the
// commit endpoint returns its /ui/projects/{id} redirect. Without this
// the redirect raced the async file-watcher and rendered "project not
// found" until the watcher fired (or the daemon restarted) — the
// 2026-05-30 "created project not picked up; restart fixes it" bug. The
// watcher remains the fallback for no-reloader deployments.
type fsProjectWriter struct {
	configsDir string       // typically ~/.config/vornik/configs
	reload     func() error // synchronous registry reload; nil = rely on watcher
}

func newFSProjectWriter(configsDir string, reload func() error) projectwizard.ProjectWriter {
	if configsDir == "" {
		return nil
	}
	return &fsProjectWriter{configsDir: configsDir, reload: reload}
}

func (w *fsProjectWriter) Write(_ context.Context, projectID string, body []byte) (string, error) {
	if w == nil || w.configsDir == "" {
		return "", errors.New("project writer not configured")
	}
	if !safeProjectConfigID(projectID) {
		return "", fmt.Errorf("invalid project id %q", projectID)
	}
	target := filepath.Join(w.configsDir, "projects", projectID+".yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	// 0o600 — project YAML can carry LLM/MCP credentials inline;
	// no other user on the host needs read access. The daemon
	// owns the file and reads it back on reload.
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("project file %s already exists", target)
		}
		return "", fmt.Errorf("write: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	// Register the new project in-memory NOW so the commit redirect to
	// /ui/projects/{id} resolves immediately (the file-watcher is async
	// and the redirect would otherwise race it → "project not found").
	// Best-effort: the file is already on disk, so a reload failure
	// leaves the watcher fallback intact — don't fail the commit.
	// ConfigReloader.Reload logs its own errors.
	if w.reload != nil {
		_ = w.reload()
	}
	return "/ui/projects/" + projectID, nil
}

// WriteFiles lands a full rendered template file set (project.yaml +
// swarm.md + any others) below the configs root, refusing if any
// target already exists, then triggers the same synchronous reload
// as Write so the new project is registered before the commit
// redirect resolves. This is the multi-file path the template-
// anchored wizard commit uses — identical to the gallery's
// WriteRenderedFilesExclusive contract.
func (w *fsProjectWriter) WriteFiles(_ context.Context, projectID string, files map[string]string) (string, error) {
	if w == nil || w.configsDir == "" {
		return "", errors.New("project writer not configured")
	}
	if !safeProjectConfigID(projectID) {
		return "", fmt.Errorf("invalid project id %q", projectID)
	}
	if len(files) == 0 {
		return "", errors.New("no files to write")
	}
	if _, err := templates.WriteRenderedFilesExclusive(w.configsDir, files); err != nil {
		return "", err
	}
	// Register the new project in-memory now (see Write) so the commit
	// redirect to /ui/projects/{id} resolves immediately instead of
	// racing the async file-watcher. Best-effort — files are on disk.
	if w.reload != nil {
		_ = w.reload()
	}
	return "/ui/projects/" + projectID, nil
}

func safeProjectConfigID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

// projectWizardAdapter satisfies api.ProjectWizard by wrapping a
// *projectwizard.Wizard. The two packages don't share envelope
// types (the api surface carries its own JSON-shaped types) so
// the adapter does the field-by-field translation in one place.
// Keeps the api package free of an import on projectwizard and
// lets the wire contract evolve independently of the internal
// type.
type projectWizardAdapter struct {
	wizard *projectwizard.Wizard
}

func newProjectWizardAdapter(w *projectwizard.Wizard) api.ProjectWizard {
	if w == nil {
		return nil
	}
	return &projectWizardAdapter{wizard: w}
}

// buildProjectWizardOrNil constructs the wizard from the
// container's existing dependencies. Returns nil (handler 503s)
// when:
//   - chat router is missing (no LLM to call)
//   - project wizard sessions repo is missing (no place to persist)
//
// Template priors are loaded best-effort from
// configs/project-templates/ relative to the daemon's working
// directory. An empty catalog is fine — the wizard runs without
// suggested-template hints.
func buildProjectWizardOrNil(c *Container) api.ProjectWizard {
	if c == nil || c.repos == nil {
		return nil
	}
	if c.repos.ProjectWizardSessions == nil {
		return nil
	}
	if c.ChatClient == nil {
		return nil
	}
	// Template priors. Same resolution rules as the gallery loader
	// in container_http.go: configs/project-templates relative to
	// the daemon's config root, override via VORNIK_TEMPLATES_DIR.
	// An empty / failed catalog is fine — the wizard just runs
	// without suggested-template hints.
	var priors []projectwizard.TemplatePrior
	var templateSource projectwizard.TemplateSource
	templatesDir := ""
	if configsDir := resolveRegistryConfigDir(c.ConfigPath); configsDir != "" {
		templatesDir = filepath.Join(configsDir, "project-templates")
	}
	if env := os.Getenv("VORNIK_TEMPLATES_DIR"); env != "" {
		templatesDir = env
	}
	if templatesDir != "" {
		if cat, err := templates.Load(templatesDir); err == nil {
			priors = projectwizard.BuildPriors(cat)
			// Same catalog anchors the commit path: a wizard project is
			// materialised from the matched template exactly like a
			// gallery one (project.yaml + swarm.md), so it loads and runs
			// rather than depending on the LLM to author a valid swarmId.
			templateSource = catalogTemplateSource{cat: cat}
		}
	}

	wiz := &projectwizard.Wizard{
		Sessions: c.repos.ProjectWizardSessions,
		Chat:     c.ChatClient,
		// Model override: chat.wizard_model when set, else "" (the chat
		// provider's own default — historical behaviour). Lets operators
		// point the wizard at a large-context / unthrottled model when
		// the dispatcher default isn't a good fit for the wizard's
		// per-turn prompt size.
		Model:             resolveWizardModel(c.Config),
		Priors:            priors,
		LLMUsage:          c.repos.LLMUsage,
		Validator:         projectwizard.RegistryValidator{},
		Templates:         templateSource,
		Metrics:           c.projectWizardMetrics,
		MaxActiveSessions: 5,
		MaxTurns:          20,
	}
	// Project writer — Phase B commit endpoint. Resolved from the
	// daemon's configs root; without it Commit returns
	// ErrWriterUnwired (handler 503s). The reload closure is lazy over
	// c.ConfigReloader so wiring order doesn't matter and a no-reloader
	// deployment degrades to the file-watcher.
	if configsDir := resolveRegistryConfigDir(c.ConfigPath); configsDir != "" {
		wiz.Writer = newFSProjectWriter(configsDir, func() error {
			if c.ConfigReloader == nil {
				return nil
			}
			return c.ConfigReloader.Reload()
		})
	}

	// Log the EFFECTIVE wizard model at startup so "did chat.wizard_model
	// actually load?" is answerable from the journal without debug-level
	// router tracing. wizard_model="" means the daemon read a config
	// without the key (or it's unset) and the wizard inherits chat.model
	// — the usual cause of a wizard that ignores the configured override.
	effective := wiz.Model
	inherited := wiz.Model == ""
	if inherited && c.Config != nil {
		effective = c.Config.Chat.Model
	}
	c.Logger.Info().
		Str("wizard_model", wiz.Model).
		Str("effective_model", effective).
		Bool("inherits_chat_model", inherited).
		Msg("project wizard wired")
	return newProjectWizardAdapter(wiz)
}

// catalogTemplateSource adapts the shared templates.Catalog to the
// narrow projectwizard.TemplateSource seam, so the wizard can anchor
// a committed proposal on the same vetted templates the gallery
// renders without the projectwizard package importing the templates
// concrete types directly.
type catalogTemplateSource struct {
	cat *templates.Catalog
}

func (s catalogTemplateSource) Lookup(slug string) (projectwizard.TemplateSpec, bool) {
	if s.cat == nil {
		return projectwizard.TemplateSpec{}, false
	}
	m, ok := s.cat.Get(slug)
	if !ok {
		return projectwizard.TemplateSpec{}, false
	}
	params := make([]projectwizard.TemplateParamSpec, 0, len(m.Parameters))
	for _, p := range m.Parameters {
		params = append(params, projectwizard.TemplateParamSpec{
			Name:    p.Name,
			Type:    p.Type,
			Options: p.Options,
		})
	}
	return projectwizard.TemplateSpec{Slug: m.Slug, Params: params}, true
}

func (s catalogTemplateSource) Materialise(slug string, params map[string]string) (map[string]string, error) {
	if s.cat == nil {
		return nil, fmt.Errorf("template catalog not configured")
	}
	m, ok := s.cat.Get(slug)
	if !ok {
		return nil, fmt.Errorf("unknown template %q", slug)
	}
	return s.cat.MaterialiseFiles(m, params)
}

func (a *projectWizardAdapter) Commit(ctx context.Context, sessionID, operatorID string) (*api.ProjectWizardCommitResult, error) {
	res, err := a.wizard.Commit(ctx, sessionID, operatorID)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	return &api.ProjectWizardCommitResult{
		SessionID: res.SessionID,
		ProjectID: res.ProjectID,
		URL:       res.URL,
	}, nil
}

func (a *projectWizardAdapter) Cancel(ctx context.Context, sessionID, operatorID string) error {
	return a.wizard.Cancel(ctx, sessionID, operatorID)
}

func (a *projectWizardAdapter) Converse(ctx context.Context, sessionID, operatorID, userMessage string) (*api.ProjectWizardResult, error) {
	res, err := a.wizard.Converse(ctx, sessionID, operatorID, userMessage)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	apiResult := &api.ProjectWizardResult{
		SessionID: res.SessionID,
	}
	if res.Envelope != nil {
		apiResult.Envelope = &api.ProjectWizardEnvelope{
			Message:           res.Envelope.Message,
			ReadyToCommit:     res.Envelope.ReadyToCommit,
			SuggestedTemplate: res.Envelope.SuggestedTemplate,
			OpenQuestions:     res.Envelope.OpenQuestions,
		}
		if res.Envelope.Proposal != nil {
			apiResult.Envelope.Proposal = res.Envelope.Proposal.Raw
		}
	}
	return apiResult, nil
}
