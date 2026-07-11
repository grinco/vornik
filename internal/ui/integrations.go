package ui

// Guided Integrations Hub — server-rendered UI + HTTP handlers (design doc:
// https://docs.vornik.io, §5.1, §5.2, §5.5, §5.6,
// §5.7, §5.8, §6; task 5.3). Builds on the committed probe layer + write
// path in internal/integrations (Registry, Prober, Save, SaveTargetForKind)
// — this file is the thin HTTP/template layer over that package: it never
// re-implements probing, field-splitting, or the transactional config
// patch, and it never lets a caller reach a scope Save's own Caller.
// Authorized check would refuse (§6) — every route funnels through the
// SAME integrationsCaller helper Save's caller argument is built from, so
// the UI-side gate and the save-service's own authorization cannot drift.
//
// Metrics (vornik_integration_probe_total / _save_total, integrations_
// metrics.go) and external deep-links (setup/project-doctor/admin "Edit in
// hub" links) are task 5.4.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/registry"
)

// integrationsRegistryFunc builds the Integrations Hub catalog. Package var
// (mirrors mcpProbeConnect's injection seam) so tests substitute a registry
// with a fake Prober on one kind instead of dialing real providers, while
// still exercising the real Fields/SaveTarget shape via
// integrations.Registry itself.
var integrationsRegistryFunc = integrations.Registry

// integrationsCaller resolves the integrations.Caller for r — the ONE
// scope-resolution helper shared by the catalog GET and every write POST
// (design §6), so they cannot drift from each other or from Save's own
// authorization check. An unscoped request (admin session, auth-disabled
// install, or an unscoped API key — api.RequestScopedProjects' `ok=false`)
// is all-access; a scoped request (project-scoped session or API key,
// including an awaiting-access session stamped with the no-access
// sentinel project) is limited to exactly its allow-set.
func integrationsCaller(r *http.Request) integrations.Caller {
	scoped, ok := api.RequestScopedProjects(r)
	if !ok {
		return integrations.Caller{IsAdmin: true}
	}
	return integrations.Caller{ScopedProjectIDs: scoped}
}

// integrationsDialGuard builds the shared SSRF guard every Prober dials
// through (design §6), reading the opt-in integrations.allowed_hosts list
// from config (task 5.4). An unset/nil onboardingDetector.Config (or an
// absent/empty allowed_hosts) yields a DialGuard with no allowlist — the
// secure default that blocks RFC1918/loopback/link-local outright.
func (s *Server) integrationsDialGuard() integrations.DialGuard {
	cfg := s.onboardingDetector.Config
	if cfg == nil {
		return integrations.DialGuard{}
	}
	return integrations.DialGuard{AllowedHosts: cfg.Integrations.AllowedHosts}
}

func (s *Server) integrationsRegistry() []integrations.IntegrationKind {
	return integrationsRegistryFunc(s.integrationsDialGuard())
}

func findIntegrationKind(kinds []integrations.IntegrationKind, id string) (integrations.IntegrationKind, bool) {
	for _, k := range kinds {
		if k.ID == id {
			return k, true
		}
	}
	return integrations.IntegrationKind{}, false
}

// integrationsReloaderAdapter adapts ui.ConfigReloader (Reload() error) to
// featuredoctor.Reloader (Reload(ctx) error), mirroring
// api.configReloaderAdapter (internal/api/featuredoctor_enable_handler.go)
// — the underlying reload is synchronous and bounded internally, so ctx is
// accepted but ignored.
type integrationsReloaderAdapter struct{ r ConfigReloader }

func (a integrationsReloaderAdapter) Reload(_ context.Context) error { return a.r.Reload() }

func (s *Server) integrationsReloader() featuredoctor.Reloader {
	if s.configReloader == nil {
		return nil
	}
	return integrationsReloaderAdapter{r: s.configReloader}
}

// integrationsReloadStatusReader is the optional capability
// *config.ConfigReloader exposes (Status() config.ReloadStatus). Consulted
// via type-assertion, mirroring the existing boundedReloader optional-
// capability pattern (config_reload.go) — test fakes need not implement it.
type integrationsReloadStatusReader interface {
	Status() config.ReloadStatus
}

func (s *Server) integrationsReloadStatus() integrations.ReloadStatusChecker {
	if r, ok := s.configReloader.(integrationsReloadStatusReader); ok {
		return r
	}
	return nil
}

