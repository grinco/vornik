// Package ui provides a web-based dashboard for viewing projects, tasks, and executions.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/sessionstore"
	"vornik.io/vornik/internal/taskcreate"
	"vornik.io/vornik/internal/templates"
	"vornik.io/vornik/internal/webchat"
	"vornik.io/vornik/internal/workspacelock"
)

// parsePlanSubRole extracts the role name from a synthetic plan step ID
// produced by the adaptive workflow's per-role loop. IDs follow
// "<outerStepID>_<index>_<role>" where <role> is any swarm-defined role
// name — it may itself contain underscores (e.g. "code_reviewer",
// "senior_engineer"). Returns empty string for IDs that don't match the
// pattern so regular workflow steps skip the sub-role pill.
//
// SplitN with N=3 guarantees the role segment keeps underscores intact:
// "plan_0_code_reviewer" → ["plan", "0", "code_reviewer"] → "code_reviewer".
func parsePlanSubRole(id string) string {
	parts := strings.SplitN(id, "_", 3)
	if len(parts) < 3 {
		return ""
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return ""
	}
	if parts[2] == "" {
		return ""
	}
	return parts[2]
}

//go:embed templates/*.html
var templatesFS embed.FS

// staticFS holds vendored client-side dependencies served from /ui/static/.
// Only HTMX is vendored; Tailwind stays on its CDN because vendoring it
// would require a node toolchain to compile a project-specific build.
//
//go:embed static/*
var staticFS embed.FS

// ExecutorInterface provides task lifecycle control for the UI.
type ExecutorInterface interface {
	Cancel(taskID string) error
	// Pause stops a RUNNING task's container, flips
	// execution_status + task.Status to PAUSED, cancels the
	// per-execution context, and blocks until the goroutine's
	// activeExecutions entry is cleared. The UI's pause action
	// MUST call this for RUNNING tasks — without it, only
	// task.Status flips while the goroutine runs to completion
	// and overwrites PAUSED with FAILED/COMPLETED. Returned
	// error is "no active execution" when the task isn't
	// currently RUNNING in this process; callers fall through
	// to the bare TransitionConditional in that case.
	Pause(taskID string) error
	// ResumeTask is the task-driven inverse of Pause: load the
	// existing PAUSED execution for this task, flip it back to
	// RUNNING in-place, and spawn the goroutine off the existing
	// row (preserving checkpoint / step pointer / fork lineage).
	// Returns "no active execution" or "execution is not paused"
	// when there's nothing to resume; callers fall through to a
	// fresh dispatch in that case (2026-05-26 fix — the UI's
	// resume button was flipping task→QUEUED which created a NEW
	// execution while the paused one sat parked).
	ResumeTask(taskID string) error
	// ResumePaused kicks an in-process resume of a Paused
	// execution — added 2026.6.0 for the retry-from-step
	// surface so the operator doesn't have to wait for a daemon
	// restart's Recover() loop to pick the rewound execution up.
	ResumePaused(executionID string) error
	// NotifyChildTerminal drives the parent-unblock sweep when a
	// child reaches a terminal status outside the executor's own
	// flow (e.g. the UI close path setting CLOSED on an
	// AWAITING_INPUT child). The executor loads the child by ID,
	// checks whether all siblings are terminal, and resumes /
	// fails the parent accordingly. Added 2026-05-21 after a
	// closed child left its parent stuck in WAITING_FOR_CHILDREN
	// (task_20260521111852_8016a4a902b4f959).
	NotifyChildTerminal(ctx context.Context, childTaskID string)
}

// TaskLogSource provides best-effort logs for task debugging.
type TaskLogSource interface {
	TaskLogs(ctx context.Context, taskID string, tail int) (string, error)
}

// ConfigReloader reloads daemon config after validated UI edits.
type ConfigReloader interface {
	Reload() error
}

// ArchiveSweeper is the narrow contract the UI uses to kick a
// project-archival sweep early. Set by the service container so
// the "Delete now" button doesn't have to wait for the next
// scheduled tick. Sweepers must be idempotent — multiple
// overlapping kicks are safe.
type ArchiveSweeper interface {
	SweepNow(ctx context.Context)
}

// ArchiveLifecycle is the narrow contract the UI uses to mutate
// the project YAML's lifecycle block + reload the registry. The
// concrete implementation is projectarchive.LifecycleService; the
// UI package keeps the interface so it doesn't pull the
// internal/projectarchive types into its public surface.
type ArchiveLifecycle interface {
	Archive(ctx context.Context, projectID string, in ArchiveLifecycleInput) (ArchiveLifecycleSnapshot, error)
	Unarchive(ctx context.Context, projectID string) error
	ScheduleDeleteNow(ctx context.Context, projectID string, isArchived bool) error
}

// ArchiveLifecycleInput mirrors projectarchive.ArchiveInput; held
// at the UI boundary so the public surface doesn't leak the
// underlying type.
type ArchiveLifecycleInput struct {
	Grace     time.Duration
	Reason    string
	Principal string
}

// ArchiveLifecycleSnapshot mirrors projectarchive.LifecycleSnapshot.
type ArchiveLifecycleSnapshot struct {
	Status            string
	ArchivedAt        time.Time
	ScheduledDeleteAt time.Time
	Reason            string
	ArchivedBy        string
}

