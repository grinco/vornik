package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/registry"
)

// DoctorCheck is a single diagnostic finding.
type DoctorCheck struct {
	Name    string   `json:"name"`
	Status  string   `json:"status"` // OK, WARNING, ERROR
	Message string   `json:"message"`
	Items   []string `json:"items,omitempty"`
	Fixed   int      `json:"fixed,omitempty"`
}

// DoctorReport is the full doctor response.
type DoctorReport struct {
	Timestamp string        `json:"timestamp"`
	Checks    []DoctorCheck `json:"checks"`
	Summary   string        `json:"summary"`
}

// DoctorHandlers provides the /api/v1/doctor endpoint.
type DoctorHandlers struct {
	db             *sql.DB
	configDir      string
	configPath     string // path config.yaml was loaded from, for checkConfigSecretHygiene
	serverAddress  string
	apiAuthEnabled bool
	apiKeys        []string
	pricingPath    string
	artifactsRoot  string
	workspacesRoot string

	// server is a back-reference to the API server used to build
	// featuredoctor.Deps. Set via SetServer; nil-safe (feature
	// endpoints return empty Deps).
	server *Server
	// featureDepsFunc allows tests to inject stub Deps without
	// wiring a full Server. When non-nil, overrides server.featureDeps().
	featureDepsFunc func() featuredoctor.Deps

	// enableApplierFunc allows tests to inject a fake enable applier so
	// the handler's apply=true path can be exercised without a real
	// config file or reloader. When non-nil, overrides the real path.
	// Signature matches ApplyEnable.
	enableApplierFunc func(ctx context.Context, f featuredoctor.Feature, deps featuredoctor.Deps,
		plan *featuredoctor.EnablePlan, w featuredoctor.ConfigWriter, r featuredoctor.Reloader,
	) (featuredoctor.PrereqResult, error)

	// snapshot of sensitive-field values captured at boot. Holding
	// them here avoids re-invoking config.Load() at request time,
	// which redefines the package-global `--config` flag and panics
	// on the second doctor call.
	secretFields map[string]string

	// dispatcherProjectID is telegram.dispatcher_project_id at boot.
	// When non-empty, checkDispatcherRole validates the chosen
	// project's swarm has a "dispatcher" role for clean dashboard
	// role+model aggregation. Empty disables the check.
	dispatcherProjectID string
	// dispatcherChatModel is the daemon's configured chat model,
	// used by the --fix path to populate the patched dispatcher
	// role's model field. Empty falls back to a documented
	// placeholder so the role is still added.
	dispatcherChatModel string

	// apiMetrics is the registered cost-attribution counter set
	// (see metrics.go). The per-project-API-key migration doctor
	// check reads the live counter values to compute the
	// "% of cost rows came from a DB-backed key" KPI.
	// Nil-safe — the check downgrades to a "metrics not wired"
	// OK when absent.
	apiMetrics *APIMetrics

	// leaderLockRepo backs the daemon_leader_locks_health
	// check. Nil-safe — the check returns OK with
	// "leader-election not wired" when absent (SQLite branch
	// or any deployment that hasn't migrated to 57).
	leaderLockRepo persistence.DaemonLeaderLockRepository

	// configReloader is the daemon's config reload trigger. Used by
	// the feature-enable endpoint's real Reloader implementation.
	// Nil-safe — the enable endpoint returns 503 when absent.
	configReloader *config.ConfigReloader

	// chatRoutePrefixes is a snapshot of the configured chat
	// model_route prefixes (config.Chat.Router.Routes[].Prefix),
	// captured at boot by SetServerConfig. checkModelRouteCoverage
	// uses it to assert every swarm-role model resolves to a route.
	// Empty (no routes configured) downgrades the check to a skip.
	chatRoutePrefixes []string

	// modelHealthSource supplies recent per-model runtime health
	// statistics for checkModelHealth. Defaults to a DB-backed query
	// over execution_step_outcomes + task_llm_usage; tests inject a
	// fake so the check is exercisable without a database.
	modelHealthSource func(ctx context.Context) ([]modelHealthStat, error)

	// scraperProfileRoot is the root directory of scraper browser-profile
	// trees, typically ~/.config/vornik/scraper/profiles/<project>/<profile>/.
	// Populated from SCRAPER_PROFILE_ROOT at construction. Empty disables
	// checkScraperProfileFreshness (returns OK "not configured").
	scraperProfileRoot string

	// loginRequired maps profile name → required re-login cadence. Loaded
	// from login-required.yaml in scraperProfileRoot. Nil-safe — when the
	// file is absent all profile intervals fall back to a default cadence.
	loginRequired map[string]time.Duration
}

// SetAPIMetrics wires the registered APIMetrics so the
// cost-attribution doctor check can read live counter values.
// Called from the service container alongside the other doctor
// setters. Optional — the check returns OK when nil.
func (h *DoctorHandlers) SetAPIMetrics(m *APIMetrics) {
	if h == nil {
		return
	}
	h.apiMetrics = m
}

// SetLeaderLockRepository wires the leader-lock repo so the
// daemon_leader_locks_health check can enumerate active
// rows. Optional — single-process / SQLite deployments leave
// it nil.
func (h *DoctorHandlers) SetLeaderLockRepository(repo persistence.DaemonLeaderLockRepository) {
	if h == nil {
		return
	}
	h.leaderLockRepo = repo
}

// SetServer wires the API server back-reference so the feature-doctor
// endpoints can build featuredoctor.Deps from live daemon components.
// Called once at boot from the service container after both the Server
// and the DoctorHandlers are constructed.
func (h *DoctorHandlers) SetServer(s *Server) {
	if h == nil {
		return
	}
	h.server = s
}

// NewDoctorHandlers creates doctor handlers backed by the given database.
func NewDoctorHandlers(db *sql.DB) *DoctorHandlers {
	return &DoctorHandlers{db: db}
}

// SetConfigDir sets the config directory for config validation checks.
func (h *DoctorHandlers) SetConfigDir(dir string) {
	h.configDir = dir
}

