// Package api provides HTTP handlers for the vornik data plane API.
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/conversation/a2a"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/postmortem"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/projectarchive"
	"vornik.io/vornik/internal/queue"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/taskcreate"
	"vornik.io/vornik/internal/templates"
	"vornik.io/vornik/internal/tradingauth"
	"vornik.io/vornik/internal/workspacelock"
)

// MemorySearcher is the interface required by the API server for memory search.
type MemorySearcher interface {
	Search(ctx context.Context, projectID, query string, limit int) ([]MemorySearchResult, error)
}

// WebhookForwarder relays a verified webhook to the job tier (slice B).
// On a RelayMode DMZ node the HMAC signature is verified first; then the
// event is handed to the forwarder instead of being enqueued in-process.
// The job tier's mTLS relay-ingress endpoint calls enqueueVerifiedWebhook
// directly — it does NOT re-run HMAC (mTLS authenticates the caller).
type WebhookForwarder interface {
	Forward(ctx context.Context, projectID, source, deliveryID string, body []byte) (int, error)
}

// ReadinessCheck is a single named check /readyz runs in order. Must be
// cheap and bounded: the handler deadlines the whole call to 3 s. A nil
// error means ready. Non-nil means the check failed; its error text and
// the check's name are surfaced in the JSON response body.
type ReadinessCheck struct {
	Name  string
	Check func(ctx context.Context) error
}

// MemoryStatsProvider is the interface required for GET /api/v1/memory/stats.
// Kept separate from MemorySearcher so a deployment that wants search
// but not stats (or vice versa) can wire them independently.
type MemoryStatsProvider interface {
	Stats(ctx context.Context) ([]MemoryProjectStats, error)
}

// MemoryCacheStatsProvider returns the embedding + response cache
// summaries powering GET /api/v1/memory/cache-stats. The two caches
// share an endpoint because operators want to see them side-by-side
// ("which of my caches is actually paying off?"). Either cache may
// be nil-statted if disabled at the daemon — the API just returns
// zero values, and the CLI renders the "disabled" placeholder.
type MemoryCacheStatsProvider interface {
	EmbeddingCacheStats(ctx context.Context) (EmbeddingCacheStatsResult, error)
	ResponseCacheStats(ctx context.Context) (ResponseCacheStatsResult, error)
}

// EmbeddingCacheStatsResult mirrors memory.EmbeddingCacheStats at
// the API surface so the API package doesn't import memory's
// pgvector wiring directly.
type EmbeddingCacheStatsResult struct {
	Enabled        bool  `json:"enabled"`
	RowCount       int64 `json:"row_count"`
	ApproxBytes    int64 `json:"approx_bytes"`
	DistinctModels int   `json:"distinct_models"`
}

// ResponseCacheStatsResult mirrors memory.ResponseCacheStats at the
// API surface. Includes TotalSavingsUSD so the CLI shows the
// headline number that justifies the cache.
type ResponseCacheStatsResult struct {
	Enabled          bool    `json:"enabled"`
	RowCount         int64   `json:"row_count"`
	ApproxBytes      int64   `json:"approx_bytes"`
	DistinctPurposes int     `json:"distinct_purposes"`
	TotalHits        int64   `json:"total_hits"`
	TotalSavingsUSD  float64 `json:"total_savings_usd"`
}

// MemoryTitleBackfiller drives one batch of the LLM title-backfill
// per call. The API layer holds only the interface so it doesn't
// pull in the memory package's LLM client construction.
type MemoryTitleBackfiller interface {
	CountRemaining(ctx context.Context) (int, error)
	BackfillBatch(ctx context.Context, batchSize int) (*MemoryTitleBackfillResult, error)
}

// MemoryTitleBackfillResult mirrors memory.BackfillResult — duplicated
// here so the API surface stays stable even if the memory package
// reshapes its internals.
type MemoryTitleBackfillResult struct {
	Processed int      `json:"processed"`
	Succeeded int      `json:"succeeded"`
	Failed    int      `json:"failed"`
	Skipped   int      `json:"skipped"`
	Remaining int      `json:"remaining"`
	Errors    []string `json:"errors,omitempty"`
}

// MemoryClassifyBackfiller drives one batch of the LLM
// classification backfill per call. Mirrors MemoryTitleBackfiller's
// shape; required so the API package doesn't pull in the memory
// package's LLM-client construction.
type MemoryClassifyBackfiller interface {
	CountRemaining(ctx context.Context, projectID string) (int, error)
	BackfillBatch(ctx context.Context, projectID string, batchSize int) (*MemoryClassifyBackfillResult, error)
}

// MemoryClassifyBackfillResult mirrors memory.ClassifyBackfillResult.
type MemoryClassifyBackfillResult struct {
	Processed int      `json:"processed"`
	Succeeded int      `json:"succeeded"`
	Failed    int      `json:"failed"`
	Skipped   int      `json:"skipped"`
	Remaining int      `json:"remaining"`
	Errors    []string `json:"errors,omitempty"`
}

// MemoryGraphReflagger drives POST /api/v1/memory/regraph — flips
// needs_graph_extraction = TRUE on every project chunk that produced
// zero published edges so the KG worker reprocesses them with current
// pipeline logic. countOnly=true returns the candidate count without
// writing (powers the CLI's --dry-run).
//
// Narrow interface so the api package doesn't import persistence
// directly; the postgres ChunkGraphExtractionRepository satisfies
// this structurally.
type MemoryGraphReflagger interface {
	ReflagChunksMissingEdges(ctx context.Context, projectID string, countOnly bool) (int, error)
}

// WorkflowTelemetry drives GET /api/v1/admin/workflow-stats —
// computes the per-workflow execution-evidence rollup the
// architect agent (memetic-workflows arc) will consume. Narrow
// interface so the api package doesn't import
// internal/workflowtelemetry directly; *workflowtelemetry.Service
// satisfies it structurally. Returns the rollup as a JSON-friendly
// shape opaque to the api package — the consumer (CLI / future
// architect agent) handles rendering.
type WorkflowTelemetry interface {
	ForWorkflow(ctx context.Context, workflowID string, since time.Time) (any, error)
}

// WorkflowArchitect drives POST /api/v1/admin/workflow-architect/
// propose — runs one architect turn and persists a pending
// proposal. Narrow interface so the api package doesn't import
// internal/memetic directly; *memetic.Architect satisfies it via
// a one-line service-layer adapter. Returns the inserted
// proposal as an opaque shape (json-marshallable) so the api
// package isn't entangled with persistence.WorkflowProposal.
type WorkflowArchitect interface {
	Propose(ctx context.Context, workflowID string) (any, error)
}

// WorkflowApplier drives POST /api/v1/admin/workflow-proposals/
// {id}/apply — runs the apply path (filesystem write + git
// commit + config reload + status transition). Narrow interface
// so the api package doesn't import internal/memetic directly;
// *memetic.Applier satisfies it via a one-line adapter.
type WorkflowApplier interface {
	Apply(ctx context.Context, proposalID, appliedBy string) (any, error)
}

// WorkflowRollbacker drives POST /api/v1/admin/workflow-proposals/
// {id}/rollback — git revert + config reload + status transition.
// Slice 5 of the memetic-workflows arc.
type WorkflowRollbacker interface {
	Rollback(ctx context.Context, proposalID, revertedBy string) (any, error)
}

// WorkflowRejectionRecorder records an operator's rejection of a
// workflow proposal as an instinct-layer contradiction so the architect
// stops re-proposing what was declined (Consumer B). Narrow interface
// so the api package doesn't import internal/memetic / internal/instinct
// directly; a service-layer adapter over *memetic.Architect +
// persistence.InstinctRepository satisfies it. Only wired when
// instinct.enabled && instinct.consumers.architect_priors; nil → no
// write-back (gate-off behaviour unchanged). The proposal is passed as
// an opaque value (the handler holds *persistence.WorkflowProposal); the
// adapter type-asserts it, keeping the persistence type out of this
// interface's signature.
type WorkflowRejectionRecorder interface {
	RecordRejection(ctx context.Context, proposal any) error
}

// MemoryGraphReflagResult is the wire shape for POST /api/v1/memory/regraph.
type MemoryGraphReflagResult struct {
	Project   string `json:"project"`
	DryRun    bool   `json:"dryRun"`
	ReFlagged int    `json:"reFlagged"`
}

// MemoryProjectStats is the per-project row returned by memory stats.
// Mirrors memory.ProjectMemoryStats; kept in this package to avoid an
// import cycle back into memory.
type MemoryProjectStats struct {
	ProjectID      string `json:"projectId"`
	ChunksTotal    int64  `json:"chunksTotal"`
	ChunksEmbedded int64  `json:"chunksEmbedded"`
	QueueDepth     int64  `json:"queueDepth"`
}

// MemorySearchResult is a single search result returned by the memory searcher.
// We define it here (mirroring memory.SearchResult) so the API package has no
// import cycle with the memory package.
type MemorySearchResult struct {
	ChunkID    string  `json:"chunk_id"`
	ProjectID  string  `json:"project_id"`
	TaskID     string  `json:"task_id"`
	SourceName string  `json:"source_name"`
	Content    string  `json:"content"`
	Score      float64 `json:"score"`
	// IsAlive carries the URL liveness verdict for this chunk's
	// referenced URLs. nil = never checked or chunk contains no
	// URLs; *true = at least one HEAD recheck succeeded; *false =
	// all of the chunk's URLs were dead at last check. Consuming
	// agents (researcher, dispatcher) should prefer alive hits and
	// warn when only dead ones survive.
	IsAlive *bool `json:"is_alive,omitempty"`
	// LastCheckedAt is the RFC3339 timestamp of the most recent
	// recheck. nil = never checked.
	LastCheckedAt *string `json:"last_checked_at,omitempty"`
	// RepoScope is the chunk's repo-scope token (migration 75).
	// Empty string = NULL-scoped (the migration-grace bucket — these
	// chunks match every scope-filtered query under the default
	// non-strict mode); a non-empty value is the operator-supplied
	// scope token. Surface this so clients can disambiguate which
	// scope a hit actually carries. 2026-05-28 investigation
	// closure: the field was previously only on UI search results;
	// companion MCP recall hits had no scope visibility.
	RepoScope string `json:"repo_scope,omitempty"`
	// ContentClass is the chunk's class label (research / spec /
	// decision / diagnostic / summary / companion_note / …). Populated
	// by the search SQL; surfaced so the companion `recall` tool can
	// honour its `class` filter without a second DB hop. Empty when an
	// older repository returns chunks without the column.
	ContentClass string `json:"content_class,omitempty"`
}

// MCPExecutor is the subset of mcp.Manager the API needs to proxy tool
// calls from agent containers to per-project MCP servers.
type MCPExecutor interface {
	Tools(projectID string) []chat.Tool
	Execute(ctx context.Context, projectID, qualifiedName, argsJSON string) (string, error)
}

// MCPRegistrySource is the subset of mcp.Registry the API needs to
// render the daemon-level discovery surface at /api/v1/mcp/servers.
// Separate from MCPExecutor because the discovery surface is purely
// read-only and explicitly does NOT grant tool access to projects —
// the two interfaces stay isolated so neither path drifts into the
// other's job. Nil source = endpoint returns {"servers":[]}.
type MCPRegistrySource interface {
	// Snapshot returns the current daemon-level catalog. Must not
	// block on slow MCP servers — implementations cache and refresh
	// asynchronously.
	Snapshot(ctx context.Context) []mcp.ServerSnapshot
}

// TaskLogSource provides best-effort task logs for API/CLI/UI debugging.
type TaskLogSource interface {
	TaskLogs(ctx context.Context, taskID string, tail int) (string, error)
}

