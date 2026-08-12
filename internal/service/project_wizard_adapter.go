package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/projectdoctor"
	"vornik.io/vornik/internal/projectwizard"
	"vornik.io/vornik/internal/rolelibrary"
	"vornik.io/vornik/internal/templates"
)

// configReloaderAdapter adapts *config.ConfigReloader.TryReload to the
// narrow projectwizard.Reloader seam (design §5.6 step 5; task 1.2b
// slice ii). Maps every non-applied ReloadOutcome to a non-nil error
// so the commit path's rollback reason is never blank: ReloadBlocked
// carries the activation-blocked error already returned by TryReload;
// ReloadDeferred (busy/wedged/timed out — TryReload never blocks past
// d, see watcher.go's own doc) has no underlying error, so a
// descriptive one is synthesized; ReloadFailed carries the hard
// validate/activate error TryReload returned.
type configReloaderAdapter struct {
	reloader *config.ConfigReloader
}

func (a configReloaderAdapter) TryReload(d time.Duration) (bool, error) {
	outcome, err := a.reloader.TryReload(d)
	switch outcome {
	case config.ReloadApplied:
		return true, nil
	case config.ReloadDeferred:
		return false, fmt.Errorf("reload did not complete within %s (daemon busy or a reload was already in flight)", d)
	default: // config.ReloadBlocked, config.ReloadFailed
		return false, err
	}
}

// composerRecoveryDoctorAdapter adapts projectwizard's leftover-
// journal scan to the project-doctor's ComposerRecovery seam (design
// §5.6 step 4's project-doctor surfacing leg — internal/projectdoctor
// cannot import internal/projectwizard directly without risking an
// import cycle with the rest of its Deps-only contract, so this
// narrow adapter lives in the wiring layer instead).
type composerRecoveryDoctorAdapter struct {
	liveConfigDir string
}

func (a composerRecoveryDoctorAdapter) LeftoverJournal(projectID string) (bool, string) {
	if a.liveConfigDir == "" {
		return false, ""
	}
	lj, found, err := projectwizard.FindLeftoverJournalForProject(a.liveConfigDir, projectID)
	if err != nil || !found {
		return false, ""
	}
	if lj.ProjectFileLive(a.liveConfigDir) {
		return true, "A composer commit for this project fully landed, but its staging cleanup was interrupted (a daemon restart, no data at risk). It will finish automatically on the next daemon restart."
	}
	return true, "A composer commit for this project was interrupted mid-write and never activated. The next daemon restart will roll it back automatically; the session stays resumable."
}

// newComposerRecoveryChecker builds the project-doctor's
// ComposerRecovery dependency from the container's resolved live
// config dir. Always returns a non-nil checker (degrading to
// "not found" for every project) when no live config dir is wired, so
// callers never need a nil-guard around it.
func newComposerRecoveryChecker(c *Container) projectdoctor.ComposerRecovery {
	return composerRecoveryDoctorAdapter{liveConfigDir: resolveRegistryConfigDir(c.ConfigPath)}
}

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
	return "/ui/projects/" + projectID + "/setup", nil
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
	return "/ui/projects/" + projectID + "/setup", nil
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

// buildProjectWizardOrNil constructs the wizard from the container's
// existing dependencies and wraps it for the api package. Returns nil
// (handler 503s) exactly when buildProjectWizard returns nil — see
// its doc comment for the gating conditions.
func buildProjectWizardOrNil(c *Container) api.ProjectWizard {
	return newProjectWizardAdapter(buildProjectWizard(c))
}