// integrationProbeCacheEntry is one cached probe store entry — the shape
// design §5.5 calls for (map[kind+project]ProbeResult + timestamp),
// mirroring mcpRegistry.Snapshot / the project doctor's in-memory smoke map.
type integrationProbeCacheEntry struct {
	Result integrations.ProbeResult
	At     time.Time
}

func integrationProbeCacheKey(kindID, projectID string) string { return kindID + "|" + projectID }

func (s *Server) cachedIntegrationProbe(kindID, projectID string) (integrationProbeCacheEntry, bool) {
	s.integrationProbesMu.Lock()
	defer s.integrationProbesMu.Unlock()
	e, ok := s.integrationProbes[integrationProbeCacheKey(kindID, projectID)]
	return e, ok
}

// integrationProbeCacheTTL bounds how long a cached probe entry survives
// before storeIntegrationProbe sweeps it out — companion review
// review-20260710-05a2 finding I1: the map has no eviction, so a
// long-uptime daemon touched by many (kind, project) combinations grows
// it unboundedly. 24h comfortably outlives any single operator session
// while still bounding steady-state growth.
const integrationProbeCacheTTL = 24 * time.Hour

// integrationProbeCacheMaxEntries is a belt-and-braces cap independent of
// TTL — a burst of saves/rechecks within the TTL window shouldn't be
// allowed to grow the map unboundedly either. Comfortably above any
// realistic (kind × project) cardinality for a single-node deployment.
const integrationProbeCacheMaxEntries = 1000

// storeIntegrationProbe caches result for (kindID, projectID) — populated on
// save + explicit re-check ONLY (design §5.5: never on catalog page load,
// and no background prober in v1). Every write sweeps stale entries and,
// if the map is still oversized, evicts the oldest remainder (finding I1).
func (s *Server) storeIntegrationProbe(kindID, projectID string, result integrations.ProbeResult) {
	s.integrationProbesMu.Lock()
	defer s.integrationProbesMu.Unlock()
	if s.integrationProbes == nil {
		s.integrationProbes = map[string]integrationProbeCacheEntry{}
	}
	now := time.Now()
	s.integrationProbes[integrationProbeCacheKey(kindID, projectID)] = integrationProbeCacheEntry{Result: result, At: now}
	evictStaleIntegrationProbes(s.integrationProbes, now)
}

// evictStaleIntegrationProbes drops entries older than
// integrationProbeCacheTTL, then — if the map is still over
// integrationProbeCacheMaxEntries — evicts the oldest remaining entries
// until it's back within the cap. Callers must hold integrationProbesMu.
func evictStaleIntegrationProbes(m map[string]integrationProbeCacheEntry, now time.Time) {
	for key, entry := range m {
		if now.Sub(entry.At) > integrationProbeCacheTTL {
			delete(m, key)
		}
	}
	excess := len(m) - integrationProbeCacheMaxEntries
	if excess <= 0 {
		return
	}
	type keyedAt struct {
		key string
		at  time.Time
	}
	byAge := make([]keyedAt, 0, len(m))
	for key, entry := range m {
		byAge = append(byAge, keyedAt{key, entry.At})
	}
	sort.Slice(byAge, func(i, j int) bool { return byAge[i].at.Before(byAge[j].at) })
	for i := 0; i < excess; i++ {
		delete(m, byAge[i].key)
	}
}

// integrationsRouter dispatches /integrations/{kind}[/{action}] — mirrors
// projectRouter's bare-path-strip convention.
func (s *Server) integrationsRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path[len("/integrations/"):], "/")
	if path == "" {
		s.IntegrationsCatalog(w, r)
		return
	}
	parts := strings.SplitN(path, "/", 2)
	kindID := parts[0]
	if len(parts) == 1 {
		s.IntegrationForm(w, r, kindID)
		return
	}
	switch parts[1] {
	case "probe":
		s.IntegrationProbe(w, r, kindID)
	case "save":
		s.IntegrationSave(w, r, kindID)
	case "recheck":
		s.IntegrationRecheck(w, r, kindID)
	case "assist":
		s.IntegrationAssist(w, r, kindID)
	default:
		http.NotFound(w, r)
	}
}

// --- GET /integrations — catalog + health tiles (design §5.8) ---