// LiveSubscriber is the narrow interface the WebSocket handler
// uses to fan an execution's events to the client + the chat
// proxy uses to publish per-LLM-call events (Feature #3 Phase B
// follow-up). Concrete impl is *livepubsub.Publisher; the
// channel + event type come straight from livepubsub so the
// handler writes each event as one JSON frame with no
// translation hop.
type LiveSubscriber interface {
	Subscribe(executionID string, fromSeq int64) (events <-chan livepubsub.LiveEvent, cancel func(), err error)
	// Publish records one event for the given execution. The
	// chat proxy uses this to emit `llm_call_started` /
	// `llm_call_finished` from inbound agent chat requests so
	// the live view shows "agent is thinking" granularity even
	// without per-token streaming.
	Publish(ctx context.Context, executionID, kind string, payload any) int64

	// SubscribeAll taps EVERY execution's events (live-only) — backs the
	// fleet "Now Running" SSE feed. See livepubsub.Publisher.SubscribeAll.
	SubscribeAll() (events <-chan livepubsub.LiveEvent, cancel func(), err error)
}

// ProjectWizard is the narrow interface POST
// /api/v1/projects/wizard/converse calls into. Concrete
// implementation is *projectwizard.Wizard; this surface keeps
// the api package free of an import on projectwizard.
type ProjectWizard interface {
	Converse(ctx context.Context, sessionID, operatorID, userMessage string) (*ProjectWizardResult, error)
	// Commit finalises a ready-to-commit session by writing the
	// proposal as a real project. Returns the new project's ID +
	// redirect URL. See Feature #2 Phase B.
	Commit(ctx context.Context, sessionID, operatorID string) (*ProjectWizardCommitResult, error)
	// Cancel terminally cancels an in-progress session, freeing the
	// operator's active-session slot. Returns persistence.ErrNotFound
	// when missing/not owned and persistence.ErrInvalidTransition (or
	// ErrSessionCommitted) when already committed.
	Cancel(ctx context.Context, sessionID, operatorID string) error
}

// ProjectWizardCommitResult mirrors projectwizard.CommitResult at
// the API boundary.
type ProjectWizardCommitResult struct {
	SessionID string `json:"session_id"`
	ProjectID string `json:"project_id"`
	URL       string `json:"url"`
}

// ProjectWizardResult mirrors projectwizard.Result at the API
// boundary. Owned by this package so the JSON contract is
// stable.
type ProjectWizardResult struct {
	SessionID string                 `json:"session_id"`
	Envelope  *ProjectWizardEnvelope `json:"envelope"`
}

// ProjectWizardEnvelope mirrors projectwizard.Envelope at the API
// boundary.
type ProjectWizardEnvelope struct {
	Message           string         `json:"message"`
	Proposal          map[string]any `json:"proposal,omitempty"`
	ReadyToCommit     bool           `json:"ready_to_commit"`
	SuggestedTemplate string         `json:"suggested_template,omitempty"`
	OpenQuestions     []string       `json:"open_questions,omitempty"`
}

// ForkExecutor is the narrow interface POST
// /api/v1/executions/{id}/fork-from-step calls into. The concrete
// implementation lives in internal/replay; the interface is here so
// the API package doesn't import replay (which would cycle via the
// internal/executor that already imports replay for the lineage
// stamper). Result fields stay shaped at the API boundary so the
// JSON contract is owned by this package.
type ForkExecutor interface {
	Fork(ctx context.Context, sourceExecutionID string, req ForkExecutorRequest) (*ForkExecutorResult, error)
}

// ForkExecutorRequest mirrors replay.ForkRequest at the API
// surface. Owned by this package so the JSON contract is stable
// even if the replay package reshapes internally.
type ForkExecutorRequest struct {
	StepID         string `json:"step_id"`
	PromptOverride string `json:"prompt_override,omitempty"`
}

// ForkExecutorResult is the API-surface response shape.
type ForkExecutorResult struct {
	TaskID string `json:"task_id"`
	URL    string `json:"url"`
}