// SetServerConfig captures just the bits of the daemon config the
// security-posture and secret-hygiene checks need. Called once at
// boot from the service container. Avoiding a live reference to the
// whole config keeps the handler from drifting with hot-reload and,
// more importantly, avoids calling config.Load() at request time —
// that function calls flag.Parse() and panics with "flag redefined"
// on every request past the first.
func (h *DoctorHandlers) SetServerConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	h.serverAddress = cfg.Server.Address
	h.apiAuthEnabled = cfg.API.AuthEnabled
	h.apiKeys = append(h.apiKeys[:0], cfg.API.APIKeys...)
	h.artifactsRoot = cfg.Storage.ArtifactsPath
	h.workspacesRoot = cfg.Runtime.ProjectWorkspacePath

	// Snapshot secret-bearing fields for checkConfigSecretHygiene.
	// Keep the VALUES (not the whole struct) so a future hot-reload
	// that mutates the pointer doesn't silently change what we lint.
	h.secretFields = map[string]string{
		"database.password":                   cfg.Database.Password,
		"chat.api_key":                        cfg.Chat.APIKey,
		"chat.router.http.api_key":            cfg.Chat.Router.HTTP.APIKey,
		"runtime.agent_llm.api_key":           cfg.Runtime.AgentLLM.APIKey,
		"telegram.bot_token":                  cfg.Telegram.BotToken,
		"memory.embedding_api_key":            cfg.Memory.EmbeddingAPIKey,
		"auth.providers.github.client_secret": githubClientSecret(cfg),
	}

	// Snapshot the chat model-route prefixes for checkModelRouteCoverage.
	// VALUES not the slice header so a later hot-reload can't mutate what
	// the check sees mid-request.
	h.chatRoutePrefixes = h.chatRoutePrefixes[:0]
	for _, rt := range cfg.Chat.Router.Routes {
		h.chatRoutePrefixes = append(h.chatRoutePrefixes, rt.Prefix)
	}

	h.dispatcherProjectID = cfg.Telegram.DispatcherProjectID
	// Prefer the agent_llm model (used by container agents) only as a
	// fallback because the bot's chat client typically uses chat.model.
	h.dispatcherChatModel = cfg.Chat.Model
	if h.dispatcherChatModel == "" {
		h.dispatcherChatModel = cfg.Runtime.AgentLLM.Model
	}
}

// githubClientSecret returns the resolved GitHub OAuth client_secret from
// the config, or "" when no GitHub provider is configured. Used to populate
// the secret-hygiene snapshot (auth.providers.github.client_secret).
// The operator should use client_secret_file rather than inlining the secret;
// the hygiene check steers them toward that path.
func githubClientSecret(cfg *config.Config) string {
	if cfg.Auth.Providers.GitHub == nil {
		return ""
	}
	return cfg.Auth.Providers.GitHub.ClientSecret
}

// SetConfigPath records the filesystem path config.yaml was loaded
// from. Used by checkConfigSecretHygiene to stat the file's
// permissions — without a real path the perm check silently skips.
func (h *DoctorHandlers) SetConfigPath(path string) {
	h.configPath = path
}

// SetConfigReloader wires the daemon's ConfigReloader so the feature-enable
// endpoint can trigger a reload after writing gate changes. Called from the
// service container alongside SetConfigHandlers. Optional — the enable endpoint
// returns 503 when the reloader is absent.
func (h *DoctorHandlers) SetConfigReloader(r *config.ConfigReloader) {
	if h == nil {
		return
	}
	h.configReloader = r
}

// WireDoctorServer wires the API server back-reference into the singleton
// DoctorHandlers so feature-doctor endpoints can build featuredoctor.Deps
// from live daemon components. Called from the service container after both
// NewServer and SetDoctorHandlers have run.
func WireDoctorServer(s *Server) {
	if doctorHandlers == nil {
		return
	}
	doctorHandlers.SetServer(s)
}

// WireDoctorReloader wires the daemon's ConfigReloader into the singleton
// DoctorHandlers. Called from the service container alongside SetConfigHandlers.
func WireDoctorReloader(r *config.ConfigReloader) {
	if doctorHandlers == nil {
		return
	}
	doctorHandlers.SetConfigReloader(r)
}

// SetPricingPath records where configs/pricing.yaml lives so the
// pricing-coverage check can load it fresh without needing the
// daemon's live table (avoids a shared-state dependency that would
// make the check surprising under hot-reload).
func (h *DoctorHandlers) SetPricingPath(path string) {
	h.pricingPath = path
}

// RunReportReadOnly runs the diagnostic checks in fix=false mode and
// returns the assembled report. It is the read-only collection path the
// support-report bundle builder embeds (support-report-design.md §4.1);
// the HTTP RunDoctor handler keeps its own inline assembly so its
// response stays byte-stable. Read-only by construction — fix=false
// means no check mutates state.
func (h *DoctorHandlers) RunReportReadOnly(ctx context.Context) DoctorReport {
	const fix = false
	report := DoctorReport{Timestamp: time.Now().UTC().Format(time.RFC3339)}
	report.Checks = append(report.Checks, h.checkStaleLeases(ctx, fix))
	report.Checks = append(report.Checks, h.checkOrphanedWatchers(ctx, fix))
	report.Checks = append(report.Checks, h.checkStuckExecutions(ctx, fix))
	report.Checks = append(report.Checks, h.checkTaskStateAudit(ctx, fix))
	report.Checks = append(report.Checks, h.checkConfigValidation())
	report.Checks = append(report.Checks, h.checkWorkflowSwarmCompat())
	report.Checks = append(report.Checks, h.checkSchemaGateDrift())
	report.Checks = append(report.Checks, h.checkDatabaseSchema(ctx))
	report.Checks = append(report.Checks, h.checkPodmanConfig(ctx))
	report.Checks = append(report.Checks, h.checkAPISecurityPosture())
	report.Checks = append(report.Checks, h.checkBudgetUtilisation(ctx))
	report.Checks = append(report.Checks, h.checkLeaderLocksHealth())
	report.Checks = append(report.Checks, h.checkModelHealth(ctx))
	report.Checks = append(report.Checks, h.checkConfigCRLF(fix))
	report.Checks = append(report.Checks, h.checkModelRouteCoverage())
	report.Checks = append(report.Checks, h.checkScraperProfileFreshness(ctx, fix))
	issues := 0
	for _, c := range report.Checks {
		if c.Status != "OK" {
			issues++
		}
	}
	if issues == 0 {
		report.Summary = "All checks passed"
	} else {
		report.Summary = fmt.Sprintf("%d issues found", issues)
	}
	return report
}