// Server handles HTTP requests for the web UI.
type Server struct {
	taskRepo     persistence.TaskRepository
	execRepo     persistence.ExecutionRepository
	artifactRepo persistence.ArtifactRepository
	// artifactReader (optional) routes blob reads through the
	// backend-aware Store. Nil falls back to the legacy direct-disk
	// path — correct only on the filesystem backend.
	artifactReader   ArtifactReader
	auditRepo        persistence.ToolAuditRepository
	webhookEventRepo persistence.WebhookEventRepository
	llmUsageRepo     persistence.TaskLLMUsageRepository
	// apiKeyRepo backs /ui/projects/{id}/keys. Nil disables the
	// page (renders 503).
	apiKeyRepo          persistence.APIKeyRepository
	outcomeRepo         persistence.ExecutionStepOutcomeRepository
	judgeVerdictRepo    persistence.TaskJudgeVerdictRepository
	recoveryEventRepo   persistence.RecoveryEventRepository
	tradingSnapshotRepo persistence.TradingPositionsSnapshotRepository
	tradingOrderRepo    persistence.TradingOrderRepository
	tradingSafetyRepo   persistence.TradingSafetyEventRepository
	// tradingFillRepo (optional) backs the soak-panel "volume
	// today / 7d" tile with precise SUM(qty × fill_price) over
	// trading_fills. nil falls back to the trading_orders
	// LMT-price estimate (legacy) so deployments without Phase-3
	// fill ingestion keep their existing tile populated.
	tradingFillRepo persistence.TradingFillRepository
	// tradingEnabled gates the /trading dashboard route server-side on the EE
	// trading capability (c.providers.Trading != nil). Community builds leave it
	// false so /trading 404s instead of leaking the dashboard — the nav item's
	// data-cap is only a client-side, fail-open hint. (2026-06-27.)
	tradingEnabled      bool
	postMortemRepo      persistence.TaskPostMortemRepository
	postMortemExplainer PostMortemExplainer
	// instinctRepo backs the (advisory) "similar failures here resolved
	// by …" panel on the failed-task page — Consumer A, slice 3. Nil-
	// safe AND gated: it is consulted only when instinctPlaybooks is
	// true (the container passes false unless instinct.enabled AND
	// instinct.consumers.failure_playbooks are both on), so with the
	// gate off the failed-task page is byte-for-byte unchanged.
	instinctRepo      persistence.InstinctRepository
	instinctPlaybooks bool
	// reminderRepo backs the per-project upcoming-reminders tile.
	// Nil-safe — when unwired the tile renders empty.
	reminderRepo persistence.ReminderRepository

	// blackboxService backs /ui/admin/blackbox (Autonomy Black
	// Box Phase A). Nil-safe — when unwired the tab renders
	// "trace service not configured on this deployment". The
	// concrete type (*blackbox.Service) is injected via an EE
	// adapter that satisfies BlackBoxTraceService.
	blackboxService BlackBoxTraceService

	// healingTriggerRepo backs the Phase B triggers list on
	// /ui/admin/blackbox. Nil-safe — when unwired the section
	// is hidden.
	healingTriggerRepo persistence.WorkflowHealingTriggerRepository
	// healingOverrideRepo backs /ui/admin/blackbox/overrides
	// (Phase B stretch — per-(project, workflow, class)
	// threshold + mute knobs the detector consults). Nil-safe.
	healingOverrideRepo persistence.HealingTriggerOverrideRepository

	// healingCandidateRepo / healingTrialRepo back the Self-Healing
	// Workflow Genome v1 candidate + trial views on
	// /ui/admin/blackbox/candidates. Nil-safe — when unwired the page
	// surfaces an empty state.
	healingCandidateRepo persistence.WorkflowHealingCandidateRepository
	healingTrialRepo     persistence.WorkflowHealingTrialRepository
	// healingTrialRunner drives the operator-triggered run-trial button.
	// Nil hides the button.
	healingTrialRunner HealingTrialRunnerUI
	// healingPromoter drives the promote/reject buttons. Promotion runs
	// the gate + the memetic apply path (manual operator action). Nil
	// hides both buttons.
	healingPromoter HealingCandidatePromoterUI

	// memoryPolicyEvaluations backs /ui/admin/memory/firewall.
	// Postgres-only — SQLite stub returns empty so the page
	// renders with no data rather than 503.
	memoryPolicyEvaluations persistence.MemoryPolicyEvaluationRepository
	// memoryFirewallMode is the daemon's current enforcement
	// mode, snapshotted at boot.
	memoryFirewallMode string
	// firewallEditor backs the per-chunk detail page
	// (/ui/admin/memory/firewall/chunks/{id}). Nil = page
	// shows the "not configured" state. Same shape as the
	// api package's MemoryFirewallEditor but lives behind a
	// ui-local interface so the ui package doesn't import
	// internal/api.
	firewallEditor FirewallEditor

	// crossProjectCallRepo + projectSpawnRepo back the multi-
	// hop replay tree (inter-project orchestration Phase C).
	// nil-safe — when not wired the replay builder skips the
	// cross-project section and renders a single-project
	// timeline. Same fail-soft contract every other optional
	// surface uses.
	crossProjectCallRepo persistence.CrossProjectCallRepository
	projectSpawnRepo     persistence.ProjectSpawnRepository
	// Phase 26 — conversational task lifecycle. Both nil-safe;
	// the task-detail page degrades to the legacy view when not
	// wired.
	taskMessageRepo    persistence.TaskMessageRepository
	taskScratchpadRepo persistence.TaskScratchpadRepository
	// hintRepo lets the task-detail page interleave task-scoped
	// steering hints with the message thread (2026-05-26 unified-
	// timeline refactor). Nil-safe — when the repo isn't wired the
	// conversation view renders messages only, same as before.
	hintRepo    persistence.ExecutionHintRepository
	rescheduler Rescheduler
	// Phase 80 — SSE pub/sub. Lazy-initialised on first subscribe
	// so deployments without SSE wiring don't allocate the bus.
	sseBus     *SSEBus
	projectReg *registry.Registry
	// publicBaseURL is the operator-configured server.public_base_url value
	// (e.g. "https://vornik.example.com"). Used to derive the git-over-HTTPS
	// clone URL on the project Git-access panel. Empty string means "not
	// configured" — the panel renders the relative path + a hint instead.
	publicBaseURL    string
	executor         ExecutorInterface
	taskLogSource    TaskLogSource
	templates        *template.Template
	logger           zerolog.Logger
	artifactBasePath string // root directory for artifact files; used to validate download paths
	// extractedDocsRepo backs the /ui/projects/{id}/documents pages.
	// nil disables the listing (handlers return 503) so a daemon
	// running without the extraction pipeline doesn't render a
	// broken page.
	extractedDocsRepo persistence.ExtractedDocumentRepository
	// documentReExtractor is the narrow seam for the re-extract
	// button on the per-document detail page. nil disables the
	// button rendering; the rest of the documents UI still works
	// (read-only).
	documentReExtractor DocumentReExtractor
	// projectWorkspaceRoot is the daemon's runtime.project_workspace_path
	// — the base dir under which per-project persistent workspaces
	// live. Used by the /ui/projects/{id}/artifacts surface to
	// resolve <root>/<projectID>/artifacts/<file>. Empty disables
	// the page (handler returns 503) so a misconfigured deployment
	// doesn't accidentally surface the wrong directory tree.
	projectWorkspaceRoot string
	// workspaceLock is the shared per-project workspace lock. The
	// service container injects the SAME *workspacelock.Locker the
	// executor and API server hold, so the live artifact-delete path
	// (commitArtifactDeletion) is mutually exclusive per project with
	// task execution and git-over-HTTPS pushes. Never nil after
	// NewServer (falls back to workspacelock.New()).
	workspaceLock  *workspacelock.Locker
	configReloader ConfigReloader
	// archiveSweeper is the project-archival deletion runner.
	// Optional — when nil the archive/unarchive endpoints still
	// work (writing the YAML + reloading the registry); only the
	// "delete now" path's synchronous kick degrades to "wait
	// until the next sweeper tick".
	archiveSweeper ArchiveSweeper

	// archiveLifecycle is the shared service the archive /
	// unarchive / delete-now handlers delegate to. Same instance
	// the REST API uses so both surfaces produce identical YAML
	// mutations + audit shapes. Nil falls back to the legacy
	// inline-mutation path so existing tests continue to work.
	archiveLifecycle ArchiveLifecycle
	rateLimiter      ratelimit.ProjectLimiter
	// apiKeyLimiter is the per-API-key token-bucket — same instance
	// shared with the API subtree. UI reads its bucket levels for
	// the project homepage "approaching limit" banner.
	apiKeyLimiter *ratelimit.APIKeyLimiter
	// rateLimitMetrics is the shared Prometheus + event-ring
	// observability surface. UI calls StatusFor to drive the
	// homepage banner without scraping Prometheus.
	rateLimitMetrics *ratelimit.Metrics
	budgetNotifier   budget.Notifier
	// Landing-page extras (2026-04-30). autonomyEvalRepo provides
	// the most-recent evaluation per project so the dashboard can
	// project the next tick. activeChatSource is the Telegram bot
	// (or any other type implementing ActiveChatCount) — kept as a
	// narrow interface so the ui package doesn't pull telegram in.
	autonomyEvalRepo persistence.AutonomyEvaluationRepository
	activeChatSource ActiveChatSource
	// Memory hardening (Phase 2-4) repos for the /ui/memory section.
	// Each is nil-safe: handlers degrade with a "memory hardening
	// not enabled" message when the corresponding repo isn't wired.
	// memoryConfigured snapshots config.memory.enabled so the UI can
	// distinguish "memory is off in config" from "memory is on but the
	// hardening surfaces were not wired".
	memoryConfigured bool
	memoryQuarantine persistence.MemoryQuarantineRepository
	corpusEpochs     persistence.CorpusEpochRepository
	ingestQueue      persistence.IngestQueueRepository
	chunkGraph       persistence.ChunkGraphExtractionRepository
	// kgSearcher backs the knowledge-graph READ pages (entity
	// browser, entity detail, subgraph viewer). nil-safe — the
	// pages render a "KG read not enabled" state when unwired. See
	// memory_kg.go + LLD §7.
	kgSearcher     KnowledgeGraphReader
	vectorViz      VectorVizSource
	pipelineDryRun PipelineDryRunner
	memorySearcher MemorySearcher
	// memoryEvictor wires the hard-eviction surface (Operator
	// actions → Erase chunks) + the audit-log viewer. nil-safe:
	// the eviction form + audit panel render an "evict not enabled"
	// placeholder when the wiring is absent.
	memoryEvictor MemoryEvictor
	// retentionPreviewer estimates the next sweep's pruning counts
	// for the project-detail page panel. nil-safe.
	retentionPreviewer RetentionPreviewer
	// embeddingCacheStats reports embedding_cache table size for
	// the /ui/spend panel. nil-safe.
	embeddingCacheStats EmbeddingCacheStatsSource
	// responseCacheStats reports llm_response_cache table size +
	// hit total for the /ui/spend Phase E panel. nil-safe.
	responseCacheStats ResponseCacheStatsSource
	// wizardSessions backs the Feature #2 Phase C drafts banner
	// on /ui/projects. nil-safe — banner hides when unwired.
	wizardSessions WizardSessionLister
	// onboardingDetector backs /ui/setup. Nil falls back to the
	// same conservative heuristic as the API status endpoint.
	onboardingDetector onboarding.Detector
	// Phase 2 — prompt-writing assistant. assistantLLM is the
	// LLM backend the assistant handler calls; nil means the
	// feature is disabled (handler returns 503). assistantDefault-
	// Model is used when neither the project nor the swarm's
	// leadRole pins a specific model. assistantPricing is the
	// daemon's pricing.Table used to compute per-call cost for
	// the spend dashboard + budget guard; nil falls back to
	// cost=0 (budget guard still works via other usage rows).
	assistantLLM          AssistantLLM
	assistantDefaultModel string
	assistantPricing      *pricing.Table
	// Project template gallery (2026.6.0 F2 slice 2). Optional —
	// nil renders the /ui/projects/new page in a "no catalog
	// installed" state instead of crashing.
	projectTemplates *templates.Catalog
	// configsDir is the writable root where /ui/projects/new
	// materialises rendered template files. Same value the API
	// server uses; required to be non-empty for the POST flow.
	configsDir string
	// retentionDefaults is the daemon-wide ProjectRetention used
	// to resolve effective per-project retention when the project
	// YAML doesn't override a given field. 2026.7.0 F9 surface.
	// Zero-value means "no defaults wired" — the template fills
	// in the hardcoded fallbacks (90/30/60/60/60 days).
	retentionDefaults registry.ProjectRetention
	// taskCreator is the shared task-creation core that backs the
	// /ui/projects/{id}/tasks/new form. Same instance the REST
	// API uses, so the two surfaces can't drift on validation /
	// rate-limit / budget semantics. Nil disables the form
	// (renders 503). See package internal/taskcreate.
	taskCreator *taskcreate.Creator

	// singleTenantOperatorID is the resolved auth-off operator
	// fallback (api.SingleTenantOperatorIDFromConfig output). Used
	// by operatorIDForRequest so the drafts banner + any future
	// operator-keyed UI surface stays visible on local installs
	// without `X-Operator-Id`. Empty leaves the legacy header-only
	// behaviour intact.
	singleTenantOperatorID string

	// chatDispatcher backs the per-project web chat surface at
	// /ui/projects/<id>/chat. nil renders a "chat not configured"
	// banner instead of returning 500 so deployments without a
	// chat provider keep the rest of the UI usable.
	chatDispatcher ChatDispatcher

	// chatStores keeps one in-memory webchat SessionStore per
	// project so two browsers on different projects can't read
	// each other's history. Allocated lazily on first chat hit.
	chatStoresMu sync.Mutex
	chatStores   map[string]*webchat.SessionStore

	// chatSessionPersister (optional) DB-backs every webchat
	// SessionStore the lazy chatStoreFor constructs. Set via
	// WithChatSessionPersister at boot; nil leaves stores
	// in-memory-only (pre-feature behaviour).
	chatSessionPersister *sessionstore.Persister

	// chatContextBudget is the token-window size the webchat
	// SessionStore uses to compute the per-turn context-budget
	// tier. Zero (the default) disables the tier surface entirely
	// — Session.ContextTier stays at TierPeak ("no signal"), the
	// chat panel renders no badge. In production, operators pin
	// this to the deployment's effective context window so the
	// PEAK / GOOD / DEGRADING / POOR signal is meaningful.
	chatContextBudget int

	// webUIBaseURL is the daemon's externally-reachable web UI
	// prefix. Used by the chat surface to render absolute
	// deliverable-link URLs alongside assistant replies. Empty
	// falls back to a relative path (same-origin clickable, not
	// portable outside).
	webUIBaseURL string

	// mcpRegistry powers the /ui/mcp daemon-level discovery page.
	// Nil = the page renders the "no daemon mcp block configured"
	// empty state. Same source the API server wires onto
	// /api/v1/mcp/servers so both surfaces stay coherent.
	mcpRegistry MCPRegistrySource

	// mcpRegistrySource feeds the project config form's "MCP
	// servers" section with the daemon-level registry of known
	// MCP servers + their advertised tool catalogs. Optional —
	// nil makes the form render an empty state banner with a
	// link to the operator's MCP UI (slice 1's page). The
	// parallel-agent's HTTP client adapter satisfies this
	// interface once GET /api/v1/mcp/servers lands.
	mcpRegistrySource MCPFormRegistrySource

	// Admin UI wiring — admin-ui-design.md slice 1. Every field is
	// nil-safe: handlers degrade with "not wired" empty states so
	// a partially-configured admin surface still renders rather
	// than 500-ing. Operators see the gap and configure the missing
	// piece without losing access to the rest.
	adminAuditRepo persistence.AdminAuditRepository
	// identityRepo backs /ui/admin/users (login approval). Nil (e.g.
	// non-postgres / identity core off) → the page renders a
	// "requires the identity core" notice and the nav badge is omitted.
	identityRepo persistence.IdentityRepository
	// uiSessionRepo backs the /ui/admin/users/{id}/sessions viewer
	// (list + terminate). Nil → the sessions page renders a notice.
	uiSessionRepo  persistence.UISessionRepository
	adminChatAudit persistence.ChatAuditRepository
	// memoryRetrievalAudit + memoryIngestAudit back /ui/admin/memory-audit
	// (B-16). Either can be nil; the page renders a per-tab "not
	// wired" hint when so.
	memoryRetrievalAudit persistence.MemoryRetrievalAuditRepository
	memoryIngestAudit    persistence.MemoryIngestAuditRepository
	// workflowProposalsRepo backs /ui/admin/workflow-proposals
	// (list + drill-down + decide). Same nil-safe convention as
	// the other admin surfaces. Slice 3c of the memetic-workflows
	// arc.
	workflowProposalsRepo persistence.WorkflowProposalRepository
	// workflowApplier powers the POST /apply form on the drill-
	// down page. Slice 4. nil hides the Apply button.
	workflowApplier WorkflowApplierUI
	// workflowRollbacker powers the POST /rollback form on the
	// drill-down page. Slice 5. nil hides the Rollback button.
	workflowRollbacker WorkflowRollbackerUI
	// workflowSourceUI loads the current on-disk WORKFLOW.md so the
	// proposal detail page can render a before/after diff (§8.5).
	// nil → the diff panel falls back to showing proposed YAML only.
	workflowSourceUI WorkflowSourceUI
	// workflowRollupSource fetches the workflow's current telemetry
	// rollup so the predicted-impact panel can show the real cost /
	// failure-rate baseline the proposal targets (Slice 3). nil →
	// the panel falls back to the heuristic predictedImpactSummary.
	workflowRollupSource WorkflowRollupSource
	// blackboxArchitect powers the "Generate candidate" button on
	// the workflow-healing trigger detail page. Wraps
	// *memetic.Architect via a service-layer adapter so the ui
	// package doesn't import internal/memetic. nil-safe — the
	// button is hidden when unwired.
	blackboxArchitect MemeticArchitectUI
	adminReadiness    ReadinessProvider
	adminLeaseAudit   LeaseAuditSource
	adminStuckExecs   StuckExecutionSource
	// leaderLockSource powers /ui/admin/health/cluster — cluster
	// topology + per-worker health derived from the
	// daemon_leader_locks table. Nil-safe; page renders the
	// "single-process deployment" placeholder when absent.
	leaderLockSource LeaderLockSource
	// clusterNodeSource powers the fleet section of
	// /ui/admin/health/cluster — every heartbeating node from the
	// cluster_nodes registry, including webhook/relay nodes that hold no
	// leases. Nil-safe; the fleet section is omitted when absent.
	clusterNodeSource ClusterNodeSource
	// operatorProfiles powers /ui/memory/operators — per-operator
	// profile listing + detail. Nil-safe; the page renders a
	// "not wired" hint when absent (SQLite + pre-migration-60
	// deployments).
	operatorProfiles OperatorProfileSource
	// operatorProfileAudit reads admin_audit rows for the
	// operator-profile detail page's audit panel. Lifted off
	// the wider AdminAuditRepository to keep the UI surface
	// narrow. Nil-safe; the panel hides when unwired.
	operatorProfileAudit OperatorProfileAuditSource
	adminMCPSource       MCPInventorySource
	adminMCPRefresher    MCPRefresher
	adminMCPConfig       MCPConfigSource
	// runtimeReadiness powers /ui/admin/health/runtime — voice STT/
	// TTS provider health + storage backend reachability. Nil-safe;
	// page renders an "Available: false" placeholder when absent.
	runtimeReadiness RuntimeReadinessSource
	// emailChannelInventory powers /ui/admin/integrations/email —
	// one row per project email channel with live ListSessions
	// data. Nil-safe; page renders an "Available: false"
	// placeholder when absent.
	emailChannelInventory EmailChannelInventory
	// dispatcherToolInventory powers
	// /ui/admin/integrations/dispatcher-tools — one row per
	// registered dispatcher tool + its backing-service wiring
	// state. Nil-safe; page renders the not-wired placeholder.
	dispatcherToolInventory DispatcherToolInventory

	// loginProviders is the ordered list of configured browser-login
	// provider names (e.g. ["github"]). Empty → /ui/login shows only
	// the break-glass key form. Set by WithLoginProviders (github-
	// login phase 3).
	loginProviders []string
	// logoutHandler serves POST /ui/logout — revokes the session and
	// clears the cookie. Nil → the nav renders no logout button and
	// the route 404s. Wired by WithLogoutHandler to the loginflow
	// Logout handler so the ui package keeps no auth dependency.
	logoutHandler http.Handler
}

// ActiveChatSource exposes a count of live chat sessions for the
// landing page tile. Production wires this to *telegram.Bot.
// Optional — without it the tile renders "—".
type ActiveChatSource interface {
	ActiveChatCount() int
}

// ServerOption is a functional option for configuring the Server.
type ServerOption func(*Server)

// WithLogger sets the logger for the server.
func WithLogger(logger zerolog.Logger) ServerOption {
	return func(s *Server) {
		s.logger = logger
	}
}

// WithTaskRepository sets the task repository.
func WithTaskRepository(repo persistence.TaskRepository) ServerOption {
	return func(s *Server) {
		s.taskRepo = repo
	}
}

// WithExecutionRepository sets the execution repository.
func WithExecutionRepository(repo persistence.ExecutionRepository) ServerOption {
	return func(s *Server) {
		s.execRepo = repo
	}
}

// WithProjectRegistry sets the project registry.
func WithProjectRegistry(r *registry.Registry) ServerOption {
	return func(s *Server) {
		s.projectReg = r
	}
}

// WithExecutor sets the executor for task lifecycle control.
func WithExecutor(e ExecutorInterface) ServerOption {
	return func(s *Server) {
		s.executor = e
		if src, ok := e.(TaskLogSource); ok {
			s.taskLogSource = src
		}
	}
}

// WithWorkspaceLock injects the shared per-project workspace lock so the
// artifact-delete path serialises with the executor and the git-over-HTTPS
// handler per project. The service container passes the SAME
// *workspacelock.Locker instance here, into the executor, and into the API
// server. Omitting it falls back to a private workspacelock.New() (correct in
// isolation; only the shared instance gives cross-subsystem exclusion).
func WithWorkspaceLock(l *workspacelock.Locker) ServerOption {
	return func(s *Server) {
		if l != nil {
			s.workspaceLock = l
		}
	}
}