// Server handles HTTP API requests for the data plane.
// This is the primary external interface for all execution-related traffic.
type Server struct {
	logger         zerolog.Logger
	taskRepo       persistence.TaskRepository
	executionRepo  persistence.ExecutionRepository
	artifactRepo   persistence.ArtifactRepository
	artifactOpener ArtifactOpener
	// forker backs POST /executions/{id}/fork-from-step (Feature
	// #1 Phase B). nil → endpoint returns 503.
	forker ForkExecutor
	// projectWizard backs POST /projects/wizard/converse
	// (Feature #2). nil → endpoint returns 503.
	projectWizard ProjectWizard
	// setupDetector backs GET /api/v1/setup/status and the
	// install-scoped onboarding page. Zero value means the
	// endpoint renders a conservative "fresh install" heuristic.
	setupDetector onboarding.Detector
	// setupSessions backs POST /api/v1/setup/session, /validate, /commit.
	// nil → the endpoints return 503. Wired by the service container.
	setupSessions persistence.InstallationOnboardingSessionRepository
	// setupValidator tests proposed chat credentials. nil → validate/commit
	// return 503. Defaults are NOT assumed; the container injects a real
	// onboarding.ChatValidator.
	setupValidator onboarding.ChatValidatorInterface
	// setupConfigPath is the daemon's resolved config.yaml path. The commit
	// handler patches this file via featuredoctor.FileConfigWriter.
	setupConfigPath string
	// setupSecretsDir is <configDir>/secrets, derived from setupConfigPath
	// by the container. The commit handler writes chat.env here.
	setupSecretsDir string
	// liveSub powers GET /executions/{id}/live (Feature #3
	// Phase B). nil → endpoint returns 503.
	liveSub LiveSubscriber
	// liveAllowedOrigins are extra Origin patterns the live
	// WebSocket handler accepts beyond same-host. The coder/
	// websocket library always authorises the request host —
	// these entries cover reverse-proxy deployments where the
	// public origin differs from the daemon's internal host
	// (e.g. vornik.example.com proxied to localhost:7070).
	// Empty == same-origin only.
	liveAllowedOrigins []string
	// hintRepo persists operator-injected hints for live
	// executions (Feature #3 Phase C). nil → POST /executions/
	// {id}/hints returns 503.
	hintRepo persistence.ExecutionHintRepository
	// a2aHandler serves the A2A protocol surface at
	// /.well-known/agent.json + /a2a/v1/agents/. nil → endpoints
	// return 404 so an operator who hasn't opted in shows no
	// agent surface to external scanners. See
	// https://docs.vornik.io
	a2aHandler *a2a.Handler
	// reminderRepo backs /api/v1/reminders[/{id}[/cancel]] for
	// the scheduled-reminders CLI surface. Nil keeps the
	// endpoints at 503 with a "not configured" message.
	reminderRepo persistence.ReminderRepository

	// archiveService backs the project archive / unarchive /
	// delete-now REST endpoints. Same instance the UI server
	// uses so both surfaces emit identical YAML mutations + audit
	// rows. Nil leaves the endpoints at 503.
	archiveService *projectarchive.LifecycleService

	// healingTriggerRepo backs the workflow-healing trigger admin
	// endpoints (Black Box Phase B). Nil keeps endpoints at 503.
	healingTriggerRepo persistence.WorkflowHealingTriggerRepository
	// healingOverrideRepo backs the Phase B operator-override
	// surface (migration 81). Nil keeps the override endpoints
	// at 503. SQLite deployments leave this unwired.
	healingOverrideRepo persistence.HealingTriggerOverrideRepository
	// healingCandidateRepo backs the Self-Healing Workflow Genome v1
	// candidate ledger (migration 87). When wired, the
	// generate-candidate endpoint ALSO persists a candidate row linked
	// to the architect's WorkflowProposal. Nil leaves generate-candidate
	// stamping only the trigger (the pre-genome behaviour) — candidate
	// persistence is best-effort and never blocks the proposal.
	healingCandidateRepo persistence.WorkflowHealingCandidateRepository
	// healingTrialRepo backs the trial history rendered on the candidate
	// GET endpoint (migration 88). Nil leaves the trials slice empty.
	healingTrialRepo persistence.WorkflowHealingTrialRepository
	// stepOutcomeRepo powers deterministic-recipe step selection in the
	// healing generate-candidate path (Self-Healing Workflow Genome v1,
	// part 2): it tallies per-step failures across a trigger's evidence
	// executions to pick the offending step. Nil → recipe generation is
	// skipped and generate-candidate uses the architect only.
	stepOutcomeRepo persistence.ExecutionStepOutcomeRepository
	// healingTrialRunner drives the operator-triggered run-trial endpoint
	// (Self-Healing Workflow Genome v1). Nil keeps run-trial at 503.
	// There is NO background auto-trial loop — trials are operator-
	// triggered only (LLD non-negotiable #5).
	healingTrialRunner HealingTrialRunner
	// healingPromoter drives the promote/reject endpoints. Promotion runs
	// the gate + the memetic apply path and is ALWAYS a manual operator
	// action (LLD non-negotiable #1). Nil keeps those endpoints at 503.
	healingPromoter HealingCandidatePromoter

	// blackboxService backs /api/v1/admin/blackbox/traces/{task_id}.
	// Autonomy Black Box Phase A — read-side trace assembly +
	// memoization. Nil keeps the endpoints at 503 with a clear
	// "not configured" message; SQLite/non-Postgres deployments
	// leave it unwired. The concrete type (*blackbox.Service) is
	// injected via an EE adapter that satisfies BlackBoxTraceService.
	blackboxService BlackBoxTraceService

	// blackboxEngine backs the Phase C counterfactual replay
	// endpoints (/api/v1/admin/blackbox/replay). Nil keeps those
	// endpoints at 503 — Phase C requires a Postgres backend +
	// taskcreate.Creator wired (the engine submits new tasks
	// via the standard creator gates). The concrete type
	// (*blackbox.Engine) is injected via an EE adapter that
	// satisfies BlackBoxReplayEngine.
	blackboxEngine BlackBoxReplayEngine
	// blackboxReplaySafety exposes the active replay-safe allow-list
	// classifier to the admin surface (/sideeffects endpoint) + the
	// counterfactual MCP gate. Nil-safe — nil means CE/Community edition
	// with replay-safety enforcement OFF (all tools allowed in replay).
	blackboxReplaySafety contracts.ReplaySafetyClassifier

	// memoryPolicyEvaluations backs the Policy-Aware Memory
	// Firewall admin endpoints. Postgres-only — SQLite stub
	// returns empty so the endpoint reports "no evaluations"
	// rather than 503.
	memoryPolicyEvaluations persistence.MemoryPolicyEvaluationRepository
	// memoryFirewallMode is the daemon's current enforcement
	// mode, snapshotted at boot. The /policy/mode endpoint
	// reports it. Future config-reload picks up changes via
	// the Server's reload hook.
	memoryFirewallMode string
	// memoryFirewallEditor backs the POST .../policy/chunks/{id}
	// endpoint — per-chunk policy edits with digest recompute.
	// Nil → POST endpoint returns 503.
	memoryFirewallEditor MemoryFirewallEditor
	// memoryFirewallProjectModeFn resolves per-project
	// enforcement-mode overrides (Phase D follow-on of the
	// firewall LLD). Mirrors the Searcher's FirewallDeps
	// resolver; production wires both to the same Container
	// helper. Nil keeps /policy/mode at daemon-level reporting.
	memoryFirewallProjectModeFn func(projectID string) (memoryfirewall.EnforcementMode, bool)

	// cpcRepo backs the admin CPC endpoints (list / show /
	// cancel). Phase D operator-cleanup surface (LLD §11
	// follow-on). Nil → endpoints return 503 with a clear
	// "not configured" message.
	cpcRepo persistence.CrossProjectCallRepository
	// extractedDocsRepo + extractorRegistry back the document-
	// extraction endpoint (POST /artifacts/{id}/extract). Both nil =
	// endpoint returns 503; same fail-soft contract every other
	// optional surface uses. See
	// https://docs.vornik.io
	extractedDocsRepo persistence.ExtractedDocumentRepository
	extractorRegistry *extractor.Registry
	extractorRunner   *extractor.Runner
	memoryIndexer     ExtractedDocumentIndexer
	// inputArtifactStore lets the REST CreateTask handler snapshot
	// inline InputArtifacts payloads into durable storage before
	// passing them through the auto-extract pipeline. Same shape as
	// dispatcher.InputArtifactStore — both surfaces share the
	// underlying *artifacts.Store via the service container, so
	// snapshots from either path are immediately visible to the
	// other (idempotent on content hash).
	inputArtifactStore InputArtifactStore
	llmUsageRepo       persistence.TaskLLMUsageRepository
	reservRepo         persistence.BudgetReservationRepository
	// chatAuditRepo persists one chat_audit_log row per external
	// chat-proxy + ollama-proxy call so the operator can answer
	// "what did agent X / external client Y ask the LLM at 03:14?"
	// from the same surface as Telegram / email / webchat
	// conversations. Nil disables the layer — proxy calls still
	// run, just no audit row. Internal agent calls (those carrying
	// X-Vornik-Task-ID / X-Vornik-Execution-ID) are skipped so
	// step-level tool_audit_log doesn't get duplicated.
	chatAuditRepo persistence.ChatAuditRepository
	// anonAttrWarner rate-limits the "external API call landed on
	// _external" diagnostic warn so a misconfigured client polling
	// every 30s doesn't flood the log. Lazy-initialised on first
	// anonymous-attribution call. See cost_attribution.go.
	anonAttrWarner *anonymousAttributionWarner
	// anonAttrWarnerOnce / legacyHeaderShadowedWarnerOnce guard the
	// lazy init of the two warners. net/http dispatches handlers
	// concurrently, so the previous unsynchronised check-then-set on
	// these *Server fields was a data race (and could hand a goroutine
	// a half-written pointer) under concurrent anonymous proxy traffic
	// (bug sweep 2026-06-04).
	anonAttrWarnerOnce sync.Once
	// legacyHeaderShadowedWarner fires when a client presents
	// X-Vornik-Project-ID alongside a DB-backed key — same rate-
	// limited shape as anonAttrWarner. The matching response
	// header (`Deprecation: true`) is stamped on every shadowed
	// request; the log warn is suppressed past the first
	// occurrence per (path, UA) per 5 minutes.
	legacyHeaderShadowedWarner     *legacyHeaderShadowedWarner
	legacyHeaderShadowedWarnerOnce sync.Once
	webhookEventRepo               persistence.WebhookEventRepository
	// webhookRelay forwards verified webhooks to the job tier on a
	// RelayMode (DMZ) node. When nil, IngestWebhook enqueues in-process.
	// Set via WithWebhookRelay (production) or SetWebhookRelay (tests).
	webhookRelay WebhookForwarder
	// forgeClassifier turns a verified webhook body into the provider-neutral
	// forge_job stamped on the created task, so the deterministic forge.*
	// system steps run without parsing free text. Nil → no forge_job stamped
	// (back-compat). Runs on the job tier (where the provider config lives).
	forgeClassifier ForgeClassifier
	// adminAuditRepo backs GET /api/v1/admin/audit (admin-only).
	// Nil makes the endpoint return 503 — same fail-soft contract
	// every other optional surface uses.
	adminAuditRepo persistence.AdminAuditRepository
	// operatorProfileRepo backs /api/v1/operators surface (list /
	// show / set / forget) consumed by the `vornikctl operator` CLI
	// + future external integrations. Nil → endpoints 503. The
	// same repo instance is shared with the dispatcher's
	// update_operator_profile tool and the /ui/memory/operators
	// admin UI; all three surfaces enforce identical allow-list +
	// rationale-required semantics.
	operatorProfileRepo persistence.OperatorProfileRepository

	// operatorIdentityLinkRepo backs /api/v1/operators/{id}/links
	// for the `vornikctl operator link / unlink / show-links`
	// CLI surface + the canonical-id walk on
	// /operators/{id}/forget (DeleteAllForOperator). Same repo
	// the dispatcher resolver uses. Nil falls through to 503.
	operatorIdentityLinkRepo persistence.OperatorIdentityLinkRepository
	// profileUseAuditRepo backs /api/v1/operators/{id}/audit
	// for the `vornikctl operator audit` CLI surface +
	// best-effort DeleteAllForOperator from `forget --include-audit`.
	// Same repo the dispatcher's per-turn audit write uses.
	// Nil falls through to 503. Phase B.
	profileUseAuditRepo persistence.ProfileUseAuditRepository
	// adminConfig carries the daemon's admin block; the endpoint
	// gate reads it to decide 404 / 401 / 403 / 200.
	adminConfig config.AdminConfig
	// instinctRepo backs the instinct surfaces (continuous-learning
	// instinct layer): GET /api/v1/instincts (list + filter), GET
	// /api/v1/instincts/{id}, POST /api/v1/instincts/{id}/retire, and
	// the admin-gated POST /api/v1/admin/instincts/recompute. All four
	// are read/inspect/retire only — they never mutate behaviour. Nil
	// makes the endpoints return 503 (same fail-soft contract every
	// other optional surface uses).
	instinctRepo persistence.InstinctRepository
	// instinctScorer is the Wilson+decay confidence model the
	// /admin/instincts/recompute endpoint hands to RecomputeConfidence.
	// Nil makes recompute 503 even when the repo is wired.
	instinctScorer persistence.InstinctScorer
	// githubAppWebhook is the GitHub App's HTTP entry point —
	// mounted at /api/v1/github-app/webhook when set by
	// WithGitHubAppWebhookHandler. Nil leaves the route unmounted
	// so deployments without a GitHub App return 404 rather than
	// a misleading 401 / 503. See internal/github.
	githubAppWebhook http.HandlerFunc
	// slackWebhook is the Slack bot's HTTP entry point — mounted at
	// /api/v1/slack/webhook when set by WithSlackWebhookHandler. Nil
	// leaves the route unmounted so deployments without a Slack
	// channel configured return 404 rather than a misleading 401 /
	// 503. See internal/slack.
	slackWebhook http.HandlerFunc
	// apiKeyRepo backs the DB-backed bearer-token surface (replaces
	// the operator-trusted X-Vornik-Project-ID + static YAML keys
	// for new deployments). nil falls back to the static-keys path
	// only — both coexist during the 2026.6.0 → 2026.8.0
	// deprecation window.
	apiKeyRepo persistence.APIKeyRepository
	// sessionBackend authenticates browser login sessions via the
	// vornik_session cookie (github-login phase 3). nil leaves cookie
	// auth off. Threaded into AuthMiddleware by applyMiddleware, the
	// same way apiKeyRepo is — and shared with the UI subtree's
	// AuthConfig so both surfaces honour the same sessions.
	sessionBackend auth.Backend
	// apiKeyLimiter throttles per-key request rate based on the
	// rate_limit_rps / rate_limit_burst columns on api_keys. nil
	// disables enforcement. Same instance is shared with the UI
	// subtree (service/container wires both).
	apiKeyLimiter *ratelimit.APIKeyLimiter
	// rateLimitMetrics records per-key and per-project limiter outcomes.
	// Nil keeps enforcement working without Prometheus emission.
	rateLimitMetrics *ratelimit.Metrics
	// dryRunMetrics holds the Prometheus counter for dry-run denial
	// events. Shared with the UI subtree so both call sites use the
	// same registered CounterVec. Nil disables metric emission; the
	// dedup warn log still fires.
	dryRunMetrics *DryRunMetrics
	// chainMetrics holds the auth-chain backend-verdict counter.
	// Same shared-instance + nil-disables contract as dryRunMetrics.
	chainMetrics *AuthChainMetrics
	// perIPLimiter is the unauthenticated per-IP backstop
	// (hardening sub-item 2). nil disables the layer entirely.
	perIPLimiter        *ratelimit.PerIPLimiter
	perIPRateLimitRPS   int
	perIPRateLimitBurst int
	// gistReader backs GET /api/v1/projects/{id}/gist. nil disables
	// the endpoint (returns 503 GIST_NOT_CONFIGURED).
	gistReader       GistReader
	autonomyEvalRepo persistence.AutonomyEvaluationRepository
	readinessChecks  []ReadinessCheck
	// draining is the graceful-shutdown gate. SIGTERM in
	// container.Run flips this true, then sleeps the grace period
	// before calling shutdown(). While set, /readyz returns 503
	// {"status":"draining"} so load balancers stop sending new
	// requests; /livez keeps returning 200 so k8s doesn't escalate
	// to kill -9. Atomic so the SIGTERM goroutine and every probe
	// request can read/write it without taking a mutex on the
	// hot path.
	draining        atomic.Bool
	rateLimiter     ratelimit.ProjectLimiter
	queue           *queue.Queue
	projectRegistry *registry.Registry
	// featureTradingProbe backs the "trading-series" feature-doctor check.
	// nil in minimal deployments (the check degrades to a graceful skip).
	featureTradingProbe featuredoctor.TradingSeriesProbe
	projectTemplates    *templates.Catalog
	configsDir          string
	// reloadHook triggers a synchronous config reload after a
	// create-from-template write so the new project is registered
	// in-memory before the client navigates to it. Nil = rely on the
	// async file-watcher. See WithConfigReloadHook.
	reloadHook func() error
	// workspaceLock is the shared per-project workspace lock. The
	// service container injects the SAME *workspacelock.Locker held by
	// the executor and UI server. Stored here for the git-over-HTTPS
	// push/read handler (Task 2.4), which will take it exclusively for
	// the receive-pack and shared for upload-pack so pushes serialise
	// with task execution and UI artifact deletes per project. Not yet
	// consumed by any handler in this task.
	workspaceLock *workspacelock.Locker
	// gitReceiveGuards re-asserts the server-side push guards
	// (receive.denyCurrentBranch=updateInstead + the pre-receive hook)
	// on the project repo immediately before a git-over-HTTPS push runs
	// receive-pack (Task 2.4). The service container wires it to
	// executor.EnsureReceiveGuards bound to the project's workspace dir.
	// Nil → the push handler skips the re-assert (the guards installed at
	// repo-bootstrap time still apply); tests inject a spy to assert it
	// is called before exec.
	gitReceiveGuards func(ctx context.Context, projectID string) error
	executor         ExecutorInterface
	taskLogSource    TaskLogSource
	config           *config.Config
	metricsRegistry  *prometheus.Registry
	// apiMetrics is the registered counter / histogram set
	// (NewAPIMetrics). Stashed here so the cost-attribution
	// hot path can bump the per-source counter without
	// re-resolving the registry.
	apiMetrics     *APIMetrics
	memorySearcher MemorySearcher
	memoryStats    MemoryStatsProvider
	// memoryCompanion powers the companion MCP server's `recall` and
	// `remember` tools (LLD 22). Nil-safe: those tools return a
	// "memory subsystem not wired on this daemon" error when this
	// field is unset, so deployments that don't run the memory
	// subsystem (e.g. tests, minimal harnesses) continue to work.
	memoryCompanion          MemoryCompanionAdapter
	memoryTitleBackfiller    MemoryTitleBackfiller
	memoryClassifyBackfiller MemoryClassifyBackfiller
	// memoryGraphReflagger powers POST /api/v1/memory/regraph —
	// scoped to a project, re-flags chunks with zero edges for the
	// KG worker to reprocess. Production wires the postgres
	// ChunkGraphExtractionRepository; nil makes the endpoint 503.
	memoryGraphReflagger MemoryGraphReflagger
	// workflowTelemetry powers GET /api/v1/admin/workflow-stats —
	// returns the per-workflow execution-evidence rollup the
	// architect agent (memetic-workflows arc) will consume.
	// Production wires *workflowtelemetry.Service; nil makes the
	// endpoint 503.
	workflowTelemetry WorkflowTelemetry
	// workflowArchitect powers POST /api/v1/admin/workflow-
	// architect/propose — runs the memetic-workflows architect
	// agent. Production wires *memetic.Architect via a service-
	// layer adapter; nil makes the endpoint 503.
	workflowArchitect WorkflowArchitect
	// workflowProposals backs the GET / GET / POST-decide trio
	// at /api/v1/admin/workflow-proposals. Same admin gate matrix.
	// nil makes the endpoints 503. Production wires the postgres
	// repo directly (the persistence interface matches the API's
	// needs without an adapter).
	workflowProposals persistence.WorkflowProposalRepository
	// workflowApplier powers POST /api/v1/admin/workflow-proposals/
	// {id}/apply — the Slice 4 apply path. nil makes the endpoint
	// return 503. Production wires *memetic.Applier.
	workflowApplier WorkflowApplier
	// workflowRollbacker powers POST /api/v1/admin/workflow-
	// proposals/{id}/rollback — Slice 5 of the memetic-workflows
	// arc. nil makes the endpoint return 503.
	workflowRollbacker WorkflowRollbacker
	// proposalRejectionRecorder writes a rejected workflow proposal
	// back to the instinct layer as an 'architect-reject' contradiction
	// (Consumer B — instinct.consumers.architect_priors). Best-effort:
	// nil (gate off / not wired) means the reject path behaves exactly
	// as before, and a write-back error never fails the operator's
	// rejection — it is logged and swallowed.
	proposalRejectionRecorder WorkflowRejectionRecorder
	// memoryCacheStats backs GET /api/v1/memory/cache-stats — Phase D
	// embedding-cache + Phase E response-cache aggregates for the
	// vornikctl CLI. nil makes the endpoint return 503.
	memoryCacheStats MemoryCacheStatsProvider
	// memoryAuditRepo (optional) backs GET /api/v1/projects/{p}/memory/feedback
	// — chunk-utility analytics. nil makes the endpoint return 503.
	memoryAuditRepo persistence.MemoryRetrievalAuditRepository
	// Phase 2/3 of memory hardening — wired by the service container.
	// Each is nil-safe: handlers return 503 MEMORY_HARDENING_DISABLED
	// when the corresponding repo isn't configured.
	memoryQuarantine persistence.MemoryQuarantineRepository
	corpusEpochs     persistence.CorpusEpochRepository
	ingestQueue      persistence.IngestQueueRepository
	// toolAuditRepo backs POST /api/v1/internal/tool-audit so
	// agents can stream per-tool-call audit rows in realtime
	// instead of waiting for the post-step batch from result.json.
	// nil makes the endpoint return 503; production always wires it.
	toolAuditRepo persistence.ToolAuditRepository
	// tradingOrderRepo backs POST /api/v1/internal/trading-orders.
	// The broker MCP's AuditWriter posts one row per place_order
	// call (success or refused) so the daemon has an
	// independent audit trail. nil makes the endpoint return
	// 503 — production always wires it; the failure mode is
	// "row stays in broker journal until daemon comes back".
	tradingOrderRepo persistence.TradingOrderRepository
	// tradingSafetyRepo backs POST /api/v1/internal/trading-
	// safety-events — kill-switch toggles, breaker trips, cap
	// refusals, idempotency replay hits. Same writer + retry
	// machinery as tradingOrderRepo on the broker side; same
	// fail-safe contract.
	tradingSafetyRepo persistence.TradingSafetyEventRepository
	// tradingFillRepo backs POST /api/v1/internal/trading-fills —
	// the broker MCP's poll loop posts one row per fill it
	// observes. Same trust + idempotency contract as the order
	// + safety-event channels.
	tradingFillRepo persistence.TradingFillRepository
	// tradingPositionsRepo backs the equity-snapshot history. Read
	// by GetTradingStateReplay so the broker can restore its HWM +
	// daily-loss baseline across a restart (audit T5). nil → the
	// state-replay response omits breaker state (broker keeps its
	// optimistic boot defaults).
	tradingPositionsRepo persistence.TradingPositionsSnapshotRepository
	// tradingAuthVerifier enforces HMAC request-authentication on the
	// /api/v1/internal/trading-* endpoints (backlog: "HMAC/mTLS on
	// /internal/trading-*"). nil → feature disabled; the endpoints
	// keep their bearer-only behaviour (backward-compatible). When
	// non-nil the handlers fail CLOSED on a missing/invalid signature.
	// See https://docs.vornik.io
	tradingAuthVerifier *tradingauth.Verifier
	// tradingRateLimiter caps per-project trading order placement on
	// POST /api/v1/internal/trading-orders, using the project's
	// TradingRateLimit caps. nil → no trading-specific cap. Distinct
	// from rateLimiter (task creation). Concrete *ratelimit.Limiter so
	// the key-scoped CheckKey surface is available.
	tradingRateLimiter *ratelimit.Limiter
	mcpExecutor        MCPExecutor
	// mcpRegistry powers GET /api/v1/mcp/servers — the daemon-level
	// MCP discovery surface. Distinct from mcpExecutor (per-project
	// tool routing): this is read-only, never participates in tool
	// invocation, and exists so operators can see what's installed
	// at the daemon scope without hand-walking every project YAML.
	// Nil = the endpoint returns {"servers":[]}.
	mcpRegistry MCPRegistrySource
	// chatProvider powers POST /api/v1/chat/completions — the internal
	// OpenAI-compatible proxy that lets agent containers reuse whatever
	// LLM the dispatcher is configured with (typically Claude via the
	// `claude` CLI) without knowing how the provider is implemented.
	chatProvider chat.Provider
	// explainRenderer powers POST /api/v1/projects/{p}/tasks/{id}/explain.
	// Deterministic — never calls an LLM. Joins playbook.Lookup with the
	// task's step outcomes / tool audit / log tail to produce a plain-
	// text summary. The previous design called an LLM here; that cost
	// is gone, and the endpoint is now free + instant. Operators who
	// want an LLM-elaborated prose narrative use the UI "Post-mortem"
	// button which hits postmortem.Explainer.Generate (persisted, billed).
	explainRenderer *postmortem.Renderer
	// pricingPath points at configs/pricing.yaml so GET /api/v1/models
	// can crosswalk every discovered model against the static pricing
	// table. Empty disables the crosswalk; the endpoint still returns
	// the discovery list, just without per-model $/1M numbers.
	// Also used by the chat-proxy recordChatAPIUsage helper to
	// compute cost on third-party /v1 + /api/... calls.
	pricingPath string
	// promptCacheMode is the daemon-wide default prompt-cache mode
	// applied to every inbound /api/v1/chat/completions request
	// that doesn't carry an explicit CacheStrategy. Empty / "off"
	// disables caching; "auto" / "prefix" turn on the
	// provider-native annotation in Bedrock + Anthropic
	// converters. See ChatConfig.PromptCacheMode.
	promptCacheMode string
	// externalAPIBillingProjectID is the fallback project ID used
	// to attribute /v1/chat/completions + /api/chat + /api/generate
	// cost rows when the caller doesn't supply X-Vornik-Project-ID.
	// Empty means "_external" — but in production the operator
	// should pin this to a real project so the cost shows up on
	// that project's dashboard instead of a synthetic bucket.
	externalAPIBillingProjectID string

	// chatContextBudget is the token-window size against which the
	// per-request context-budget tier is computed on the chat-proxy
	// surface. The proxy reads resp.Usage.PromptTokens (the actual
	// prompt the upstream provider charged us) and divides by this
	// budget to derive PEAK / GOOD / DEGRADING / POOR; the value is
	// surfaced to the client via the X-Vornik-Context-Tier response
	// header and to operators via the vornik_chat_context_tier_total
	// counter. Zero disables the tier surface (header omitted, no
	// metric emission) — useful on deployments without a configured
	// model context size, where the % math would be meaningless.
	chatContextBudget int

	// chatDispatcherMetrics is the dispatcher's *Metrics handle. The
	// chat-proxy reuses the existing vornik_chat_context_tier_total
	// counter / headroom-pct histogram declared on it so operators
	// see proxy traffic + dispatcher traffic in a single timeseries
	// (split by project label). Nil disables the metric path
	// cleanly — the X-Vornik-Context-Tier header still fires.
	chatDispatcherMetrics ChatContextTierMetrics
	chatCacheMetrics      ChatCacheMetrics
	// pricingTableCache is the lazily-loaded pricing table reused
	// across chat-proxy calls. Loaded on first need to keep the
	// hot path off-disk; reloads only on operator-initiated
	// config reload (the path itself doesn't change at runtime).
	pricingTableCache *pricing.Table
	pricingTableOnce  sync.Once
	// budgetNotifier receives soft- and hard-cap alerts when a task
	// creation trips the project budget. Optional — nil means
	// alerts are logged only (the historical behaviour).
	budgetNotifier budget.Notifier

	// fillNotifier (optional) receives a notification per ingested
	// trading_fills row. Telegram bot is the only production
	// implementation today — narrow interface so the api package
	// doesn't import telegram (which would pull a long tail of
	// chat-machinery types into the api binary's import graph).
	fillNotifier FillNotifier

	// Phase 24 — conversational task lifecycle.
	// taskMessageRepo backs the per-task chat thread; scratchpadRepo
	// the lead's running summary. Both nil-safe — handlers respond
	// 503 TASK_LIFECYCLE_DISABLED when not wired.
	taskMessageRepo    persistence.TaskMessageRepository
	taskScratchpadRepo persistence.TaskScratchpadRepository
	// rescheduler signals the scheduler to wake the leasing loop
	// after a manual re-queue (operator answer / amend / resume).
	// Nil → scheduler picks up on its next periodic poll, which is
	// fine for correctness but slower for responsiveness.
	rescheduler Rescheduler

	// secretsDetector + secretsActions back the Phase 2 webhook
	// payload Block-mode scan. Default action for CheckpointWebhook
	// is Block — inbound payloads carrying credentials usually
	// indicate a misconfiguration the operator wants to fix at
	// source rather than have us silently rewrite. Per-source opt-out
	// is via ProjectWebhookSource.AllowSecrets (signed-payload formats
	// that legitimately carry long high-entropy tokens).
	secretsDetector secrets.Detector
	secretsActions  map[string]secrets.Action

	webhookRejectMu        sync.Mutex
	webhookRejectLog       map[string][]time.Time
	webhookRejectGCCounter uint64

	// tradingMetrics observes broker→daemon audit-channel ingest
	// (orders, fills, safety events). Nil-safe; ingest still
	// persists rows when metrics is unset.
	tradingMetrics *TradingMetrics
	// taskCreator is the shared task-creation core that both
	// surfaces (REST + UI form) funnel through. When wired,
	// CreateTask delegates the persist/enqueue/rate-limit/budget
	// pipeline to it; when nil, the legacy inline path runs
	// (preserves coverage of the existing test fixtures that
	// don't supply a Creator).
	taskCreator *taskcreate.Creator

	// cfGateCache memoizes the counterfactual replay-ness of each
	// task the MCP gate has resolved at least once. Keyed by task
	// ID; value is a cfGateEntry snapshot of the extracted payload
	// overrides. A task payload's replay-ness (and its recorded
	// original_tools / stubs) is IMMUTABLE once created, so caching
	// it is safe forever.
	//
	// Purpose (FIX 1, review of a799e3f2): the gate calls
	// taskRepo.Get on every agent MCP dispatch; a transient DB blip
	// or an archived/purged task row would otherwise 503 ALL agent
	// tool calls (every call carries a task ID), not just replays.
	// On a Get error we consult this cache: a cached non-replay
	// passes through; a cached replay re-enforces from the snapshot;
	// no cache entry still fails closed.
	//
	// Unbounded is acceptable: entries are tiny (a few flags + small
	// maps) and the live task count per daemon process is bounded by
	// the scheduler's concurrency + retention, not user input. No
	// eviction is wired because there is no codebase precedent for a
	// bounded LRU here and the footprint is negligible.
	cfGateCache sync.Map // map[string]cfGateEntry

	// clusterNodeRepo backs GET /api/v1/cluster — fleet heartbeat
	// registry. nil → endpoint returns 503 CLUSTER_NOT_CONFIGURED.
	// Wired by WithClusterNodeRepository (slice C1).
	clusterNodeRepo persistence.ClusterNodeRepository
	// leaderLockRepo backs GET /api/v1/cluster — singleton-lease
	// ownership map (instance_id ↔ holder_id join). Nil-safe: the
	// endpoint returns an empty leases array when unset so a
	// deployment that hasn't migrated leader-election yet still
	// returns the node fleet.
	leaderLockRepo persistence.DaemonLeaderLockRepository

	// support-report collectors (POST /api/v1/support-report). All
	// optional: a nil collector degrades the corresponding bundle
	// section gracefully (best-effort, support-report-design.md §7).
	// supportDoctor runs the in-process doctor checks; supportHealth /
	// supportMetrics snapshot the always-on health + metrics sections;
	// supportJudgeRepo / supportPostMortemRepo are the two per-task
	// repos not otherwise held on the Server.
	supportDoctor         SupportDoctorRunner
	supportHealth         SupportHealthSource
	supportMetrics        SupportMetricsSource
	supportJudgeRepo      SupportJudgeReader
	supportPostMortemRepo SupportPostMortemReader
}