// buildProjectWizard constructs the concrete *projectwizard.Wizard
// from the container's existing dependencies. Returns nil when:
//   - chat router is missing (no LLM to call)
//   - project wizard sessions repo is missing (no place to persist)
//
// Exported to the service package (not just api-facing) so the
// dispatcher's compose_automation bridge (task 1.4;
// composer_bridge.go) can wrap the SAME typed Wizard — with typed
// access to Envelope.Bundle.Plan — that the HTTP API wraps via
// buildProjectWizardOrNil above. Both call sites reconstruct a fresh
// *projectwizard.Wizard from the container's shared repos/config
// rather than caching one on the Container: the wizard itself holds
// no per-instance mutable state (every field is a repo/config
// reference), so two instances behave identically, and this mirrors
// the pre-existing pattern of initHTTPServer running twice across the
// pre-/post-observability boot passes (see the "TWO-PASS TRAP" note
// elsewhere in this package) and rebuilding its own wizard each time.
//
// Template priors are loaded best-effort from
// configs/project-templates/ relative to the daemon's working
// directory. An empty catalog is fine — the wizard runs without
// suggested-template hints.
func buildProjectWizard(c *Container) *projectwizard.Wizard { //nolint:funlen // pre-existing body moved verbatim from buildProjectWizardOrNil (task 1.4 rename-only refactor); no new complexity added
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
	var templateMeta projectwizard.TemplateMetaLookup
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
			templateMeta = newTemplateMetaLookup(cat, templatesDir)
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
		Spend:             c.llmSpend("project_wizard", projectwizard.RoleProjectWizard),
		Validator:         projectwizard.RegistryValidator{},
		Templates:         templateSource,
		TemplateMeta:      templateMeta,
		Metrics:           c.projectWizardMetrics,
		MaxActiveSessions: 5,
		MaxTurns:          20,
	}
	// NL Automation Composer (task 1.1b) wiring: the tier-3 engine's
	// staged validation layers a synthesized bundle over the SAME live
	// configs root the writer/templates above use, and the role library
	// it materializes composed roles against. composer.enabled is the
	// feature-doctor gate (internal/featuredoctor/feature_composer.go);
	// wiring the deps here regardless of that gate is harmless — the
	// wizard only ever reaches the tier-3 branch when the LLM itself
	// emits tier=3, and composer.max_tier still caps that fleet-wide.
	if configsDir := resolveRegistryConfigDir(c.ConfigPath); configsDir != "" {
		wiz.LiveConfigDir = configsDir
		if archetypes, err := rolelibrary.Load(configsDir); err == nil {
			roleLib := make(map[string]*rolelibrary.RoleArchetype, len(archetypes))
			for _, a := range archetypes {
				roleLib[a.ArchetypeID] = a
			}
			wiz.RoleLibrary = roleLib
		} else {
			c.Logger.Warn().Err(err).Msg("composer: role-library load failed; tier-3 turns will fail materialization until fixed")
		}
	}
	if c.Config != nil {
		wiz.Composer = c.Config.Composer
	}
	// Whole-branch review C1 fix: the tier-3 system prompt's step-
	// vocabulary grounding (design §5.3) names the daemon's actual
	// registered system-step handlers, not just the agent/gate/approval
	// kinds. c.systemHandlerNames is set by initScheduler (before this
	// initHTTPServer pass runs) — the SAME snapshot
	// api.DoctorHandlers.SetSystemHandlerNames already receives for the
	// role-library doctor check.
	wiz.SystemHandlerNames = c.systemHandlerNames
	// Hot-reload trigger (design §5.6 step 5, task 1.2b slice ii): the
	// SAME *config.ConfigReloader instance api.NewConfigHandlers and the
	// doctor reloader already wrap (container_http.go). nil c.ConfigReloader
	// (CE/minimal wiring, or a boot ordering where the registry never
	// initialised) leaves wiz.Reloader nil — commitBundleSession's
	// documented degradation: files still land, no synchronous reload
	// triggered, no hard failure.
	if c.ConfigReloader != nil {
		wiz.Reloader = configReloaderAdapter{reloader: c.ConfigReloader}
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
		// Ledger-gated commit path: when the control-plane proposal store is
		// wired, route commit through it — the composed file set becomes a
		// reviewable DRAFT scaffold proposal instead of a direct disk write
		// (reusing the shipped Phase-2b apply/rollback engine). The Writer
		// above stays as the CE fallback for deployments without the ledger.
		if c.repos != nil && c.repos.Proposals != nil {
			wiz.Proposer = newScaffoldProposer(c.repos.Proposals, c.ConfigPath, configsDir)
		}
	}

	// Wizard v2 grounding deps — same live daemon state Phase 1/2
	// already wired for the template gallery / project doctor
	// (container_http.go's mcpServerNamesFn/modelIDsFn and
	// doctorMCP). Without these the wizard still runs (BuildGrounding
	// and composeFromEnvelope are nil-safe on MCP/Models/KnownMCP —
	// see wizard.go), it just degrades to the addon-vocab-only
	// grounding block and an empty KnownMCP set.
	if c.mcpRegistry != nil {
		// MCP servers + tools grounding block, and the known-server set
		// the commit-time compose engine's mcp_server applier checks a
		// proposed addon's server name against.
		wiz.MCP = &wizardMCPGroundingAdapter{registry: c.mcpRegistry}
		wiz.KnownMCP = wizardKnownMCPServers(c.mcpRegistry)
	}
	// c.ChatClient is guaranteed non-nil here — buildProjectWizardOrNil
	// already returned early above when it was nil — so the model
	// catalog grounding is always wired alongside the wizard itself.
	wiz.Models = wizardModelLister{provider: c.ChatClient}
	// Resolver: same optionsFrom(mcp_registry)/optionsFrom(models)
	// resolver the project-template gallery uses, needed at commit
	// time when composeFromEnvelope materialises a template's
	// dynamic-select parameters (ComposeDeps.Resolver). Degrades
	// per-source (not to nil) when a subsystem is unwired — see
	// buildTemplateOptionsResolver.
	wiz.Resolver = buildTemplateOptionsResolver(c)

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
	return wiz
}