// WithTaskLogSource sets the source used for task log streams.
func WithTaskLogSource(src TaskLogSource) ServerOption {
	return func(s *Server) {
		s.taskLogSource = src
	}
}

// WithToolAuditRepository sets the tool audit repository for the audit page.
func WithToolAuditRepository(repo persistence.ToolAuditRepository) ServerOption {
	return func(s *Server) {
		s.auditRepo = repo
	}
}

// WithWebhookEventRepository sets the webhook ingress audit repository.
func WithWebhookEventRepository(repo persistence.WebhookEventRepository) ServerOption {
	return func(s *Server) {
		s.webhookEventRepo = repo
	}
}

// WithAPIKeyRepository wires the DB-backed API-key surface so the
// /ui/projects/{id}/keys page can render and mutate rows. Nil leaves
// the page rendering 503.
func WithAPIKeyRepository(repo persistence.APIKeyRepository) ServerOption {
	return func(s *Server) {
		s.apiKeyRepo = repo
	}
}

// WithPublicBaseURL sets the operator-configured server.public_base_url.
// The value is used to derive the full git-over-HTTPS clone URL on the
// project Git-access panel. When empty the panel shows the relative path
// plus a "set server.public_base_url" hint.
func WithPublicBaseURL(u string) ServerOption {
	return func(s *Server) {
		s.publicBaseURL = u
	}
}

// WithArtifactRepository sets the artifact repository for the task detail page.
func WithArtifactRepository(repo persistence.ArtifactRepository) ServerOption {
	return func(s *Server) {
		s.artifactRepo = repo
	}
}

// ArtifactReader is the narrow interface the UI uses to read
// artifact bytes via the backend-aware artifact store. Implemented
// by *artifacts.Store. Phase-4 storage abstraction: routing reads
// through the Store lets the same UI work against the S3 backend
// without the handlers learning a new code path.
type ArtifactReader interface {
	Retrieve(ctx context.Context, artifactID string) ([]byte, error)
	Open(ctx context.Context, artifactID string) (io.ReadCloser, error)
}

// WithArtifactReader wires the backend-aware artifact reader. When
// supplied, task_detail.go reads the changelog body through it and
// ArtifactDownload streams via Open. Nil falls back to the legacy
// direct-disk path — only correct on the filesystem backend.
func WithArtifactReader(r ArtifactReader) ServerOption {
	return func(s *Server) {
		s.artifactReader = r
	}
}

// WithLLMUsageRepository wires the per-step LLM usage repo so the project
// detail page can render the spend panel (last 24h / 7d / 30d, by role).
func WithLLMUsageRepository(repo persistence.TaskLLMUsageRepository) ServerOption {
	return func(s *Server) {
		s.llmUsageRepo = repo
	}
}

// WithAutonomyEvaluationRepository wires the autonomy eval repo so
// the dashboard's "next autonomy eval" tile can render. Optional —
// without it the dashboard just hides the tile.
func WithAutonomyEvaluationRepository(repo persistence.AutonomyEvaluationRepository) ServerOption {
	return func(s *Server) {
		s.autonomyEvalRepo = repo
	}
}

// WithActiveChatSource wires the bot's chat count for the landing
// page tile. Optional — without it the tile shows "—".
func WithActiveChatSource(src ActiveChatSource) ServerOption {
	return func(s *Server) {
		s.activeChatSource = src
	}
}

// WithProjectTemplates wires the project-template catalog so the
// /ui/projects/new gallery can render. Optional — without it the
// gallery page degrades to a "no catalog installed" empty state
// instead of returning a 500. Pair with WithConfigsDir to enable
// the POST materialisation flow.
func WithProjectTemplates(cat *templates.Catalog) ServerOption {
	return func(s *Server) {
		s.projectTemplates = cat
	}
}

// WithConfigsDir sets the writable root the gallery materialises
// rendered template files under. Same value the API server uses;
// required for the POST /ui/projects/new flow. Without it the form
// renders read-only with an explanatory banner.
func WithConfigsDir(dir string) ServerOption {
	return func(s *Server) {
		s.configsDir = dir
	}
}

// WithRetentionDefaults wires the daemon-wide retention defaults
// so the per-project retention panel can mark which fields are
// inherited vs overridden by the project YAML. Optional — without
// it the panel renders using the hardcoded fallbacks (matching
// retention.Default* constants).
func WithRetentionDefaults(defaults registry.ProjectRetention) ServerOption {
	return func(s *Server) {
		s.retentionDefaults = defaults
	}
}

// WithMCPRegistry wires the daemon-level MCP discovery source used
// by /ui/mcp. Nil leaves the page in its empty state ("no daemon
// mcp.servers block configured"). Same source the API server
// consumes via api.WithMCPRegistry; pass the same instance to
// keep the two surfaces coherent.
func WithMCPRegistry(r MCPRegistrySource) ServerOption {
	return func(s *Server) {
		s.mcpRegistry = r
	}
}

// WithMemoryQuarantineRepository wires the Phase-2 quarantine repo
// for the /ui/memory section.
func WithMemoryQuarantineRepository(repo persistence.MemoryQuarantineRepository) ServerOption {
	return func(s *Server) { s.memoryQuarantine = repo }
}

// WithMemoryConfigured snapshots whether config.memory.enabled is on
// so /ui/memory can render the correct disabled-vs-unavailable state.
func WithMemoryConfigured(enabled bool) ServerOption {
	return func(s *Server) { s.memoryConfigured = enabled }
}

// WithMemoryEvictor wires the hard-eviction surface (operator-
// triggered DELETE + audit-log viewer). Without it, the form
// renders a "evict not enabled" placeholder.
func WithMemoryEvictor(ev MemoryEvictor) ServerOption {
	return func(s *Server) { s.memoryEvictor = ev }
}

// WithRetentionPreviewer wires the retention dry-run estimator
// for the project-detail page. Optional — without it the retention
// preview panel renders an "N/A" placeholder.
func WithRetentionPreviewer(p RetentionPreviewer) ServerOption {
	return func(s *Server) { s.retentionPreviewer = p }
}

// EmbeddingCacheStatsSource returns row-count + on-disk-size stats
// for the embedding_cache table. Optional — when nil the
// /ui/spend embedding-cache panel renders the "disabled"
// placeholder. Concrete impl is in internal/memory; the type lives
// here so the ui package doesn't import memory.
type EmbeddingCacheStatsSource interface {
	CacheStats(ctx context.Context) (EmbeddingCacheStats, error)
}

// EmbeddingCacheStats mirrors memory.EmbeddingCacheStats at the
// UI boundary. Identical fields; defined here to keep the ui
// package importer-free of memory's pgvector wiring.
type EmbeddingCacheStats struct {
	RowCount       int64
	ApproxBytes    int64
	DistinctModels int
}

// WithEmbeddingCacheStatsSource wires the embedding-cache stats
// probe for the /ui/spend panel. Nil leaves the panel disabled.
func WithEmbeddingCacheStatsSource(src EmbeddingCacheStatsSource) ServerOption {
	return func(s *Server) { s.embeddingCacheStats = src }
}

// ResponseCacheStatsSource returns row-count + on-disk-size + hit
// total for the llm_response_cache table (Phase E). Optional —
// when nil the /ui/spend response-cache panel renders the
// "disabled" placeholder. Concrete impl is in internal/memory.
type ResponseCacheStatsSource interface {
	CacheStats(ctx context.Context) (ResponseCacheStats, error)
}

// ResponseCacheStats mirrors memory.ResponseCacheStats at the UI
// boundary. Identical fields; defined here to keep the ui package
// importer-free of memory's postgres wiring.
type ResponseCacheStats struct {
	RowCount         int64
	ApproxBytes      int64
	DistinctPurposes int
	TotalHits        int64
	// TotalSavingsUSD is the lifetime $ amount saved by cache hits.
	// Zero when no pricing function was wired or all rows are on
	// un-priced models.
	TotalSavingsUSD float64
}

// WithResponseCacheStatsSource wires the response-cache stats
// probe for the /ui/spend Phase E panel. Nil leaves the panel
// disabled.
func WithResponseCacheStatsSource(src ResponseCacheStatsSource) ServerOption {
	return func(s *Server) { s.responseCacheStats = src }
}

// WizardSessionLister is the narrow interface the
// /ui/projects drafts banner consumes — ListByOperator only.
// Concrete impl is persistence.ProjectWizardSessionRepository.
type WizardSessionLister interface {
	ListByOperator(ctx context.Context, operatorID string, pageSize int) ([]*persistence.ProjectWizardSession, error)
}

// WithWizardSessionLister wires the drafts source for the
// /ui/projects banner. Optional — nil hides the banner.
func WithWizardSessionLister(src WizardSessionLister) ServerOption {
	return func(s *Server) { s.wizardSessions = src }
}

// WithOnboardingDetector wires the install-scoped setup detector used by
// /ui/setup. Nil keeps the page on the same conservative heuristic as
// the API status endpoint.
func WithOnboardingDetector(det onboarding.Detector) ServerOption {
	return func(s *Server) { s.onboardingDetector = det }
}

// WithSingleTenantOperatorID wires the auth-off operator fallback the
// drafts banner uses to stay visible on local installs. The service
// container resolves the value via api.SingleTenantOperatorIDFromConfig
// so the UI sees the same identity wizard handlers stamp on auth-off
// requests. Empty string disables the fallback (legacy header-only
// behaviour).
func WithSingleTenantOperatorID(id string) ServerOption {
	return func(s *Server) { s.singleTenantOperatorID = id }
}

// VectorVizSource provides the data for the per-project vector
// scatter on /ui/memory/<project>. Concrete implementation lives
// in the memory package (Repository.SampleChunksForViz +
// pca2 projection) — kept behind an interface so the ui package
// doesn't pull memory's pgvector wiring directly into its tests.
type VectorVizSource interface {
	SampleProjection(ctx interface{ Done() <-chan struct{} }, projectID string, activeEpochs []string, limit int) ([]VizPoint, error)
}

// PipelineDryRunner runs the ingest gate stack against arbitrary
// content without writing to the corpus. Used by the pipeline
// inspector UI to let operators test what the gates would do.
// Concrete implementation is *memory.Pipeline; we hide it behind
// an interface so the UI tests don't need a running pgvector.
//
// DryRunWithExecution adds the ingest_execution_id surface used
// by the claim_audit_overlap gate — operators paste a real
// execution_id to reproduce a production verdict.
type PipelineDryRunner interface {
	DryRun(projectID, sourceName, producerRole, content string) DryRunResult
	DryRunWithExecution(projectID, sourceName, producerRole, executionID, content string) DryRunResult
}

// MemorySearcher backs the operator-facing search box on
// /ui/memory/<project>. Hidden behind an interface so the ui
// package's tests don't need pgvector wired.
type MemorySearcher interface {
	Search(ctx context.Context, projectID, query string, limit int) ([]MemorySearchResult, error)
	// SearchWithScope is the B-6 extension: filters results to
	// chunks tagged with repoScope OR scope='*' (cross-cutting)
	// OR repo_scope IS NULL (uncategorized, kept visible during
	// the migration window). Empty repoScope behaves like the
	// legacy Search() — project-wide, no scope filter.
	SearchWithScope(ctx context.Context, projectID, query string, limit int, repoScope string) ([]MemorySearchResult, error)
	// ListRepoScopes backs the /ui/memory scope dropdown. Returns
	// distinct repo_scope tokens for the project + chunk counts.
	// Sorted by count desc. Empty `Scope` field = uncategorized
	// (NULL in the DB).
	ListRepoScopes(ctx context.Context, projectID string) ([]MemoryRepoScope, error)
}

// MemoryRepoScope is the UI-layer mirror of memory.RepoScopeCount.
// Kept here to keep the ui package free of memory-package imports.
type MemoryRepoScope struct {
	Scope  string // "" for uncategorized
	Chunks int
}

// RetentionPreviewer estimates how many rows the next retention
// sweep would prune for a project, without actually deleting.
// Implemented by an adapter in the service container that wraps
// retention.Sweeper.Preview + the daemon's resolved policy for
// the project.
type RetentionPreviewer interface {
	Preview(ctx context.Context, projectID string) (RetentionPreviewCounts, error)
}

// RetentionPreviewCounts mirrors retention.Counts at the UI
// boundary. Pre-rendered fields kept absent so the template stays
// dumb — operator sees the row count per table, knows the next
// scheduled sweep would delete them.
type RetentionPreviewCounts struct {
	TaskLLMUsage  int
	ToolAudit     int
	Tasks         int
	Executions    int
	Artifacts     int
	ArtifactFiles int
	TaskMessages  int
	MemoryChunks  int
}

// Total reports the sum of all row counts across tables. Used by
// the template's "X rows pruned next sweep" headline so the
// operator gets one signal at a glance.
func (c RetentionPreviewCounts) Total() int {
	return c.TaskLLMUsage + c.ToolAudit + c.Tasks + c.Executions +
		c.Artifacts + c.TaskMessages + c.MemoryChunks
}

// MemoryEvictor is the narrow interface the UI uses for hard-
// eviction operations + the eviction audit log. Implemented by an
// adapter in the service container that wraps memory.Corrector +
// memory.Repository. Defined here so the ui package doesn't import
// internal/memory directly.
type MemoryEvictor interface {
	// HardEvict permanently deletes the named chunks under
	// projectID, writes one memory_eviction_audit row per
	// deleted chunk, and returns the number actually deleted
	// (may be shorter than chunkIDs when some are stale).
	HardEvict(ctx context.Context, projectID string, chunkIDs []string, reason, evictedBy string) (int, error)
	// ListEvictionAudits returns recent tombstones for the
	// project, newest first. Powers the audit panel under
	// /ui/memory/<project>.
	ListEvictionAudits(ctx context.Context, projectID string, limit int) ([]MemoryEvictionAuditRow, error)
}