// WithSupportReportCollectors wires the optional support-report
// section collectors (doctor / health / metrics) + the two per-task
// repos (judge verdict, post-mortem) not otherwise held on the Server.
// Any nil arg leaves that section to degrade gracefully. See
// https://docs.vornik.io
func WithSupportReportCollectors(
	doctor SupportDoctorRunner,
	health SupportHealthSource,
	metrics SupportMetricsSource,
	judge SupportJudgeReader,
	postMortem SupportPostMortemReader,
) ServerOption {
	return func(s *Server) {
		s.SetSupportReportCollectors(doctor, health, metrics, judge, postMortem)
	}
}

// SetSupportReportCollectors is the post-construction wiring path for
// the support-report collectors. The doctor + health adapters depend on
// the fully-built Server (doctor handler, readiness checks), so the
// service container wires them after NewServer returns — mirroring
// SetTradingMetrics. Nil-safe.
func (s *Server) SetSupportReportCollectors(
	doctor SupportDoctorRunner,
	health SupportHealthSource,
	metrics SupportMetricsSource,
	judge SupportJudgeReader,
	postMortem SupportPostMortemReader,
) {
	if s == nil {
		return
	}
	s.supportDoctor = doctor
	s.supportHealth = health
	s.supportMetrics = metrics
	s.supportJudgeRepo = judge
	s.supportPostMortemRepo = postMortem
}