// IntegrationTileVM is one catalog row: a kind, possibly scoped to a
// project. The status badge is a CHEAP, no-network read (config presence +
// the cached probe store) — never a live probe (design §5.5).
type IntegrationTileVM struct {
	KindID       string
	DisplayName  string
	Category     string
	ProjectID    string
	ProjectBadge bool
	Configured   bool
	// Status is "unconfigured" | "unknown" (configured, never probed) |
	// "ok" | "fail" | "error" (the cached Outcome).
	Status      string
	Summary     string
	CheckedAt   string
	FormHref    string
	RecheckHref string
	// CanRecheck gates the tile's Re-check button on Configured. (The
	// former MCP exception left with the MCP kind itself, 2026-07-10 —
	// see integrations.Registry's doc.)
	CanRecheck bool
	// FixItHref (task 3.4, fix-it-doctor-design.md §5.5/§7) deep-links
	// the Fix-It Doctor's red_integration panel. Populated ONLY when
	// BOTH a cached probe exists with Outcome != ok AND the Fix-It
	// Doctor is wired on this deployment (s.fixItDoctor != nil) — the
	// design's §7 "Integration hub not yet deployed" failure mode
	// requires the button be HIDDEN entirely when the fix-it surface
	// isn't present, never shown-then-404. Empty for "unconfigured" /
	// "unknown" (no probe has run yet — nothing to ground a repair
	// chat on) and for "ok".
	FixItHref string
}

// IntegrationsCatalogData backs integrations_catalog.html.
type IntegrationsCatalogData struct {
	Title       string
	CurrentPage string
	IsAdmin     bool
	Tiles       []IntegrationTileVM
}

func integrationFormHref(kindID, projectID string) string {
	if projectID == "" {
		return "/ui/integrations/" + kindID
	}
	// url.QueryEscape (companion review-20260710-05a2 finding M2): projectID
	// is operator-controlled (a project slug) but not validated against a
	// URL-safe charset here, so escape it rather than trust it's always
	// safe to concatenate raw into a query string.
	return "/ui/integrations/" + kindID + "?project=" + url.QueryEscape(projectID)
}

// integrationActionHref builds a probe/save/recheck/assist POST target. The
// project id, when relevant, travels as a hidden form field (not the URL) —
// every write handler reads it via r.FormValue("project"), so this helper
// takes no projectID parameter.
func integrationActionHref(kindID, action string) string {
	return "/ui/integrations/" + kindID + "/" + action
}