// catalogTemplateSource adapts the shared templates.Catalog to the
// narrow projectwizard.TemplateSource seam, so the wizard can anchor
// a committed proposal on the same vetted templates the gallery
// renders without the projectwizard package importing the templates
// concrete types directly.
type catalogTemplateSource struct {
	cat *templates.Catalog
}

// newTemplateMetaLookup builds the convergence normalizer's TemplateMeta
// dependency: a base template slug → its declared params (from the
// manifest) + its declared autonomy block (scanned from the projects/*.yaml
// template source). Returns nil when no catalog is loaded. baseAutonomy
// degrades to disabled when the autonomy block can't be read — the safe
// default that lets the common (disabled-base) case compose.
func newTemplateMetaLookup(cat *templates.Catalog, templatesDir string) projectwizard.TemplateMetaLookup {
	if cat == nil {
		return nil
	}
	return func(slug string) ([]projectwizard.TemplateParam, projectwizard.BaseAutonomy, bool) {
		m, ok := cat.Get(slug)
		if !ok {
			return nil, projectwizard.BaseAutonomy{}, false
		}
		params := make([]projectwizard.TemplateParam, 0, len(m.Parameters))
		for _, p := range m.Parameters {
			params = append(params, projectwizard.TemplateParam{
				Name:     p.Name,
				Required: p.Required,
				Default:  p.Default,
			})
		}
		return params, readBaseAutonomy(templatesDir, slug, m), true
	}
}

// readBaseAutonomy scans a template's project YAML source for its declared
// autonomy `enabled`/`mode`. The source is a Go text/template (contains
// `{{ }}`), so it can't be yaml-parsed wholesale; the autonomy enabled/mode
// values are literals in practice, so a small block scan is robust. Missing
// or unreadable → zero value (disabled), the safe default.
func readBaseAutonomy(templatesDir, slug string, m templates.Manifest) projectwizard.BaseAutonomy {
	var src string
	for _, f := range m.Files {
		if strings.Contains(f.Target, "projects/") && strings.HasSuffix(f.Target, ".yaml") {
			src = f.Source
			break
		}
	}
	if src == "" || templatesDir == "" {
		return projectwizard.BaseAutonomy{}
	}
	body, err := os.ReadFile(filepath.Join(templatesDir, slug, src))
	if err != nil {
		return projectwizard.BaseAutonomy{}
	}
	return scanAutonomyBlock(string(body))
}