// SetTradingMetrics wires the Prometheus counters for trading
// ingest. Called by the service container after the metrics
// registry is initialised.
func (s *Server) SetTradingMetrics(m *TradingMetrics) {
	if s != nil {
		s.tradingMetrics = m
	}
}

// SetChatCacheMetrics wires the prompt-cache observability surface
// (audit N8) after the metrics registry is initialised. Mirrors the
// WithChatCacheMetrics option for the post-construction wiring path the
// service container uses (chat metrics are built in wireComponentMetrics,
// which runs after the API server is constructed). Nil-safe.
func (s *Server) SetChatCacheMetrics(m ChatCacheMetrics) {
	if s != nil {
		s.chatCacheMetrics = m
	}
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
		s.executionRepo = repo
	}
}

// WithExecutionStepOutcomeRepository wires the per-step outcome repo used by
// deterministic-recipe step selection in the healing generate-candidate path
// (Self-Healing Workflow Genome v1, part 2). Optional; absent → the handler
// generates candidates via the architect only.
func WithExecutionStepOutcomeRepository(repo persistence.ExecutionStepOutcomeRepository) ServerOption {
	return func(s *Server) {
		s.stepOutcomeRepo = repo
	}
}

// WithForker wires the failure-forensics fork primitive behind
// POST /api/v1/executions/{id}/fork-from-step. nil keeps the
// endpoint at 503 (deployment hasn't enabled the feature).
func WithForker(f ForkExecutor) ServerOption {
	return func(s *Server) {
		s.forker = f
	}
}

// WithWorkspaceLock injects the shared per-project workspace lock. The service
// container passes the SAME *workspacelock.Locker instance held by the
// executor and UI server. Stored only — the git-over-HTTPS handler (Task 2.4)
// consumes it; no handler in this task uses it yet. Omitting it falls back to
// a private workspacelock.New() so standalone constructions don't carry a nil.
func WithWorkspaceLock(l *workspacelock.Locker) ServerOption {
	return func(s *Server) {
		if l != nil {
			s.workspaceLock = l
		}
	}
}

// WithGitReceiveGuards wires the idempotent push-guard re-assert the
// git-over-HTTPS push handler runs immediately before invoking receive-pack
// (Task 2.4). The function receives the project ID and must ensure
// receive.denyCurrentBranch=updateInstead + the pre-receive hook are installed
// on that project's workspace repo. Production binds it to
// executor.EnsureReceiveGuards over <ProjectWorkspacePath>/<projectID>; nil
// leaves the push handler relying on the bootstrap-time guards.
func WithGitReceiveGuards(fn func(ctx context.Context, projectID string) error) ServerOption {
	return func(s *Server) {
		s.gitReceiveGuards = fn
	}
}

// WithProjectWizard wires the conversational project setup
// wizard behind POST /api/v1/projects/wizard/converse. nil keeps
// the endpoint at 503.
func WithProjectWizard(w ProjectWizard) ServerOption {
	return func(s *Server) {
		s.projectWizard = w
	}
}

// WithOnboardingDetector wires the installation onboarding detector
// used by /api/v1/setup/status. Nil detector fields keep the endpoint
// conservative rather than crashing.
func WithOnboardingDetector(det onboarding.Detector) ServerOption {
	return func(s *Server) {
		s.setupDetector = det
	}
}

// WithSetupSessions wires the onboarding session repository backing the
// setup write/validate/commit endpoints. nil → endpoints return 503.
func WithSetupSessions(repo persistence.InstallationOnboardingSessionRepository) ServerOption {
	return func(s *Server) { s.setupSessions = repo }
}

// WithSetupValidator wires the chat-config validator. nil → validate/commit
// return 503.
func WithSetupValidator(v onboarding.ChatValidatorInterface) ServerOption {
	return func(s *Server) { s.setupValidator = v }
}

// WithSetupConfigPath records the daemon's resolved config.yaml path so
// the commit handler can patch it via featuredoctor.FileConfigWriter.
func WithSetupConfigPath(path string) ServerOption {
	return func(s *Server) { s.setupConfigPath = path }
}

// WithSetupSecretsDir records the <configDir>/secrets directory the
// commit handler writes chat.env into.
func WithSetupSecretsDir(dir string) ServerOption {
	return func(s *Server) { s.setupSecretsDir = dir }
}

// WithLiveSubscriber wires the live-observation event source
// behind GET /api/v1/executions/{id}/live. nil keeps the
// endpoint at 503.
func WithLiveSubscriber(sub LiveSubscriber) ServerOption {
	return func(s *Server) {
		s.liveSub = sub
	}
}

// WithLiveAllowedOrigins extends the live WebSocket's Origin
// check with extra host patterns beyond the daemon's own
// request host. Pass the public hostnames a reverse proxy
// terminates at — typically just one. Wildcards (*.example.com)
// are matched with path.Match. Empty == same-origin only.
//
// SECURITY: do NOT pass "*" or similar wildcard-all patterns.
// The live stream carries tool I/O, file paths, and LLM tokens;
// allowing any origin enables CSRF-grade exfiltration where any
// site the operator browses can subscribe to live events.
func WithLiveAllowedOrigins(patterns []string) ServerOption {
	return func(s *Server) {
		s.liveAllowedOrigins = patterns
	}
}

// WithExecutionHintRepository wires the hint repo behind POST
// /api/v1/executions/{id}/hints (Feature #3 Phase C). nil keeps
// the endpoint at 503.
func WithExecutionHintRepository(repo persistence.ExecutionHintRepository) ServerOption {
	return func(s *Server) {
		s.hintRepo = repo
	}
}

// WithA2AHandler wires the A2A (Agent-to-Agent) protocol surface
// served at /.well-known/agent.json + /a2a/v1/agents/. nil keeps
// the endpoints at 404 (silent — non-A2A deployments shouldn't
// advertise a feature flag).
//
// See https://docs.vornik.io
func WithA2AHandler(h *a2a.Handler) ServerOption {
	return func(s *Server) {
		s.a2aHandler = h
	}
}

// WithCrossProjectCallRepository wires the CPC ledger behind
// the admin endpoints (/api/v1/admin/cpc). Inter-project
// orchestration Phase D follow-on. Nil keeps the endpoints
// at 503 with a clear "not configured" message.
func WithCrossProjectCallRepository(repo persistence.CrossProjectCallRepository) ServerOption {
	return func(s *Server) {
		s.cpcRepo = repo
	}
}

// WithReminderRepository wires the dispatcher_reminders ledger
// behind /api/v1/reminders. Scheduled-reminders LLD §5.2.
// Nil keeps the endpoint at 503 with a "not configured" message.
func WithReminderRepository(repo persistence.ReminderRepository) ServerOption {
	return func(s *Server) {
		s.reminderRepo = repo
	}
}

// WithArchiveService wires the project-archival lifecycle service
// behind /api/v1/projects/{id}/archive,/unarchive,/delete-now.
// Nil keeps the endpoints at 503.
func WithArchiveService(svc *projectarchive.LifecycleService) ServerOption {
	return func(s *Server) {
		s.archiveService = svc
	}
}

// Rescheduler is the narrow interface the conversational lifecycle
// uses to nudge the scheduler after re-queueing a task. Concrete
// implementation is *scheduler.Scheduler; the api package keeps
// the dependency narrow so handlers don't pull the full scheduler
// surface.
type Rescheduler interface {
	// Wake hints the scheduler to scan for newly-queued tasks now
	// rather than waiting for the next periodic poll. Best-effort;
	// returns nil even if the scheduler is busy.
	Wake()
}

// WithTaskMessageRepository wires the task_messages backing.
func WithTaskMessageRepository(repo persistence.TaskMessageRepository) ServerOption {
	return func(s *Server) { s.taskMessageRepo = repo }
}

// WithTaskScratchpadRepository wires the task_scratchpad backing.
func WithTaskScratchpadRepository(repo persistence.TaskScratchpadRepository) ServerOption {
	return func(s *Server) { s.taskScratchpadRepo = repo }
}

// WithRescheduler wires the scheduler-wake hook used by the
// conversational lifecycle handlers after a re-queue.
func WithRescheduler(r Rescheduler) ServerOption {
	return func(s *Server) { s.rescheduler = r }
}

// WithArtifactRepository sets the artifact repository.
func WithArtifactRepository(repo persistence.ArtifactRepository) ServerOption {
	return func(s *Server) {
		s.artifactRepo = repo
	}
}