// RunDoctor handles POST /api/v1/doctor?fix=true|false
func (h *DoctorHandlers) RunDoctor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}

	fix := r.URL.Query().Get("fix") == "true"
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	report := DoctorReport{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	report.Checks = append(report.Checks, h.checkStaleLeases(ctx, fix))
	report.Checks = append(report.Checks, h.checkOrphanedWatchers(ctx, fix))
	report.Checks = append(report.Checks, h.checkStuckExecutions(ctx, fix))
	report.Checks = append(report.Checks, h.checkTaskStateAudit(ctx, fix))
	report.Checks = append(report.Checks, h.checkConfigValidation())
	report.Checks = append(report.Checks, h.checkWorkflowMdShape())
	report.Checks = append(report.Checks, h.checkWorkflowSwarmCompat())
	report.Checks = append(report.Checks, h.checkSchemaGateDrift())
	report.Checks = append(report.Checks, h.checkWorkflowOnFailMasking())
	report.Checks = append(report.Checks, h.checkWorkflowMDShape())
	report.Checks = append(report.Checks, h.checkRolePromptSanity())
	report.Checks = append(report.Checks, h.checkEvalSuiteLint())
	report.Checks = append(report.Checks, h.checkDatabaseSchema(ctx))
	report.Checks = append(report.Checks, h.checkOrphanFKRows(ctx, fix))
	report.Checks = append(report.Checks, h.checkPodmanConfig(ctx))
	report.Checks = append(report.Checks, h.checkEnvFileFreshness())
	report.Checks = append(report.Checks, h.checkAgentImages(ctx))
	report.Checks = append(report.Checks, h.checkAPISecurityPosture())
	report.Checks = append(report.Checks, h.checkAPIKeyStrength())
	report.Checks = append(report.Checks, h.checkPricingCoverage())
	report.Checks = append(report.Checks, h.checkAutonomyBudgetGuard())
	report.Checks = append(report.Checks, h.checkBudgetUtilisation(ctx))
	report.Checks = append(report.Checks, h.checkOrphanWorktrees(fix))
	report.Checks = append(report.Checks, h.checkSecretsPermissions(fix))
	report.Checks = append(report.Checks, h.checkConfigSecretHygiene())
	report.Checks = append(report.Checks, h.checkDispatcherRole(fix))
	report.Checks = append(report.Checks, h.checkWorkspaceCanonical())
	report.Checks = append(report.Checks, h.checkCostAttribution())
	report.Checks = append(report.Checks, h.checkLeaderLocksHealth())
	report.Checks = append(report.Checks, h.checkModelHealth(ctx))
	report.Checks = append(report.Checks, h.checkConfigCRLF(fix))
	report.Checks = append(report.Checks, h.checkModelRouteCoverage())
	report.Checks = append(report.Checks, h.checkScraperProfileFreshness(ctx, fix))

	issues := 0
	fixed := 0
	for _, c := range report.Checks {
		if c.Status != "OK" {
			issues++
		}
		fixed += c.Fixed
	}

	if issues == 0 {
		report.Summary = "All checks passed"
	} else if fix && fixed > 0 {
		report.Summary = fmt.Sprintf("%d issues found, %d fixed", issues, fixed)
	} else {
		report.Summary = fmt.Sprintf("%d issues found (run with ?fix=true to repair)", issues)
	}

	respondJSON(w, http.StatusOK, report)
}

// checkStaleLeases finds tasks stuck in LEASED status with expired leases.
func (h *DoctorHandlers) checkStaleLeases(ctx context.Context, fix bool) DoctorCheck {
	rows, err := h.db.QueryContext(ctx, `
		SELECT id, project_id, lease_expires_at
		FROM tasks
		WHERE status IN ('LEASED', 'RUNNING')
		  AND lease_expires_at IS NOT NULL
		  AND lease_expires_at < NOW()
		ORDER BY lease_expires_at ASC
		LIMIT 100
	`)
	if err != nil {
		return DoctorCheck{Name: "stale_leases", Status: "ERROR", Message: fmt.Sprintf("query failed: %v", err)}
	}
	defer func() { _ = rows.Close() }()

	type staleTask struct {
		id        string
		projectID string
		expiresAt time.Time
	}
	var stale []staleTask
	for rows.Next() {
		var t staleTask
		if err := rows.Scan(&t.id, &t.projectID, &t.expiresAt); err != nil {
			continue
		}
		stale = append(stale, t)
	}
	if err := rows.Err(); err != nil {
		return DoctorCheck{Name: "stale_leases", Status: "ERROR", Message: fmt.Sprintf("row iteration failed: %v", err)}
	}

	if len(stale) == 0 {
		return DoctorCheck{Name: "stale_leases", Status: "OK", Message: "no stale leases"}
	}

	items := make([]string, len(stale))
	for i, t := range stale {
		age := time.Since(t.expiresAt).Truncate(time.Second)
		items[i] = fmt.Sprintf("%s (project=%s, expired %s ago)", t.id, t.projectID, age)
	}

	check := DoctorCheck{
		Name:    "stale_leases",
		Status:  "WARNING",
		Message: fmt.Sprintf("%d tasks stuck in LEASED/RUNNING with expired leases", len(stale)),
		Items:   items,
	}

	if fix {
		for _, t := range stale {
			_, err := h.db.ExecContext(ctx, `
				UPDATE tasks
				SET status = 'QUEUED',
				    lease_id = NULL, leased_at = NULL, leased_by = NULL, lease_expires_at = NULL,
				    updated_at = NOW()
				WHERE id = $1 AND status IN ('LEASED', 'RUNNING')
			`, t.id)
			if err == nil {
				check.Fixed++
			}
		}
		if check.Fixed == len(stale) {
			check.Status = "OK"
			check.Message = fmt.Sprintf("released %d stale leases back to QUEUED", check.Fixed)
		}
	}

	return check
}

// checkOrphanedWatchers finds watchers for tasks already in terminal status.
func (h *DoctorHandlers) checkOrphanedWatchers(ctx context.Context, fix bool) DoctorCheck {
	rows, err := h.db.QueryContext(ctx, `
		SELECT tw.task_id, tw.chat_id, t.status
		FROM task_watchers tw
		JOIN tasks t ON tw.task_id = t.id
		WHERE t.status IN ('COMPLETED', 'FAILED', 'CANCELLED')
	`)
	if err != nil {
		return DoctorCheck{Name: "orphaned_watchers", Status: "ERROR", Message: fmt.Sprintf("query failed: %v", err)}
	}
	defer func() { _ = rows.Close() }()

	type orphan struct {
		taskID string
		chatID int64
		status string
	}
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.taskID, &o.chatID, &o.status); err != nil {
			continue
		}
		orphans = append(orphans, o)
	}
	if err := rows.Err(); err != nil {
		return DoctorCheck{Name: "orphaned_watchers", Status: "ERROR", Message: fmt.Sprintf("row iteration failed: %v", err)}
	}

	if len(orphans) == 0 {
		return DoctorCheck{Name: "orphaned_watchers", Status: "OK", Message: "no orphaned watchers"}
	}

	items := make([]string, len(orphans))
	for i, o := range orphans {
		items[i] = fmt.Sprintf("task=%s chat=%d (task is %s)", o.taskID, o.chatID, o.status)
	}

	check := DoctorCheck{
		Name:    "orphaned_watchers",
		Status:  "WARNING",
		Message: fmt.Sprintf("%d watchers for tasks already in terminal status", len(orphans)),
		Items:   items,
	}

	if fix {
		taskIDs := make(map[string]bool)
		for _, o := range orphans {
			taskIDs[o.taskID] = true
		}
		for taskID := range taskIDs {
			_, err := h.db.ExecContext(ctx, `DELETE FROM task_watchers WHERE task_id = $1`, taskID)
			if err == nil {
				check.Fixed++
			}
		}
		if check.Fixed > 0 {
			check.Status = "OK"
			check.Message = fmt.Sprintf("removed watchers for %d terminal tasks", check.Fixed)
		}
	}

	return check
}