// MemoryEvictionAuditRow mirrors memory.EvictionAuditEntry at the
// UI boundary so the ui package doesn't depend on the memory
// package's types. The service-container adapter copies values
// across.
type MemoryEvictionAuditRow struct {
	ID           string
	ChunkID      string
	ContentHash  string
	SourceName   string
	ContentClass string
	ProducerRole string
	Reason       string
	EvictedBy    string
	EvictedAt    string
}

// MemorySearchResult mirrors memory.SearchResult at the UI
// boundary. Score is the RRF/hybrid combined score.
type MemorySearchResult struct {
	ChunkID    string  `json:"chunk_id"`
	ProjectID  string  `json:"project_id"`
	TaskID     string  `json:"task_id"`
	SourceName string  `json:"source_name"`
	Content    string  `json:"content"`
	Score      float64 `json:"score"`
	// RepoScope is the chunk's repo_scope token. Empty = NULL in
	// the DB (uncategorized). Surfaced to the template so the
	// operator can see at a glance whether the hit matches the
	// scope they filtered for, is a cross-cutting '*' chunk, or
	// (when running in non-strict mode) an uncategorized chunk
	// that leaked through.
	RepoScope string `json:"repo_scope,omitempty"`
}

// DryRunResult mirrors memory.DryRunResult at the UI boundary so
// the ui package's tests don't pull the memory package. The
// service-container adapter copies values across.
type DryRunResult struct {
	Final                DryRunGateOutcome
	Trail                []DryRunGateOutcome
	Class                string
	TTLDays              int
	DefaultConfidence    float32
	RoleOfRecordEligible bool
	PostRedactContent    string
	// Claims is the inspector view of the candidate's
	// extracted-claim set + audit-overlap verdict. Empty when no
	// execution_id was supplied (the gate can't score without one).
	Claims []DryRunClaim
}

// DryRunGateOutcome — UI-facing copy of memory.GateOutcome. Action
// is a string because the ui templates render it directly; the
// memory package's typed enum stays internal. JSON tags are
// lower-cased so the inline JS at /ui/memory/<project> can read
// `final.action` directly without a TitleCase rename pass.
type DryRunGateOutcome struct {
	Gate         string `json:"gate"`
	Action       string `json:"action"` // "allow" | "redact" | "quarantine" | "reject"
	Detail       string `json:"detail"`
	NewContent   string `json:"newContent,omitempty"`
	ShadowSignal bool   `json:"shadowSignal,omitempty"`
}

// DryRunClaim — UI-facing copy of memory.ClaimMatch. Carries the
// per-claim audit-grounded verdict for the inspector's "Claims
// extracted" panel.
type DryRunClaim struct {
	Category   string `json:"category"`
	Value      string `json:"value"`
	Found      bool   `json:"found"`
	AuditRowID string `json:"auditRowId,omitempty"`
}

// VizPoint is one scatter dot — server-rendered coordinates
// (already in the SVG viewBox after PCA + autoscale) plus the
// metadata the tooltip + legend need. Neighbors carries the top-K
// nearest chunks by cosine similarity for the relationship overlay.
type VizPoint struct {
	X, Y, Z          float32
	ContentSize      int
	ChunkID          string
	SourceName       string
	ContentClass     string
	ValidationStatus string
	ProducerRole     string
	Preview          string
	Neighbors        []VizNeighbor
}

// VizNeighbor is one edge endpoint — chunk ID + similarity. The UI
// uses this to draw lines from a selected node to its top-K
// nearest in the scatter and to populate the detail sidebar's
// "related chunks" list.
type VizNeighbor struct {
	ChunkID    string
	Similarity float32
}

// WithVectorVizSource wires the projection source for /ui/memory.
// nil-safe — without it the scatter panel renders an empty-state
// note instead of a viz.
func WithVectorVizSource(src VectorVizSource) ServerOption {
	return func(s *Server) { s.vectorViz = src }
}

// WithPipelineDryRunner wires the gate-stack dry runner for the
// pipeline inspector. nil-safe — without it the inspector form
// returns 503.
func WithPipelineDryRunner(dr PipelineDryRunner) ServerOption {
	return func(s *Server) { s.pipelineDryRun = dr }
}

// WithMemorySearcher wires the hybrid (RRF) memory searcher
// backing the search box on /ui/memory/<project>. nil-safe —
// without it the search panel renders a "not configured" notice.
func WithMemorySearcher(ms MemorySearcher) ServerOption {
	return func(s *Server) { s.memorySearcher = ms }
}

// WithCorpusEpochRepository wires the Phase-3 epoch repo for the
// /ui/memory section.
func WithCorpusEpochRepository(repo persistence.CorpusEpochRepository) ServerOption {
	return func(s *Server) { s.corpusEpochs = repo }
}

// WithIngestQueueRepository wires the Phase-1 ingest queue repo
// for the /ui/memory section's queue-depth tile.
func WithIngestQueueRepository(repo persistence.IngestQueueRepository) ServerOption {
	return func(s *Server) { s.ingestQueue = repo }
}

// WithChunkGraphRepository wires the KG extraction-progress repo
// for the /ui/memory landing page widget that shows pending vs
// done chunks + entity/edge/mention totals. Optional — without
// it the widget renders "KG pipeline not enabled".
func WithChunkGraphRepository(repo persistence.ChunkGraphExtractionRepository) ServerOption {
	return func(s *Server) { s.chunkGraph = repo }
}

// WithKnowledgeGraphReader wires the KG read surface backing the
// entity browser, entity detail, and subgraph viewer pages under
// /ui/memory/<project>/entities + /subgraph. Optional — without it
// those pages render a "knowledge-graph read not enabled" state.
// see https://docs.vornik.io §7.
func WithKnowledgeGraphReader(r KnowledgeGraphReader) ServerOption {
	return func(s *Server) { s.kgSearcher = r }
}

// WithStepOutcomeRepository wires the per-step outcome repo so the
// execution detail page can render the "Step Outcomes" panel — the
// per-step quality signal (ok / parse_error / refused / etc.) that's
// more useful than the binary success/fail in the execution header.
func WithStepOutcomeRepository(repo persistence.ExecutionStepOutcomeRepository) ServerOption {
	return func(s *Server) {
		s.outcomeRepo = repo
	}
}

// WithJudgeVerdictRepository wires the Phase 3 LLM-as-judge
// verdict repo so the task detail page can render the verdict
// panel. Optional — without it the panel doesn't appear.
func WithJudgeVerdictRepository(repo persistence.TaskJudgeVerdictRepository) ServerOption {
	return func(s *Server) {
		s.judgeVerdictRepo = repo
	}
}

// WithRecoveryEventRepository wires the recovery-event store for the Trends
// page's recovery series. Optional — without it the recovery chart is hidden.
func WithRecoveryEventRepository(repo persistence.RecoveryEventRepository) ServerOption {
	return func(s *Server) {
		s.recoveryEventRepo = repo
	}
}

// WithHintRepository wires the steering-hint backing so the task
// detail page can interleave hints with the message thread.
// 2026-05-26 — added with the unified-timeline refactor; nil-safe.
func WithHintRepository(repo persistence.ExecutionHintRepository) ServerOption {
	return func(s *Server) { s.hintRepo = repo }
}

// WithTaskMessageRepository wires the conversation thread backing.
// Phase 26+ of the conversational task lifecycle.
func WithTaskMessageRepository(repo persistence.TaskMessageRepository) ServerOption {
	return func(s *Server) { s.taskMessageRepo = repo }
}

// WithTaskScratchpadRepository wires the lead's running summary.
func WithTaskScratchpadRepository(repo persistence.TaskScratchpadRepository) ServerOption {
	return func(s *Server) { s.taskScratchpadRepo = repo }
}

// Rescheduler is the narrow interface the UI conversation
// handlers use to wake the scheduler after a re-queue. Same shape
// as api.Rescheduler — kept duplicate so the two packages don't
// import each other.
type Rescheduler interface {
	Wake()
}

// WithRescheduler wires the scheduler-wake hook for the UI.
func WithRescheduler(r Rescheduler) ServerOption {
	return func(s *Server) { s.rescheduler = r }
}

// WithTradingSnapshotRepository wires the equity-snapshot repo
// so the project-detail page's trading panel can compute Sharpe
// + max drawdown over the soak window. Optional — without it
// the soak metrics tile renders "no data yet" while the live
// portfolio block continues to work off the broker MCP.
func WithTradingSnapshotRepository(repo persistence.TradingPositionsSnapshotRepository) ServerOption {
	return func(s *Server) {
		s.tradingSnapshotRepo = repo
	}
}

// WithTradingOrderRepository wires the trading-orders audit repo
// so the trading panel can render the orders today / 7d /
// volume USD tiles populated by the broker→daemon audit channel.
// Optional — without it those tiles render zeroes (or are
// hidden by the template's {{if}} guards).
func WithTradingOrderRepository(repo persistence.TradingOrderRepository) ServerOption {
	return func(s *Server) {
		s.tradingOrderRepo = repo
	}
}

// WithTradingSafetyRepository wires the safety-event audit repo
// for the project-page recent-safety-events timeline. Phase 2
// of the broker→daemon audit channel — populated by every
// refusal / replay / kill / breaker event the broker emits.
func WithTradingSafetyRepository(repo persistence.TradingSafetyEventRepository) ServerOption {
	return func(s *Server) {
		s.tradingSafetyRepo = repo
	}
}

// WithTradingFillRepository wires the fill-events repo so the
// soak panel can render precise volume (SUM(qty × fill_price))
// instead of the trading_orders LMT-price estimate. Optional —
// nil keeps the legacy estimate path active so deployments
// without Phase-3 fill ingestion keep their existing tile.
func WithTradingFillRepository(repo persistence.TradingFillRepository) ServerOption {
	return func(s *Server) {
		s.tradingFillRepo = repo
	}
}

// WithTradingEnabled registers the /trading dashboard route. Wired by the
// container only when the EE trading capability is present
// (c.providers.Trading != nil); Community builds omit it so /trading 404s.
func WithTradingEnabled() ServerOption {
	return func(s *Server) {
		s.tradingEnabled = true
	}
}

// PostMortemExplainer is the narrow surface the task-detail
// page needs from internal/postmortem. Defined here so the ui
// package doesn't import postmortem (which would pull chat +
// pricing into the UI binary's import graph). The daemon's
// container constructs the real explainer and passes it in.
type PostMortemExplainer interface {
	Generate(ctx context.Context, taskID string, forceRefresh bool) (*PostMortemResult, error)
}

// PostMortemResult mirrors postmortem.Result without the
// import. The handler treats the two identically; the
// adapter in container.go wraps a *postmortem.Explainer to
// satisfy this interface.
type PostMortemResult struct {
	Cached     bool
	PostMortem *persistence.TaskPostMortem
}

// WithPostMortemRepository wires the task_post_mortems repo so
// the task-detail page can show the cached explainer without
// re-firing the LLM on every page load. Optional — without
// it the panel won't render an "Explain this failure" button.
func WithPostMortemRepository(repo persistence.TaskPostMortemRepository) ServerOption {
	return func(s *Server) {
		s.postMortemRepo = repo
	}
}

// WithInstinctPlaybooks wires the continuous-learning instinct
// repository so the failed-task page can show the (advisory) "similar
// failures here resolved by …" panel — Consumer A, slice 3. enabled
// mirrors the instinct.consumers.failure_playbooks gate (the container
// passes false unless that AND instinct.enabled are both on). With the
// gate off (or a nil repo) the panel never renders and the page is
// byte-for-byte unchanged. Read-only: the UI surface only lists; it
// records no application rows.
func WithInstinctPlaybooks(repo persistence.InstinctRepository, enabled bool) ServerOption {
	return func(s *Server) {
		s.instinctRepo = repo
		s.instinctPlaybooks = enabled
	}
}

// WithPostMortemExplainer wires the LLM-backed explainer so
// the operator's "Explain this failure" button has something
// to call. Optional — without it the button isn't rendered
// (the cached panel still works if the repo is wired).
func WithPostMortemExplainer(e PostMortemExplainer) ServerOption {
	return func(s *Server) {
		s.postMortemExplainer = e
	}
}

// WithCrossProjectCallRepository wires the cross-project call
// ledger so the replay page can render the multi-hop tree
// (inter-project orchestration Phase C). Optional — when not
// wired the replay falls back to the single-project timeline.
func WithCrossProjectCallRepository(repo persistence.CrossProjectCallRepository) ServerOption {
	return func(s *Server) {
		s.crossProjectCallRepo = repo
	}
}

// WithProjectSpawnRepository wires the spawn lineage ledger
// so the replay page renders spawn edges with the template
// slug. Optional — Phase C surface.
func WithProjectSpawnRepository(repo persistence.ProjectSpawnRepository) ServerOption {
	return func(s *Server) {
		s.projectSpawnRepo = repo
	}
}

// WithArtifactBasePath sets the root directory for artifact storage.
// When set, ArtifactDownload validates that the requested file is within
// this directory before serving it.
func WithArtifactBasePath(path string) ServerOption {
	return func(s *Server) {
		s.artifactBasePath = path
	}
}

// WithConfigReloader wires the daemon config reload path for UI config edits.
func WithConfigReloader(reloader ConfigReloader) ServerOption {
	return func(s *Server) {
		s.configReloader = reloader
	}
}

// WithAssistantLLM wires the prompt-writing assistant backend
// (Phase 2 of the web authoring UX). When nil/unset, the
// POST /assistant/draft handler returns 503 with an actionable
// "not configured" message rather than failing silently.
func WithAssistantLLM(llm AssistantLLM) ServerOption {
	return func(s *Server) {
		s.assistantLLM = llm
	}
}

// WithAssistantDefaultModel sets the fallback model the
// assistant uses when neither the project nor the swarm's
// leadRole pins one. Typically the daemon's configured default.
func WithAssistantDefaultModel(model string) ServerOption {
	return func(s *Server) {
		s.assistantDefaultModel = model
	}
}

// WithAssistantPricing wires the daemon's pricing table for
// computing assistant-call cost. The handler still works
// without one (cost lands as zero on the usage row); pricing
// just makes the spend dashboard reflect authoring cost.
func WithAssistantPricing(t *pricing.Table) ServerOption {
	return func(s *Server) {
		s.assistantPricing = t
	}
}