// allProjectIDs lists every project the registry knows, sorted — used to
// build the admin's cross-project catalog rows.
func (s *Server) allProjectIDs() []string {
	if s.projectReg == nil {
		return nil
	}
	projects := s.projectReg.ListProjects()
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		if p != nil {
			ids = append(ids, p.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *Server) integrationProject(projectID string) *registry.Project {
	if projectID == "" || s.projectReg == nil {
		return nil
	}
	return s.projectReg.GetProject(projectID)
}

// integrationConfigured is the cheap (no-network) "is this kind set up at
// all" check backing the catalog badge, reusing the real registry.Project
// sub-struct Enabled() methods where they exist (design §5.5).
func (s *Server) integrationConfigured(kindID, projectID string) bool {
	switch kindID {
	case "telegram":
		cfg := s.onboardingDetector.Config
		return cfg != nil && strings.TrimSpace(cfg.Telegram.BotToken) != ""
	case "email":
		proj := s.integrationProject(projectID)
		return proj != nil && proj.Email.Enabled()
	case "slack":
		proj := s.integrationProject(projectID)
		return proj != nil && proj.Slack.Enabled()
	case "github_app":
		proj := s.integrationProject(projectID)
		return proj != nil && proj.GitHubApp.Enabled()
	default:
		return false
	}
}

func (s *Server) integrationTile(k integrations.IntegrationKind, projectID string) IntegrationTileVM {
	vm := IntegrationTileVM{
		KindID:       k.ID,
		DisplayName:  k.DisplayName,
		Category:     k.Category,
		ProjectID:    projectID,
		ProjectBadge: k.Scope == integrations.ScopeProject,
		Configured:   s.integrationConfigured(k.ID, projectID),
		FormHref:     integrationFormHref(k.ID, projectID),
		RecheckHref:  integrationActionHref(k.ID, "recheck"),
	}
	vm.CanRecheck = vm.Configured
	if entry, ok := s.cachedIntegrationProbe(k.ID, projectID); ok {
		vm.Status = string(entry.Result.Outcome)
		vm.Summary = entry.Result.Summary
		vm.CheckedAt = entry.At.Format(time.RFC3339)
		// No separate feature flag gates this tile: the Phase-5
		// integrations hub is itself additive/read-safe with no flag
		// of its own (integrations-hub-design.md §10), so it only
		// renders here at all when the always-available integrations
		// catalog does — s.fixItDoctor != nil (doctor wired on this
		// deployment) + a non-OK outcome IS the complete gate.
		if entry.Result.Outcome != integrations.OutcomeOK && s.fixItDoctor != nil {
			vm.FixItHref = integrationFixItHref(k.ID, projectID)
		}
	} else if vm.Configured {
		vm.Status = "unknown"
	} else {
		vm.Status = "unconfigured"
	}
	return vm
}

// integrationFixItHref builds the red_integration Fix-It Doctor deep
// link for one tile (task 3.4). Mirrors integrationFormHref's
// project-as-query-param convention — the panel route
// (/ui/fixit/{kind}/{id}) reads project via r.URL.Query().Get("project")
// exactly like every other /ui/fixit and /ui/integrations handler.
func integrationFixItHref(kindID, projectID string) string {
	if projectID == "" {
		return "/ui/fixit/red_integration/" + kindID
	}
	return "/ui/fixit/red_integration/" + kindID + "?project=" + url.QueryEscape(projectID)
}

// IntegrationsCatalog handles GET /integrations. Scope-filtered (design
// §5.8): an admin sees every kind (daemon-scope + project-scope across
// every project, project-scope rows carrying a project badge); a
// project-scoped caller sees ONLY their scoped project(s)' project-scope
// kinds — the daemon-scope kind (telegram) is omitted entirely, not
// merely disabled.
func (s *Server) IntegrationsCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	caller := integrationsCaller(r)
	data := IntegrationsCatalogData{
		Title:       "Integrations",
		CurrentPage: "integrations",
		IsAdmin:     caller.IsAdmin,
	}
	for _, k := range s.integrationsRegistry() {
		if k.Scope == integrations.ScopeDaemon {
			if !caller.IsAdmin {
				continue
			}
			data.Tiles = append(data.Tiles, s.integrationTile(k, ""))
			continue
		}
		if caller.IsAdmin {
			for _, p := range s.allProjectIDs() {
				data.Tiles = append(data.Tiles, s.integrationTile(k, p))
			}
			continue
		}
		for _, p := range caller.ScopedProjectIDs {
			data.Tiles = append(data.Tiles, s.integrationTile(k, p))
		}
	}
	s.render(w, "integrations_catalog.html", data)
}

// --- GET /integrations/{kind} — guided form (design §5.8) ---

// IntegrationFieldVM is one guided-form input. Value is only ever populated
// for non-secret fields (the Enabled()/prefill read is safe to show);
// ConfiguredSecret is a bare bool for Secret fields — the literal secret
// value NEVER lands in this struct, so it can never round-trip into the
// rendered HTML (design §6 "secrets never round-trip to the browser").
type IntegrationFieldVM struct {
	Key              string
	Label            string
	DocHint          string
	Required         bool
	Secret           bool
	List             bool
	Int              bool
	Value            string
	ConfiguredSecret bool
}

// IntegrationFormData backs integrations_form.html.
type IntegrationFormData struct {
	Title              string
	CurrentPage        string
	KindID             string
	DisplayName        string
	Category           string
	Scope              string
	DocURL             string
	ProjectID          string
	Fields             []IntegrationFieldVM
	ProbeHref          string
	SaveHref           string
	RecheckHref        string
	AssistHrefBase     string
	NeedsProjectPicker bool
	ProjectOptions     []string
	Error              string
}

// resolveIntegrationProjectID picks the project a ScopeProject kind's form
// targets: the explicit ?project= param, else the caller's sole scoped
// project (design §5.1: "defaulting to the user's single scoped project
// when there is exactly one"). Returns "" when still ambiguous (multiple
// scoped projects, or an admin with none specified) — the caller renders a
// project picker rather than guessing.
func resolveIntegrationProjectID(r *http.Request, kind integrations.IntegrationKind, caller integrations.Caller) string {
	if kind.Scope != integrations.ScopeProject {
		return ""
	}
	if explicit := strings.TrimSpace(r.URL.Query().Get("project")); explicit != "" {
		return explicit
	}
	if len(caller.ScopedProjectIDs) == 1 {
		return caller.ScopedProjectIDs[0]
	}
	return ""
}

// secretFieldConfigured reports whether kindID's Secret field key already
// has a value on disk, WITHOUT ever reading the literal secret. Daemon
// scope (telegram) holds the (loader-expanded) live value in cfg directly —
// checked for non-empty and not-still-a-"${"-placeholder — but that value
// is only ever tested here, never assigned into a template field. Project
// scope stores the bare ENV VAR NAME (never the secret) on the registry
// struct, so its presence alone is the safe "configured" signal.
func secretFieldConfigured(kindID, key string, cfg *config.Config, proj *registry.Project) bool {
	switch kindID {
	case "telegram":
		if cfg == nil {
			return false
		}
		v := strings.TrimSpace(cfg.Telegram.BotToken)
		return v != "" && !strings.HasPrefix(v, "${")
	case "email":
		if proj == nil {
			return false
		}
		switch key {
		case "imap_password_env":
			return proj.Email.IMAPPasswordEnv != ""
		case "smtp_password_env":
			return proj.Email.SMTPPasswordEnv != ""
		}
	case "slack":
		if proj == nil {
			return false
		}
		switch key {
		case "bot_token_env":
			return proj.Slack.BotTokenEnv != ""
		case "signing_secret_env":
			return proj.Slack.SigningSecretEnv != ""
		}
	case "github_app":
		if proj == nil {
			return false
		}
		switch key {
		case "private_key_path":
			return proj.GitHubApp.PrivateKeyPath != ""
		case "webhook_secret_env":
			return proj.GitHubApp.WebhookSecretEnv != ""
		}
	}
	return false
}

// nonSecretFieldValue prefills a non-Secret field from the on-disk config —
// safe to show verbatim (design: only Secret fields are ever masked).
func nonSecretFieldValue(kindID, key string, proj *registry.Project) string {
	if proj == nil {
		return ""
	}
	switch kindID {
	case "email":
		switch key {
		case "imap_host":
			return proj.Email.IMAPHost
		case "imap_port":
			return intOrEmpty(int64(proj.Email.IMAPPort))
		case "imap_username":
			return proj.Email.IMAPUsername
		case "smtp_host":
			return proj.Email.SMTPHost
		case "smtp_port":
			return intOrEmpty(int64(proj.Email.SMTPPort))
		case "smtp_username":
			return proj.Email.SMTPUsername
		case "from_address":
			return proj.Email.FromAddress
		}
	case "slack":
		if key == "team_id" {
			return proj.Slack.TeamID
		}
	case "github_app":
		switch key {
		case "app_id":
			return intOrEmpty(proj.GitHubApp.AppID)
		case "installation_id":
			return intOrEmpty(proj.GitHubApp.InstallationID)
		case "repo_allowlist":
			return strings.Join(proj.GitHubApp.RepoAllowlist, ", ")
		case "api_base_url":
			return proj.GitHubApp.APIBaseURL
		}
	}
	return ""
}

func intOrEmpty(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

// prefillIntegrationFields builds the guided form's field view-models: safe
// prefill for non-secret fields, a bare "already configured" bool (never
// the literal) for secret fields.
func (s *Server) prefillIntegrationFields(kindID string, fields []integrations.CredentialField, projectID string) []IntegrationFieldVM {
	cfg := s.onboardingDetector.Config
	proj := s.integrationProject(projectID)
	out := make([]IntegrationFieldVM, 0, len(fields))
	for _, f := range fields {
		vm := IntegrationFieldVM{
			Key: f.Key, Label: f.Label, DocHint: f.DocHint,
			Required: f.Required, Secret: f.Secret, List: f.List, Int: f.Int,
		}
		if f.Secret {
			vm.ConfiguredSecret = secretFieldConfigured(kindID, f.Key, cfg, proj)
		} else {
			vm.Value = nonSecretFieldValue(kindID, f.Key, proj)
		}
		out = append(out, vm)
	}
	return out
}

// IntegrationForm handles GET /integrations/{kind} — the guided per-kind
// connect form (design §5.8). Daemon-scope kinds 403 for any non-admin
// caller (hidden from the catalog too, but the route itself must refuse a
// direct hit — design §6). Project-scope kinds require scope on the
// resolved target project; an unresolvable project (ambiguous multi-
// project caller, or an admin with none picked) renders a project picker
// instead of guessing.
func (s *Server) IntegrationForm(w http.ResponseWriter, r *http.Request, kindID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	kind, ok := findIntegrationKind(s.integrationsRegistry(), kindID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	caller := integrationsCaller(r)

	if kind.Scope == integrations.ScopeDaemon && !caller.IsAdmin {
		http.Error(w, "admin scope required", http.StatusForbidden)
		return
	}

	projectID := resolveIntegrationProjectID(r, kind, caller)
	if kind.Scope == integrations.ScopeProject && projectID == "" {
		options := caller.ScopedProjectIDs
		if caller.IsAdmin {
			options = s.allProjectIDs()
		}
		s.render(w, "integrations_form.html", IntegrationFormData{
			Title:              kind.DisplayName + " — Integrations",
			CurrentPage:        "integrations",
			KindID:             kind.ID,
			DisplayName:        kind.DisplayName,
			Category:           kind.Category,
			Scope:              string(kind.Scope),
			DocURL:             kind.DocURL,
			NeedsProjectPicker: true,
			ProjectOptions:     options,
		})
		return
	}

	if !caller.Authorized(kind.Scope, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	data := IntegrationFormData{
		Title:          kind.DisplayName + " — Integrations",
		CurrentPage:    "integrations",
		KindID:         kind.ID,
		DisplayName:    kind.DisplayName,
		Category:       kind.Category,
		Scope:          string(kind.Scope),
		DocURL:         kind.DocURL,
		ProjectID:      projectID,
		Fields:         s.prefillIntegrationFields(kind.ID, kind.Fields, projectID),
		ProbeHref:      integrationActionHref(kind.ID, "probe"),
		SaveHref:       integrationActionHref(kind.ID, "save"),
		RecheckHref:    integrationActionHref(kind.ID, "recheck"),
		AssistHrefBase: integrationActionHref(kind.ID, "assist"),
	}
	s.render(w, "integrations_form.html", data)
}

// candidateValuesFromForm reads one literal value per CredentialField from
// the submitted form — the browser-supplied candidate integrations.Probe /
// Save validate (never read back from disk).
func candidateValuesFromForm(kind integrations.IntegrationKind, r *http.Request) map[string]string {
	values := make(map[string]string, len(kind.Fields))
	for _, f := range kind.Fields {
		values[f.Key] = r.FormValue(f.Key)
	}
	return values
}

// --- POST /integrations/{kind}/probe — live probe, never writes ---

// IntegrationResultVM backs integrations_probe.html — the Outcome-colored
// banner shared by /probe and /save (design §5.2: the tile and the "Test
// connection" fragment both render from one ProbeResult shape).
type IntegrationResultVM struct {
	Result  integrations.ProbeResult
	Saved   bool
	SaveErr string
}

// IntegrationProbe handles POST /integrations/{kind}/probe — "Test
// connection": runs the kind's Prober against the submitted candidate and
// renders the resulting ProbeResult as an HTMX fragment. Never writes
// config (design §5.8).
func (s *Server) IntegrationProbe(w http.ResponseWriter, r *http.Request, kindID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	kind, ok := findIntegrationKind(s.integrationsRegistry(), kindID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	caller := integrationsCaller(r)
	projectID := strings.TrimSpace(r.FormValue("project"))
	if !caller.Authorized(kind.Scope, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if kind.Prober == nil {
		http.Error(w, "integration not probeable", http.StatusServiceUnavailable)
		return
	}
	cand := integrations.CandidateConfig{Kind: kind.ID, ProjectID: projectID, Values: candidateValuesFromForm(kind, r)}
	result := kind.Prober.Probe(r.Context(), cand)
	s.integrationsMetrics.RecordProbe(kind.ID, string(result.Outcome))
	s.render(w, "integrations_probe.html", IntegrationResultVM{Result: result})
}

// --- POST /integrations/{kind}/save — write path (design §5.4) ---

// IntegrationSave handles POST /integrations/{kind}/save — calls
// integrations.Save, which re-probes and refuses the write on anything but
// OutcomeOK (design §5.4). On success the tile fragment renders green and
// the cached probe store is updated; on failure it shows the rolled-back
// error.
func (s *Server) IntegrationSave(w http.ResponseWriter, r *http.Request, kindID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	kind, ok := findIntegrationKind(s.integrationsRegistry(), kindID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	target, ok := integrations.SaveTargetForKind(kind.ID)
	if !ok {
		http.Error(w, "integration is not writable yet", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	caller := integrationsCaller(r)
	projectID := strings.TrimSpace(r.FormValue("project"))
	// Fast-fail 403 before touching Save at all — Save re-checks the exact
	// same caller.Authorized internally (design §6: the UI must not be able
	// to bypass the save service's own authorization), so this is defense
	// in depth, not the only gate.
	if !caller.Authorized(kind.Scope, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	configDir := s.configDir()
	if configDir == "" {
		http.Error(w, "registry config directory is not configured", http.StatusServiceUnavailable)
		return
	}

	cand := integrations.CandidateConfig{Kind: kind.ID, ProjectID: projectID, Values: candidateValuesFromForm(kind, r)}
	deps := integrations.SaveDeps{
		ConfigDir:    configDir,
		Reloader:     s.integrationsReloader(),
		ReloadStatus: s.integrationsReloadStatus(),
	}
	result, err := integrations.Save(r.Context(), kind, target, cand, caller, deps)
	// Save's step 1 is itself a Prober.Probe call the hub makes (the
	// re-probe-inside-save path, design §5.8) — record it here rather than
	// duplicating the probe inside Save (which stays metrics-free per its
	// own package doc). result.Probe.Outcome is the zero value ("") only
	// when Save short-circuited before ever probing (e.g. an out-of-scope
	// or malformed-project-id caller); RecordProbe no-ops on that.
	s.integrationsMetrics.RecordProbe(kind.ID, string(result.Probe.Outcome))
	s.integrationsMetrics.RecordSave(kind.ID, integrationSaveResultLabel(result.Saved, err))
	if err != nil {
		s.render(w, "integrations_probe.html", IntegrationResultVM{Result: result.Probe, SaveErr: err.Error()})
		return
	}
	if result.Saved {
		s.storeIntegrationProbe(kind.ID, projectID, result.Probe)
	}
	s.render(w, "integrations_probe.html", IntegrationResultVM{Result: result.Probe, Saved: result.Saved})
}

// integrationSaveResultLabel derives the vornik_integration_save_total
// "result" label (design §5.8: ok|probe_failed|write_failed|reload_failed)
// from Save's return values. Save's step 1 probe failure is a clean
// refusal (err == nil, Saved == false, design §5.4) — everything else is a
// genuine error, further split by whether the *SaveError step was "reload"
// (hot-reload/poll failed after a good write) or an earlier write-path
// step (backup/read/patch/write/validate, or a pre-write error like an
// invalid project id).
func integrationSaveResultLabel(saved bool, err error) string {
	if err == nil {
		if saved {
			return "ok"
		}
		return "probe_failed"
	}
	var saveErr *integrations.SaveError
	if errors.As(err, &saveErr) && saveErr.Step == "reload" {
		return "reload_failed"
	}
	return "write_failed"
}

// --- POST /integrations/{kind}/recheck — re-probe a configured tile ---

// currentIntegrationValues reads the ALREADY-SAVED literal candidate values
// server-side (never round-tripped through the browser — the recheck
// button posts no field values) so an explicit re-check can re-probe live
// config. Daemon-scope secrets are already expanded in cfg by the config
// loader (os.ExpandEnv, internal/config/loader.go); project-scope secrets
// are read via os.Getenv against the *_env var name the registry stores
// (placeSecret / EnvSecrets.Set both os.Setenv the value on save, per
// save.go's doc) — never from a file the UI itself would have to decrypt.
func (s *Server) currentIntegrationValues(kindID, projectID string) map[string]string {
	values := map[string]string{}
	cfg := s.onboardingDetector.Config
	proj := s.integrationProject(projectID)
	switch kindID {
	case "telegram":
		if cfg != nil {
			values["bot_token"] = cfg.Telegram.BotToken
		}
	case "email":
		if proj != nil {
			values["imap_host"] = proj.Email.IMAPHost
			values["imap_port"] = intOrEmpty(int64(proj.Email.IMAPPort))
			values["imap_username"] = proj.Email.IMAPUsername
			values["imap_password_env"] = os.Getenv(proj.Email.IMAPPasswordEnv)
			values["smtp_host"] = proj.Email.SMTPHost
			values["smtp_port"] = intOrEmpty(int64(proj.Email.SMTPPort))
			values["smtp_username"] = proj.Email.SMTPUsername
			values["smtp_password_env"] = os.Getenv(proj.Email.SMTPPasswordEnv)
			values["from_address"] = proj.Email.FromAddress
		}
	case "slack":
		if proj != nil {
			values["team_id"] = proj.Slack.TeamID
			values["bot_token_env"] = os.Getenv(proj.Slack.BotTokenEnv)
			values["signing_secret_env"] = os.Getenv(proj.Slack.SigningSecretEnv)
		}
	case "github_app":
		if proj != nil {
			values["app_id"] = intOrEmpty(proj.GitHubApp.AppID)
			values["installation_id"] = intOrEmpty(proj.GitHubApp.InstallationID)
			values["api_base_url"] = proj.GitHubApp.APIBaseURL
			if proj.GitHubApp.PrivateKeyPath != "" {
				if pem, err := os.ReadFile(proj.GitHubApp.PrivateKeyPath); err == nil { //nolint:gosec // operator-configured secrets-dir path, not user input
					values["private_key_path"] = string(pem)
				}
			}
		}
	}
	return values
}

// IntegrationRecheck handles POST /integrations/{kind}/recheck — re-probes
// an already-configured kind using its saved (server-side) values and
// re-renders the tile fragment, updating the cached probe store.
func (s *Server) IntegrationRecheck(w http.ResponseWriter, r *http.Request, kindID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	kind, ok := findIntegrationKind(s.integrationsRegistry(), kindID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	caller := integrationsCaller(r)
	projectID := strings.TrimSpace(r.FormValue("project"))
	if !caller.Authorized(kind.Scope, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if kind.Prober == nil {
		http.Error(w, "integration not probeable", http.StatusServiceUnavailable)
		return
	}
	cand := integrations.CandidateConfig{Kind: kind.ID, ProjectID: projectID, Values: s.currentIntegrationValues(kind.ID, projectID)}
	result := kind.Prober.Probe(r.Context(), cand)
	s.integrationsMetrics.RecordProbe(kind.ID, string(result.Outcome))
	s.storeIntegrationProbe(kind.ID, projectID, result)
	s.render(w, "integrations_tile.html", s.integrationTile(kind, projectID))
}

// --- POST /integrations/{kind}/assist — the doc-helper (design §5.6) ---

// IntegrationAssistVM backs integrations_assist.html.
type IntegrationAssistVM struct {
	Answer  string
	DocHint string
}

// IntegrationAssist handles the per-field "where do I find this?" helper.
// A dedicated, narrow handler rather than a new buildAssistantPrompt kind:
// that dispatch is grounded on a *registry.Project (brief, sibling roster,
// per-project budget) which the daemon-scope kind (telegram) has none of
// — forcing a nil-project shim through project-authoring's budget-guard +
// JSON envelope would be a worse fit than a small, self-contained handler
// calling the same AssistantLLM.Complete seam directly (see the task
// report). Degrades to the field's static DocHint when assistantLLM is nil
// or the call errors (design §5.6).
func (s *Server) IntegrationAssist(w http.ResponseWriter, r *http.Request, kindID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	kind, ok := findIntegrationKind(s.integrationsRegistry(), kindID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	caller := integrationsCaller(r)
	projectID := strings.TrimSpace(r.FormValue("project"))
	if !caller.Authorized(kind.Scope, projectID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	fieldKey := r.FormValue("field")
	var field *integrations.CredentialField
	for i := range kind.Fields {
		if kind.Fields[i].Key == fieldKey {
			field = &kind.Fields[i]
			break
		}
	}
	if field == nil {
		http.Error(w, "unknown field", http.StatusBadRequest)
		return
	}

	if s.assistantLLM == nil {
		s.render(w, "integrations_assist.html", IntegrationAssistVM{DocHint: field.DocHint})
		return
	}
	question := strings.TrimSpace(r.FormValue("question"))
	if question == "" {
		question = fmt.Sprintf("Where do I find the %q credential for %s?", field.Label, kind.DisplayName)
	}
	system := fmt.Sprintf(
		"You help a user locate the %q credential for the %s integration. "+
			"Documentation: %s. Static hint: %s. Answer in 2-3 short, plain-language "+
			"sentences with no markdown headers. Never ask the user to paste a secret "+
			"value back to you, and never repeat one.",
		field.Label, kind.DisplayName, kind.DocURL, field.DocHint,
	)
	result, err := s.assistantLLM.Complete(r.Context(), s.assistantDefaultModel, system, question)
	if err != nil || result == nil {
		s.render(w, "integrations_assist.html", IntegrationAssistVM{DocHint: field.DocHint})
		return
	}
	s.render(w, "integrations_assist.html", IntegrationAssistVM{Answer: result.Text, DocHint: field.DocHint})
}