// ExtractedDocumentIndexer is the minimal seam between the api
// package and internal/memory. Implemented by *memory.Indexer; the
// interface is here so api doesn't import memory directly (which
// would pull the embedder + worker into every binary that links
// api).
type ExtractedDocumentIndexer interface {
	IngestExtractedSections(
		ctx context.Context,
		projectID, taskID, sourceArtifactID, extractedDocumentID string,
		sections []ExtractedSectionInput,
	) (int, error)
}

// ExtractedSectionInput mirrors memory.ExtractedSection at the api
// boundary so the interface above is free of an internal/memory
// import. The two structs are kept in lockstep by intent — when
// memory.ExtractedSection grows a field, this one follows.
type ExtractedSectionInput struct {
	SectionID  string
	SourceName string
	Content    string
}

// WithExtractorPipeline wires the document-extraction surface: the
// registry (which extractor matches which MIME), the runner (which
// persists results), the extracted-docs repo (cache lookup), and
// the memory indexer (so extraction also chunks into project
// memory). Any nil disables the extraction endpoint cleanly — same
// fail-soft contract every other optional surface uses.
func WithExtractorPipeline(
	reg *extractor.Registry,
	runner *extractor.Runner,
	repo persistence.ExtractedDocumentRepository,
	indexer ExtractedDocumentIndexer,
) ServerOption {
	return func(s *Server) {
		s.extractorRegistry = reg
		s.extractorRunner = runner
		s.extractedDocsRepo = repo
		s.memoryIndexer = indexer
	}
}

// InputArtifactStore mirrors dispatcher.InputArtifactStore — the
// narrow seam the api layer needs to snapshot inline
// CreateTaskRequest.InputArtifacts into durable storage. Defined
// here (not imported from dispatcher) so the api package's
// dependency graph stays one-way.
type InputArtifactStore interface {
	StoreInput(ctx context.Context, projectID, name, sourcePath string) (*persistence.Artifact, error)
}

// ArtifactOpener is the backend-aware artifact byte source used by the
// extraction pipeline. *artifacts.Store implements it for both local and S3
// storage, avoiding direct reads from Artifact.StoragePath when that column is
// only a compatibility path.
type ArtifactOpener interface {
	Open(ctx context.Context, artifactID string) (io.ReadCloser, error)
}

// WithInputArtifactStore wires the artifact-store seam used by
// POST /api/v1/projects/{p}/tasks when inputArtifacts is non-empty.
// nil leaves the InputArtifacts field unprocessed (legacy
// behaviour); operators who need REST-driven uploads must wire
// this for the snapshot to happen.
func WithInputArtifactStore(s InputArtifactStore) ServerOption {
	return func(srv *Server) { srv.inputArtifactStore = s }
}

// WithArtifactOpener wires backend-aware artifact reads for extraction.
func WithArtifactOpener(o ArtifactOpener) ServerOption {
	return func(srv *Server) { srv.artifactOpener = o }
}

// WithLLMUsageRepository sets the per-step LLM usage repository. When set,
// POST /tasks runs a budget check and returns 429 when the project is over
// its configured daily or monthly hard cap.
func WithLLMUsageRepository(repo persistence.TaskLLMUsageRepository) ServerOption {
	return func(s *Server) {
		s.llmUsageRepo = repo
	}
}

// WithBudgetReservationRepository wires the reservation ledger so the
// webhook task-admission path atomically reserves hard-cap headroom before
// inserting (trading-hardening §1). The POST /tasks path reserves inside the
// shared taskcreate.Creator instead. Optional.
func WithBudgetReservationRepository(repo persistence.BudgetReservationRepository) ServerOption {
	return func(s *Server) {
		s.reservRepo = repo
	}
}

// WithChatAuditRepository wires the chat-audit repo for the
// chat-proxy + ollama-proxy paths. Each external-API call lands
// one chat_audit_log row so the operator-visible audit surface
// covers external traffic the same way it covers Telegram /
// email / webchat conversations. Nil disables the layer cleanly
// (proxy calls still run, no row).
func WithChatAuditRepository(repo persistence.ChatAuditRepository) ServerOption {
	return func(s *Server) {
		s.chatAuditRepo = repo
	}
}

// WithWebhookEventRepository wires durable webhook ingress auditing.
func WithWebhookEventRepository(repo persistence.WebhookEventRepository) ServerOption {
	return func(s *Server) {
		s.webhookEventRepo = repo
	}
}

// WithOperatorProfileRepository wires the per-operator profile
// store behind /api/v1/operators (list / show / set / forget).
// Nil keeps the surface dormant (503). Same repo instance is
// shared with the dispatcher's update_operator_profile tool and
// the /ui/memory/operators admin UI — wiring it here grants
// CLI parity with the chat-side write tool.
func WithOperatorProfileRepository(repo persistence.OperatorProfileRepository) ServerOption {
	return func(s *Server) {
		s.operatorProfileRepo = repo
	}
}

// WithOperatorIdentityLinkRepository wires the cross-channel
// identity-link repo behind /api/v1/operators/{id}/links.
// Powers the `vornikctl operator link / unlink / show-links` CLI
// surface and lets ForgetOperator also drop link rows so the
// canonical id doesn't outlive its profile. Nil keeps the
// link endpoints at 503 (same fail-soft contract).
func WithOperatorIdentityLinkRepository(repo persistence.OperatorIdentityLinkRepository) ServerOption {
	return func(s *Server) {
		s.operatorIdentityLinkRepo = repo
	}
}

// WithProfileUseAuditRepository wires the per-turn audit log of
// operator-profile use behind /api/v1/operators/{id}/audit
// (Phase B). nil keeps the audit endpoint at 503.
func WithProfileUseAuditRepository(repo persistence.ProfileUseAuditRepository) ServerOption {
	return func(s *Server) {
		s.profileUseAuditRepo = repo
	}
}

// WithAdminAuditRepository wires the admin-action audit repo so
// the daemon can expose GET /api/v1/admin/audit. Nil leaves the
// endpoint returning 503 — same fail-soft contract every other
// optional surface uses. See admin-ui-design.md slice 1.
func WithAdminAuditRepository(repo persistence.AdminAuditRepository) ServerOption {
	return func(s *Server) {
		s.adminAuditRepo = repo
	}
}

// WithClusterNodeRepository wires the fleet heartbeat registry behind
// GET /api/v1/cluster. Nil leaves the endpoint at 503 (slice C1).
func WithClusterNodeRepository(repo persistence.ClusterNodeRepository) ServerOption {
	return func(s *Server) {
		s.clusterNodeRepo = repo
	}
}

// WithLeaderLockRepository wires the singleton-leader lock repo so
// GET /api/v1/cluster can join holder_id → node profile (lease map).
// Nil-safe: the endpoint returns an empty leases slice when unset.
func WithLeaderLockRepository(repo persistence.DaemonLeaderLockRepository) ServerOption {
	return func(s *Server) {
		s.leaderLockRepo = repo
	}
}

// WithAdminConfig wires the daemon's admin config (enabled flag +
// allowed-keys list) into the api Server so /api/v1/admin/* can
// enforce the same gate the UI subtree uses.
func WithAdminConfig(cfg config.AdminConfig) ServerOption {
	return func(s *Server) {
		s.adminConfig = cfg
	}
}

// WithInstinctRepository wires the instinct repository so the daemon
// can expose the read/inspect/retire instinct surfaces. Nil leaves the
// endpoints returning 503 — same fail-soft contract every other
// optional surface uses. See
// https://docs.vornik.io
func WithInstinctRepository(repo persistence.InstinctRepository) ServerOption {
	return func(s *Server) {
		s.instinctRepo = repo
	}
}

// WithInstinctScorer wires the confidence scorer used by the
// admin-gated POST /api/v1/admin/instincts/recompute endpoint. Nil
// leaves recompute returning 503 even when the repo is wired (the
// repository needs an injected scorer to recompute confidence).
func WithInstinctScorer(scorer persistence.InstinctScorer) ServerOption {
	return func(s *Server) {
		s.instinctScorer = scorer
	}
}

// WithGitHubAppWebhookHandler wires the GitHub App webhook entry
// point. The service container constructs an internal/github.Channel
// from project config and passes channel.HandleWebhook here. Nil
// or unset leaves the route unmounted entirely so deployments
// without a GitHub App configured 404 the URL — clearer than a
// 401 or 503 from a stub.
func WithGitHubAppWebhookHandler(h http.HandlerFunc) ServerOption {
	return func(s *Server) {
		s.githubAppWebhook = h
	}
}

// WithSlackWebhookHandler wires the Slack Events API webhook entry
// point. The service container constructs internal/slack.Channel
// instances from per-project config and registers a multiplexer
// HandlerFunc here that fans inbound deliveries out to the matching
// channel by team_id. Nil or unset leaves the route unmounted so
// deployments without a Slack channel configured 404 the URL —
// clearer than a 401 or 503 from a stub.
func WithSlackWebhookHandler(h http.HandlerFunc) ServerOption {
	return func(s *Server) {
		s.slackWebhook = h
	}
}

// WithAPIKeyRepository wires the DB-backed API-key surface. When
// non-nil, AuthMiddleware checks DB rows BEFORE the static-keys
// map and the bound project becomes the authoritative cost-row
// target (X-Vornik-Project-ID overrides are ignored on the DB
// path). Nil preserves the legacy static-keys-only behaviour.
func WithAPIKeyRepository(repo persistence.APIKeyRepository) ServerOption {
	return func(s *Server) {
		s.apiKeyRepo = repo
	}
}

// WithServerSessionBackend wires the browser-session cookie backend
// onto the api Server so applyMiddleware can thread it into
// AuthMiddleware (github-login phase 3). nil leaves cookie auth off.
// Distinct name from the AuthConfigOption-returning WithSessionBackend
// (routes.go) so the two option types don't collide — same pattern as
// WithAuthAPIKeyLimiter vs WithAPIKeyLimiter. The service container
// passes the same backend instance to the UI subtree's AuthConfig so
// both surfaces honour one set of sessions.
func WithServerSessionBackend(b auth.Backend) ServerOption {
	return func(s *Server) {
		s.sessionBackend = b
	}
}

// WithGistReader wires the project-gist read surface. Used by
// GET /api/v1/projects/{id}/gist; nil leaves the endpoint
// returning 503 GIST_NOT_CONFIGURED.
func WithGistReader(r GistReader) ServerOption {
	return func(s *Server) {
		s.gistReader = r
	}
}

// WithAPIKeyLimiter wires the per-key request-rate token-bucket
// limiter. AuthMiddleware enforces per-key limits using this
// instance after a successful DB-key lookup. The service
// container constructs a single shared limiter so the api
// router and UI subtree count requests against the same buckets.
func WithAPIKeyLimiter(l *ratelimit.APIKeyLimiter) ServerOption {
	return func(s *Server) {
		s.apiKeyLimiter = l
	}
}

// WithPerIPLimiter wires the unauthenticated per-IP backstop
// (hardening sub-item 2) onto the Server. AuthMiddleware fires
// it BEFORE auth so an unauthenticated flood can't reach the
// auth path. rps/burst echo the daemon's config block and are
// shared with the AuthConfig built in applyMiddleware.
func WithPerIPLimiter(l *ratelimit.PerIPLimiter, rps, burst int) ServerOption {
	return func(s *Server) {
		s.perIPLimiter = l
		s.perIPRateLimitRPS = rps
		s.perIPRateLimitBurst = burst
	}
}

// WithRateLimitMetrics wires Prometheus counters/gauges for project
// and API-key rate-limit decisions. Enforcement itself is independent
// from metrics; nil disables emission only.
func WithRateLimitMetrics(m *ratelimit.Metrics) ServerOption {
	return func(s *Server) {
		s.rateLimitMetrics = m
	}
}