// WithRateLimiter wires the shared task-creation rate limiter for UI task creation.
// Accepts any backend (in-process or postgres) via the ProjectLimiter interface.
func WithRateLimiter(l ratelimit.ProjectLimiter) ServerOption {
	return func(s *Server) {
		s.rateLimiter = l
	}
}

// WithAPIKeyLimiter wires the per-API-key token-bucket the API
// subtree enforces. UI reads its bucket levels to render the
// project homepage "approaching limit" banner. Optional; nil
// disables the banner without breaking the page.
func WithAPIKeyLimiter(l *ratelimit.APIKeyLimiter) ServerOption {
	return func(s *Server) {
		s.apiKeyLimiter = l
	}
}

// WithRateLimitMetrics wires the shared rate-limit observability
// surface. UI reads StatusFor to surface recent warnings and the
// last 429 on the homepage banner. Optional; nil keeps the page
// rendering at zero counts.
func WithRateLimitMetrics(m *ratelimit.Metrics) ServerOption {
	return func(s *Server) {
		s.rateLimitMetrics = m
	}
}

// WithReminderRepository wires the dispatcher_reminders repo so
// the per-project page can render an "upcoming reminders" tile.
// Optional — when nil the tile is empty.
func WithReminderRepository(repo persistence.ReminderRepository) ServerOption {
	return func(s *Server) { s.reminderRepo = repo }
}

// WithBlackBoxService wires the Autonomy Black Box read-side
// service behind /ui/admin/blackbox. Optional — when nil the
// page renders a "trace service not configured" message and the
// per-task drill-down links degrade gracefully. The concrete
// implementation is injected via an EE adapter (BlackBoxTraceService).
func WithBlackBoxService(svc BlackBoxTraceService) ServerOption {
	return func(s *Server) { s.blackboxService = svc }
}

// WithHealingTriggerRepository wires the workflow-healing
// trigger repo behind /ui/admin/blackbox's triggers panel.
// Optional — when nil the panel is hidden.
func WithHealingTriggerRepository(repo persistence.WorkflowHealingTriggerRepository) ServerOption {
	return func(s *Server) { s.healingTriggerRepo = repo }
}

// WithBlackBoxArchitect wires the memetic architect behind the
// "Generate candidate" button on the workflow-healing trigger
// detail page. Optional — when nil the button is hidden and the
// trigger can only be Dismissed.
func WithBlackBoxArchitect(a MemeticArchitectUI) ServerOption {
	return func(s *Server) { s.blackboxArchitect = a }
}

// WithHealingOverrideRepository wires the per-(project, workflow,
// trigger_class) override repo behind /ui/admin/blackbox/overrides.
// Optional — when nil the page surfaces an empty state and the
// links from the trigger detail page hide.
func WithHealingOverrideRepository(repo persistence.HealingTriggerOverrideRepository) ServerOption {
	return func(s *Server) { s.healingOverrideRepo = repo }
}

// WithHealingCandidateRepository wires the candidate ledger behind
// /ui/admin/blackbox/candidates. Optional — when nil the page surfaces
// an empty state.
func WithHealingCandidateRepository(repo persistence.WorkflowHealingCandidateRepository) ServerOption {
	return func(s *Server) { s.healingCandidateRepo = repo }
}

// WithHealingTrialRepository wires the trial ledger so the candidate
// detail page can render the trial scorecard history. Optional.
func WithHealingTrialRepository(repo persistence.WorkflowHealingTrialRepository) ServerOption {
	return func(s *Server) { s.healingTrialRepo = repo }
}

// WithHealingTrialRunner wires the operator-triggered trial runner
// behind the run-trial button. Optional — when nil the button is hidden.
func WithHealingTrialRunner(runner HealingTrialRunnerUI) ServerOption {
	return func(s *Server) { s.healingTrialRunner = runner }
}

// WithHealingCandidatePromoter wires the promote/reject actions.
// Optional — when nil both buttons are hidden.
func WithHealingCandidatePromoter(p HealingCandidatePromoterUI) ServerOption {
	return func(s *Server) { s.healingPromoter = p }
}

// WithArchiveSweeper wires the project-archival deletion runner
// so the "Delete now" button can kick a sweep immediately rather
// than waiting for the next scheduled tick. Optional.
func WithArchiveSweeper(sw ArchiveSweeper) ServerOption {
	return func(s *Server) { s.archiveSweeper = sw }
}

// WithArchiveLifecycle wires the shared lifecycle service so
// archive/unarchive/delete-now go through the same code path the
// REST API uses. Optional — when nil the UI handlers fall back to
// the legacy inline mutation path (still works, just doesn't
// share state with the REST API).
func WithArchiveLifecycle(svc ArchiveLifecycle) ServerOption {
	return func(s *Server) { s.archiveLifecycle = svc }
}

// WithAdminAuditRepository wires the admin-audit repository for
// the /ui/admin/audit page and for the audit-row write the
// `/ui/admin/health/mcp` refresh POST emits. Optional — without
// it the admin landing page renders an empty audit panel.
func WithAdminAuditRepository(repo persistence.AdminAuditRepository) ServerOption {
	return func(s *Server) { s.adminAuditRepo = repo }
}

// WithIdentityRepository wires the identity-core repository for the
// /ui/admin/users login-approval page. Optional — without it the page
// renders a "requires the identity core (postgres)" notice.
func WithIdentityRepository(repo persistence.IdentityRepository) ServerOption {
	return func(s *Server) { s.identityRepo = repo }
}

// WithUISessionRepository wires the browser-session repository for the
// /ui/admin/users/{id}/sessions viewer. Optional — without it the
// sessions page renders a "requires the identity core" notice.
func WithUISessionRepository(repo persistence.UISessionRepository) ServerOption {
	return func(s *Server) { s.uiSessionRepo = repo }
}

// WithWorkflowProposalsRepository wires the proposals repo for the
// /ui/admin/workflow-proposals page (memetic-workflows arc, Slice 3c).
// Nil-safe: the page renders an empty state when the repo isn't wired.
func WithWorkflowProposalsRepository(repo persistence.WorkflowProposalRepository) ServerOption {
	return func(s *Server) { s.workflowProposalsRepo = repo }
}

// WithWorkflowApplierUI wires the applier behind the POST /apply
// form on the drill-down page (memetic-workflows arc, Slice 4).
// nil hides the Apply button.
func WithWorkflowApplierUI(a WorkflowApplierUI) ServerOption {
	return func(s *Server) { s.workflowApplier = a }
}

// WithWorkflowRollbackerUI wires the rollbacker behind the POST
// /rollback form on the drill-down page (Slice 5). nil hides the
// Rollback button.
func WithWorkflowRollbackerUI(r WorkflowRollbackerUI) ServerOption {
	return func(s *Server) { s.workflowRollbacker = r }
}

// WithWorkflowSourceUI wires the current-WORKFLOW.md loader behind
// the proposal detail page's diff panel (§8.5). nil → the panel
// shows the proposed YAML alone (no before/after).
func WithWorkflowSourceUI(src WorkflowSourceUI) ServerOption {
	return func(s *Server) { s.workflowSourceUI = src }
}

// WithWorkflowRollupSource wires the telemetry rollup source behind
// the proposal detail page's predicted-impact "Current baseline"
// block (Slice 3). nil-safe: the panel falls back to the heuristic
// summary when the source is absent / returns an error / has no runs.
// see https://docs.vornik.io §Slice-3
func WithWorkflowRollupSource(src WorkflowRollupSource) ServerOption {
	return func(s *Server) { s.workflowRollupSource = src }
}

// WithAdminChatAuditRepository wires the chat-audit repository
// (one row per dispatcher turn) for /ui/admin/chat-audit. Optional
// — without it the page renders a "not wired" empty state.
func WithAdminChatAuditRepository(repo persistence.ChatAuditRepository) ServerOption {
	return func(s *Server) { s.adminChatAudit = repo }
}

// WithMemoryRetrievalAuditRepository wires the retrieval-audit repo
// for /ui/admin/memory-audit (B-16). Optional — without it the page
// renders a "not wired" hint on the retrieval tab.
func WithMemoryRetrievalAuditRepository(repo persistence.MemoryRetrievalAuditRepository) ServerOption {
	return func(s *Server) { s.memoryRetrievalAudit = repo }
}

// WithMemoryIngestAuditRepository wires the ingest-audit repo for
// /ui/admin/memory-audit (B-16). Optional; ingest-tab empty state
// when nil.
func WithMemoryIngestAuditRepository(repo persistence.MemoryIngestAuditRepository) ServerOption {
	return func(s *Server) { s.memoryIngestAudit = repo }
}

// WithAdminReadinessProvider wires an in-process readiness source
// for the admin landing tile. Same checks /readyz runs over HTTP;
// keeping them in-process avoids the daemon self-curling itself.
func WithAdminReadinessProvider(p ReadinessProvider) ServerOption {
	return func(s *Server) { s.adminReadiness = p }
}

// WithAdminLeaseAuditSource wires the tasks_lease_audit reader
// for the /ui/admin/health/leases page.
func WithAdminLeaseAuditSource(src LeaseAuditSource) ServerOption {
	return func(s *Server) { s.adminLeaseAudit = src }
}

// WithAdminStuckExecutionSource wires the executions-watchdog
// reader for /ui/admin/health/watchdog.
func WithAdminStuckExecutionSource(src StuckExecutionSource) ServerOption {
	return func(s *Server) { s.adminStuckExecs = src }
}

// WithLeaderLockSource wires the daemon_leader_locks reader for
// /ui/admin/health/cluster. Nil-safe; page renders a
// "single-process deployment" placeholder when absent.
func WithLeaderLockSource(src LeaderLockSource) ServerOption {
	return func(s *Server) { s.leaderLockSource = src }
}

// WithClusterNodeSource wires the cluster_nodes reader for the fleet section
// of /ui/admin/health/cluster. Nil-safe; the fleet section is omitted when
// absent. Surfaces webhook/relay nodes the lease tables can't show.
func WithClusterNodeSource(src ClusterNodeSource) ServerOption {
	return func(s *Server) { s.clusterNodeSource = src }
}

// WithOperatorProfileSource wires the operator_profile reader
// for /ui/memory/operators. Nil-safe; page renders a "not
// wired" hint when absent.
func WithOperatorProfileSource(src OperatorProfileSource) ServerOption {
	return func(s *Server) { s.operatorProfiles = src }
}

// WithOperatorProfileAuditSource wires the audit-log reader
// the operator-profile detail page consumes. Nil-safe; the
// "Recent changes" panel hides when unwired.
func WithOperatorProfileAuditSource(src OperatorProfileAuditSource) ServerOption {
	return func(s *Server) { s.operatorProfileAudit = src }
}

// WithAdminMCPInventory wires the MCP inventory snapshot source
// for /ui/admin/health/mcp.
func WithAdminMCPInventory(src MCPInventorySource) ServerOption {
	return func(s *Server) { s.adminMCPSource = src }
}

// WithAdminMCPRefresher wires the daemon-wide MCP refresh trigger
// for the POST surface on /ui/admin/health/mcp.
func WithAdminMCPRefresher(r MCPRefresher) ServerOption {
	return func(s *Server) { s.adminMCPRefresher = r }
}

// WithAdminMCPConfigSource wires the read-only MCP configuration
// listing for /ui/admin/integrations/mcp.
func WithAdminMCPConfigSource(src MCPConfigSource) ServerOption {
	return func(s *Server) { s.adminMCPConfig = src }
}

// WithRuntimeReadinessSource wires the voice + storage probe
// surface used by /ui/admin/health/runtime. Without it the page
// renders an "Available: false" placeholder so operators see the
// route is alive even before the wiring lands.
func WithRuntimeReadinessSource(src RuntimeReadinessSource) ServerOption {
	return func(s *Server) { s.runtimeReadiness = src }
}

// WithEmailChannelInventory wires the live email-channel snapshot
// powering /ui/admin/integrations/email. The source returns one
// row per configured email channel including the live ListSessions
// result. Without it the page renders an "Available: false"
// placeholder.
func WithEmailChannelInventory(src EmailChannelInventory) ServerOption {
	return func(s *Server) { s.emailChannelInventory = src }
}

// WithDispatcherToolInventory wires the dispatcher-tool inventory
// source powering /ui/admin/integrations/dispatcher-tools. The
// source returns one row per registered tool + its backing-service
// wiring state. Without it the page renders the "not wired"
// placeholder so operators still see the route is alive.
func WithDispatcherToolInventory(src DispatcherToolInventory) ServerOption {
	return func(s *Server) { s.dispatcherToolInventory = src }
}

// WithBudgetNotifier wires optional budget breach notifications for UI task creation.
func WithBudgetNotifier(n budget.Notifier) ServerOption {
	return func(s *Server) {
		s.budgetNotifier = n
	}
}

// WithTaskCreator wires the shared task-creation core used by the
// /ui/projects/{id}/tasks/new form. Without it the form returns
// 503; production must always supply it (the service container
// builds one and passes it to both api.Server and ui.Server).
func WithTaskCreator(c *taskcreate.Creator) ServerOption {
	return func(s *Server) {
		s.taskCreator = c
	}
}

// WithMCPFormRegistrySource wires the daemon-level MCP server
// registry used by the project config form's "MCP servers"
// section. Optional — nil leaves the section in its empty-state
// banner with a link to /ui/mcp so operators know where to look.
func WithMCPFormRegistrySource(src MCPFormRegistrySource) ServerOption {
	return func(s *Server) {
		s.mcpRegistrySource = src
	}
}

// WithExtractedDocumentsRepository wires the read side of the
// document-extraction pipeline into the /ui/projects/{id}/documents
// pages. nil disables the surface (handler returns 503), matching
// the contract every other optional read-only repo uses.
func WithExtractedDocumentsRepository(repo persistence.ExtractedDocumentRepository) ServerOption {
	return func(s *Server) { s.extractedDocsRepo = repo }
}