// scanAutonomyBlock extracts enabled/mode from a top-level `autonomy:`
// mapping in project-YAML template text, tolerating interleaved
// `{{ }}`-templated lines it doesn't understand.
func scanAutonomyBlock(body string) projectwizard.BaseAutonomy {
	var out projectwizard.BaseAutonomy
	inBlock := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inBlock {
			if trimmed == "autonomy:" {
				inBlock = true
			}
			continue
		}
		// A non-indented, non-empty line ends the block.
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			break
		}
		switch {
		case strings.HasPrefix(trimmed, "enabled:"):
			out.Enabled = strings.TrimSpace(strings.TrimPrefix(trimmed, "enabled:")) == "true"
		case strings.HasPrefix(trimmed, "mode:"):
			out.Mode = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "mode:")), `"'`)
		}
	}
	return out
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

// MaterialiseMulti renders the template with multi-value params (the
// wizard v2 composition path). Mirrors Materialise but calls the
// catalog's MaterialiseFilesMulti so list/multiselect/optionsFrom
// params work. Satisfies projectwizard.MultiMaterialiser.
func (s catalogTemplateSource) MaterialiseMulti(slug string, params map[string][]string, resolver templates.OptionsResolver) (map[string]string, error) {
	if s.cat == nil {
		return nil, fmt.Errorf("template catalog not configured")
	}
	m, ok := s.cat.Get(slug)
	if !ok {
		return nil, fmt.Errorf("unknown template %q", slug)
	}
	return s.cat.MaterialiseFilesMulti(m, params, resolver)
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
		SessionID:  res.SessionID,
		ProjectID:  res.ProjectID,
		ProposalID: res.ProposalID,
		URL:        res.URL,
	}, nil
}

func (a *projectWizardAdapter) Cancel(ctx context.Context, sessionID, operatorID string) error {
	return a.wizard.Cancel(ctx, sessionID, operatorID)
}

func (a *projectWizardAdapter) ConfirmSchedule(ctx context.Context, sessionID, operatorID, cron string) error {
	return a.wizard.ConfirmSchedule(ctx, sessionID, operatorID, cron)
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
		if res.Envelope.Composition != nil {
			apiResult.Envelope.Composition = toAPIComposition(res.Envelope.Composition)
		}
		if res.Envelope.Bundle != nil {
			apiResult.Envelope.Bundle = toAPIBundle(res.Envelope.Bundle)
		}
	}
	return apiResult, nil
}

// toAPIComposition mirrors a projectwizard.Composition into the API's
// JSON-generic api.WizardComposition. Round-trips through JSON rather
// than walking each field so it reuses projectwizard.Addon's own
// MarshalJSON (which reproduces the addon's verbatim JSON object —
// see the long comment on Addon.MarshalJSON) instead of re-deriving a
// shape that would drop every addon field but "type".
func toAPIComposition(c *projectwizard.Composition) *api.WizardComposition {
	if c == nil {
		return nil
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return &api.WizardComposition{Template: c.Template}
	}
	out := &api.WizardComposition{}
	if err := json.Unmarshal(raw, out); err != nil {
		return &api.WizardComposition{Template: c.Template}
	}
	return out
}

// toAPIBundle mirrors a projectwizard.ComposedBundle into the API's
// JSON-generic map[string]any, the same round-trip-through-JSON
// approach as toAPIComposition (rather than walking each field), so
// the browser sees the exact nested shape (bundle.project,
// bundle.swarm, bundle.workflows, bundle.plan.steps,
// bundle.plan.schedule, bundle.plan.cost_band, bundle.plan.approvals,
// bundle.plan.approvals_bypassed) that ComposedBundle/ComposedPlan's
// own json tags define, without this package needing a typed mirror
// struct. Returns nil (never an empty map) on nil input or a marshal/
// unmarshal failure so the caller's omitempty keeps the field out of
// the response rather than emitting a misleading empty object.
func toAPIBundle(b *projectwizard.ComposedBundle) map[string]any {
	if b == nil {
		return nil
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