// WithDryRunMetrics wires the dry-run denial counter into the Server
// so applyMiddleware can thread it into the auth config. Pass the same
// instance you supply to the UI subtree — a CounterVec can only be
// registered once per Prometheus registerer.
func WithDryRunMetrics(m *DryRunMetrics) ServerOption {
	return func(s *Server) {
		s.dryRunMetrics = m
	}
}

// WithChainMetrics wires the auth-chain backend-verdict counter into
// the Server so applyMiddleware can thread it into the auth config.
// Pass the same instance you supply to the UI subtree — a CounterVec
// can only be registered once per Prometheus registerer.
func WithChainMetrics(m *AuthChainMetrics) ServerOption {
	return func(s *Server) {
		s.chainMetrics = m
	}
}

// WithAutonomyEvaluationRepository wires the autonomy evaluation audit
// trail. Drives GET /api/v1/projects/{p}/autonomy/evaluations and the
// vornikctl autonomy evaluations CLI.
func WithAutonomyEvaluationRepository(repo persistence.AutonomyEvaluationRepository) ServerOption {
	return func(s *Server) {
		s.autonomyEvalRepo = repo
	}
}

// WithReadinessCheck appends one named check to the /readyz handler.
// Checks run sequentially under a 3 s deadline; the handler returns 503
// on the first failure with a per-check JSON report listing every check
// that ran (ok / error details). Register one per dependency you want
// reflected in the readiness signal (DB, chat provider, podman,
// scheduler heartbeat, retention heartbeat, …).
func WithReadinessCheck(name string, check func(ctx context.Context) error) ServerOption {
	return func(s *Server) {
		if name == "" || check == nil {
			return
		}
		s.readinessChecks = append(s.readinessChecks, ReadinessCheck{Name: name, Check: check})
	}
}

// WithRateLimiter wires the shared task-creation rate limiter. When set,
// POST /tasks returns 429 RATE_LIMITED when the project is over its
// configured per-minute/per-hour caps. Accepts any backend (in-process
// or postgres) via the ProjectLimiter interface — see hardening
// sub-item 5 for the distributed-state path.
func WithRateLimiter(l ratelimit.ProjectLimiter) ServerOption {
	return func(s *Server) {
		s.rateLimiter = l
	}
}

// WithBudgetNotifier wires a sink that receives soft- and hard-cap
// alerts when POST /tasks trips the project budget. Optional — nil
// skips the alert; the log line and the 429 response are unchanged.
func WithBudgetNotifier(n budget.Notifier) ServerOption {
	return func(s *Server) {
		s.budgetNotifier = n
	}
}

// FillNotifier receives a per-fill notification call from the
// trading-fills ingest handler. Best-effort, async — the
// implementation must not block the ingest critical path.
type FillNotifier interface {
	NotifyFill(ctx context.Context, fill *persistence.TradingFill)
}

// WithFillNotifier wires the per-fill notification sink (typically
// the Telegram bot). Optional — without it fills are persisted
// silently.
func WithFillNotifier(n FillNotifier) ServerOption {
	return func(s *Server) {
		s.fillNotifier = n
	}
}

// WithSecrets wires the secret-leak detector and per-checkpoint
// action map into the API surface. Currently only the webhook
// ingest handler uses it (Phase 2 Block-mode enforcement); other
// API surfaces inherit protection through the executor + memory +
// artifact paths the daemon already wires elsewhere.
func WithSecrets(d secrets.Detector, actions map[string]secrets.Action) ServerOption {
	return func(s *Server) {
		s.secretsDetector = d
		s.secretsActions = actions
	}
}

// WithQueue sets the queue manager.
func WithQueue(q *queue.Queue) ServerOption {
	return func(s *Server) {
		s.queue = q
	}
}

// WithTaskCreator wires the shared task-creation core. When set,
// CreateTask delegates the persist / enqueue / rate-limit /
// budget pipeline to it instead of running the inline copy.
// Production wires this so the REST API and the /ui/.../tasks/new
// form go through the same code path; tests that don't supply a
// Creator continue exercising the inline legacy path so existing
// fixtures keep working unchanged.
func WithTaskCreator(c *taskcreate.Creator) ServerOption {
	return func(s *Server) {
		s.taskCreator = c
	}
}

// WithProjectRegistry sets the project registry.
func WithProjectRegistry(r *registry.Registry) ServerOption {
	return func(s *Server) {
		s.projectRegistry = r
	}
}

// WithFeatureTradingProbe sets the trading time-series validator backing the
// "trading-series" feature-doctor check. Optional; nil → the check skips.
func WithFeatureTradingProbe(p featuredoctor.TradingSeriesProbe) ServerOption {
	return func(s *Server) {
		s.featureTradingProbe = p
	}
}

// WithProjectTemplates sets the project-template catalog. Optional —
// when nil, the template-list and create-from-template endpoints
// return 503. Added 2026.6.0 for the SaaS-readiness project gallery.
func WithProjectTemplates(c *templates.Catalog) ServerOption {
	return func(s *Server) {
		s.projectTemplates = c
	}
}

// WithConfigsDir sets the daemon's configs/ root. Templates write
// new project YAML into this directory; without it the template
// gallery refuses to materialise. Set by the service container at
// startup from VORNIK_CONFIGS_DIR / Config.Storage.ConfigsPath.
func WithConfigsDir(dir string) ServerOption {
	return func(s *Server) {
		s.configsDir = dir
	}
}

// WithExecutor sets the executor.
func WithExecutor(e ExecutorInterface) ServerOption {
	return func(s *Server) {
		s.executor = e
		if src, ok := e.(TaskLogSource); ok {
			s.taskLogSource = src
		}
	}
}

// WithTaskLogSource sets the source used by task log endpoints.
func WithTaskLogSource(src TaskLogSource) ServerOption {
	return func(s *Server) {
		s.taskLogSource = src
	}
}

// WithConfig sets the configuration.
func WithConfig(cfg *config.Config) ServerOption {
	return func(s *Server) {
		s.config = cfg
	}
}

// WithMetricsRegistry sets the Prometheus registry for the /metrics endpoint.
func WithMetricsRegistry(reg *prometheus.Registry) ServerOption {
	return func(s *Server) {
		s.metricsRegistry = reg
	}
}

// WithMemorySearcher sets the memory searcher for the /memory/search endpoint.
func WithMemorySearcher(ms MemorySearcher) ServerOption {
	return func(s *Server) {
		s.memorySearcher = ms
	}
}

// WithMemoryCompanionAdapter wires the companion-side RAG adapter that
// backs the `recall` / `remember` MCP tools (LLD 22). Nil-safe — when
// unset, both tools return a clean "not wired" error to the caller
// rather than panicking.
func WithMemoryCompanionAdapter(a MemoryCompanionAdapter) ServerOption {
	return func(s *Server) {
		s.memoryCompanion = a
	}
}

// WithToolAuditRepository wires the per-tool-call audit repo so
// the realtime streaming endpoint at POST /api/v1/internal/tool-audit
// can persist rows as agents call tools. Required for production
// deployments where audit completeness matters.
func WithToolAuditRepository(repo persistence.ToolAuditRepository) ServerOption {
	return func(s *Server) {
		s.toolAuditRepo = repo
	}
}

// WithTradingOrderRepository wires the trading-order audit repo
// so POST /api/v1/internal/trading-orders can persist rows the
// broker MCP's AuditWriter posts. Required for the broker→daemon
// audit channel; without it the broker side keeps the rows in
// its journal indefinitely.
func WithTradingOrderRepository(repo persistence.TradingOrderRepository) ServerOption {
	return func(s *Server) {
		s.tradingOrderRepo = repo
	}
}

// WithTradingAuthVerifier wires the HMAC verifier guarding the
// /api/v1/internal/trading-* endpoints. nil (the default) leaves the
// feature off — the endpoints keep bearer-only auth. When non-nil the
// handlers reject any request without a valid, fresh, unreplayed
// signature (fail-closed).
func WithTradingAuthVerifier(v *tradingauth.Verifier) ServerOption {
	return func(s *Server) {
		s.tradingAuthVerifier = v
	}
}

// WithTradingRateLimiter wires the per-project trading-order rate
// limiter (POST /api/v1/internal/trading-orders). nil disables the
// trading-specific cap. The caps themselves come from each project's
// TradingRateLimit block; a project with zero caps is unlimited.
func WithTradingRateLimiter(l *ratelimit.Limiter) ServerOption {
	return func(s *Server) {
		s.tradingRateLimiter = l
	}
}

// WithTradingSafetyEventRepository wires the safety-event audit
// repo for the broker→daemon channel's Phase 2 stream (kill-
// switch toggles, breaker trips, cap refusals, replay hits).
// Same trust + idempotency contract as tradingOrderRepo.
func WithTradingSafetyEventRepository(repo persistence.TradingSafetyEventRepository) ServerOption {
	return func(s *Server) {
		s.tradingSafetyRepo = repo
	}
}

// WithTradingFillRepository wires the fill-ingestion repo for
// the broker→daemon channel's Phase 3 stream. Required for the
// poll loop to land fills in trading_fills; without it the
// broker side returns 503 and keeps fill rows in its journal.
func WithTradingFillRepository(repo persistence.TradingFillRepository) ServerOption {
	return func(s *Server) {
		s.tradingFillRepo = repo
	}
}

// WithTradingPositionsSnapshotRepository wires the equity-snapshot
// history so GetTradingStateReplay can return the broker's HWM +
// daily-loss baseline for restart recovery (audit T5). Optional;
// nil omits breaker-state recovery from the replay response.
func WithTradingPositionsSnapshotRepository(repo persistence.TradingPositionsSnapshotRepository) ServerOption {
	return func(s *Server) {
		s.tradingPositionsRepo = repo
	}
}

// WithMemoryAuditRepository wires the per-search retrieval audit
// repo so GET /api/v1/projects/{p}/memory/feedback can render
// chunk-utility analytics. Optional — without it the endpoint
// returns 503 MEMORY_AUDIT_NOT_CONFIGURED.
func WithMemoryAuditRepository(repo persistence.MemoryRetrievalAuditRepository) ServerOption {
	return func(s *Server) {
		s.memoryAuditRepo = repo
	}
}

// WithMemoryQuarantine wires the quarantine repo for the Phase 2
// memory-hardening endpoints (list / drop / release).
func WithMemoryQuarantine(repo persistence.MemoryQuarantineRepository) ServerOption {
	return func(s *Server) {
		s.memoryQuarantine = repo
	}
}

// WithCorpusEpochs wires the epoch repo for Phase 3 endpoints
// (epochs / rollback / rollbacks history).
func WithCorpusEpochs(repo persistence.CorpusEpochRepository) ServerOption {
	return func(s *Server) {
		s.corpusEpochs = repo
	}
}

// WithIngestQueueRepo wires the ingest queue repo for the
// memory-health endpoint (queue depth gauge).
func WithIngestQueueRepo(repo persistence.IngestQueueRepository) ServerOption {
	return func(s *Server) {
		s.ingestQueue = repo
	}
}

// WithMemoryStats wires the provider behind GET /api/v1/memory/stats. A
// nil provider leaves the endpoint off (503 MEMORY_DISABLED on call).
func WithMemoryStats(sp MemoryStatsProvider) ServerOption {
	return func(s *Server) {
		s.memoryStats = sp
	}
}

// WithMemoryTitleBackfiller wires the driver behind POST
// /api/v1/memory/backfill-titles. nil makes the endpoint return 503.
func WithMemoryTitleBackfiller(b MemoryTitleBackfiller) ServerOption {
	return func(s *Server) {
		s.memoryTitleBackfiller = b
	}
}