// checkStuckExecutions finds executions in RUNNING/PENDING that have no
// recent activity (older than 1 hour without a state snapshot update).
func (h *DoctorHandlers) checkStuckExecutions(ctx context.Context, fix bool) DoctorCheck {
	rows, err := h.db.QueryContext(ctx, `
		SELECT e.id, e.task_id, e.project_id, e.status, e.created_at
		FROM executions e
		WHERE e.status IN ('RUNNING', 'PENDING')
		  AND e.created_at < NOW() - INTERVAL '1 hour'
		ORDER BY e.created_at ASC
		LIMIT 100
	`)
	if err != nil {
		return DoctorCheck{Name: "stuck_executions", Status: "ERROR", Message: fmt.Sprintf("query failed: %v", err)}
	}
	defer func() { _ = rows.Close() }()

	type stuck struct {
		id        string
		taskID    string
		projectID string
		status    string
		createdAt time.Time
	}
	var stuckExecs []stuck
	for rows.Next() {
		var s stuck
		if err := rows.Scan(&s.id, &s.taskID, &s.projectID, &s.status, &s.createdAt); err != nil {
			continue
		}
		stuckExecs = append(stuckExecs, s)
	}
	if err := rows.Err(); err != nil {
		return DoctorCheck{Name: "stuck_executions", Status: "ERROR", Message: fmt.Sprintf("row iteration failed: %v", err)}
	}

	// Surface the watchdog gauge (audit R13). The scan queries the two
	// possible stuck statuses, so reset both each run — a status that
	// cleared since the last scan must drop back to 0 rather than stay
	// pinned at its prior count.
	byStatus := map[string]int{"RUNNING": 0, "PENDING": 0}
	for _, s := range stuckExecs {
		byStatus[s.status]++
	}
	h.apiMetrics.SetExecutionsStuck(byStatus)

	if len(stuckExecs) == 0 {
		return DoctorCheck{Name: "stuck_executions", Status: "OK", Message: "no stuck executions"}
	}

	items := make([]string, len(stuckExecs))
	for i, s := range stuckExecs {
		age := time.Since(s.createdAt).Truncate(time.Minute)
		items[i] = fmt.Sprintf("%s (task=%s, project=%s, status=%s, age=%s)", s.id, s.taskID, s.projectID, s.status, age)
	}

	check := DoctorCheck{
		Name:    "stuck_executions",
		Status:  "WARNING",
		Message: fmt.Sprintf("%d executions stuck in %s for >1 hour", len(stuckExecs), stuckExecs[0].status),
		Items:   items,
	}

	if fix {
		for _, s := range stuckExecs {
			_, err := h.db.ExecContext(ctx, `
				UPDATE executions SET status = 'FAILED', error_message = 'marked failed by vornikctl doctor', updated_at = NOW()
				WHERE id = $1 AND status IN ('RUNNING', 'PENDING')
			`, s.id)
			if err == nil {
				check.Fixed++
			}
		}
		if check.Fixed > 0 {
			check.Status = "OK"
			check.Message = fmt.Sprintf("marked %d stuck executions as FAILED", check.Fixed)
		}
	}

	return check
}