// WithLoginProviders sets the ordered list of configured browser-
// login provider names rendered as buttons on /ui/login (github-
// login phase 3). Empty / unset → the login page shows only the
// break-glass "Sign in with an API key" path. The container passes
// the providers it actually wired so the page never offers a button
// that 404s on /auth/<name>/start.
func WithLoginProviders(providers []string) ServerOption {
	return func(s *Server) { s.loginProviders = providers }
}

// WithLogoutHandler wires the POST /ui/logout handler (the loginflow
// Logout handler). When set, the nav renders a logout button and the
// route is mounted; nil leaves both absent. Passed as an http.Handler
// so the ui package keeps no dependency on internal/auth/loginflow.
func WithLogoutHandler(h http.Handler) ServerOption {
	return func(s *Server) { s.logoutHandler = h }
}

// NewServer creates a new UI server.
func NewServer(opts ...ServerOption) *Server {
	s := &Server{
		logger: zerolog.Nop(),
		sseBus: NewSSEBus(), // Phase 80 — always allocated; cheap when no subscribers.
	}

	for _, opt := range opts {
		opt(s)
	}

	// Fall back to a private workspace lock when none was injected, so
	// the artifact-delete path always has a valid lock to take. The
	// container injects the shared instance via WithWorkspaceLock.
	if s.workspaceLock == nil {
		s.workspaceLock = workspacelock.New()
	}

	// Parse templates from embedded FS
	tmpl, err := template.New("").
		Funcs(uiFuncMap()).
		ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		// Templates are embedded at build time — a parse error means the
		// binary is broken. Failing loud at startup beats serving 500s on
		// every request with an "empty" template that has no named blocks.
		s.logger.Error().Err(err).Msg("failed to parse UI templates")
		panic(fmt.Errorf("ui: embedded template parse failed: %w", err))
	}
	s.templates = tmpl

	return s
}