// WithMemoryClassifyBackfiller wires the driver behind POST
// /api/v1/memory/reclassify-llm. nil makes the endpoint return 503.
func WithMemoryClassifyBackfiller(b MemoryClassifyBackfiller) ServerOption {
	return func(s *Server) {
		s.memoryClassifyBackfiller = b
	}
}

// WithMemoryGraphReflagger wires the driver behind POST
// /api/v1/memory/regraph. nil makes the endpoint return 503.
// Production wires the postgres ChunkGraphExtractionRepository.
func WithMemoryGraphReflagger(r MemoryGraphReflagger) ServerOption {
	return func(s *Server) {
		s.memoryGraphReflagger = r
	}
}

// WithWorkflowTelemetry wires the driver behind GET
// /api/v1/admin/workflow-stats. nil makes the endpoint return
// 503. Production wires *workflowtelemetry.Service.
func WithWorkflowTelemetry(t WorkflowTelemetry) ServerOption {
	return func(s *Server) {
		s.workflowTelemetry = t
	}
}

// WithWorkflowArchitect wires the driver behind POST
// /api/v1/admin/workflow-architect/propose. nil makes the
// endpoint return 503. Production wires *memetic.Architect via
// a service-layer adapter.
func WithWorkflowArchitect(a WorkflowArchitect) ServerOption {
	return func(s *Server) {
		s.workflowArchitect = a
	}
}

// WithWorkflowProposals wires the persistence repo behind the
// admin workflow-proposal review endpoints (list / show / decide).
// nil makes the endpoints return 503.
func WithWorkflowProposals(repo persistence.WorkflowProposalRepository) ServerOption {
	return func(s *Server) {
		s.workflowProposals = repo
	}
}

// WithWorkflowApplier wires the driver behind POST
// /api/v1/admin/workflow-proposals/{id}/apply. nil makes the
// endpoint return 503. Production wires *memetic.Applier.
func WithWorkflowApplier(a WorkflowApplier) ServerOption {
	return func(s *Server) {
		s.workflowApplier = a
	}
}

// WithConfigReloadHook wires a synchronous config-reload trigger used
// after create-from-template writes, so the new project is registered
// in-memory before the client navigates to /ui/projects/{id}. Nil-safe:
// without it the create still works and the file-watcher picks the
// project up asynchronously.
func WithConfigReloadHook(reload func() error) ServerOption {
	return func(s *Server) {
		s.reloadHook = reload
	}
}

// WithWorkflowRollbacker wires the driver behind POST
// /api/v1/admin/workflow-proposals/{id}/rollback. nil makes the
// endpoint return 503. Production wires *memetic.Rollbacker.
func WithWorkflowRollbacker(r WorkflowRollbacker) ServerOption {
	return func(s *Server) {
		s.workflowRollbacker = r
	}
}

// WithWorkflowRejectionRecorder wires the Consumer B write-back that
// records rejected proposals as 'architect-reject' contradiction
// instincts. nil (gate off / not wired) leaves the reject path
// behaving exactly as before. Best-effort: a recorder error never
// fails the operator's rejection.
func WithWorkflowRejectionRecorder(r WorkflowRejectionRecorder) ServerOption {
	return func(s *Server) {
		s.proposalRejectionRecorder = r
	}
}

// WithMemoryCacheStats wires the provider behind GET
// /api/v1/memory/cache-stats. nil leaves the endpoint returning
// 503 so the CLI prints "cache stats not available on this
// deployment".
func WithMemoryCacheStats(p MemoryCacheStatsProvider) ServerOption {
	return func(s *Server) {
		s.memoryCacheStats = p
	}
}

// WithChatProvider wires the dispatcher's chat provider into the API
// server so the internal chat-completions proxy can forward agent
// requests through it. When nil, POST /api/v1/chat/completions returns
// 503 CHAT_NOT_CONFIGURED.
func WithChatProvider(p chat.Provider) ServerOption {
	return func(s *Server) {
		s.chatProvider = p
	}
}

// WithExplainRenderer wires the deterministic explain renderer used
// by POST /api/v1/projects/{p}/tasks/{id}/explain. Optional — nil
// makes the endpoint return 503 EXPLAIN_NOT_CONFIGURED.
func WithExplainRenderer(r *postmortem.Renderer) ServerOption {
	return func(s *Server) {
		s.explainRenderer = r
	}
}

// WithPricingPath wires configs/pricing.yaml so GET /api/v1/models can
// crosswalk discovered models against the static pricing table. The
// doctor handler also reads from a path; passing the same value here
// keeps both surfaces in sync without a shared loader.
func WithPricingPath(path string) ServerOption {
	return func(s *Server) {
		s.pricingPath = path
	}
}

// WithPromptCacheMode sets the daemon-wide default prompt-cache
// mode the chat-completions proxy stamps onto inbound requests
// that don't carry an explicit CacheStrategy. Empty / "off"
// preserves the slice-0 behaviour (no annotations).
func WithPromptCacheMode(mode string) ServerOption {
	return func(s *Server) {
		s.promptCacheMode = mode
	}
}

// ChatContextTierMetrics is the narrow contract the chat-proxy uses
// to bump the context-tier counter + headroom histogram without
// importing dispatcher (which would create a cycle: dispatcher
// already imports api for the audit repo shim in test rigs). The
// *dispatcher.Metrics satisfies this; the api package only needs the
// two record calls.
type ChatContextTierMetrics interface {
	// ObserveContextTier records one (project, tier, headroom%) tuple.
	ObserveContextTier(project string, tier chat.ContextTier, headroomPct float64)
}

// ChatCacheMetrics is the narrow contract the chat-proxy + internal
// llm-usage handler use to surface prompt-cache token usage on
// Prometheus (audit N8). *chat.Metrics satisfies it. Kept as an
// interface so the api package doesn't take a hard dependency on a
// constructed chat.Metrics — nil disables the surface.
type ChatCacheMetrics interface {
	// ObserveCacheUsage records one recorded usage row's cache tokens,
	// labelled by model/role/source, plus the USD saved.
	ObserveCacheUsage(model, role, source string, creationTokens, readTokens int64, dollarsSaved float64)
}

// WithChatCacheMetrics wires the chat prompt-cache observability
// surface so every recorded LLM-usage row (external-API proxy + the
// internal workflow-step report) bumps the cache token / ratio /
// dollars-saved series. Nil-safe.
func WithChatCacheMetrics(m ChatCacheMetrics) ServerOption {
	return func(s *Server) {
		s.chatCacheMetrics = m
	}
}

// WithChatContextBudget sets the token-window budget against which
// the chat-proxy computes the per-request context tier. Zero (the
// default) disables the tier surface — no X-Vornik-Context-Tier
// header, no metric emission. In production, operators pin this to
// the deployment's effective context window (200_000 for most Claude
// Sonnet/Opus configs; 128_000 for GPT-4-class). Per-model overrides
// belong in a future iteration.
func WithChatContextBudget(tokens int) ServerOption {
	return func(s *Server) {
		if tokens > 0 {
			s.chatContextBudget = tokens
		}
	}
}

// WithChatContextTierMetrics wires the dispatcher's per-turn tier
// counter into the chat-proxy so external-API traffic shares the
// timeseries with dispatcher (Telegram / UI) traffic. Nil-safe: when
// unset the chat-proxy still emits the X-Vornik-Context-Tier header,
// just without metric observation.
func WithChatContextTierMetrics(m ChatContextTierMetrics) ServerOption {
	return func(s *Server) {
		s.chatDispatcherMetrics = m
	}
}

// WithExternalAPIBillingProjectID pins the project ID used to
// attribute cost rows from the third-party OpenAI- and Ollama-
// compatible proxy surfaces when the caller doesn't supply
// X-Vornik-Project-ID. Without this option set the chat-proxy
// records under the literal "_external" so the cost shows up on
// the spend dashboard but isn't mixed with any real project.
// Operators with a dedicated billing project for SaaS usage pin
// it here so the cost rolls up onto that project's panel.
func WithExternalAPIBillingProjectID(projectID string) ServerOption {
	return func(s *Server) {
		s.externalAPIBillingProjectID = projectID
	}
}

// WithMCPExecutor sets the MCP executor used by the /projects/{id}/mcp/*
// endpoints. Agent containers call these endpoints via mcp-bridge's HTTP
// mode; with this set, agents see the same per-project MCP tools the
// daemon does without having to spawn their own subprocesses.
func WithMCPExecutor(m MCPExecutor) ServerOption {
	return func(s *Server) {
		s.mcpExecutor = m
	}
}

// WithMCPRegistry wires the daemon-level MCP discovery source that
// powers GET /api/v1/mcp/servers. Nil leaves the endpoint working
// in "empty catalog" mode — useful for deployments with no daemon-
// level mcp block declared. Operators MUST also wire the same
// source into the UI server so /ui/mcp renders the same rows.
func WithMCPRegistry(r MCPRegistrySource) ServerOption {
	return func(s *Server) {
		s.mcpRegistry = r
	}
}

// NewServer creates a new API server.
func NewServer(opts ...ServerOption) *Server {
	s := &Server{
		logger: zerolog.Nop(),
	}
	for _, opt := range opts {
		opt(s)
	}
	// Never carry a nil workspace lock — the git-over-HTTPS handler
	// (Task 2.4) dereferences it. The container injects the shared
	// instance via WithWorkspaceLock.
	if s.workspaceLock == nil {
		s.workspaceLock = workspacelock.New()
	}
	return s
}

// Routes returns the HTTP handler with all routes configured.
func (s *Server) Routes() http.Handler {
	return SetupRoutes(s, s.config)
}

// APIMetrics exposes the registered counter set so the service
// container can wire it into the doctor handlers AFTER
// Routes() has run (Routes constructs the APIMetrics). Returns
// nil when no metrics registry was wired.
func (s *Server) APIMetrics() *APIMetrics {
	return s.apiMetrics
}

// WithWebhookRelay injects the DMZ→job-tier relay forwarder. When set,
// IngestWebhook forwards verified events instead of enqueuing locally.
// The HMAC signature is always verified before forwarding — a bad
// signature still returns 401 and never reaches the forwarder.
func WithWebhookRelay(f WebhookForwarder) ServerOption {
	return func(s *Server) { s.webhookRelay = f }
}

// ForgeClassifier turns a verified webhook body into the provider-neutral
// forge_job JSON for a project (deterministic, no LLM). ok=false means the event
// isn't a forge action or the project has no forge configured — the task is then
// created without a forge_job (back-compat). Body-based so it works on the
// relay-forwarded path where provider headers may be absent.
type ForgeClassifier interface {
	// Returns the marshaled forge_job, a clean human-readable task prompt built
	// from the issue/CR title+body (so the agent gets a spec, not raw webhook
	// JSON), and ok=false for non-forge events.
	ClassifyWebhook(ctx context.Context, projectID string, body []byte) (forgeJob json.RawMessage, prompt string, ok bool)
}

// WithForgeClassifier wires the forge-job classifier used by the generic webhook
// path. Job-tier only (the DMZ relay node forwards before task creation).
func WithForgeClassifier(c ForgeClassifier) ServerOption {
	return func(s *Server) { s.forgeClassifier = c }
}

// SetWebhookRelay is the test-friendly setter mirroring WithWebhookRelay.
// Production code should use WithWebhookRelay (a ServerOption) instead.
func (s *Server) SetWebhookRelay(f WebhookForwarder) { s.webhookRelay = f }

// WireDoctorAPIMetrics is the post-Routes hand-off that lets
// the cost-attribution doctor check read live counter values.
// Called by the service container right after
// `mux.Handle("/", apiServer.Routes())` so the singleton
// DoctorHandlers picks up the registered metric set.
func WireDoctorAPIMetrics(m *APIMetrics) {
	if doctorHandlers == nil {
		return
	}
	doctorHandlers.SetAPIMetrics(m)
}