// checkTaskStateAudit detects terminal tasks with leaked lease fields.
func (h *DoctorHandlers) checkTaskStateAudit(ctx context.Context, fix bool) DoctorCheck {
	var leaked int64
	err := h.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tasks
		WHERE status IN ('COMPLETED', 'FAILED', 'CANCELLED')
		  AND lease_id IS NOT NULL
	`).Scan(&leaked)
	if err != nil {
		return DoctorCheck{Name: "task_state_audit", Status: "ERROR", Message: fmt.Sprintf("query failed: %v", err)}
	}

	if leaked == 0 {
		return DoctorCheck{Name: "task_state_audit", Status: "OK", Message: "no state leaks"}
	}

	check := DoctorCheck{
		Name:    "task_state_audit",
		Status:  "WARNING",
		Message: fmt.Sprintf("%d terminal tasks still have lease fields set (data leak)", leaked),
	}

	if fix {
		res, err := h.db.ExecContext(ctx, `
			UPDATE tasks
			SET lease_id = NULL, leased_at = NULL, leased_by = NULL, lease_expires_at = NULL, updated_at = NOW()
			WHERE status IN ('COMPLETED', 'FAILED', 'CANCELLED')
			  AND lease_id IS NOT NULL
		`)
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				check.Fixed = int(n)
				check.Status = "OK"
				check.Message = fmt.Sprintf("cleaned lease fields from %d terminal tasks", n)
			}
		}
	}

	return check
}

// checkConfigValidation validates all swarm/project/workflow YAML configs.
func (h *DoctorHandlers) checkConfigValidation() DoctorCheck {
	name := "config_validation"
	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no config directory configured, skipping validation"}
	}

	reg := &registry.Registry{}
	if err := reg.Stage(h.configDir); err != nil {
		return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf("failed to load configs: %v", err)}
	}
	if err := reg.ValidateStaged(); err != nil {
		return DoctorCheck{
			Name:    name,
			Status:  "ERROR",
			Message: "config validation failed",
			Items:   []string{err.Error()},
		}
	}

	return DoctorCheck{Name: name, Status: "OK", Message: "all configs valid"}
}

// checkDatabaseSchema verifies that expected tables and indexes exist.
func (h *DoctorHandlers) checkDatabaseSchema(ctx context.Context) DoctorCheck {
	name := "database_schema"

	requiredTables := []string{
		"tasks", "executions", "artifacts",
		"tool_audit_log", "task_watchers", "migrations",
		"project_memory_chunks", // 2026.4.7
		"memory_embed_queue",    // 2026.4.7
		"task_llm_usage",        // 2026.4.11
		"webhook_events",        // 2026.4.15
	}
	requiredIndexes := []string{
		"idx_tasks_queue_lookup",
		"idx_tasks_project",
		"idx_tasks_status",
		"idx_tasks_lease_expired",
		"idx_executions_task",
		"idx_executions_project",
		"idx_executions_status",
		"idx_artifacts_execution",
		"idx_tool_audit_log_project",
		"idx_tool_audit_log_task",
		"idx_task_watchers_task",
		"idx_memory_project",              // 2026.4.7
		"idx_memory_hash",                 // 2026.4.7
		"idx_task_llm_usage_project_time", // 2026.4.11
		"idx_task_llm_usage_task",         // 2026.4.11
		"idx_task_llm_usage_role_model",   // 2026.4.11
	}

	var missing []string

	for _, table := range requiredTables {
		var exists bool
		err := h.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table,
		).Scan(&exists)
		if err != nil || !exists {
			missing = append(missing, "table: "+table)
		}
	}

	for _, idx := range requiredIndexes {
		var exists bool
		err := h.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, idx,
		).Scan(&exists)
		if err != nil || !exists {
			missing = append(missing, "index: "+idx)
		}
	}

	if len(missing) > 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "ERROR",
			Message: fmt.Sprintf("%d missing tables/indexes (run migrations)", len(missing)),
			Items:   missing,
		}
	}

	return DoctorCheck{Name: name, Status: "OK", Message: "all tables and indexes present"}
}

// checkPodmanConfig verifies podman runtime configuration.
func (h *DoctorHandlers) checkPodmanConfig(ctx context.Context) DoctorCheck {
	name := "podman_config"

	podmanPath, err := exec.LookPath("podman")
	if err != nil {
		return DoctorCheck{Name: name, Status: "ERROR", Message: "podman not found in PATH"}
	}

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(checkCtx, podmanPath, "info", "--format",
		"{{.Host.RemoteSocket.Exists}} {{.Store.GraphRoot}}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return DoctorCheck{
			Name:    name,
			Status:  "ERROR",
			Message: "podman info failed",
			Items:   []string{strings.TrimSpace(string(output))},
		}
	}

	var items []string

	// Check subuid/subgid for rootless operation
	cmd = exec.CommandContext(checkCtx, podmanPath, "info", "--format", "{{.Host.IDMappings.UIDMap}}")
	uidOut, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(uidOut)) == "[]" {
		items = append(items, "WARNING: no UID mappings — rootless containers may fail (check /etc/subuid)")
	}

	if len(items) > 0 {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "podman available with warnings", Items: items}
	}
	return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("podman OK (%s)", podmanPath)}
}

// checkAgentImages verifies that agent images referenced in swarm configs are available locally.
func (h *DoctorHandlers) checkAgentImages(ctx context.Context) DoctorCheck {
	name := "agent_images"

	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no config directory configured, skipping image check"}
	}

	swarms, err := registry.LoadSwarms(h.configDir)
	if err != nil {
		return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf("failed to load swarms: %v", err)}
	}

	// Collect unique images. Skip the "noop:" sentinel prefix used for
	// non-containerised roles like the dispatcher — runtime.image is
	// required by the registry loader, but those roles never launch a
	// container, so podman image exists would always falsely flag them.
	images := make(map[string]bool)
	for _, swarm := range swarms {
		for _, role := range swarm.Roles {
			if role.Runtime.Image == "" || strings.HasPrefix(role.Runtime.Image, "noop:") {
				continue
			}
			images[role.Runtime.Image] = true
		}
	}

	if len(images) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no agent images configured"}
	}

	podmanPath, err := exec.LookPath("podman")
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "podman not found, cannot verify images"}
	}

	var missing []string
	for image := range images {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(checkCtx, podmanPath, "image", "exists", image)
		if err := cmd.Run(); err != nil {
			missing = append(missing, image)
		}
		cancel()
	}

	if len(missing) > 0 {
		// Split by whether the missing reference can actually be pulled.
		// A qualified ref (registry host, digest, or localhost/) is
		// fetchable on first use, so it stays a WARNING. An unqualified
		// short name is NOT: under `short-name-mode = enforced` (the host
		// default) podman must prompt for a registry and, with no TTY, the
		// run fails outright — every job using that swarm dies at container
		// start. That is precisely the 2026-06-27 incident, where the
		// swarmd→vornik rename left configs pointing at the unbuilt short
		// name `swarmd-agent:latest`. Such a miss is an ERROR: it is broken
		// now, not "will be pulled later".
		var blocking []string
		for _, m := range missing {
			if imageIsUnqualified(m) {
				blocking = append(blocking, m)
			}
		}
		if len(blocking) > 0 {
			sort.Strings(blocking)
			return DoctorCheck{
				Name:    name,
				Status:  "ERROR",
				Message: fmt.Sprintf("%d agent image(s) missing AND unqualified — podman cannot resolve a short name without a TTY (short-name resolution enforced), so every job using these swarms will fail at container start. Build/tag the image locally or qualify the reference with a registry.", len(blocking)),
				Items:   blocking,
			}
		}
		sort.Strings(missing)
		return DoctorCheck{
			Name:    name,
			Status:  "WARNING",
			Message: fmt.Sprintf("%d agent images not found locally (will be pulled on first use)", len(missing)),
			Items:   missing,
		}
	}

	return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("all %d agent images available", len(images))}
}

// imageIsUnqualified reports whether ref is a container "short name" — an
// image reference with no registry component (e.g. "vornik-agent:latest"
// or "library/ubuntu"). Such names rely on unqualified-search-registries
// plus an interactive prompt to resolve; under `short-name-mode =
// enforced` with no TTY they cannot be pulled at all. A reference whose
// first path segment looks like a registry host (contains '.' or ':') or
// is the special "localhost" is qualified and remains pullable.
func imageIsUnqualified(ref string) bool {
	slash := strings.IndexByte(ref, '/')
	if slash < 0 {
		return true
	}
	first := ref[:slash]
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return false
	}
	return true
}