// Handler returns the HTTP handler for the UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Login page (github-login phase 3). Public — the auth
	// middleware exempts /ui/login so it renders unauthenticated.
	// Shows a button per configured provider plus the break-glass
	// "Sign in with an API key" link, and surfaces error / awaiting-
	// access banners passed via query string.
	mux.HandleFunc("/login", s.Login)

	// Logout — POST only. Dispatches to the wired logout handler when
	// session login is configured; 404s otherwise (an explicit route
	// so the catch-all Dashboard handler doesn't render a page for
	// /ui/logout on deployments without session login).
	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		if s.logoutHandler == nil {
			http.NotFound(w, r)
			return
		}
		s.logoutHandler.ServeHTTP(w, r)
	})

	// Dashboard - root path after strip
	mux.HandleFunc("/", s.Dashboard)

	// Projects
	mux.HandleFunc("/projects", s.Projects)
	mux.HandleFunc("/projects/", s.projectRouter)
	mux.HandleFunc("/setup", s.Setup)

	// Swarms — top-level list (IA completion) + editor/create under
	// the prefix. Exact /swarms hits the list; /swarms/ subtree (incl.
	// the trailing-slash root) goes through swarmRouter.
	mux.HandleFunc("/swarms", s.SwarmsList)
	mux.HandleFunc("/swarms/", s.swarmRouter)

	// Workflows — same shape: list + editor/create.
	mux.HandleFunc("/workflows", s.WorkflowsList)
	mux.HandleFunc("/workflows/", s.workflowRouter)

	// Phase 2 — prompt-writing assistant endpoint. POST only.
	// Returns JSON, consumed by the AI-assist button rendered
	// next to each prompt textarea in the swarm + workflow
	// editors. Returns 503 when no AssistantLLM is wired.
	mux.HandleFunc("/assistant/draft", s.AssistantSuggest)

	// Spend deep-dive — token + cost analytics with drill-down by
	// project, source, task, role/model.
	mux.HandleFunc("/spend", s.Spend)
	mux.HandleFunc("/insights/tool-budget", s.InsightsToolBudget)
	mux.HandleFunc("/insights/trends", s.InsightsTrends)

	// Trading dashboard — end-to-end trading overview (broker account
	// snapshot, recent trades, safety events) with a trading-enabled
	// project filter + 24h/7d/30d window, under the Insight area.
	// Edition-gated on the EE trading capability (WithTradingEnabled). On
	// Community we register an explicit 404 rather than leaving it unregistered,
	// because an unregistered /trading would fall through to the "/" catch-all
	// and render 200 — leaking the dashboard. The handler stays unchanged.
	if s.tradingEnabled {
		mux.HandleFunc("/trading", s.Trading)
	} else {
		mux.HandleFunc("/trading", http.NotFound)
	}

	// Tasks list with filter support
	mux.HandleFunc("/tasks", s.Tasks)
	// Task detail and actions share the /tasks/ prefix.
	// POST requests with /cancel or /retry suffix are routed to action handlers.
	mux.HandleFunc("/tasks/", s.taskRouter)
	// Bulk task actions — separate prefix so the /tasks/{id}/cancel
	// pattern in taskRouter doesn't shadow them.
	mux.HandleFunc("/tasks-bulk/cancel", s.TaskBulkCancel)
	mux.HandleFunc("/tasks-bulk/retry", s.TaskBulkRetry)
	mux.HandleFunc("/tasks-bulk/close", s.TaskBulkClose)
	mux.HandleFunc("/executions-bulk/cancel", s.ExecutionBulkCancel)

	// Live — fleet "Now Running" view (the front door to live monitoring).
	mux.HandleFunc("/live", s.LiveNow)

	// Inbox — unified "what needs me" operator-action queue.
	mux.HandleFunc("/inbox", s.Inbox)

	// Executions — cross-task run list (IA completion) + detail/actions
	// under the prefix.
	mux.HandleFunc("/executions", s.ExecutionsList)
	mux.HandleFunc("/executions/", s.executionRouter)

	// Artifact downloads
	mux.HandleFunc("/artifacts/", s.ArtifactDownload)

	// Audit log
	mux.HandleFunc("/audit", s.Audit)

	// MCP daemon-level discovery page. Shows every server declared
	// in the daemon's top-level mcp.servers block with its tool
	// catalog and reachability state. Read-only — adding a server
	// to a project still requires an explicit YAML edit on that
	// project's config.
	mux.HandleFunc("/mcp", s.MCPIndex)

	// Admin surface — admin-ui-design.md slice 1. The mux registers
	// the routes unconditionally; the actual auth gate is applied
	// outside this package (see internal/admin.Middleware) so the
	// gate's 404/401/403/200 decision happens before the handler
	// runs at all. Without the gate the routes are still reachable
	// (the data the handlers read is non-sensitive read-only state)
	// but the operator only sees the Admin nav link when the gate
	// has marked them as admin in context.
	mux.HandleFunc("/admin", s.adminRouter)
	mux.HandleFunc("/admin/", s.adminRouter)

	// Memory hardening section (Phase 2-4): per-project view of
	// epochs, quarantine, rollback history.
	mux.HandleFunc("/memory", s.Memory)
	mux.HandleFunc("/memory/", s.memoryRouter)

	// Vendored client-side assets (htmx.min.js). Serve with a long
	// Cache-Control since the asset is shipped pinned to a binary
	// version — any change in JS implies a redeploy.
	mux.Handle("/static/", http.StripPrefix("/", http.FileServer(http.FS(staticFS))))

	// Cmd+K palette JSON endpoint. Mounted at /palette/search so
	// the global key handler in pageHead can fetch+render results
	// without a page reload.
	mux.HandleFunc("/palette/search", s.PaletteSearch)

	// Reminders dashboard + cancel/delete POSTs.
	mux.HandleFunc("/reminders", s.Reminders)
	mux.HandleFunc("/reminders/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/reminders/")
		if strings.HasSuffix(path, "/cancel") {
			id := strings.TrimSuffix(path, "/cancel")
			s.ReminderCancel(w, r, id)
			return
		}
		if strings.HasSuffix(path, "/delete") {
			id := strings.TrimSuffix(path, "/delete")
			s.ReminderDelete(w, r, id)
			return
		}
		// Trailing slash with no suffix → render the dashboard
		// like /reminders.
		if path == "" {
			s.Reminders(w, r)
			return
		}
		http.NotFound(w, r)
	})

	// Phase 80 — SSE event stream per task. The actual handler
	// path is /tasks/<id>/events but the existing taskRouter
	// already catches /tasks/, so dispatch via suffix match.

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if api.SessionRoleFromContext(r.Context()) == auth.RoleUser && sessionUserGlobalConfigPath(r.URL.Path) {
			http.Error(w, "admin scope required", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// uiRequireAdminMutation gates a mutating UI handler on admin scope.
// Returns true when the caller may proceed; writes a 403 and returns
// false otherwise.
//
// D2/D3 (audit 2026-06-10): project-scoped RoleUser browser sessions
// could mint/rotate/revoke project API keys and rewrite project YAML —
// credential-issuer + autonomy-gate authoring from an "operate" role.
// The decision is: RoleUser is operate-not-author; mutations require
// admin scope. This mirrors how internal/ui/memory_operators.go gates
// its writes via admin.IsAdminFromContext.
//
// The check is belt-and-suspenders: it admits when the request resolved
// to admin (the admin.Middleware stamps IsAdmin=true for both admin-key
// and session-admin callers, AND for every caller when auth is disabled)
// OR when auth is disabled outright (single-tenant homelab — explicit
// fallback so the gate holds even if a future wrap-order regression
// stops the middleware from stamping the auth-off IsAdmin bit, the same
// failure mode incident b777ef4a hit).
func (s *Server) uiRequireAdminMutation(w http.ResponseWriter, r *http.Request) bool {
	if admin.IsAdminFromContext(r.Context()) {
		return true
	}
	if !api.IsAuthEnabledFromContext(r.Context()) {
		return true
	}
	http.Error(w, "admin scope required", http.StatusForbidden)
	return false
}

// sessionUserGlobalAuthoringPrefixes are the daemon-global authoring
// surfaces a project-scoped RoleUser browser session may NOT reach.
// Browser session users are project-scoped; letting them edit shared
// swarms/workflows, create projects, or touch daemon-level MCP/audit
// would affect other tenants.
//
// A4 (audit 2026-06-10): these are PREFIXES, matched fail-safe — every
// path AT or UNDER one of these roots is denied, so a newly added
// subpath (e.g. /swarms/x/new, /mcp/import, /audit/export) is denied by
// default instead of slipping through until someone remembers to extend
// an exact-match list. Each entry covers both the bare root ("/audit")
// and any subpath ("/audit/..."). Keep behaviour identical for the
// previously-listed routes; the change is that new SUBPATHS under these
// roots now fail closed.
var sessionUserGlobalAuthoringPrefixes = []string{
	"/swarms",
	"/workflows",
	"/projects/new",
	"/assistant/draft",
	"/audit",
	"/mcp",
}

// sessionUserGlobalConfigPath reports whether path is a daemon-global
// authoring surface a RoleUser session must be denied. Fail-safe: a
// path matches if it equals one of the authoring roots OR sits under it
// (root + "/..."). See sessionUserGlobalAuthoringPrefixes.
func sessionUserGlobalConfigPath(path string) bool {
	for _, root := range sessionUserGlobalAuthoringPrefixes {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

func (s *Server) projectRouter(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path[len("/projects/"):]
	switch {
	// Project template gallery — 2026.6.0 F2 slice 2.
	// /projects/new → GET renders the catalog; POST materialises
	// the chosen template via the same path the API uses.
	case path == "new" && r.Method == http.MethodGet:
		s.ProjectsNew(w, r)
	case path == "new" && r.Method == http.MethodPost:
		s.ProjectsCreateFromTemplate(w, r)
	// Conversational wizard — Feature #2. Renders the chat pane;
	// JS POSTs to /api/v1/projects/wizard/converse.
	case path == "new/wizard" && r.Method == http.MethodGet:
		s.ProjectsNewWizard(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/brief"):
		projectID := strings.TrimSuffix(path, "/brief")
		s.ProjectBriefEdit(w, r, projectID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/brief"):
		projectID := strings.TrimSuffix(path, "/brief")
		s.ProjectBriefSave(w, r, projectID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/config/schema"):
		projectID := strings.TrimSuffix(path, "/config/schema")
		s.ProjectSchemaConfigEdit(w, r, projectID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/config/schema"):
		projectID := strings.TrimSuffix(path, "/config/schema")
		s.ProjectSchemaConfigSave(w, r, projectID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/config/form"):
		projectID := strings.TrimSuffix(path, "/config/form")
		s.ProjectConfigFormEdit(w, r, projectID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/config/form"):
		projectID := strings.TrimSuffix(path, "/config/form")
		s.ProjectConfigFormSave(w, r, projectID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/config"):
		projectID := strings.TrimSuffix(path, "/config")
		s.ProjectConfigEdit(w, r, projectID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/config"):
		projectID := strings.TrimSuffix(path, "/config")
		s.ProjectConfigSave(w, r, projectID)
	case strings.HasSuffix(path, "/keys"):
		// GET renders the per-project API-key panel; POST handles
		// create / rotate / revoke based on the form's `action`.
		projectID := strings.TrimSuffix(path, "/keys")
		s.ProjectKeys(w, r, projectID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/tasks/new"):
		projectID := strings.TrimSuffix(path, "/tasks/new")
		s.ProjectCreateTaskForm(w, r, projectID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/tasks/new"):
		projectID := strings.TrimSuffix(path, "/tasks/new")
		s.ProjectCreateTaskSubmit(w, r, projectID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/chat"):
		projectID := strings.TrimSuffix(path, "/chat")
		s.ChatPage(w, r, projectID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/chat/messages"):
		projectID := strings.TrimSuffix(path, "/chat/messages")
		s.ChatPostMessage(w, r, projectID)
	case strings.HasSuffix(path, "/wizard"):
		// Phase 3 autonomy-gated wizard. POST-only;
		// WizardGenerate enforces the method.
		projectID := strings.TrimSuffix(path, "/wizard")
		s.WizardGenerate(w, r, projectID)
	// Git-over-HTTPS access toggle (POST-only). Flips git.enabled on the
	// project YAML from the detail-page panel.
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/toggle"):
		projectID := strings.TrimSuffix(path, "/git/toggle")
		s.ProjectGitToggle(w, r, projectID)
	case strings.HasSuffix(path, "/git/toggle"):
		// Any non-POST on /git/toggle is rejected at the router so a
		// stray GET doesn't fall through to ProjectDetail and 200.
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	// Lifecycle (archive → grace → delete) endpoints. All POST-only.
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/archive"):
		projectID := strings.TrimSuffix(path, "/archive")
		s.ProjectArchive(w, r, projectID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/unarchive"):
		projectID := strings.TrimSuffix(path, "/unarchive")
		s.ProjectUnarchive(w, r, projectID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/delete-now"):
		projectID := strings.TrimSuffix(path, "/delete-now")
		s.ProjectDeleteNow(w, r, projectID)
	// Filesystem-artifact management surface. The /artifacts page
	// lists every file under the per-project workspace tree; /raw
	// streams a single file's bytes; /delete (POST-only) removes
	// one. All three live under the same /artifacts prefix so the
	// project-detail link target stays the same shape as the rest
	// of the per-project routes.
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/artifacts/raw"):
		projectID := strings.TrimSuffix(path, "/artifacts/raw")
		s.ProjectArtifactView(w, r, projectID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/artifacts/delete"):
		projectID := strings.TrimSuffix(path, "/artifacts/delete")
		s.ProjectArtifactDelete(w, r, projectID)
	case strings.HasSuffix(path, "/artifacts/delete"):
		// Any non-POST method on /artifacts/delete must be
		// rejected at the routing layer; otherwise a stray GET
		// would fall through to ProjectDetail and 200 on a URL
		// that operators expect to be POST-only.
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/artifacts"):
		projectID := strings.TrimSuffix(path, "/artifacts")
		s.ProjectArtifacts(w, r, projectID)
	// /ui/projects/{id}/documents — Phase 6: list extracted_documents.
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/documents"):
		projectID := strings.TrimSuffix(path, "/documents")
		s.ProjectDocuments(w, r, projectID)
	// /ui/projects/{id}/documents/{docID}/re-extract — POST trigger.
	case r.Method == http.MethodPost && strings.Contains(path, "/documents/") && strings.HasSuffix(path, "/re-extract"):
		trimmed := strings.TrimSuffix(path, "/re-extract")
		// trimmed looks like "<projectID>/documents/<docID>"
		parts := strings.SplitN(trimmed, "/documents/", 2)
		if len(parts) != 2 || parts[1] == "" {
			http.NotFound(w, r)
			return
		}
		s.ProjectDocumentReExtract(w, r, parts[0], parts[1])
	// /ui/projects/{id}/documents/{docID} — GET detail view.
	case r.Method == http.MethodGet && strings.Contains(path, "/documents/"):
		parts := strings.SplitN(path, "/documents/", 2)
		if len(parts) != 2 || parts[1] == "" {
			http.NotFound(w, r)
			return
		}
		s.ProjectDocumentDetail(w, r, parts[0], parts[1])
	default:
		s.ProjectDetail(w, r)
	}
}

// swarmRouter dispatches swarm requests under /swarms/. v1
// only exposes the editor at /swarms/{id}/edit (GET + POST) and
// the delete endpoint at /swarms/{id}/delete (POST). Other paths
// under the prefix fall through to a 404 so future routes don't
// accidentally inherit a default detail page that doesn't exist
// yet.
func (s *Server) swarmRouter(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path[len("/swarms/"):]
	switch {
	// Trailing-slash root → the list page (exact /swarms is registered
	// separately to SwarmsList).
	case path == "" && r.Method == http.MethodGet:
		s.SwarmsList(w, r)
	// Blank-starter create surface. Mirrors /projects/new: GET
	// renders the form, POST validates + writes + reloads +
	// redirects to the existing editor.
	case path == "new" && r.Method == http.MethodGet:
		s.SwarmsNew(w, r)
	case path == "new" && r.Method == http.MethodPost:
		s.SwarmsCreate(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/schema/role"):
		swarmID := strings.TrimSuffix(path, "/schema/role")
		s.SwarmSchemaRoleCard(w, r, swarmID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/schema"):
		swarmID := strings.TrimSuffix(path, "/schema")
		s.SwarmSchemaConfigEdit(w, r, swarmID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/schema"):
		swarmID := strings.TrimSuffix(path, "/schema")
		s.SwarmSchemaConfigSave(w, r, swarmID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/edit"):
		swarmID := strings.TrimSuffix(path, "/edit")
		s.SwarmEdit(w, r, swarmID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/edit"):
		swarmID := strings.TrimSuffix(path, "/edit")
		s.SwarmSave(w, r, swarmID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/delete"):
		swarmID := strings.TrimSuffix(path, "/delete")
		s.SwarmDelete(w, r, swarmID)
	default:
		http.NotFound(w, r)
	}
}

// workflowRouter dispatches /workflows/. Same shape as swarmRouter —
// editor + delete endpoints only.
func (s *Server) workflowRouter(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path[len("/workflows/"):]
	switch {
	case path == "" && r.Method == http.MethodGet:
		s.WorkflowsList(w, r)
	// Blank-starter create surface — companion to /swarms/new.
	case path == "new" && r.Method == http.MethodGet:
		s.WorkflowsNew(w, r)
	case path == "new" && r.Method == http.MethodPost:
		s.WorkflowsCreate(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/schema/step"):
		workflowID := strings.TrimSuffix(path, "/schema/step")
		s.WorkflowSchemaStepCard(w, r, workflowID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/schema"):
		workflowID := strings.TrimSuffix(path, "/schema")
		s.WorkflowSchemaConfigEdit(w, r, workflowID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/schema"):
		workflowID := strings.TrimSuffix(path, "/schema")
		s.WorkflowSchemaConfigSave(w, r, workflowID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/graph/edge/delete"):
		workflowID := strings.TrimSuffix(path, "/graph/edge/delete")
		s.WorkflowGraphEdgeDelete(w, r, workflowID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/graph/edge"):
		workflowID := strings.TrimSuffix(path, "/graph/edge")
		s.WorkflowGraphEdge(w, r, workflowID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/graph/node/delete"):
		workflowID := strings.TrimSuffix(path, "/graph/node/delete")
		s.WorkflowGraphNodeDelete(w, r, workflowID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/graph/node"):
		workflowID := strings.TrimSuffix(path, "/graph/node")
		s.WorkflowGraphNode(w, r, workflowID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/graph/entrypoint"):
		workflowID := strings.TrimSuffix(path, "/graph/entrypoint")
		s.WorkflowGraphEntrypoint(w, r, workflowID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/clone"):
		workflowID := strings.TrimSuffix(path, "/clone")
		s.WorkflowClone(w, r, workflowID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/graph"):
		workflowID := strings.TrimSuffix(path, "/graph")
		s.WorkflowGraph(w, r, workflowID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/edit"):
		workflowID := strings.TrimSuffix(path, "/edit")
		s.WorkflowEdit(w, r, workflowID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/edit"):
		workflowID := strings.TrimSuffix(path, "/edit")
		s.WorkflowSave(w, r, workflowID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/delete"):
		workflowID := strings.TrimSuffix(path, "/delete")
		s.WorkflowDelete(w, r, workflowID)
	default:
		http.NotFound(w, r)
	}
}

// taskRouter dispatches task requests: GET for detail, POST for actions.
func (s *Server) taskRouter(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path[len("/tasks/"):]
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/cancel"):
		taskID := strings.TrimSuffix(path, "/cancel")
		s.TaskCancel(w, r, taskID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/retry"):
		taskID := strings.TrimSuffix(path, "/retry")
		s.TaskRetry(w, r, taskID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/logs/stream"):
		taskID := strings.TrimSuffix(path, "/logs/stream")
		s.TaskLogsStream(w, r, taskID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/status"):
		taskID := strings.TrimSuffix(path, "/status")
		s.TaskStatusPartial(w, r, taskID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/post-mortem"):
		taskID := strings.TrimSuffix(path, "/post-mortem")
		s.TaskPostMortemGenerate(w, r, taskID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/events"):
		// Phase 80 — SSE stream per task.
		s.TaskEventsStream(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/live"):
		// Feature #3 Phase C — operator-facing live observation
		// page. Terminal-status visits redirect to /ui/tasks/<id>.
		taskID := strings.TrimSuffix(path, "/live")
		s.TaskLive(w, r, taskID)
	// Phase 26+27 — conversational task lifecycle actions. The
	// per-action POST handler dispatches on the trailing path
	// segment (message, directive, answer, amend, pause, resume,
	// approve, reject, close).
	case r.Method == http.MethodPost && (strings.HasSuffix(path, "/message") ||
		strings.HasSuffix(path, "/directive") ||
		strings.HasSuffix(path, "/answer") ||
		strings.HasSuffix(path, "/amend") ||
		strings.HasSuffix(path, "/pause") ||
		strings.HasSuffix(path, "/resume") ||
		strings.HasSuffix(path, "/approve") ||
		strings.HasSuffix(path, "/reject") ||
		strings.HasSuffix(path, "/close")):
		s.TaskConversationAction(w, r)
	default:
		s.TaskDetail(w, r)
	}
}

// executionRouter dispatches execution requests: GET for detail, POST for actions.
func (s *Server) executionRouter(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path[len("/executions/"):]
	switch {
	case path == "" && r.Method == http.MethodGet:
		s.ExecutionsList(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/status"):
		execID := strings.TrimSuffix(path, "/status")
		s.ExecutionStatusPartial(w, r, execID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/cancel"):
		execID := strings.TrimSuffix(path, "/cancel")
		s.ExecutionCancel(w, r, execID)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/retry-from-step"):
		execID := strings.TrimSuffix(path, "/retry-from-step")
		s.ExecutionRetryFromStep(w, r, execID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/replay"):
		execID := strings.TrimSuffix(path, "/replay")
		s.ExecutionReplay(w, r, execID)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/live"):
		// Feature #3 Phase C — deep-link from the execution side to
		// the same live observation page. Terminal-status executions
		// redirect to /ui/executions/<id>/replay.
		execID := strings.TrimSuffix(path, "/live")
		s.ExecutionLive(w, r, execID)
	default:
		s.ExecutionDetail(w, r)
	}
}

// TasksData holds data for the tasks list template.
// TaskDetailData holds data for the task detail template.
// ExecutionDetailData holds data for the execution detail template.

// AuditData holds data for the audit log page.
// Path: /artifacts/{artifactID}
// render renders a template with the base layout.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	err := s.templates.ExecuteTemplate(w, name, data)
	if err != nil {
		s.logger.Error().Err(err).Str("template", name).Msg("failed to render template")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// --- Task actions ---

// TaskCancel cancels a queued or running task.
// ExecutionCancel cancels the associated task of a running execution.

// uiFuncMap returns the template FuncMap shared by the production
// template registry and tests. Extracted from the inline literal so the
// nav helpers (and any future entry) are unit-testable in isolation.
func uiFuncMap() template.FuncMap {
	return template.FuncMap{
		"index": func(m map[persistence.TaskStatus]int64, key persistence.TaskStatus) int64 {
			if m == nil {
				return 0
			}
			return m[key]
		},
		// roleQuality looks up per-role quality stats by role name.
		// Separate helper because the custom `index` above is
		// narrowly typed to TaskStatus maps (overrides the builtin).
		"roleQuality": func(m map[string]*persistence.RoleQuality, name string) *persistence.RoleQuality {
			if m == nil {
				return nil
			}
			return m[name]
		},
		// formValue looks up a submitted form value by parameter
		// name. Like roleQuality above, a dedicated helper because
		// the custom `index` is narrowly typed for the dashboard
		// TaskStatus map and won't accept map[string]string.
		"formValue": func(m map[string]string, name string) string {
			if m == nil {
				return ""
			}
			return m[name]
		},
		// boolFromMap returns m[key] for a map[string]bool. Used
		// by the project config form's MCP section to look up
		// per-tool checked state. The custom `index` above is
		// narrowly typed to TaskStatus maps, so this needs its
		// own helper rather than reusing the builtin.
		"boolFromMap": func(m map[string]bool, key string) bool {
			if m == nil {
				return false
			}
			return m[key]
		},
		// prettyJSON formats raw JSON bytes with indentation for display.
		// Strips toolAudit (large) and truncates long output fields for readability.
		"prettyJSON": func(data []byte) string {
			if len(data) == 0 {
				return ""
			}
			var parsed map[string]any
			if err := json.Unmarshal(data, &parsed); err != nil {
				// Not an object — try as generic value
				var generic any
				if err2 := json.Unmarshal(data, &generic); err2 != nil {
					return string(data)
				}
				out, _ := json.MarshalIndent(generic, "", "  ")
				return string(out)
			}
			// Strip verbose fields from display
			delete(parsed, "toolAudit")
			out, err := json.MarshalIndent(parsed, "", "  ")
			if err != nil {
				return string(data)
			}
			return string(out)
		},
		// shortID compacts entity IDs into a typed prefix + last
		// 4 hex chars for phone-friendly display. Phase 33 of
		// the phone-first UI redesign.
		//
		//   task_20260416080348_8738884ace60f9c7 → T-f9c7
		//   exec_…b7edd48f2fb4c68a              → X-c68a
		//   tmsg_…1c6b7e05e63e52bf              → M-52bf
		//   cep_…2b8a9c4d                       → E-9c4d (epoch)
		//   art_…1f0a                           → A-1f0a (artifact)
		//
		// Last 4 chars chosen because: (a) it's the highest-entropy
		// segment of the ULID-style suffix; (b) 16⁴=65k forms per
		// prefix is collision-safe inside a project's open set; (c)
		// 4 chars fit a tap target without truncation. Callers
		// disambiguate via title="..." on the long-form ID for
		// hover/long-press copy.
		//
		// Inputs that don't fit the prefix_…_hex shape return the
		// input unchanged (no-op for legacy / external IDs).
		"shortID": shortID,
		// taskSummary extracts a one-line excerpt from a task
		// payload for the task-list "Summary" column.
		// Implementation lives in template_helpers.go.
		"taskSummary": taskSummary,
		// indentStyle renders an inline padding-left in rem for
		// the indented task list. Empty for depth 0; capped at 10
		// levels to bound the layout impact.
		"indentStyle": indentStyle,
		// hierarchyMeta looks up the per-row hierarchy metadata
		// (depth + child count) by task ID. Returns zero meta
		// when the map is nil or the key is missing — i.e. flat
		// mode or partial renders.
		"hierarchyMeta": hierarchyMeta,
		// statusPill renders a Tailwind-styled span with the right
		// colour for a task/execution status. Phase 33 + 41 (a11y)
		// guarantees: colour + text label, never colour-only.
		"statusPill":        statusPill,
		"statusPillClasses": statusPillClasses,
		"statusDot":         statusDot,
		// renderMarkdown — minimal inline markdown for
		// conversation messages. Bold/italic/code/links/
		// fenced blocks. See template_helpers.go.
		"renderMarkdown": renderInlineMarkdown,
		// lower — basic string lowercase. Used by the
		// conversation thread's data-search attribute so
		// the client-side filter is case-insensitive.
		"lower": strings.ToLower,
		// add is a simple integer add — handy for `{{add $i 1}}` to
		// render 1-based row indices from 0-based range iteration.
		"add": func(a, b int) int { return a + b },
		// humanizeSince renders a time relative to now ("3m ago",
		// "2h ago", "1d ago"). Zero times collapse to a dash so
		// templates don't show a bogus "57 years ago".
		// humanizeSize renders a byte count as B / KB / MB /
		// GB / TB. Used by the /ui/projects/{id}/artifacts
		// listing so the size column reads "3.5 MB" instead
		// of "3670016". Helper body lives in artifacts.go
		// because that's the only consumer today.
		"humanizeSize": humanizeSize,
		"humanizeSince": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			d := time.Since(t)
			switch {
			case d < 0:
				return "in the future"
			case d < time.Minute:
				return fmt.Sprintf("%ds ago", int(d.Seconds()))
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
		},
		// checkpointStale returns "stale" when t is older than the
		// watchdog's stuck threshold (i.e. the watchdog would
		// flag this row), "warn" when older than half the
		// threshold (yellow — the row is heading toward stuck),
		// and "" otherwise. Templates use the result as a CSS
		// class hint on the "last checkpoint" pill so operators
		// can see at a glance which RUNNING executions are
		// drifting toward the watchdog tripwire.
		//
		// Threshold is hardcoded to 30m to track the watchdog's
		// default. The expected-rare case where an operator has
		// tuned the threshold via config doesn't need template
		// alignment — the watchdog log/metric is the canonical
		// signal; this is a UX hint.
		"checkpointStale": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			d := time.Since(t)
			const threshold = 30 * time.Minute
			switch {
			case d >= threshold:
				return "stale"
			case d >= threshold/2:
				return "warn"
			default:
				return ""
			}
		},
		// extractPrompt pulls the context.prompt string from a task payload.
		"extractPrompt": func(data []byte) string {
			if len(data) == 0 {
				return ""
			}
			var payload struct {
				Context struct {
					Prompt string `json:"prompt"`
				} `json:"context"`
			}
			if json.Unmarshal(data, &payload) == nil && payload.Context.Prompt != "" {
				return payload.Context.Prompt
			}
			return ""
		},
		// deref returns the value behind a *string, or empty string if nil.
		"deref": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
		// getStep safely looks up a workflow step by ID. Returns a zero
		// WorkflowStep if the step doesn't exist (prevents template panic).
		// For synthetic plan sub-step IDs (e.g. "plan_0_analyst") the
		// lookup misses — the template falls back to planSubRole for the
		// role pill.
		"getStep": func(steps map[string]registry.WorkflowStep, id string) registry.WorkflowStep {
			if steps == nil {
				return registry.WorkflowStep{}
			}
			return steps[id]
		},
		"planSubRole": parsePlanSubRole,
		// outcomeFor looks up a step's outcome row from the
		// per-execution OutcomeByStep map. Returns the zero
		// StepOutcomeRow on miss so templates can branch on
		// .DurationMS > 0 instead of doing nil checks.
		"outcomeFor": func(byStep map[string]StepOutcomeRow, stepID string) StepOutcomeRow {
			if byStep == nil {
				return StepOutcomeRow{}
			}
			return byStep[stepID]
		},
		// stepColor looks up a step's identity color from the
		// per-execution StepColorByID map. Returns a neutral
		// gray on miss so templates that draw the bar
		// unconditionally don't render an empty/black stripe.
		//
		// Returns template.CSS rather than a plain string so
		// html/template's contextual auto-escape trusts the
		// value in a `style="background: ..."` attribute.
		// Without this typed wrapper html/template emits
		// ZgotmplZ (its placeholder for "value not trusted in
		// this context") because the `hsl(190, 65%, 60%)`
		// string contains parens and commas that look like a
		// possible CSS expression vector. The map only ever
		// holds values produced by stepIdentityColor, which is
		// purely numeric — safe to surface as CSS.
		"stepColor": func(byID map[string]string, stepID string) template.CSS {
			if byID == nil {
				return template.CSS("hsl(0, 0%, 40%)")
			}
			if c, ok := byID[stepID]; ok {
				return template.CSS(c)
			}
			return template.CSS("hsl(0, 0%, 40%)")
		},
		// durationPct returns (numerator / denominator * 100)
		// rounded to an int and clamped to [0, 100]. Used to
		// drive the per-step Step Progress bars whose width
		// is the step's duration relative to the longest step
		// in the execution. denominator==0 returns 0 so the
		// template can render a no-data row identically to a
		// zero-duration row.
		"durationPct": func(numerator, denominator int64) int {
			if denominator <= 0 || numerator <= 0 {
				return 0
			}
			pct := numerator * 100 / denominator
			if pct > 100 {
				return 100
			}
			if pct < 0 {
				return 0
			}
			return int(pct)
		},
		// toJSON marshals any value to a JSON string for embedding in <script> tags.
		"toJSON": func(v any) template.JS {
			b, err := json.Marshal(v)
			if err != nil {
				return template.JS("{}")
			}
			return template.JS(b)
		},
		// parseResult extracts structured fields from an execution result.
		"parseResult": func(data []byte) map[string]any {
			out := map[string]any{
				"status":        "",
				"message":       "",
				"artifactNames": []string{},
				"toolCount":     0,
				"exitCode":      0,
				"duration":      0,
				"error":         "",
				"hasResult":     false,
			}
			if len(data) == 0 {
				return out
			}
			var r struct {
				Status          string           `json:"status"`
				Message         string           `json:"message"`
				OutputArtifacts []map[string]any `json:"outputArtifacts"`
				ToolAudit       []any            `json:"toolAudit"`
				Diagnostics     struct {
					ExitCode        int    `json:"exitCode"`
					DurationSeconds int    `json:"durationSeconds"`
					Error           string `json:"error"`
				} `json:"diagnostics"`
			}
			if json.Unmarshal(data, &r) != nil {
				return out
			}
			out["hasResult"] = true
			out["status"] = r.Status
			out["message"] = r.Message
			out["toolCount"] = len(r.ToolAudit)
			out["exitCode"] = r.Diagnostics.ExitCode
			out["duration"] = r.Diagnostics.DurationSeconds
			out["error"] = r.Diagnostics.Error
			// Extract artifact names safely
			var names []string
			for _, a := range r.OutputArtifacts {
				if n, ok := a["name"]; ok {
					if s, ok := n.(string); ok && s != "" {
						names = append(names, s)
					}
				}
			}
			out["artifactNames"] = names
			return out
		},
		// extractResultMessage pulls the message string from an execution result.
		"extractResultMessage": func(data []byte) string {
			if len(data) == 0 {
				return ""
			}
			var result struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(data, &result) == nil && result.Message != "" {
				return result.Message
			}
			return ""
		},
		// fmtUSD renders a dollar amount with magnitude-aware
		// precision so judge / post-mortem micro-costs remain
		// legible without dragging extra decimals onto every
		// cell. Tiers:
		//   >= $1     → 2 decimals ($12.34)
		//   >= $0.01  → 3 decimals ($0.123)
		//   >= $0.001 → 4 decimals ($0.0042)
		//   else      → 5 decimals ($0.00021)
		// The 5-decimal tier was added 2026-05-04 because the
		// judge's per-call cost typically lands in $0.0001-
		// $0.0005 range and 4-decimal rounding made the cell
		// read $0.0001 even when the real value was $0.00007.
		"fmtUSD": func(v float64) string {
			abs := v
			if abs < 0 {
				abs = -abs
			}
			switch {
			case abs >= 1.0:
				return fmt.Sprintf("$%.2f", v)
			case abs >= 0.01:
				return fmt.Sprintf("$%.3f", v)
			case abs >= 0.001:
				return fmt.Sprintf("$%.4f", v)
			default:
				return fmt.Sprintf("$%.5f", v)
			}
		},
		// fmtUSD2 always renders with 2 decimals. Use for headline
		// totals where alignment with budget caps matters.
		"fmtUSD2": func(v float64) string { return fmt.Sprintf("$%.2f", v) },
		// fmtTokens renders an int64 token count with thousand
		// separators when small, k/M shorthand above 10,000 to keep
		// columns narrow.
		"fmtTokens": func(n int64) string {
			if n < 0 {
				return "—"
			}
			switch {
			case n < 10_000:
				return fmt.Sprintf("%d", n)
			case n < 1_000_000:
				return fmt.Sprintf("%.1fk", float64(n)/1000.0)
			default:
				return fmt.Sprintf("%.2fM", float64(n)/1_000_000.0)
			}
		},
		// fmtBytes renders an int64 byte count using binary
		// prefixes (KiB / MiB / GiB) for storage panels.
		// The embedding cache panel uses this for the
		// pg_total_relation_size readout; operators read this
		// as "how much disk does the cache cost."
		"fmtBytes": func(n int64) string {
			switch {
			case n < 1024:
				return fmt.Sprintf("%d B", n)
			case n < 1024*1024:
				return fmt.Sprintf("%.1f KiB", float64(n)/1024.0)
			case n < 1024*1024*1024:
				return fmt.Sprintf("%.1f MiB", float64(n)/(1024.0*1024.0))
			default:
				return fmt.Sprintf("%.2f GiB", float64(n)/(1024.0*1024.0*1024.0))
			}
		},
		// div is float64 division for template arithmetic — used
		// by the spend dashboard's "avg cost / call" cell. Treats
		// divide-by-zero as 0 rather than panicking the
		// template render path.
		"div": func(a float64, b int) float64 {
			if b == 0 {
				return 0
			}
			return a / float64(b)
		},
		// mul is float64 multiplication for template arithmetic.
		// Used by the project-detail dispatcher-share row to
		// render "0.42" as "42%" without a separate Go-side
		// percent field.
		"mul": func(a, b float64) float64 {
			return a * b
		},
		// humanizeBytes formats an int byte count the way
		// humanizeSize does for int64 — the documents-detail
		// outline carries per-section text counts as int.
		"humanizeBytes": func(n int) string {
			return humanizeSize(int64(n))
		},
		// indentDots renders 'depth' dots so a nested outline
		// entry indents visually without per-row inline-style
		// arithmetic. Keeps the template free of mul/add
		// hacks on heterogeneous int / float types.
		"indentDots": func(depth int) string {
			if depth <= 0 {
				return ""
			}
			if depth > 6 {
				depth = 6
			}
			out := make([]byte, depth*2)
			for i := range out {
				out[i] = '·'
			}
			return string(out)
		},
		// fmtDurationMS formats a millisecond count as "123ms" or
		// "1.4s". Negative collapses to a dash.
		"fmtDurationMS": formatOutcomeDuration,
		// dict builds a map from alternating key/value args. Useful
		// for passing structured params to sub-templates.
		"dict": func(values ...any) (map[string]any, error) {
			if len(values)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of args")
			}
			m := make(map[string]any, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				k, ok := values[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %d is not a string", i)
				}
				m[k] = values[i+1]
			}
			return m, nil
		},
		"refOpen": refOpen,
		// slice builds an []any from its args — lets templates pass a
		// literal list of dicts to statStrip's Items.
		"slice": func(v ...any) []any { return v },
		// contains reports whether substr is present in s (used by
		// the sortHeader partial to decide ?/& separator).
		"contains": strings.Contains,
		// Memory-viz helpers — class colour for the scatter +
		// stroke colour for the validation-status ring + simple
		// percentage for funnel bar widths.
		"classColour":        classColour,
		"statusStrokeColour": statusStrokeColour,
		"pctOf":              pctOf,
		// Integer arithmetic for SVG layout. Reuses the
		// existing "add"; "mul" above is float64-typed which
		// the SVG path math doesn't want.
		"sub":  func(a, b int) int { return a - b },
		"muli": func(a, b int) int { return a * b },
		// hasAdminFlag — defensive accessor for the
		// "is_admin" bit on whatever data shape a handler
		// passed. Reflection-based so we don't have to add
		// IsAdmin to every existing data struct; the admin
		// pages set it explicitly, every other page returns
		// false here and the nav partial hides the link.
		//
		// See admin-ui-design.md slice 1: hidden nav for
		// non-admins. Operators without admin scope never see
		// the "Admin" link at all — not even greyed-out.
		"hasAdminFlag":   hasAdminFlag,
		"hasSessionFlag": hasSessionFlag,
		"navModel":       navModel,
		"navAreaForPage": navAreaForPage,
	}
}