// checkOrphanFKRows detects rows that reference tasks/executions which no
// longer exist. Schema ON DELETE CASCADE covers most paths, but rows can
// be orphaned when a table is populated AFTER its parent is deleted (e.g.
// a race between retention prune and a late in-flight write) or when an
// operator runs a raw DELETE outside the repo layer. Report what we find;
// --fix removes them.
func (h *DoctorHandlers) checkOrphanFKRows(ctx context.Context, fix bool) DoctorCheck {
	name := "orphan_fk_rows"
	type probe struct {
		label     string
		countSQL  string
		deleteSQL string
	}
	// A genuine orphan is a row that NAMES a task which no longer
	// exists — a dangling reference left behind by a delete. A row
	// whose task_id is NULL or '' is NOT task-scoped at all and must
	// be excluded: task_llm_usage carries dispatcher rows (session-
	// scoped, session_id populated, task_id NULL — see the column
	// comment) and background-maintenance rows (kg_extraction /
	// memory_narrative / memory_titler etc.) that have no task by
	// design. Without the `task_id IS NOT NULL AND task_id <> ''`
	// guard, `NOT EXISTS (... WHERE id = task_id)` is TRUE for every
	// NULL task_id (since `id = NULL` is never true), so the check
	// flagged — and --fix DELETED — legitimate task-less cost records,
	// which is why the count "regenerated" after every clean: those
	// workers run continuously. task_watchers gets the same guard
	// defensively (a watcher should always be task-scoped).
	// SQL is alias-free and table-qualified so it parses identically
	// on Postgres (production) and SQLite (tests) — SQLite rejects the
	// `DELETE FROM t alias` form Postgres tolerates.
	probes := []probe{
		{
			label: "tool_audit_log",
			countSQL: `SELECT COUNT(*) FROM tool_audit_log
			           WHERE tool_audit_log.task_id IS NOT NULL AND tool_audit_log.task_id <> ''
			             AND NOT EXISTS (SELECT 1 FROM tasks WHERE tasks.id = tool_audit_log.task_id)`,
			deleteSQL: `DELETE FROM tool_audit_log
			            WHERE tool_audit_log.task_id IS NOT NULL AND tool_audit_log.task_id <> ''
			              AND NOT EXISTS (SELECT 1 FROM tasks WHERE tasks.id = tool_audit_log.task_id)`,
		},
		{
			label: "task_llm_usage",
			countSQL: `SELECT COUNT(*) FROM task_llm_usage
			           WHERE task_llm_usage.task_id IS NOT NULL AND task_llm_usage.task_id <> ''
			             AND NOT EXISTS (SELECT 1 FROM tasks WHERE tasks.id = task_llm_usage.task_id)`,
			deleteSQL: `DELETE FROM task_llm_usage
			            WHERE task_llm_usage.task_id IS NOT NULL AND task_llm_usage.task_id <> ''
			              AND NOT EXISTS (SELECT 1 FROM tasks WHERE tasks.id = task_llm_usage.task_id)`,
		},
		{
			label: "task_watchers",
			countSQL: `SELECT COUNT(*) FROM task_watchers
			           WHERE task_watchers.task_id IS NOT NULL AND task_watchers.task_id <> ''
			             AND NOT EXISTS (SELECT 1 FROM tasks WHERE tasks.id = task_watchers.task_id)`,
			deleteSQL: `DELETE FROM task_watchers
			            WHERE task_watchers.task_id IS NOT NULL AND task_watchers.task_id <> ''
			              AND NOT EXISTS (SELECT 1 FROM tasks WHERE tasks.id = task_watchers.task_id)`,
		},
	}

	var items []string
	totalOrphans := 0
	totalFixed := 0
	for _, p := range probes {
		var n int
		if err := h.db.QueryRowContext(ctx, p.countSQL).Scan(&n); err != nil {
			items = append(items, fmt.Sprintf("%s: probe failed: %v", p.label, err))
			continue
		}
		if n == 0 {
			continue
		}
		totalOrphans += n
		if fix {
			res, err := h.db.ExecContext(ctx, p.deleteSQL)
			if err != nil {
				items = append(items, fmt.Sprintf("%s: %d orphans (delete failed: %v)", p.label, n, err))
				continue
			}
			aff, _ := res.RowsAffected()
			totalFixed += int(aff)
			items = append(items, fmt.Sprintf("%s: %d orphans removed", p.label, aff))
		} else {
			items = append(items, fmt.Sprintf("%s: %d orphan row(s)", p.label, n))
		}
	}

	if totalOrphans == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no orphan FK rows across audit/usage/watchers"}
	}
	status := "WARNING"
	msg := fmt.Sprintf("%d orphan rows referencing missing tasks", totalOrphans)
	if fix {
		msg = fmt.Sprintf("%d orphan rows cleaned up", totalFixed)
		if totalFixed == totalOrphans {
			status = "OK"
		}
	}
	return DoctorCheck{Name: name, Status: status, Message: msg, Items: items, Fixed: totalFixed}
}

// checkAPISecurityPosture flags a deployment that listens on a non-loopback
// interface with API auth disabled — a classic "I set it up for local dev
// and forgot to turn auth on before exposing it" trap. The daemon refuses
// to start with auth_enabled=true and no keys, so the only way to reach
// production without auth is the explicit-disable path; this check makes
// that explicit.
func (h *DoctorHandlers) checkAPISecurityPosture() DoctorCheck {
	name := "api_security_posture"
	if h.serverAddress == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "server address not captured; skipping"}
	}
	// serverAddress is host:port. Split defensively.
	host := h.serverAddress
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	loopback := host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
	if h.apiAuthEnabled {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("API auth enabled on %s", h.serverAddress)}
	}
	if loopback {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("API auth disabled but listening on loopback (%s) — acceptable for local dev", h.serverAddress),
		}
	}
	return DoctorCheck{
		Name:    name,
		Status:  "ERROR",
		Message: fmt.Sprintf("API auth is DISABLED and listening on non-loopback %s — set api.auth_enabled: true and configure api_keys", h.serverAddress),
	}
}

// checkAPIKeyStrength flags keys that are obviously weak (short, example
// placeholders) so they don't silently survive from a quickstart into a
// production deployment.
func (h *DoctorHandlers) checkAPIKeyStrength() DoctorCheck {
	name := "api_key_strength"
	if !h.apiAuthEnabled {
		return DoctorCheck{Name: name, Status: "OK", Message: "API auth disabled; no keys to check"}
	}
	if len(h.apiKeys) == 0 {
		return DoctorCheck{Name: name, Status: "ERROR", Message: "API auth enabled but no api_keys configured"}
	}
	weakPatterns := []string{"changeme", "test", "example", "dev-key", "secret", "password", "token", "123"}
	var problems []string
	for i, key := range h.apiKeys {
		lower := strings.ToLower(key)
		switch {
		case len(key) < 24:
			problems = append(problems, fmt.Sprintf("key #%d: too short (%d chars; need ≥24)", i+1, len(key)))
		case strings.ContainsAny(key, " \t\n"):
			problems = append(problems, fmt.Sprintf("key #%d: contains whitespace", i+1))
		default:
			for _, p := range weakPatterns {
				if strings.Contains(lower, p) {
					problems = append(problems, fmt.Sprintf("key #%d: contains weak substring %q", i+1, p))
					break
				}
			}
		}
	}
	if len(problems) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("%d API key(s) look strong", len(h.apiKeys))}
	}
	return DoctorCheck{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d API key(s) look weak — rotate with strong random values", len(problems)),
		Items:   problems,
	}
}

// checkPricingCoverage verifies every model referenced in any swarm role
// has an explicit pricing.yaml entry. Missing entries mean cost metrics
// use the fallback `default` rate silently, which is usually conservative
// but sometimes wildly wrong.
func (h *DoctorHandlers) checkPricingCoverage() DoctorCheck {
	name := "pricing_coverage"
	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no config dir; skipping"}
	}
	reg := registry.New()
	if err := reg.Load(h.configDir); err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf("registry load failed: %v", err)}
	}

	path := h.pricingPath
	if path == "" {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "pricing.yaml path not configured; cost metrics use default rate"}
	}
	table, err := pricing.Load(path)
	if err != nil {
		return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf("load pricing table: %v", err)}
	}

	seen := make(map[string]bool)
	var missing []string
	for _, swarm := range reg.ListSwarms() {
		for _, role := range swarm.Roles {
			model := strings.TrimSpace(role.Model)
			if model == "" || seen[model] {
				continue
			}
			seen[model] = true
			if _, known := table.Lookup(model); !known {
				missing = append(missing, fmt.Sprintf("%s → %s (role %q in swarm %q)", swarm.ID, model, role.Name, swarm.ID))
			}
		}
	}
	if len(missing) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("all %d models have pricing entries", len(seen))}
	}
	return DoctorCheck{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d model(s) in swarm configs missing from pricing.yaml — cost metrics use default rate", len(missing)),
		Items:   missing,
	}
}

// checkAutonomyBudgetGuard warns on projects with autonomy enabled but
// no budget caps configured. The combination is a latent runaway-cost
// risk: autonomy schedules tasks on a timer, each task draws LLM tokens,
// and without a budget cap the only defence is the scheduler's rate
// limit (which only bounds frequency, not spend per task).
func (h *DoctorHandlers) checkAutonomyBudgetGuard() DoctorCheck {
	name := "autonomy_budget_guard"
	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no config dir; skipping"}
	}
	reg := registry.New()
	if err := reg.Load(h.configDir); err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf("registry load failed: %v; skipping", err)}
	}
	var unguarded []string
	for _, p := range reg.ListProjects() {
		if !p.Autonomy.Enabled {
			continue
		}
		b := p.Budget
		if b.DailyHardUSD == 0 && b.MonthlyHardUSD == 0 {
			unguarded = append(unguarded, fmt.Sprintf("%s: autonomy enabled, no hard $ cap", p.ID))
		}
	}
	if len(unguarded) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "all autonomous projects have at least one hard $ cap"}
	}
	return DoctorCheck{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d autonomous project(s) have no $ cap — a misbehaving loop can accumulate cost unbounded", len(unguarded)),
		Items:   unguarded,
	}
}

// checkBudgetUtilisation lists projects at or near their hard cap using
// live task_llm_usage aggregates. 80% of hard cap triggers a warning —
// enough lead time to raise the cap or investigate without the cap
// actually blocking work.
func (h *DoctorHandlers) checkBudgetUtilisation(ctx context.Context) DoctorCheck {
	name := "budget_utilisation"
	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no config dir; skipping"}
	}
	reg := registry.New()
	if err := reg.Load(h.configDir); err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf("registry load failed: %v; skipping", err)}
	}
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	var warnings []string
	worst := "OK"
	for _, p := range reg.ListProjects() {
		b := p.Budget
		if b.DailyHardUSD == 0 && b.MonthlyHardUSD == 0 {
			continue
		}
		checkWindow := func(label string, since time.Time, hard float64) {
			if hard <= 0 {
				return
			}
			var spend float64
			err := h.db.QueryRowContext(ctx,
				`SELECT COALESCE(SUM(cost_usd), 0) FROM task_llm_usage WHERE project_id = $1 AND recorded_at >= $2`,
				p.ID, since,
			).Scan(&spend)
			if err != nil {
				return
			}
			ratio := spend / hard
			if ratio >= 1.0 {
				warnings = append(warnings, fmt.Sprintf("%s: %s spend $%.2f / $%.2f (over cap)", p.ID, label, spend, hard))
				worst = "ERROR"
			} else if ratio >= 0.8 {
				warnings = append(warnings, fmt.Sprintf("%s: %s spend $%.2f / $%.2f (%.0f%%)", p.ID, label, spend, hard, ratio*100))
				if worst == "OK" {
					worst = "WARNING"
				}
			}
		}
		checkWindow("daily", dayStart, b.DailyHardUSD)
		checkWindow("monthly", monthStart, b.MonthlyHardUSD)
	}
	if len(warnings) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no projects near a budget cap"}
	}
	return DoctorCheck{
		Name:    name,
		Status:  worst,
		Message: fmt.Sprintf("%d project/window(s) at ≥80%% of hard cap", len(warnings)),
		Items:   warnings,
	}
}

type orphanWorktreeFinding struct {
	project string
	taskID  string
	path    string
	reason  string
}

// checkOrphanWorktrees scans every project workspace for <project>/.worktrees/
// subdirectories that don't correspond to an active task. Startup prune handles
// these, but a running deployment can accumulate them when tasks fail between
// worktree create and worktree remove. With --fix, remove only directories
// whose task is missing or terminal; non-terminal tasks are never touched.
func (h *DoctorHandlers) checkOrphanWorktrees(fix bool) DoctorCheck {
	name := "orphan_worktrees"
	if h.workspacesRoot == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no workspaces root configured; skipping"}
	}
	findings, projectsChecked, err := scanOrphanWorktrees(h.workspacesRoot, func(taskID string) (string, error) {
		var status string
		err := h.db.QueryRowContext(context.Background(),
			`SELECT status::text FROM tasks WHERE id = $1`, taskID,
		).Scan(&status)
		return status, err
	})
	if err != nil {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("workspaces root not readable: %v; skipping", err)}
	}
	if len(findings) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("no orphan worktree dirs across %d project(s)", projectsChecked)}
	}
	items := make([]string, 0, len(findings))
	check := DoctorCheck{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d orphan worktree dir(s) found", len(findings)),
	}
	if fix {
		check.Fixed, items = fixOrphanWorktreeFindings(findings, h.workspacesRoot)
	} else {
		for _, finding := range findings {
			items = append(items, formatOrphanWorktreeFinding(finding))
		}
	}
	check.Items = items
	if fix && check.Fixed == len(findings) {
		check.Status = "OK"
		check.Message = fmt.Sprintf("removed %d orphan worktree dir(s)", check.Fixed)
	}
	return check
}

func fixOrphanWorktreeFindings(findings []orphanWorktreeFinding, workspacesRoot string) (int, []string) {
	items := make([]string, 0, len(findings))
	fixed := 0
	for _, finding := range findings {
		rel := formatOrphanWorktreeFinding(finding)
		if err := os.RemoveAll(finding.path); err != nil {
			items = append(items, fmt.Sprintf("%s; remove failed: %v", rel, err))
			continue
		}
		// Also clean up git's administrative side so `git worktree
		// list` stops reporting the entry as prunable and the orphan
		// `worktree/<taskID>` branch goes away. Without this, a
		// follow-up `git worktree add` for the same task ID would
		// fail with "already exists" until the admin dir is pruned.
		// Both commands are best-effort — if the project isn't a git
		// repo, prune and branch -D no-op without surfacing an error.
		if workspacesRoot != "" {
			projectDir := filepath.Join(workspacesRoot, finding.project)
			_, _ = exec.CommandContext(context.Background(), "git", "-C", projectDir, "worktree", "prune").CombinedOutput()
			// `--` separator: defense-in-depth so the branch arg is never
			// reinterpreted as a flag, even if the source of finding.taskID
			// changes (today it comes from a directory name in
			// .worktrees/, which the prefix already protects).
			_, _ = exec.CommandContext(context.Background(), "git", "-C", projectDir, "branch", "-D", "--", "worktree/"+finding.taskID).CombinedOutput()
		}
		fixed++
		items = append(items, rel+" removed")
	}
	return fixed, items
}

func formatOrphanWorktreeFinding(finding orphanWorktreeFinding) string {
	return fmt.Sprintf("%s/.worktrees/%s (%s)", finding.project, finding.taskID, finding.reason)
}

func scanOrphanWorktrees(workspacesRoot string, taskStatus func(taskID string) (string, error)) ([]orphanWorktreeFinding, int, error) {
	entries, err := os.ReadDir(workspacesRoot)
	if err != nil {
		return nil, 0, err
	}
	var findings []orphanWorktreeFinding
	projectsChecked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		wt := filepath.Join(workspacesRoot, e.Name(), ".worktrees")
		wtEntries, err := os.ReadDir(wt)
		if err != nil {
			continue // no worktrees dir for this project — fine
		}
		projectsChecked++
		for _, w := range wtEntries {
			if !w.IsDir() {
				continue
			}
			taskID := w.Name()
			status, err := taskStatus(taskID)
			full := filepath.Join(wt, taskID)
			switch {
			case err == sql.ErrNoRows:
				findings = append(findings, orphanWorktreeFinding{
					project: e.Name(),
					taskID:  taskID,
					path:    full,
					reason:  "task does not exist",
				})
			case err != nil:
				// Query failure — don't classify as orphan; skip silently.
			case status == "COMPLETED" || status == "FAILED" || status == "CANCELLED":
				findings = append(findings, orphanWorktreeFinding{
					project: e.Name(),
					taskID:  taskID,
					path:    full,
					reason:  fmt.Sprintf("task %s terminal", status),
				})
			}
		}
	}
	return findings, projectsChecked, nil
}

// checkSecretsPermissions walks common secret locations and flags files
// that look like they carry credentials but have world-readable modes.
// OAuth tokens, bot tokens, API keys, SSH-style private keys, and session
// cookie jars should be 0600 / 0640 at worst.
//
// Intentionally name-pattern-driven rather than walking-every-file: the
// secrets/ tree on this deployment contains a ~622MB Chromium extraction
// for the LinkedIn MCP browser session (patchright-browsers/), all at
// mode 755 by design. Flagging every .pak and .dll is noise and hides
// the real findings.
//
// When fix=true, chmods each offending file to 0600 and each offending
// top-level dir to 0700. Conservative: doesn't touch anything outside
// the roots, doesn't descend into skipped subtrees.
func (h *DoctorHandlers) checkSecretsPermissions(fix bool) DoctorCheck {
	name := "secrets_permissions"

	// Search roots: $HOME/.config/vornik/secrets (operator-managed) and
	// any ./secrets relative to the config dir.
	var roots []string
	if home := os.Getenv("HOME"); home != "" {
		roots = append(roots, filepath.Join(home, ".config", "vornik", "secrets"))
	}
	if h.configDir != "" {
		roots = append(roots, filepath.Join(h.configDir, "secrets"))
	}

	// Skip subtrees that are known to be browser/runtime caches, not
	// credential material. Matched as a path substring so nested
	// installs are all caught.
	skipSubtreeSubstrings := []string{
		"/patchright-browsers/", // Patchright/Playwright Chromium extract
		"/chrome-linux",         // Chromium binary tree
		"/node_modules/",        // npm install output
		"/.cache/",              // generic cache convention
		"/__pycache__/",
	}

	// Files whose NAME matches these patterns are credential material
	// and must not be world-readable. Patterns are Boolean-OR — any
	// match marks the file as sensitive.
	isSensitiveName := func(base string) bool {
		lower := strings.ToLower(base)
		// Exact matches for well-known credential filenames.
		switch lower {
		case "credentials.json", "client_secret.json", "token.json",
			"cookies.json", "cookies.txt", "source-state.json",
			"gmail-token.json", "calendar-token.json",
			"browser-install.json",
			".env", "id_rsa", "id_ecdsa", "id_ed25519":
			return true
		}
		// Suffixes.
		for _, suffix := range []string{"-token.json", "_token.json", ".pem", ".key", ".keyfile"} {
			if strings.HasSuffix(lower, suffix) {
				return true
			}
		}
		// Substrings in filename. Also catches Google OAuth files like
		// "vadim@grinco.eu.json" (workspace-mcp convention: one JSON per
		// authenticated user) — a .json file directly under a
		// workspace/ subtree is treated as credential material even
		// without a "token" substring in the name.
		for _, substr := range []string{"credential", "secret", "apikey", "api_key", "token"} {
			if strings.Contains(lower, substr) {
				return true
			}
		}
		// Heuristic catch-all: *.json that sits inside a path segment
		// like /workspace/, /gmail/, /oauth/, /linkedin/ (OAuth scope
		// dirs the operator creates under secrets/<project>/). Pass
		// the full path here so the caller can enable this path-based
		// check. We return false from the name-only check; the walker
		// calls a companion isSensitivePath for the extra heuristics.
		return false
	}
	isSensitivePath := func(path string) bool {
		if !strings.HasSuffix(strings.ToLower(path), ".json") {
			return false
		}
		for _, seg := range []string{"/workspace/", "/gmail/", "/oauth/", "/linkedin/", "/calendar/", "/google/"} {
			if strings.Contains(path, seg) {
				return true
			}
		}
		return false
	}

	var problems []string
	fixed := 0
	sensitiveScanned := 0

	for _, root := range roots {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			continue
		}
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			// Prune excluded subtrees — don't descend into browser caches.
			for _, skip := range skipSubtreeSubstrings {
				if strings.Contains(path, skip) {
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
			if info.IsDir() {
				// Warn on world-readable dirs ONLY when they're a direct
				// child of the secrets root — the per-project / per-service
				// container whose name is itself identifying (e.g.
				// secrets/assistant, secrets/janka). Anything deeper is
				// organisational scaffolding (linkedin/, profile/, etc.)
				// and noisy to flag.
				perm := info.Mode().Perm()
				if perm&0o007 != 0 {
					rel, _ := filepath.Rel(root, path)
					if rel != "." && !strings.Contains(rel, string(filepath.Separator)) {
						if fix {
							if err := os.Chmod(path, 0o700); err == nil {
								fixed++
								problems = append(problems, fmt.Sprintf("%s (dir was mode %o → chmod 0700)", path, perm))
							} else {
								problems = append(problems, fmt.Sprintf("%s (dir mode %o, chmod failed: %v)", path, perm, err))
							}
						} else {
							problems = append(problems, fmt.Sprintf("%s (dir mode %o, should be 0o700)", path, perm))
						}
					}
				}
				return nil
			}
			if !isSensitiveName(filepath.Base(path)) && !isSensitivePath(path) {
				return nil
			}
			sensitiveScanned++
			perm := info.Mode().Perm()
			if perm&0o007 != 0 {
				if fix {
					if err := os.Chmod(path, 0o600); err == nil {
						fixed++
						problems = append(problems, fmt.Sprintf("%s (was mode %o → chmod 0600)", path, perm))
					} else {
						problems = append(problems, fmt.Sprintf("%s (mode %o, chmod failed: %v)", path, perm, err))
					}
				} else {
					problems = append(problems, fmt.Sprintf("%s (mode %o, should be 0o600)", path, perm))
				}
			}
			return nil
		})
	}
	if sensitiveScanned == 0 && len(problems) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no credential files found under secrets/; nothing to check"}
	}
	if len(problems) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("%d credential file(s) all restricted", sensitiveScanned)}
	}
	status := "WARNING"
	msg := fmt.Sprintf("%d credential file(s)/dir(s) world-readable", len(problems))
	if fix {
		msg = fmt.Sprintf("%d permission issue(s) found, %d chmod'd to restricted mode", len(problems), fixed)
		if fixed == len(problems) {
			status = "OK"
		}
	}
	return DoctorCheck{Name: name, Status: status, Message: msg, Items: problems, Fixed: fixed}
}
