// Package service provides the dependency injection container and lifecycle
// management for vornik.
//
// This is the composition root where all services are wired together.
//
// ## Initialization Order
//
// Dependencies initialize in this order to satisfy all requirements
// (matches NewContainer in this file):
//
//  1. Logger                    structured JSON logging
//  2. Database                  Postgres connection pool + migrations
//  3. Scheduler                 task leasing + executor + runtime manager
//     (Executor and Runtime are constructed inside
//     initScheduler because the scheduler needs them
//     wired before its first lease)
//  4. Watchdog                  stuck-execution scanner
//  5. EffectiveCostMonitor      effective-cost drift detector
//  6. Registry                  project / swarm / workflow definitions
//  7. Chat client               LLM provider (router / HTTP / claude-cli / codex-cli)
//  8. MCP manager               per-project MCP server connections
//  9. Telegram bot              + completion notifier, secrets detector,
//     circuit breaker (all attached here)
//  10. Autonomy manager          per-project autonomous task loops
//  11. Retention sweeper         background pruning of historical rows
//  12. HTTP server               health, API, UI, metrics — last because it
//     depends on every prior component
//
// ## Shutdown Order
//
// Shutdown happens in roughly reverse order, with care to drain
// in-flight work before any required dependency closes:
//
//  1. Stop state collectors
//  2. Observability shutdown (metrics server, tracing)
//  3. HTTP server shutdown (stop accepting new requests)
//  4. Executor shutdown (graceful pause of every active execution)
//  5. Scheduler stop (finish current leasing cycle)
//  6. Watchdog stop
//  7. EffectiveCostMonitor stop
//  8. ConfigReloader stop
//  9. Telegram bot stop
//  10. Autonomy manager stop
//  11. Retention sweeper cancel + wait for current iteration
//  12. MCP manager close (all per-project clients)
//  13. Memory manager stop
//  14. Warm container pool stop
//  15. Database close — last so any of the above shutdowns can still
//     write final-state rows (executor RecordCompletion, scheduler
//     state snapshots, etc.) before the connection goes away.
package service

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	editionpkg "vornik.io/vornik/internal/version"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/autonomy"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/httpx/realip"
	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memory/graph"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/projectarchive"
	"vornik.io/vornik/internal/projectwizard"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/reminders"
	"vornik.io/vornik/internal/replay"
	"vornik.io/vornik/internal/runtime"
	"vornik.io/vornik/internal/scheduler"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/slack"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/telegram"
	"vornik.io/vornik/internal/voice"
	"vornik.io/vornik/internal/watchdog"
	"vornik.io/vornik/internal/workspacelock"
)

// Container holds all initialized services and dependencies.
type Container struct {
	Config         *config.Config
	ConfigPath     string
	ConfigReloader *config.ConfigReloader
	// stagedConfig holds a freshly re-parsed config.yaml between the reload
	// loader (parse+validate) and activator (apply hot-reloadable keys) phases.
	// Single-threaded within one reload pass (the ConfigReloader serialises
	// loader→validator→activator), so no lock is needed.
	stagedConfig *config.Config
	Logger       zerolog.Logger
	// DB is the raw *sql.DB used for backend-agnostic call sites
	// (state collectors, retention sweeper, memory.New, doctor
	// handlers). backend owns the lifecycle + driver-specific
	// helpers (Close / Migrate / IsReady / PG pointer for the
	// pg_stat_user_tables query that has no SQLite analog).
	DB                  *sql.DB
	backend             *storage.Backend
	Registry            *registry.Registry
	Scheduler           *scheduler.Scheduler
	externalWaitMonitor *scheduler.ExternalWaitMonitor // Phase 30
	Executor            *executor.Executor
	// WorkspaceLock is the single process-wide per-project workspace
	// lock. Built once in NewContainer before initScheduler and injected
	// (the SAME instance) into the executor, the UI server, and the API
	// server so every workspace writer on this node is mutually exclusive
	// per project ("lock-on-mutation"). Single-node v1; the multi-node
	// cross-node gate (pg advisory lock) wraps this later — design §4.7.
	WorkspaceLock    *workspacelock.Locker
	Watchdog         *watchdog.Watchdog
	EffectiveCostMon *budget.EffectiveCostMonitor
	HTTPServer       *http.Server
	// relayServer is the additional mTLS listener for the relay-ingress
	// endpoint (POST /internal/v1/webhook-relay). Nil when the node does
	// not run workers or relay_ingress is not configured (Slice B).
	relayServer *http.Server
	// webhookRelayClient is the DMZ→job-tier mTLS relay client. Constructed
	// during initHTTPServer on RelayMode nodes; nil on all other profiles.
	// Task 8 reads this to register the relay_upstream readiness probe.
	//
	// Phase 2c: retyped from *webhookrelay.Client to the neutral
	// contracts.WebhookRelayClient seam — the webhookrelay package moved to
	// internal/enterprise/clustering and internal/service must not name it. The
	// EE clustering provider type-asserts this value back to the concrete client
	// for the webhook-heartbeat subsystem.
	webhookRelayClient contracts.WebhookRelayClient
	// apiServer is the inner data-plane handler that owns the
	// trading ingest endpoints. Stored so wireComponentMetrics
	// can attach a Prometheus sink after observability is up.
	apiServer     *api.Server
	Observability *observability.Observability
	// logshipForwarder is the centralised log-forwarding seam
	// (contracts.LogForwarder over internal/enterprise/logship). Non-nil only
	// in an Enterprise build with logging.forward.enabled — Community wires no
	// LogForwarderFactory, so this stays nil and the root logger is left
	// untouched (zero overhead). Built in initLogship (before the
	// scheduler/dispatcher capture the audit repos), its metrics attached
	// after observability, and drained on shutdown. See
	// https://docs.vornik.io and
	// editions-phase2c-gated-feature-relocation-design.md (Phase A).
	logshipForwarder contracts.LogForwarder
	// ChatClient holds whichever chat provider is configured. Interface
	// rather than a concrete *chat.Client so the subprocess-backed
	// CLI provider (internal/chat/cli_client.go) can be plugged in
	// alongside the HTTP one.
	ChatClient chat.Provider
	// Dispatcher is the daemon's shared LLM tool-calling loop, used
	// by every inbound conversation channel (Telegram, GitHub App,
	// future Slack / email). Constructed by initDispatcher after the
	// Telegram bot is set up (so its callbacks can wire) and before
	// Bot.Start fires (so the ConversationChannel receiver is bound
	// before the poll loop hands inbound to HandleMessage). Nil when
	// chat is disabled.
	Dispatcher  *dispatcher.Agent
	TelegramBot *telegram.Bot
	// GitHubChannel is the GitHub App conversation channel
	// constructed from per-project github_app config. Nil when no
	// project has the block configured. See
	// https://docs.vornik.io
	GitHubChannel *github.Channel
	// GitHubProject is the project the GitHub App channel is
	// pinned to in single-installation mode. Nil when the channel
	// runs with multiple installations — GitHubProjects is the
	// authoritative set in that mode. Kept around for back-compat
	// with single-tenant deployments that wired this directly.
	GitHubProject *registry.Project
	// GitHubProjects lists every project the GitHub App channel
	// routes to. Single-installation deployments have one entry
	// (mirroring GitHubProject); multi-installation deployments
	// have one per enabled project. The service container wires a
	// per-project DispatcherReceiver + session-store project
	// resolver so each project's @vornik reply path runs inside
	// its own project scope.
	GitHubProjects []*registry.Project
	// EmailChannels holds one ConversationChannel per project that
	// declared a fully configured `email` block. Slice 3 lifted the
	// slice-1/2 "first project wins" posture: every enabled project
	// now gets its own IMAP session and dispatcher receiver, so
	// inbound mail stays scoped to the project it landed at.
	//
	// EmailChannels[i] is paired with EmailProjects[i] (same index)
	// so the lifecycle code can wire each channel against its own
	// project-pinned session store.
	EmailChannels []*email.Channel
	// EmailProjects holds the projects EmailChannels are pinned to,
	// in matching order. Empty iff EmailChannels is empty.
	EmailProjects []*registry.Project
	// SlackChannels holds one ConversationChannel per project that
	// declared a fully configured `slack` block. Mirrors EmailChannels'
	// per-project routing: each channel is pinned to its own workspace
	// team_id, has its own bot token, and gets its own dispatcher
	// receiver so inbound from one workspace can't accidentally
	// invoke create_task against another project.
	//
	// SlackChannels[i] is paired with SlackProjects[i] (same index)
	// so the lifecycle code can wire each channel against its own
	// project-pinned session store.
	SlackChannels []*slack.Channel
	// SlackProjects holds the projects SlackChannels are pinned to,
	// in matching order. Empty iff SlackChannels is empty.
	SlackProjects []*registry.Project
	dbMetrics     *persistence.DBMetrics
	// repos holds the daemon's repository surface, built from the
	// configured database backend by internal/storage. Constructed
	// in initDatabase and rebuilt after observability initializes so
	// repos pick up the metrics-wrapped DBTX.
	repos           *storage.Repositories
	runtimeManager  *runtime.Manager
	warmPool        *runtime.WarmPool
	autonomyManager *autonomy.Manager
	mcpManager      *mcp.Manager
	// mcpRegistry caches the daemon-level MCP server catalog
	// declared by config.MCP.Servers. Distinct from mcpManager
	// (which holds per-project active clients used by agents) —
	// this is the discovery surface only, populated at startup
	// and exposed via /api/v1/mcp/servers + /ui/mcp. nil when
	// the operator hasn't declared a daemon-level mcp block.
	mcpRegistry    *mcp.Registry
	stateCollector *observability.StateCollector
	stopCollectors context.CancelFunc
	collectorsCtx  context.Context
	memoryManager  *memory.Manager
	memoryTitler   *memory.Titler
	// memoryConsolidateWorker runs the periodic LLM-free gist pass
	// over every enabled project. Pure-Go; the cadence is bounded
	// by Memory.ConsolidateIntervalSeconds (default 10 min). nil
	// when memory itself is disabled.
	memoryConsolidateWorker *memory.ConsolidateWorker

	// memoryLLMConsolidateWorker is the opt-in LLM-tier pass that
	// writes a short natural-language summary into
	// project_gists.narrative on top of the term-frequency cloud.
	// Cadence bounded by Memory.LLMConsolidateIntervalSeconds
	// (default 1h). nil when LLMConsolidateEnabled is false or
	// ChatClient is unavailable.
	memoryLLMConsolidateWorker *memory.LLMConsolidateWorker

	// memoryClassifyBackfiller drives the LLM-based content-class
	// backfill triggered by vornikctl memory reclassify --use-llm.
	// nil when memory.classifier.enabled is false.
	memoryClassifyBackfiller *memory.ClassifyBackfiller

	// memoryTitleBackfiller drives the one-shot LLM title backfill
	// for the operator vector-cloud UI. Wired only when the titler
	// is enabled; the API handler returns 503 otherwise.
	memoryTitleBackfiller *memory.TitleBackfiller
	// ingestWorker drains project_ingest_queue (Phase 1 memory
	// hardening). Wired only when the memory subsystem is enabled.
	// nil-safe — Start/Stop dispatch through helper guards.
	ingestWorker         *memory.IngestWorker
	ingestQueueRepo      persistence.IngestQueueRepository
	memoryQuarantineRepo persistence.MemoryQuarantineRepository
	// instinctMetricsWirer is a callback set by InstinctSubsystem.Build (via
	// SetInstinctMetricsWirer) so wireComponentMetrics can attach Prometheus
	// metrics to the EE instinct worker without the container holding a typed
	// reference to *instinct.Worker (an EE-domain type that moves to enterprise
	// in Task 6). Nil when instinct is disabled or not in the EE edition.
	instinctMetricsWirer func(*observability.InstinctMetrics)
	// instinctMetrics is the SINGLE shared vornik_instinct_* Prometheus sink
	// (TRACK ARCH-METRIC). observability.NewInstinctMetrics uses promauto and
	// PANICS on a duplicate registration against the same registry, so exactly
	// one instance must back the worker, executor, recovery resolver AND the
	// workflow architect. It is created lazily by sharedInstinctMetrics() on
	// first use — the architect adapter (built in the second initHTTPServer
	// pass, where observability already exists) needs it BEFORE
	// wireComponentMetrics runs, so both call sites resolve to the same cached
	// pointer. nil until first use, or when observability is disabled.
	instinctMetrics *observability.InstinctMetrics
	memoryPipeline  *memory.Pipeline
	// memoryMetrics is the shared Prometheus sink for the memory
	// subsystem. Held on the container so closures wired into the
	// executor (e.g. the ingest-enqueue fallback recorder) can read
	// it lazily — the executor is constructed before metrics are
	// registered, so an early-bound pointer would be nil.
	memoryMetrics *memory.Metrics
	// replayMetrics holds the vornik_replay_* Prometheus counters
	// (Phase C of the failure-forensics work). Wired alongside
	// the Forker so every fork-from-step attempt is counted by
	// outcome. nil-safe — the Forker treats a nil Metrics as
	// no-op.
	replayMetrics *replay.Metrics
	// projectWizardMetrics holds vornik_project_wizard_* counters
	// (Feature #2 Phase C). Wired into the Wizard at construction
	// so each Converse + Commit attempt ticks the matching outcome.
	projectWizardMetrics *projectwizard.Metrics
	// extractorMetrics holds the vornik_extractions_total /
	// _extraction_duration_seconds / _extracted_documents_total series
	// (document-extraction LLD Phase 7). Built at boot in
	// wireComponentMetrics so registration is deterministic, then handed
	// to the lazily-constructed extractor.Runner. nil-safe at the Runner.
	extractorMetrics *extractor.Metrics
	// livePub broadcasts per-execution events to the live-task
	// observation surface (Feature #3 Phase A). Single instance
	// per daemon; wired into the executor at construction and
	// later (Phase B) into the WebSocket handler.
	livePub         livepubsub.Publisher
	livePubShutdown func()
	// archiveSweeper deletes archived projects once their grace
	// window elapses (configs/projects/<id>.yaml, every project-
	// scoped DB row, and the artifact blob directory). Nil-safe
	// — when not wired the UI's archive button still works but
	// nothing auto-deletes until a future restart picks it up.
	archiveSweeper *projectarchive.Sweeper

	// reminderRunner drives the scheduled-reminders heartbeat
	// (2026.7.0 LLD). Polls dispatcher_reminders every 30s and
	// delivers due rows via the per-channel ConversationChannel
	// registry. Nil-safe — set_reminder tool surfaces a "not
	// configured" message when this isn't wired.
	reminderRunner *reminders.Runner

	// subsystems is the Subsystem-pattern registry. Populated by
	// registerSubsystems() near the end of New + iterated through
	// Build → Start → Stop by the run/drain loops. Each entry
	// owns one extracted feature's lifecycle. See subsystem.go.
	subsystems []Subsystem

	// archiveLifecycle is the shared archive/unarchive/delete-now
	// service the UI handlers + REST API + future vornikctl CLI
	// all funnel through. Single instance per daemon so YAML
	// mutations + reload semantics + audit shape stay consistent
	// across surfaces.
	archiveLifecycle *projectarchive.LifecycleService

	// archiveSweeperElector gates the archive sweeper on the
	// elected leader (2026.8.0 horizontal-scaling prep). Nil
	// when the leader-lock repo isn't wired (SQLite branch +
	// any deployment that hasn't migrated). Lifecycle Run
	// starts the renew goroutine; nil-safe.
	archiveSweeperElector *leaderelection.Elector
	// Per-worker electors for the rest of the singleton
	// background jobs (2026.8.0 horizontal-scaling). Each gets
	// its own worker_id row in daemon_leader_locks so leadership
	// can roll between replicas independently — losing the
	// autonomy lock doesn't drop the title-backfill lock.
	autonomyElector         *leaderelection.Elector
	titleBackfillElector    *leaderelection.Elector
	classifyBackfillElector *leaderelection.Elector
	consolidateElector      *leaderelection.Elector
	llmConsolidateElector   *leaderelection.Elector
	instinctElector         *leaderelection.Elector
	kgExtractElector        *leaderelection.Elector
	watchdogElector         *leaderelection.Elector
	// telegramPollerElector gates the long-poll loop so only
	// one replica calls Telegram getUpdates. Constructed when
	// LeaderLocks is wired AND TelegramBot is non-nil; otherwise
	// nil leaves the loop running on every replica (legacy
	// single-process behaviour).
	telegramPollerElector *leaderelection.Elector
	// Slice 1 of the horizontal-scaling design — four additional
	// singleton workers that previously ran on every replica.
	// Nil-safe: a missing elector reverts to legacy "run
	// everywhere" semantics for single-process deployments.
	retentionElector    *leaderelection.Elector
	externalWaitElector *leaderelection.Elector
	cpcTimeoutElector   *leaderelection.Elector
	remindersElector    *leaderelection.Elector
	// Slice 2 — periodic janitor for ratelimit_counters when the
	// postgres backend is selected. Idempotent sweeper; leader-
	// elected so multi-replica deployments only run one query per
	// cadence. nil when backend != "postgres".
	ratelimitCounterSweepElector *leaderelection.Elector
	// Slice C2 — the leader-gated cluster_nodes pruner + cluster monitor
	// electors moved to internal/enterprise/clustering in Phase 2c; those
	// subsystems now register their electors via RegisterExtraElector (folded
	// into extraElectors), so the former dedicated named fields are gone.
	rateLimiterRetention time.Duration

	// extraElectors is the catch-all slice for electors that
	// don't have a dedicated named field — the per-project email
	// IMAP electors minted inside EmailChannelsSubsystem.Start
	// (one per email-configured project) and, since Phase 2c, the
	// trading equity sampler + cross-checker electors registered
	// from the relocated internal/enterprise/trading subsystem via
	// RegisterExtraElector. allElectors() folds this slice into its
	// candidate list so releaseAllLeaderLeases() drains them
	// alongside the named electors. Guarded by extraElectorsMu
	// because subsystem Start runs sequentially with allElectors
	// today, but the write-back from Start is on a goroutine-
	// adjacent path; defensive synchronisation now avoids a
	// latent race if subsystems ever start in parallel.
	extraElectors   []*leaderelection.Elector
	extraElectorsMu sync.Mutex

	// healingObserver is the shared workflowhealing metrics observer (EE only).
	// Memoised via healingObserverOnce() so the trial runner and promoter
	// register against ONE observer — the EE factory (backed by *blackbox.Metrics)
	// panics on duplicate Prometheus registrations. Nil in Community and until
	// the EE factory is first called. CE callers nil-guard (workflowhealing
	// constructors are nil-safe).
	healingObserver contracts.HealingObserver

	// blackboxMetricsWirer is the deferred callback registered by
	// enterprise/blackbox.Subsystem.Build (SetBlackboxMetricsWirer). Mirrors the
	// instinctMetricsWirer pattern: Build runs before observability is up, so
	// the callback is invoked later by wireComponentMetrics with the live registry.
	blackboxMetricsWirer func(prometheus.Registerer)

	// memoryFirewallWriter is the non-blocking audit writer
	// for the Policy-Aware Memory Firewall. Wired during
	// scheduler init; the daemon's Run() launches its flusher
	// goroutine + the drain sequence calls Stop. Nil-safe.
	memoryFirewallWriter *memoryfirewall.AuditWriter

	// version holds the daemon build version string passed to Run,
	// threaded onto the Container so the cluster heartbeat (Task 4,
	// Slice C1) can populate cluster_nodes.version without reaching
	// into the global scope.
	version string

	// edition holds the build edition (community or enterprise) passed to
	// Run, threaded onto the Container so any subsystem can query it without
	// reaching into the global scope.
	edition string

	providers ProviderSet

	// adminCapabilityLogged is set to true after the first
	// initHTTPServer pass emits the capability="admin" log line.
	// initHTTPServer runs twice (pre-observability + post); the
	// guard ensures the log fires exactly once across both passes.
	adminCapabilityLogged bool

	// daemonHolderIDValue caches the holder identifier used by
	// every elector this daemon constructs. Computed lazily so
	// the import-time choice (hostname + pid + boot-uuid) lands
	// only after the logger is wired.
	daemonHolderIDValue string
	corpusEpochRepo     persistence.CorpusEpochRepository
	// graphWorker drains project_memory_chunks where
	// needs_graph_extraction = TRUE and runs the four-stage KG
	// extraction pipeline. Wired only when memory.graph.enabled.
	// nil-safe — startGraphWorker / stopGraphWorker guard.
	graphWorker  *graph.Worker
	pricingTable *pricing.Table
	// secretsDetector + secretsActions are kept on the container
	// so future bring-ups (Telegram replies, artifact uploads,
	// memory ingest) can attach themselves to the same instance
	// rather than rebuilding the regex set per consumer.
	secretsDetector secrets.Detector
	secretsActions  map[string]secrets.Action
	// judgeRunnerWired records whether buildJudgeRunner produced
	// a runner — set to true when the chat client was available
	// at executor build time. Used by the startup summary to
	// log "judge: enabled across N projects" once the registry
	// has loaded, so operators can confirm Phase 3 is actually
	// turned on without waiting for a task to terminate.
	judgeRunnerWired bool
	// judgeRunner is the constructed Phase 3 runner (or nil when
	// the chat client wasn't available). Stored so wireComponentMetrics
	// can attach a Prometheus sink after observability is initialised
	// without rebuilding the runner or the executor that holds it.
	judgeRunner          *hallucination.JudgeRunner
	rateLimiter          ratelimit.ProjectLimiter
	apiKeyLimiter        *ratelimit.APIKeyLimiter
	rateLimitMetrics     *ratelimit.Metrics
	dryRunMetrics        *api.DryRunMetrics
	chainMetrics         *api.AuthChainMetrics
	tradingSeriesMetrics *api.TradingSeriesMetrics
	equityCheckMetrics   *api.TradingEquityCheckMetrics
	// rateLimiterPostgres holds the postgres-backed limiter when
	// config selects backend=postgres. Kept on the container so
	// the periodic sweeper goroutine can call SweepExpired on
	// shutdown / interval ticks. nil when the in-process backend
	// is selected.
	rateLimiterPostgres *ratelimit.PostgresProjectLimiter
	// perIPLimiter is the unauthenticated data-plane backstop
	// (hardening sub-item 2). Wired by container_scheduler when
	// the daemon config carries a non-zero RateLimit.PerIP block.
	perIPLimiter *ratelimit.PerIPLimiter
	// realipMetrics counts spoof-attempt forwarding headers from
	// untrusted sources. Registered once in initHTTPServer; nil when
	// no observability registry is configured.
	realipMetrics *realip.Metrics
	artifactStore *artifacts.Store
	// artifactBackend is the FileBackend (filesystem or s3) the
	// Store delegates blob I/O to. Held on the Container so the
	// shutdown path can Close() it — S3 holds an HTTP client pool
	// that wants to drain cleanly.
	artifactBackend artifacts.FileBackend
	retentionCancel context.CancelFunc
	retentionDone   chan struct{}
	// voiceSTT / voiceTTS hold the local speech-to-text and text-to-
	// speech providers constructed from config.Voice. Shared across
	// channel adapters (Telegram, Slack) so a single Whisper or
	// Piper process pool serves every inbound. Either may be nil
	// when the corresponding sub-block is unset or its provider
	// name is not supported — the channel adapters are nil-safe.
	voiceSTT voice.STTProvider
	voiceTTS voice.TTSProvider

	// extractorPipeline holds the lazy-init document-extraction
	// surfaces (registry + runner). Accessed via ExtractorRegistry /
	// ExtractorRunner; see container_extractor.go.
	extractorPipeline extractorPipeline

	// testExecShutdown, when non-nil, is called in place of
	// c.Executor.Shutdown in the shutdown() sequence. Tests inject a
	// spy here to assert the ordering invariant: the executor must
	// quiesce BEFORE shutdownHTTPWithDeadline unlinks the agent socket.
	testExecShutdown executorShutdowner

	// testHTTPShutdown, when non-nil, is called in place of
	// shutdownHTTPWithDeadline(c.HTTPServer, …) in the shutdown()
	// sequence. Tests inject a spy here so the executor-before-HTTP
	// ordering invariant can be asserted without a live HTTP server.
	testHTTPShutdown httpShutdowner

	// testAgentSocketServe, when non-nil, is called in place of the
	// real serveAgentUnixSocket in Run(). Tests inject a spy here to
	// assert the startup ordering invariant: the agent unix socket must
	// be serving BEFORE c.Executor.Recover runs so resumed steps can
	// bind-mount the socket (incident restart-resume-on-boot socket
	// race, 2026-06-21).
	testAgentSocketServe func(errCh chan error) error

	// testExecutorRecover, when non-nil, is called in place of
	// c.Executor.Recover in the Run() startup sequence. Tests inject a
	// spy here to assert that the agent unix socket is already up before
	// executor recovery runs.
	testExecutorRecover executorRecoverer
}

// executorRecoverer is the subset of *executor.Executor that Run() needs for
// recovery. Extracted as an interface so tests can inject a spy without
// requiring a live database or runtime.
type executorRecoverer interface {
	Recover(ctx context.Context) error
}

// SetVersion stores the daemon build version on the container.
// Called by Run immediately after NewContainerWithObservability so
// the cluster heartbeat (Task 4, Slice C1) can read it without
// touching the package-level Run parameters.
func (c *Container) SetVersion(v string) { c.version = v }

// Version returns the daemon build version set by SetVersion.
func (c *Container) Version() string { return c.version }

func agentCallbackURL(address string) string {
	url := address
	if url == "" {
		url = "http://127.0.0.1:8080"
	}
	url = strings.ReplaceAll(url, "0.0.0.0", "host.containers.internal")
	if !strings.HasPrefix(url, "http") {
		url = "http://" + url
	}
	return url
}

// NewContainer creates and initializes all services in the correct order.
//
// Initialization order (see package docs for details):
// 1. Logger
// 2. Database
// 3. Health Checker
// 4. HTTP Server
func NewContainer(cfg *config.Config, configPath string, opts ...ContainerOption) (*Container, error) { //nolint:gocognit,funlen // pre-existing god-function; signature change only, no new complexity
	c := &Container{
		Config:     cfg,
		ConfigPath: configPath,
	}

	// Apply edition options immediately after struct allocation so c.providers
	// is set before every downstream init site (initLogship @617,
	// initScheduler @746, initHTTPServer @906, registerSubsystems @919).
	// applyOptions only sets c.providers (CommunityProviders default +
	// WithProviders override) and has no dependency on anything constructed
	// between here and line 918, so the move is safe. See design §3.
	c.applyOptions(opts)

	// Phase 1 Step 1: Initialize structured JSON logger
	c.initLogger()

	c.Logger.Info().
		Str("config_path", configPath).
		Str("server_address", cfg.Server.Address).
		Str("database_host", cfg.Database.Host).
		Int("database_port", cfg.Database.Port).
		Str("database_name", cfg.Database.Name).
		Msg("initializing vornik")

	// Phase 1 Step 2: Initialize database connection
	if err := c.initDatabase(); err != nil {
		c.Logger.Error().Err(err).Msg("failed to initialize database")
		return nil, fmt.Errorf("database initialization: %w", err)
	}
	c.Logger.Info().
		Str("host", cfg.Database.Host).
		Int("port", cfg.Database.Port).
		Str("database", cfg.Database.Name).
		Msg("database initialized")

	// Centralised log forwarding (logship). MUST run here — after the
	// audit repos exist (initDatabase) but BEFORE the scheduler/dispatcher
	// capture them — so the audit decorators reach those consumers. When
	// disabled this is a no-op and the root logger stays untouched. An
	// enabled-but-misconfigured sink fails boot (fail closed).
	if err := c.initLogship(); err != nil {
		c.Logger.Error().Err(err).Msg("failed to initialize log forwarding")
		return nil, fmt.Errorf("log forwarding initialization: %w", err)
	}

	// Health + readiness probes are served by the API layer
	// (api.Server.Healthz / Readyz). The Readyz handler does the
	// DB ping via taskRepo.Ping plus any options-registered
	// ReadinessCheck, so a separate container-side health checker
	// would just duplicate the logic in two places. Removed in
	// the 2026-05-03 audit cleanup.

	// Registry + chat client must be initialised BEFORE the
	// scheduler. initScheduler builds the executor (in the same
	// function, despite the name), and the executor wires the
	// Phase 3 judge runner via buildJudgeRunner — which reads
	// c.ChatClient — and the workflow resolver — which reads
	// c.Registry. Pre-2026-05-04 these were initialised AFTER
	// initScheduler, so the executor silently got a nil judge
	// runner AND a nil workflow resolver, which made
	// fireJudgeIfEnabled no-op for every terminal task. The
	// verdict panel stayed empty for the entire lifetime of the
	// feature until this reorder landed.
	if err := c.initRegistry(); err != nil {
		c.Logger.Warn().Err(err).Msg("registry initialization failed (continuing without project registry)")
	} else if c.Registry != nil {
		c.Logger.Info().Int("project_count", len(c.Registry.ListProjects())).Msg("project registry initialized")
		// Log per-role model configuration so operators can verify overrides.
		for _, swarm := range c.Registry.ListSwarms() {
			for _, role := range swarm.Roles {
				model := "(global)"
				if role.Model != "" {
					model = role.Model
				} else if role.Runtime.EnvVars["VORNIK_LLM_MODEL"] != "" {
					model = role.Runtime.EnvVars["VORNIK_LLM_MODEL"] + " (envVars)"
				}
				c.Logger.Info().
					Str("swarm", swarm.ID).
					Str("role", role.Name).
					Str("model", model).
					Msg("role model config")
			}
		}
	}

	// Load the pricing table BEFORE initChat (and BEFORE
	// executorOpts construction further down). Two downstream
	// consumers read c.pricingTable at the moment they're built:
	//   1. initChatRouter — assembles per-sub-provider model
	//      catalogs (Anthropic / OpenAI / Vertex / Bedrock-fallback)
	//      by filtering pricing.yaml entries. Loading after this
	//      meant every catalog was empty and providers without a
	//      live /v1/models endpoint surfaced as zero entries in
	//      `vornikctl models list`.
	//   2. buildJudgeRunner — wires the judge's own cost accounting
	//      so verdict rows carry a non-zero cost_usd alongside
	//      tokens. Pre-2026-05-04 this bug shipped silently —
	//      post_mortem cost recorded fine because its construction
	//      happens later in NewContainer; judge cost stayed zero.
	// Missing pricing file is non-fatal (executor records tokens
	// without dollar cost); malformed YAML is fatal — silent cost
	// miscounting is worse than a startup failure.
	pricingPath := resolvePricingPath(c.ConfigPath)
	pricingTable, err := pricing.Load(pricingPath)
	if err != nil {
		return nil, fmt.Errorf("load pricing table %q: %w", pricingPath, err)
	}
	pricingTable.SetWarnHook(func(model string) {
		c.Logger.Warn().Str("model", model).Msg("no pricing entry — using default rate; add to pricing.yaml")
	})
	c.pricingTable = pricingTable
	c.Logger.Info().Str("path", pricingPath).Msg("pricing table loaded")

	// Phase 1 Step 5 (moved earlier): Initialize chat client.
	// Must precede initScheduler so the executor's judge runner
	// gets a non-nil chat client.
	if err := c.initChat(); err != nil {
		c.Logger.Warn().Err(err).Msg("chat client initialization failed (continuing without chat)")
	} else if c.ChatClient != nil {
		c.Logger.Info().Msg("chat client initialized")
	}

	// Live observation publisher — Feature #3 Phase A. MUST be
	// constructed BEFORE initScheduler + initHTTPServer; both
	// consumers (executor + api server) capture c.livePub at
	// construction time, and a late init would silently leave
	// every live tap + WebSocket handler with a nil reference.
	// Pre-fix (2026-05-22 → 2026-05-23), this init lived inside
	// wireComponentMetrics which runs AFTER both initScheduler
	// and initHTTPServer — the api server returned 503
	// LIVE_DISABLED forever and executor taps no-op'd. Discovered
	// when the live page in the task UI silently stopped working.
	//
	// Sweeper interval = 10 minutes; idle-stream eviction bounds
	// memory on a daemon that sees millions of distinct executions.
	c.livePub, c.livePubShutdown = livepubsub.NewWithSweeper(0, 10*time.Minute,
		livepubsub.WithMetrics(livepubsub.NewMetrics(c.observabilityRegistry())))
	// Cross-replica fanout — wrap the in-process publisher with
	// the Postgres-backed layer when the LiveEvents repo is
	// wired AND the daemon is on Postgres. SQLite + single-
	// process deploys skip the wrap (their repo stub is a no-op).
	if c.repos != nil && c.repos.LiveEvents != nil && c.DB != nil && c.Config.Database.Driver == "postgres" {
		if wrapped, shutdown, err := c.wrapLivePublisherCrossReplica(context.Background()); err == nil && wrapped != nil {
			innerShutdown := c.livePubShutdown
			c.livePub = wrapped
			c.livePubShutdown = func() {
				if innerShutdown != nil {
					innerShutdown()
				}
				shutdown()
			}
			c.Logger.Info().Msg("livepubsub: cross-replica fanout enabled (DB persistence + Postgres NOTIFY/LISTEN)")
		} else if err != nil {
			c.Logger.Warn().Err(err).Msg("livepubsub: cross-replica wrap failed; staying with in-process publisher")
		}
	}

	// Build the single process-wide per-project workspace lock BEFORE
	// the executor (initScheduler) and the HTTP servers (initHTTPServer)
	// so the SAME instance is injected into all three. initHTTPServer
	// runs twice (pre/post observability); the lock lives on the
	// container so both passes reuse it.
	if c.WorkspaceLock == nil {
		c.WorkspaceLock = workspacelock.New()
	}

	// Phase 1 Step 4: Initialize scheduler (depends on task repository,
	// chat client for the judge runner, and registry for the workflow
	// resolver).
	if err := c.initScheduler(); err != nil {
		c.Logger.Error().Err(err).Msg("failed to initialize scheduler")
		return nil, fmt.Errorf("scheduler initialization: %w", err)
	}
	c.Logger.Info().Msg("scheduler initialized")

	// Stuck-execution watchdog. Independent of the scheduler — runs as
	// its own goroutine watching executions.updated_at. Construction
	// alone doesn't start the scan; Start() is called below alongside
	// the scheduler.
	if err := c.initWatchdog(); err != nil {
		c.Logger.Error().Err(err).Msg("failed to initialize watchdog")
		return nil, fmt.Errorf("watchdog initialization: %w", err)
	}
	c.Logger.Info().Msg("watchdog initialized")

	// Effective-cost drift monitor. Initialized here so the
	// Telegram bot wiring later (which the monitor uses as its
	// notifier) just attaches itself to the already-built monitor.
	if err := c.initEffectiveCostMonitor(); err != nil {
		c.Logger.Error().Err(err).Msg("failed to initialize effective-cost monitor")
		return nil, fmt.Errorf("effective-cost monitor initialization: %w", err)
	}
	c.Logger.Info().Msg("effective-cost monitor initialized")

	// Phase 1 Step 5b: Initialize MCP servers from project configs
	c.initMCP()
	// Daemon-level MCP discovery registry — populates /api/v1/mcp/servers
	// + /ui/mcp from the top-level mcp.servers block in the daemon
	// config. NOT the same wiring as the per-project clients above;
	// the registry is discovery-only and does not silently grant tool
	// access to any project.
	c.initMCPRegistry()

	// Voice providers — constructed BEFORE the Telegram bot and Slack
	// channels so both adapters can pick up the shared STT / TTS
	// instances when their config blocks are present. Empty
	// config.Voice leaves both fields nil and the channel adapters
	// stay on their pre-voice text-only paths.
	if err := c.initVoice(); err != nil {
		c.Logger.Error().Err(err).Msg("failed to initialize voice providers")
		return nil, fmt.Errorf("voice initialization: %w", err)
	}

	// Phase 1 Step 6: Initialize Telegram bot (optional, depends on chat client)
	if err := c.initTelegram(); err != nil {
		c.Logger.Warn().Err(err).Msg("telegram bot initialization failed (continuing without telegram)")
	} else if c.TelegramBot != nil {
		c.Logger.Info().Msg("telegram bot initialized")
		// Validate the dispatcher billing project exists when
		// configured. A typo would silently land all chat cost
		// on a non-existent project_id (the row writes succeed —
		// project_id has no FK to the registry); the check here
		// surfaces the mismatch at startup.
		if pid := c.Config.Telegram.DispatcherProjectID; pid != "" && c.Registry != nil {
			if c.Registry.GetProject(pid) == nil {
				c.Logger.Warn().
					Str("dispatcher_project_id", pid).
					Msg("telegram: dispatcher_project_id is set but no such project is loaded — dispatcher cost rows will be tagged with an unknown project_id; create the project or fix the typo")
			} else {
				c.Logger.Info().
					Str("dispatcher_project_id", pid).
					Msg("telegram: dispatcher cost will be billed to this project regardless of active chat project")
			}
		}
		// Wire bot as task completion notifier so Telegram users get
		// notified when tasks they created finish. The A2A push notifier
		// rides the same hook (POSTs terminal states to a caller's webhook).
		// The email subsystem rebuilds this multiplexer with its own channels
		// when email is configured; both paths include the A2A push.
		if c.Executor != nil {
			notifiers := []executor.CompletionNotifier{}
			if c.TelegramBot != nil {
				notifiers = append(notifiers, c.TelegramBot)
			}
			if p := c.a2aPushNotifier(); p != nil {
				notifiers = append(notifiers, p)
			}
			if multi := newMultiCompletionNotifier(notifiers...); multi != nil {
				c.Executor.SetCompletionNotifier(multi)
			}
		}
		// Phase-2 secrets backstop on the chat reply path: every
		// outbound bot message is scanned and redacted before
		// transmit. Last line of defence against a secret that
		// slipped past the result.json / container_logs / audit
		// checkpoints. Sharing the same detector instance keeps
		// the corpus aligned across all redaction layers.
		if c.secretsDetector != nil {
			c.TelegramBot.SetSecretsDetector(c.secretsDetector)
		}
		// Effective-cost drift used to fan out to Telegram via
		// SetNotifier(c.TelegramBot). Removed 2026-05-02 — drift
		// is now surfaced inline on /ui/spend (drift column on the
		// role/model leaderboard) and /ui/projects/{id} (per-project
		// drift panel). The monitor still runs and logs every drift
		// detection at warn level for forensic queries via journalctl,
		// but operators read the live state from the dashboards
		// rather than reacting to push notifications.
		// Per-project circuit breaker. Built here (rather than at
		// executor construction) because it depends on both the
		// notifier and the registry — the registry is available
		// earlier but the notifier is wired only when the bot is
		// up. Disabled deployments skip the construction entirely.
		if c.Executor != nil && c.Registry != nil && c.Config.Autonomy.CircuitBreaker.Enabled {
			cbCfg := c.Config.Autonomy.CircuitBreaker
			threshold := cbCfg.Threshold
			if threshold <= 0 {
				threshold = 5
			}
			window := 2 * time.Hour
			if cbCfg.Window != "" {
				if d, err := time.ParseDuration(cbCfg.Window); err == nil && d > 0 {
					window = d
				}
			}
			skip := cbCfg.SkipClasses
			if len(skip) == 0 {
				skip = []string{
					persistence.TaskFailureClassCancelled,
					persistence.TaskFailureClassBudgetBlocked,
					persistence.TaskFailureClassRateLimited,
				}
			}
			cb := executor.NewCircuitBreakerForExecutor(c.repos.Tasks, c.Registry, c.TelegramBot, threshold, window, skip,
				c.Logger.With().Str("component", "circuit_breaker").Logger())
			c.Executor.SetCircuitBreaker(cb)
			c.Logger.Info().
				Int("threshold", threshold).
				Dur("window", window).
				Strs("skip_classes", skip).
				Msg("circuit breaker enabled")
		}
	}

	// Slice E refactor (2026-05-17): construct the shared
	// dispatcher.Agent after the Telegram bot is wired (so bot
	// callbacks can attach) but before any channel starts. Every
	// inbound channel (Telegram, GitHub App, future Slack / email)
	// shares this one agent so retry budgets, intent verdicts, and
	// memory state stay coherent. Nil when chat is disabled.
	// Reminders heartbeat — initialised before the dispatcher so
	// the dispatcher's set_reminder tool gets a non-nil Kicker.
	// Lifecycle goroutine starts in Run() below.
	c.reminderRunner = c.initReminders()
	// Blackbox detector now lives on BlackboxSubsystem (see
	// subsystem_blackbox.go); construction happens in its Build
	// during registerSubsystems(). The imperative `c.blackboxDetector
	// = c.initBlackBoxDetector()` line previously here moved
	// inline with the subsystem.

	c.initDispatcher()

	// Phase 1 Step 6b: Initialize autonomous task manager (optional, depends on chat client + registry)
	c.initAutonomy()

	// Phase 1 Step 6c: Optional background retention sweeper.
	c.initRetention()

	// Phase 1 Step 7: Initialize HTTP server with health endpoints
	if err := c.initHTTPServer(); err != nil {
		c.Logger.Error().Err(err).Msg("failed to initialize HTTP server")
		return nil, fmt.Errorf("http server initialization: %w", err)
	}
	c.Logger.Info().Str("address", cfg.Server.Address).Msg("HTTP server initialized")

	// Phase 1 Step 8: register + build the extracted subsystems.
	// Replaces the imperative wiring of features that have been
	// pulled into Subsystem implementations. Subsystems Build
	// here (constructing their internal state) then Start in
	// Run() below + Stop in beginDrain. See subsystem.go for the
	// contract. (applyOptions was moved to immediately after struct
	// allocation so c.providers is available to every init site above.)
	c.registerSubsystems()
	c.buildSubsystems()

	return c, nil
}

// capabilities returns this node's resolved run-profile. Cheap; recomputed
// per call (the config is stable for the container's lifetime).
func (c *Container) capabilities() config.NodeCapabilities {
	if c.Config == nil {
		return config.ResolveNodeProfile(config.NodeConfig{}) // all-on fallback
	}
	return config.ResolveNodeProfile(c.Config.Node)
}

// skipNonWorker reports (and logs) whether a worker-only component must be
// skipped because this node does not run workers (ui/webhook profiles).
// It centralizes the RunWorkers gate + skip-log that the scheduler, watchdog,
// effective-cost monitor, and reminders init steps share — these were each
// open-coding or missing the check, which let a thin webhook node crash-loop
// on podman and spam reminders lease_due errors (incidents 2026-06-12). New
// worker-only init steps should start with:
//
//	if c.skipNonWorker("<component>") { return nil }
func (c *Container) skipNonWorker(component string) bool {
	if c.capabilities().RunWorkers {
		return false
	}
	c.Logger.Info().Str("component", component).
		Msg("skipping worker-only component on non-worker node")
	return true
}

// registerSubsystems appends every Subsystem to c.subsystems.
// Adding a new subsystem is a one-line edit here PLUS the
// subsystem's own .go file under internal/service/subsystem_*.go.
// The order matters insofar as Stop runs in REVERSE order during
// drain (LIFO); Build + Start run in registration order. Today
// no subsystem depends on another, so order is informational.
func (c *Container) registerSubsystems() {
	c.subsystems = append(c.subsystems,
		NewMemoryIngestSubsystem(),
		// Conversation channels. Order matters: Telegram first so
		// EmailChannelsSubsystem's multi-notifier wiring (which
		// reads c.TelegramBot) sees a started bot. The Subsystem
		// contract doesn't enforce inter-subsystem ordering today;
		// registration order is the de-facto sequence.
		NewTelegramSubsystem(),
		NewGitHubChannelSubsystem(),
		NewSlackChannelsSubsystem(),
		NewEmailChannelsSubsystem(),
		NewCPCTimeoutSubsystem(),
		// Background scanners + sweepers (2026-05-29 second pass).
		// External wait depends on Scheduler so it must Build AFTER
		// the constructor wires the scheduler — registerSubsystems
		// runs during NewContainer's final phase, after initScheduler.
		NewExternalWaitSubsystem(),
		NewWatchdogSubsystem(),
		NewEffectiveCostSubsystem(),
		NewArchiveSweeperSubsystem(),
		NewRemindersSubsystem(),
		NewRateLimitCounterSweepSubsystem(),
	)
	// Instinct is edition-gated (CE/EE). The provider yields the
	// subsystem only for editions that include it; Community yields nil.
	if p := c.providers.Instinct; p != nil {
		if sub := p.InstinctSubsystem(); sub != nil {
			c.subsystems = append(c.subsystems, sub)
			c.Logger.Info().Str("capability", "instinct").Str("edition", c.Edition()).
				Msg("EE capability registered (inner config/postgres gate may still skip)")
		} else {
			c.Logger.Info().Str("capability", "instinct").Str("edition", c.Edition()).
				Msg("EE capability omitted by edition")
		}
	}
	// Trading is edition-gated (CE/EE). The provider yields the
	// subsystem only for editions that include it; Community yields nil.
	if p := c.providers.Trading; p != nil {
		if sub := p.TradingSubsystem(); sub != nil {
			c.subsystems = append(c.subsystems, sub)
			c.Logger.Info().Str("capability", "trading").Str("edition", c.Edition()).
				Msg("EE capability registered (inner config/postgres gate may still skip)")
		} else {
			c.Logger.Info().Str("capability", "trading").Str("edition", c.Edition()).
				Msg("EE capability omitted by edition")
		}
	}
	// Black Box is edition-gated (CE/EE). The provider yields the
	// subsystem only for editions that include it; Community yields nil.
	if p := c.providers.BlackBox; p != nil {
		if sub := p.BlackBoxSubsystem(); sub != nil {
			c.subsystems = append(c.subsystems, sub)
			c.Logger.Info().Str("capability", "blackbox").Str("edition", c.Edition()).
				Msg("EE capability registered (inner config/postgres gate may still skip)")
		} else {
			c.Logger.Info().Str("capability", "blackbox").Str("edition", c.Edition()).
				Msg("EE capability omitted by edition")
		}
	}
	// Clustering is edition-gated (CE/EE) behind the Group-A ClusteringProvider
	// seam (Phase 2c). The provider lives in internal/enterprise/clustering and
	// owns the four cluster subsystems + their per-node inner gates (repo
	// presence, node capabilities, configured endpoints). Community leaves the
	// provider nil — no cluster subsystems register (today's CE behaviour).
	if cp := c.providers.Clustering; cp != nil {
		c.Logger.Info().Str("capability", "clustering").Str("edition", c.Edition()).
			Msg("EE capability registered (inner config/postgres gate may still skip)")
		deps := ClusterDeps{Container: c, WebhookRelayClient: c.webhookRelayClient}
		c.subsystems = append(c.subsystems, cp.ClusterSubsystems(deps)...)
	}
}

// buildSubsystems calls Build on each registered subsystem.
// Skip-sentinel errors (SubsystemSkipped) log at Debug; real
// errors log at Warn and remove the subsystem from the slice
// so Start doesn't re-attempt. Either way the boot continues —
// a misbehaving subsystem doesn't block the daemon.
func (c *Container) buildSubsystems() {
	deps := &BuildDeps{Container: c}
	survivors := c.subsystems[:0]
	for _, s := range c.subsystems {
		if err := s.Build(deps); err != nil {
			if IsSubsystemSkipped(err) {
				c.Logger.Debug().
					Str("subsystem", s.Name()).
					Err(err).
					Msg("subsystem skipped at build")
				continue
			}
			c.Logger.Warn().
				Str("subsystem", s.Name()).
				Err(err).
				Msg("subsystem build failed; will not be started")
			continue
		}
		survivors = append(survivors, s)
	}
	c.subsystems = survivors
}

// startSubsystems calls Start on each surviving subsystem. The
// ctx is the daemon lifetime ctx (matches the pre-extraction
// goroutine ctx). Failures log + continue; the container's run
// loop doesn't abort on a single subsystem's Start error.
func (c *Container) startSubsystems(ctx context.Context) {
	// Stamp the container on ctx so subsystems can reach back for
	// facilities not yet on BuildDeps (mainly initWorkerElector).
	// Transitional — will go away when those facilities lift to
	// BuildDeps in a later extraction.
	ctx = withContainer(ctx, c)
	for _, s := range c.subsystems {
		if err := s.Start(ctx); err != nil {
			c.Logger.Warn().
				Str("subsystem", s.Name()).
				Err(err).
				Msg("subsystem start failed; subsystem in unknown state")
		}
	}
}

// stopSubsystems iterates LIFO (last-registered, first-stopped).
// Each subsystem's Stop receives the bounded drain ctx — a Stop
// that exceeds the budget gets cancelled but the loop continues.
// Stop errors log only; drain never aborts on a single
// subsystem's failure.
func (c *Container) stopSubsystems(ctx context.Context) {
	for i := len(c.subsystems) - 1; i >= 0; i-- {
		s := c.subsystems[i]
		if err := s.Stop(ctx); err != nil {
			c.Logger.Warn().
				Str("subsystem", s.Name()).
				Err(err).
				Msg("subsystem stop reported error")
		}
	}
	// Drain the firewall's audit writer last — recall traffic
	// may still be in flight while the subsystems are stopping,
	// so we wait for everything else to drain before flushing
	// the final batch. Nil-safe.
	c.stopMemoryFirewallWriter()
}

// This is an extended initializer that adds metrics and tracing support.
func NewContainerWithObservability(cfg *config.Config, configPath string, obsCfg observability.Config, opts ...ContainerOption) (*Container, error) {
	// First create the base container
	c, err := NewContainer(cfg, configPath, opts...)
	if err != nil {
		return nil, err
	}

	// Initialize observability (metrics + optional tracing)
	obs, err := observability.New(obsCfg, c.Logger)
	if err != nil {
		c.Logger.Error().Err(err).Msg("failed to initialize observability")
		return nil, fmt.Errorf("observability initialization: %w", err)
	}
	c.Observability = obs
	c.Logger.Info().
		Str("metrics_addr", obsCfg.MetricsAddr).
		Bool("tracing_enabled", obsCfg.TracingEnabled).
		Msg("observability initialized")

	// Wire Prometheus metrics onto the already-running scheduler stack.
	// This replaces the previous full initScheduler() re-call, which
	// redundantly re-created repos, the runtime manager, the executor, and
	// the warm pool just to register metrics.
	c.rebuildSchedulerMetrics()

	// Initialize DB metrics now that the registry exists (initDB ran before observability).
	if c.DB != nil && c.dbMetrics == nil {
		if reg := c.observabilityRegistry(); reg != nil {
			c.dbMetrics = persistence.NewDBMetrics(reg)
			go c.collectDBMetrics()
			// Rebuild repos so the metrics-wrapped DBTX flows through
			// every subsequent repo handle. Existing handles captured
			// before this point keep working but won't report metrics
			// (matches the historical lazy-construct pattern).
			// SQLite backend keeps the repos that openSQLite built —
			// no metrics-wrapping path for the SQLite repo factory yet.
			if c.backend != nil && c.backend.Driver != "sqlite" {
				c.repos = storage.Build(c.instrumentedDB())
				// The rebuild replaced the audit repo handles with fresh
				// (undecorated) ones; re-apply the logship audit taps so
				// consumers built after this point (the second
				// initHTTPServer) still ship audit events.
				c.decorateAuditRepos()
			}
		}
	}

	// Attach the logship Prometheus metrics now that the registry exists
	// (the router was built in NewContainer with nil metrics).
	c.attachLogshipMetrics()

	// Rebuild the HTTP server so /metrics uses the custom Prometheus registry.
	// The initial initHTTPServer ran before observability was set, so the API
	// server was created with metricsRegistry=nil.
	if err := c.initHTTPServer(); err != nil {
		c.Logger.Error().Err(err).Msg("failed to reinitialize HTTP server with observability")
		return nil, fmt.Errorf("http server reinitialization: %w", err)
	}

	// Wire Prometheus metrics into chat, telegram, and autonomy components now
	// that the observability registry is available.
	c.wireComponentMetrics()

	// Initialize state metrics collector (task/execution/podman/registry gauges)
	c.initStateCollector()

	return c, nil
}

// Repos returns the container's repository surface. Used by EE subsystems (moved
// to internal/enterprise) that need repo access without holding a typed reference
// to the unexported repos field. Returns nil when repos haven't been initialized yet.
func (c *Container) Repos() *storage.Repositories {
	if c == nil {
		return nil
	}
	return c.repos
}

// SetRepos injects the repository set for test purposes or for EE subsystems that
// need to construct a Container with specific repos outside of a full NewContainer
// boot sequence (e.g. enterprise subsystem integration tests).
func (c *Container) SetRepos(r *storage.Repositories) {
	if c == nil {
		return
	}
	c.repos = r
}

// MemoryResponseCache returns the response cache from the memory manager.
// Used by the EE instinct distiller to memoize cheap-model calls.
// Returns nil when the memory subsystem is not configured or the cache is disabled.
func (c *Container) MemoryResponseCache() memory.ResponseCache {
	if c == nil || c.memoryManager == nil {
		return nil
	}
	return c.memoryManager.ResponseCache
}

// MemoryPolicyRepo returns the policy/chunk repository from the memory manager.
// Used by the EE instinct chunk screener (InstinctChunkScreener). Returns nil
// when the memory subsystem is not configured.
func (c *Container) MemoryPolicyRepo() *memory.Repository {
	if c == nil || c.memoryManager == nil {
		return nil
	}
	return c.memoryManager.Repository()
}

// PricingForInstinct returns the pricing table as the narrow memory.PricingTable
// interface. Used by the EE instinct distiller to cost billed tokens.
// Returns nil when the pricing table hasn't been loaded yet.
func (c *Container) PricingForInstinct() memory.PricingTable {
	if c == nil || c.pricingTable == nil {
		return nil
	}
	return c.pricingTable
}

// SetBlackboxMetricsWirer registers a callback that wireComponentMetrics invokes
// after the observability registry is ready to attach Prometheus metrics to the EE
// blackbox detector. Called by enterprise/blackbox.Subsystem.Build so the container
// avoids a direct dependency on *blackbox.Detector (an EE-domain type).
// The callback receives a prometheus.Registerer; the subsystem builds sharedMetrics
// from it and calls Detector.SetMetrics — matching the instinct metrics wirer pattern.
func (c *Container) SetBlackboxMetricsWirer(fn func(prometheus.Registerer)) {
	if c == nil {
		return
	}
	c.blackboxMetricsWirer = fn
}

// CollectorsCtx returns the context that background collector goroutines should
// use. If the daemon has started its collector lifecycle, this is the context
// that will be cancelled when the daemon begins draining. Falls back to ctx when
// the daemon hasn't started yet (e.g. during test setup). Called by EE subsystems
// (moved to internal/enterprise) that need the collectors context from their Start
// methods without accessing the unexported collectorsCtx field.
func (c *Container) CollectorsCtx(fallback context.Context) context.Context {
	if c != nil && c.collectorsCtx != nil {
		return c.collectorsCtx
	}
	return fallback
}

// RegisterExtraElector folds an elector minted by an EE subsystem (e.g. the
// trading equity sampler / cross-checker, relocated to
// internal/enterprise/trading in Phase 2c) into the container's drain list so
// releaseAllLeaderLeases() releases its lease before DB close. Without this,
// peer replicas would wait out the full TTL before claiming the lease on
// shutdown. Nil-safe (nil elector / nil receiver are no-ops). Mirrors the
// in-package email-IMAP elector write-back, but exported for subsystems that
// no longer live in package service.
func (c *Container) RegisterExtraElector(e *leaderelection.Elector) {
	if c == nil || e == nil {
		return
	}
	c.extraElectorsMu.Lock()
	c.extraElectors = append(c.extraElectors, e)
	c.extraElectorsMu.Unlock()
}

// ReconcileStaleTradingOrders is the exported wrapper over the boot-time
// stale-order reconciliation (container_trading.go). It stays in CE because it
// is pure persistence + broker-HTTP with no EE-domain coupling; the EE trading
// subsystem (relocated to internal/enterprise/trading in Phase 2c) invokes it
// through this exported method rather than the unexported one.
func (c *Container) ReconcileStaleTradingOrders(ctx context.Context) {
	if c == nil {
		return
	}
	c.reconcileStaleTradingOrders(ctx)
}

// EquityCheckRecord returns the trading equity cross-check metric sink, or nil
// when the metrics aren't wired (observability not yet up, or no registry).
// The EE trading cross-checker (internal/enterprise/trading) assigns the
// returned func to its Record field so each tick emits the drift + anomaly
// gauges. Nil-safe.
func (c *Container) EquityCheckRecord() func(projectID string, driftUSD float64, codes []string) {
	if c == nil || c.equityCheckMetrics == nil {
		return nil
	}
	return c.equityCheckMetrics.Set
}

// WireComponentMetricsForTest invokes wireComponentMetrics from test code that
// lives outside the service package (e.g. enterprise/instinct subsystem tests).
// The private wireComponentMetrics is called after observability is up in the
// real boot sequence; tests that exercise post-boot metric wiring use this
// exported wrapper instead of the private method.
func (c *Container) WireComponentMetricsForTest() {
	c.wireComponentMetrics()
}

// HasInstinctMetricsWirer reports whether the container has a registered instinct
// metrics wirer callback. Used in EE subsystem tests (which live in
// internal/enterprise/instinct) to verify Build registered the callback without
// needing to access the private instinctMetricsWirer field directly.
func (c *Container) HasInstinctMetricsWirer() bool {
	if c == nil {
		return false
	}
	return c.instinctMetricsWirer != nil
}

// SetInstinctMetricsWirer registers the callback that wireComponentMetrics invokes
// after the observability registry is ready to attach Prometheus metrics to the EE
// instinct worker. Called by enterprise/instinct.Subsystem.Build (via BuildDeps.Container)
// so the container avoids a direct dependency on *instinct.Worker (an EE-domain type).
// The callback receives the shared *observability.InstinctMetrics instance.
func (c *Container) SetInstinctMetricsWirer(fn func(*observability.InstinctMetrics)) {
	if c == nil {
		return
	}
	c.instinctMetricsWirer = fn
}

// SetInstinctElector records the leader elector for the instinct extraction worker
// so that allElectors() releases the lease on graceful drain. Called by
// InstinctSubsystem.Start (via BuildDeps.Container) instead of writing the
// unexported instinctElector field directly — public setter preserves encapsulation
// when the subsystem moves to enterprise.
func (c *Container) SetInstinctElector(e *leaderelection.Elector) {
	if c == nil || e == nil {
		return
	}
	c.instinctElector = e
}

// healingObserverOnce returns the shared contracts.HealingObserver, building it
// lazily from c.providers.HealingObserverFactory on the first call. Nil when the
// factory is not set (Community edition) or when the observability registry is not
// yet available. The workflowhealing trial runner and promoter constructors are
// nil-safe, so a nil observer is a valid "no metrics" configuration.
func (c *Container) healingObserverOnce() contracts.HealingObserver {
	if c == nil {
		return nil
	}
	if c.healingObserver != nil {
		return c.healingObserver
	}
	if c.providers.HealingObserverFactory == nil {
		return nil
	}
	reg := c.observabilityRegistry()
	if reg == nil {
		return nil
	}
	c.healingObserver = c.providers.HealingObserverFactory(reg)
	return c.healingObserver
}

// wireComponentMetrics attaches Prometheus metrics to chat, telegram, and autonomy components.
// Called after observability is initialized (metrics registry is available).
func (c *Container) wireComponentMetrics() {
	reg := c.observabilityRegistry()
	if reg == nil {
		return
	}

	if c.ChatClient != nil {
		chatMetrics := chat.NewMetrics(reg)
		c.ChatClient.SetMetrics(chatMetrics)
		// Share the same chat.Metrics with the API server so the
		// chat-proxy + internal llm-usage handler bump the prompt-cache
		// series (audit N8) on every recorded usage row.
		if c.apiServer != nil {
			c.apiServer.SetChatCacheMetrics(chatMetrics)
		}
		c.Logger.Info().Msg("chat metrics wired")
	}

	if c.TelegramBot != nil {
		c.TelegramBot.SetMetrics(telegram.NewMetrics(reg))
		c.Logger.Info().Msg("telegram metrics wired")
	}

	// Config hot-reload metrics (audit R7): reload outcome counter +
	// validation-error / last-reload-timestamp / staged-pending gauges.
	if c.ConfigReloader != nil {
		c.ConfigReloader.SetMetrics(config.NewMetrics(reg))
		c.Logger.Info().Msg("config reload metrics wired")
	}

	if c.Dispatcher != nil {
		c.Dispatcher.SetMetrics(dispatcher.NewMetrics(reg))
		c.Logger.Info().Msg("dispatcher metrics wired")
	}

	if c.autonomyManager != nil {
		c.autonomyManager.SetMetrics(autonomy.NewMetrics(reg))
		c.Logger.Info().Msg("autonomy metrics wired")
	}

	// Replay metrics — Phase C of failure forensics. Registered
	// against the same registry as everything else; lives next to
	// memory metrics so the registration order is stable.
	c.replayMetrics = replay.NewMetrics(reg)
	// Project wizard metrics — Phase C of Feature #2.
	c.projectWizardMetrics = projectwizard.NewMetrics(reg)
	// Document-extraction metrics (LLD Phase 7). Registered at boot so
	// the series exist regardless of whether the lazily-built extractor
	// Runner is ever exercised; the Runner picks this up in
	// initExtractorPipeline.
	c.extractorMetrics = extractor.NewMetrics(reg)

	// Instinct extraction-worker metrics (continuous-learning slice 1 +
	// the vornik_instinct_total population gauge). Wired via a callback set
	// by InstinctSubsystem.Build (SetInstinctMetricsWirer) so this code has
	// no direct dependency on *instinct.Worker (an EE-domain type relocated to
	// internal/enterprise in Task 6). The callback receives the shared
	// InstinctMetrics so it can wire the same instance across worker, executor,
	// and recovery resolver — preventing the promauto double-registration panic
	// that a second NewMetrics call would trigger.
	if c.instinctMetricsWirer != nil {
		c.instinctMetricsWirer(c.sharedInstinctMetrics())
		c.Logger.Info().Msg("instinct metrics wired")
	}

	if c.blackboxMetricsWirer != nil {
		c.blackboxMetricsWirer(reg)
		c.Logger.Info().Msg("blackbox detector metrics wired")
	}

	if c.memoryManager != nil {
		mems := memory.NewMetrics(reg)
		c.memoryMetrics = mems
		c.memoryManager.SetMetrics(mems)
		if c.ingestWorker != nil {
			c.ingestWorker.SetMetrics(mems)
		}
		if c.memoryPipeline != nil {
			c.memoryPipeline.SetMetrics(mems)
		}
		if c.memoryTitleBackfiller != nil {
			c.memoryTitleBackfiller.Metrics = mems
		}
		if c.memoryClassifyBackfiller != nil {
			c.memoryClassifyBackfiller.Metrics = mems
		}
		if c.memoryConsolidateWorker != nil {
			c.memoryConsolidateWorker.Metrics = mems
		}
		if c.memoryLLMConsolidateWorker != nil {
			c.memoryLLMConsolidateWorker.Metrics = mems
		}
		c.Logger.Info().Msg("memory metrics wired")
	}

	if c.graphWorker != nil {
		c.graphWorker.SetMetrics(graph.NewMetrics(reg))
		c.Logger.Info().Msg("KG extraction metrics wired")
	}

	// Hallucination subsystem (Phase 1 detector + Phase 3 judge).
	// One Metrics value is shared by the dispatcher chat-side scan,
	// the executor's per-step scan, and the judge runner so all
	// three subsystems land on the same counters under one
	// namespace.
	halluMetrics := hallucination.NewMetrics(reg)
	if c.Executor != nil {
		c.Executor.SetHallucinationMetrics(halluMetrics)
	}
	if c.Dispatcher != nil {
		c.Dispatcher.SetHallucinationMetrics(halluMetrics)
	}
	if c.judgeRunner != nil {
		c.judgeRunner.Metrics = halluMetrics
	}
	c.Logger.Info().Msg("hallucination + judge metrics wired")

	// Trading audit-channel ingest metrics (safety events, orders,
	// fills, ingest errors). One TradingMetrics value attaches to
	// the API server which owns all three ingest handlers.
	if c.apiServer != nil {
		c.apiServer.SetTradingMetrics(api.NewTradingMetrics(reg))
		c.Logger.Info().Msg("trading ingest metrics wired")
	}

	// Leader-fence rejection counter (Slice D). Registered against the SAME
	// served registry as everything above so vornik_leader_fence_rejections_total
	// is actually visible on /metrics; LeaderFenceRejected (called from autonomy
	// + telegram fail-closed paths) is a no-op until this runs.
	leaderelection.RegisterFenceMetrics(reg)
	c.Logger.Info().Msg("leader fence metrics wired")
}

// serveAgentUnixSocket starts the agent unix socket listener and begins
// serving the HTTP handler on it. It must be called BEFORE
// c.Executor.Recover and c.Scheduler.Start so that agent containers started
// by recovered/fresh steps can bind-mount the socket immediately (incident
// restart-resume-on-boot socket race, 2026-06-21).
//
// BIDIRECTIONAL ORDERING INVARIANT: the agent unix socket must be serving
// before the executor recovers/runs any step (startup) and must stay alive
// until the executor has paused all in-flight steps (shutdown — see the
// ORDERING INVARIANT comment in shutdown() Phase 2). Both ends exist because
// agent containers bind-mount this socket; running a step without it yields
// "statfs …/vornik.sock: no such file or directory".
//
// Any error from the async Serve is forwarded to errCh (same channel used
// by the TCP listener) so Run's select catches it. Setup errors (MkdirAll,
// Listen, Chmod) are returned directly so Run can fail fast.
func (c *Container) serveAgentUnixSocket(errCh chan error) error {
	sock := c.Config.Server.UnixSocket
	if sock == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return fmt.Errorf("create unix socket dir %s: %w", filepath.Dir(sock), err)
	}
	// Remove a stale socket left by an unclean shutdown; net.Listen
	// fails with EADDRINUSE otherwise.
	if _, statErr := os.Stat(sock); statErr == nil {
		_ = os.Remove(sock)
	}
	unixLn, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen on unix socket %s: %w", sock, err)
	}
	// The socket exposes the same API as the TCP port; on a
	// single-operator rootless host 0666 lets uid-mapped containers
	// connect without ownership juggling (connect() needs write perm
	// on the socket inode).
	if err := os.Chmod(sock, 0o666); err != nil {
		c.Logger.Warn().Err(err).Str("unix_socket", sock).Msg("chmod daemon unix socket")
	}
	go func() {
		c.Logger.Info().Str("unix_socket", sock).Msg("serving HTTP API on unix socket")
		if err := c.HTTPServer.Serve(unixLn); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	return nil
}

// Run starts the daemon and blocks until shutdown.
func (c *Container) Run(ctx context.Context) error {
	errCh := make(chan error, 3) // Buffer for HTTP server, metrics server, and relay-ingress errors

	// Create a cancellable context for background collector goroutines
	// so they stop cleanly during shutdown instead of racing with DB close.
	c.collectorsCtx, c.stopCollectors = context.WithCancel(ctx)

	// Autonomy elector goroutine — the Manager itself was
	// constructed + Started during NewContainer (init order
	// requirement: Telegram + project registry must be wired
	// first). The elector starts here so its renew loop ties to
	// the daemon lifecycle. BootstrapAcquire runs synchronously
	// so the per-project tick loops that have been ticking since
	// boot see an authoritative IsLeader on their first
	// post-Run-entry tick.
	if c.autonomyElector != nil {
		c.autonomyElector.BootstrapAcquire(ctx)
		go c.autonomyElector.Run(ctx)
	}

	// Start the agent unix socket BEFORE executor recovery so that any
	// resumed step's podman container can bind-mount the socket immediately.
	// See serveAgentUnixSocket for the full BIDIRECTIONAL ORDERING INVARIANT.
	// testAgentSocketServe is a test hook; nil in production.
	agentSockFn := c.testAgentSocketServe
	if agentSockFn == nil {
		agentSockFn = c.serveAgentUnixSocket
	}
	if err := agentSockFn(errCh); err != nil {
		return err
	}

	if c.capabilities().RunWorkers {
		// testExecutorRecover is a test hook; nil in production.
		execRec := c.testExecutorRecover
		if execRec == nil && c.Executor != nil {
			execRec = c.Executor
		}
		if execRec != nil {
			if err := execRec.Recover(ctx); err != nil {
				return fmt.Errorf("failed to recover in-flight executions: %w", err)
			}
			c.Logger.Info().Msg("executor recovery completed")
		}
	} else {
		c.Logger.Info().Msg("node profile: run_workers=false; task scheduler + executor recovery skipped")
	}

	// Trading boot reconciliation + equity sampler now owned by
	// TradingSubsystem (see subsystem_trading.go); their goroutines
	// launch during c.startSubsystems(ctx).

	if c.capabilities().RunWorkers {
		// Chat-provider readiness gate. Before the scheduler starts
		// dispatching, confirm the chat backend can answer at least one
		// probe — for HTTP this is /v1/models, for the CLI subprocesses it
		// is `--version`, for subscription clients it is an auth.json
		// parse. Without this gate, the first few tasks after a daemon
		// restart can race the provider's warm-up (subprocess cold, gateway
		// auth handshake in flight, OAuth token still loading from disk)
		// and surface as "empty response from LLM" inside the dispatcher.
		// The 2026-05-04 init-order fix moved chat client construction
		// ahead of the scheduler; this is the matching runtime gate that
		// makes "constructed" mean "reachable".
		c.waitForChatProviderReady(ctx)

		if c.Scheduler != nil {
			if err := c.Scheduler.Start(); err != nil {
				return fmt.Errorf("failed to start scheduler: %w", err)
			}
			c.Logger.Info().Msg("scheduler started")

			// external_wait monitor + watchdog + effective-cost monitor
			// now lifecycle-owned by their respective Subsystems
			// (subsystem_external_wait.go, subsystem_watchdog.go,
			// subsystem_effective_cost.go). All three Start during
			// c.startSubsystems(ctx) below.
		}
	}

	// Equity sampler now owned by TradingSubsystem
	// (subsystem_trading.go). Goroutine launches inside
	// startSubsystems below.

	// memory manager + worker fleet now lifecycle-owned by
	// MemoryIngestSubsystem (see subsystem_memory_ingest.go).
	// Start happens in c.startSubsystems(ctx) below; Stop runs
	// during drain via stopSubsystems(ctx).

	// Archive-sweeper + reminders heartbeat now lifecycle-owned by
	// their Subsystems (subsystem_archive_sweeper.go,
	// subsystem_reminders.go). Start during c.startSubsystems.

	// Blackbox detector now started via the Subsystem pattern
	// (see subsystem_blackbox.go). The 13-line imperative block
	// previously here shrank to one entry in registerSubsystems().
	c.startSubsystems(ctx)
	// Policy-Aware Memory Firewall Phase B — launch the
	// non-blocking audit writer's flusher goroutine. Nil-safe
	// when the firewall wasn't wired (SQLite branch). The Stop
	// counterpart runs during stopSubsystems' drain via the
	// memoryFirewallStopHook below.
	c.startMemoryFirewallWriter(ctx)

	// Ratelimit counter sweep now lifecycle-owned by
	// RateLimitCounterSweepSubsystem (subsystem_ratelimit_sweep.go).
	// Start during c.startSubsystems.

	// ingestWorker now owned by MemoryIngestSubsystem — see
	// subsystem_memory_ingest.go. Its goroutine launches during
	// c.startSubsystems below.

	if c.graphWorker != nil {
		c.kgExtractElector = c.initWorkerElector("kg_extract")
		if c.kgExtractElector != nil {
			c.graphWorker.SetLeaderGate(c.kgExtractElector)
			c.kgExtractElector.BootstrapAcquire(ctx)
			go c.kgExtractElector.Run(ctx)
		}
		c.graphWorker.Start(ctx)
	}

	// Memory worker fleet (titler backfill, classifier backfill,
	// LLM-free consolidate, LLM-tier consolidate) now owned by
	// MemoryIngestSubsystem (see subsystem_memory_ingest.go).
	// Cadence + batch-size defaults moved into Build; goroutine
	// launches happen in Start. Stop drains via Manager.Stop.

	if c.ConfigReloader != nil {
		// Install cross-instance broadcast BEFORE Start so the
		// post-reload hook is wired in time for the first file-
		// watcher-driven reload. No-op on SQLite / unit-test
		// deployments (DB nil); single-process deployments keep
		// the legacy behaviour (only the receiving instance
		// reloads).
		c.installConfigReloadBroadcast(ctx)
		if err := c.ConfigReloader.Start(ctx); err != nil {
			return fmt.Errorf("failed to start config reloader: %w", err)
		}
		c.Logger.Info().Msg("config reloader started")
	}

	// CPC timeout scanner now lifecycle-owned by
	// CPCTimeoutSubsystem (see subsystem_cpc_timeout.go).
	// Goroutine + elector launch during c.startSubsystems(ctx).

	// Conversation channels (Telegram, GitHub App, Slack, Email)
	// now lifecycle-owned by per-channel Subsystems (see
	// subsystem_{telegram,github_channel,slack_channels,
	// email_channels}.go). Their goroutines launch in order
	// during c.startSubsystems(ctx); EmailChannelsSubsystem also
	// runs the multi-channel CompletionNotifier wiring + the
	// per-channel followup-registrar wiring at the tail of its
	// Start.

	// Start HTTP server in a goroutine
	go func() {
		c.Logger.Info().Str("address", c.HTTPServer.Addr).Msg("starting HTTP server")
		if err := c.HTTPServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Start the mTLS relay-ingress listener when this node is the job tier
	// and relay_ingress is configured (Slice B). Certs are already loaded
	// into TLSConfig.Certificates, so empty cert/key args to ListenAndServeTLS.
	if rs, initErr := c.initRelayIngress(); initErr != nil {
		return fmt.Errorf("relay ingress init: %w", initErr)
	} else if rs != nil {
		c.relayServer = rs
		go func() {
			c.Logger.Info().Str("address", rs.Addr).Msg("starting mTLS relay ingress")
			if err := rs.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}()
	}

	// Start dedicated metrics server if a separate address is configured
	if c.Observability != nil && c.Observability.Metrics != nil && c.Config.Metrics.Enabled {
		go func() {
			if err := c.Observability.Metrics.StartServer(ctx); err != nil {
				errCh <- err
			}
		}()
	}

	// Phase 3 judge wiring summary — operators can now see
	// "judge: enabled, N projects opt in" in the startup log
	// instead of having to wait for a task to terminate before
	// learning whether the layer is actually live.
	c.logJudgeStartupSummary()

	c.Logger.Info().
		Str("config_path", c.ConfigPath).
		Str("server_address", c.Config.Server.Address).
		Msg("vornik started successfully")

	// Notify systemd that the daemon is ready (Type=notify)
	if err := sdNotifyReady(); err != nil {
		c.Logger.Warn().Err(err).Msg("sd_notify failed (not running under systemd?)")
	} else if os.Getenv("NOTIFY_SOCKET") != "" {
		c.Logger.Info().Msg("notified systemd: READY=1")
	}

	// Wait for shutdown signal or server error
	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		c.beginDrain()
		return c.shutdown()
	}
}

// beginDrain runs the graceful-shutdown pre-step before shutdown()
// tears anything down. The sequence is:
//
//  1. Flip the api.Server drain bit so /readyz starts returning 503.
//  2. Sleep the configured grace period so external load balancers /
//     k8s readiness probes notice and stop sending new requests.
//  3. Return to caller — the existing shutdown() path then proceeds
//     in reverse-initialisation order (executor pause → scheduler
//     stop → DB close → ...).
//
// Grace period is sourced from VORNIK_DRAIN_GRACE_SECONDS (default 5s,
// max 30s). Set to 0 to skip the wait entirely — useful for unit tests
// and single-node deployments where no LB is in front of us. The cap
// protects against operators setting an unreasonably-long grace and
// blowing through systemd's TimeoutStopSec (default 90s).
//
// During the grace window the daemon still accepts requests on every
// other endpoint; the readiness flip is purely a signal to upstream
// routers. The in-flight executor + scheduler keep working until
// shutdown() actually stops them.
func (c *Container) beginDrain() {
	if c.apiServer != nil {
		c.apiServer.SetDraining(true)
		c.Logger.Info().Msg("graceful drain: /readyz returning 503; waiting for load balancer to notice")
	}
	grace := drainGracePeriod()
	if grace <= 0 {
		return
	}
	// Use a fresh timer (not ctx.Done) — the parent context is
	// already cancelled here. Operators sending a second SIGTERM
	// during drain still escape via the default shutdown timeout
	// inside shutdown().
	c.Logger.Info().Dur("grace", grace).Msg("graceful drain: sleeping before shutdown")
	time.Sleep(grace)
}

// drainGracePeriod parses VORNIK_DRAIN_GRACE_SECONDS into a
// time.Duration. Default 5s; negative → 0; capped at 30s so a
// typo can't blow the systemd stop timeout.
func drainGracePeriod() time.Duration {
	const def = 5 * time.Second
	const max = 30 * time.Second
	raw := os.Getenv("VORNIK_DRAIN_GRACE_SECONDS")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	if n <= 0 {
		return 0
	}
	d := time.Duration(n) * time.Second
	if d > max {
		return max
	}
	return d
}

// executorShutdowner is the subset of *executor.Executor that shutdown()
// needs. Extracted as an interface so tests can inject a spy to assert the
// ordering invariant without requiring a live database or runtime.
type executorShutdowner interface {
	Shutdown(ctx context.Context) error
}

// shutdown performs graceful shutdown in reverse initialization order.
func (c *Container) shutdown() error {
	c.Logger.Info().Msg("shutting down vornik")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Phase 0a: Release every leader-election lease this daemon
	// holds BEFORE anything touches the DB lifecycle. Peer replicas
	// then claim the leases within ~1s instead of waiting out the
	// TTL window (typically minutes). Best-effort: TTL expiry is
	// the safety net for any per-lease DELETE that fails.
	//
	// Must precede stopCollectors / livePub / HTTP shutdown /
	// executor pause because each Release issues a SQL DELETE and
	// needs the DB pool open. See
	// https://docs.vornik.io §3a.
	c.releaseAllLeaderLeases(ctx)

	// Phase 0b: drain extracted subsystems in LIFO order. Best-
	// effort with the same 30s budget the rest of shutdown uses;
	// per-subsystem failures log + the loop continues. Sequencing
	// is here (between lease release + collectorsCtx cancellation)
	// because most subsystems own goroutines tied to
	// c.collectorsCtx — cancelling the ctx is the universal stop
	// signal, but a subsystem with externally-visible state may
	// want a graceful drain before the ctx flips.
	c.stopSubsystems(ctx)

	// Phase 0: Stop background metric collectors before closing DB.
	if c.stopCollectors != nil {
		c.stopCollectors()
	}

	// Stop live-events publisher (sweeper goroutine + cross-replica
	// LISTEN). Must happen before DB.Close so the Postgres NOTIFY
	// connection drains cleanly.
	if c.livePubShutdown != nil {
		c.livePubShutdown()
	}

	// Phase 1: Stop observability (metrics server and tracing)
	if c.Observability != nil {
		if err := c.Observability.Shutdown(ctx); err != nil {
			c.Logger.Error().Err(err).Msg("observability shutdown error")
		}
	}

	// Phase 2: Drain in-flight executions BEFORE the HTTP server shuts
	// down. Executor.Shutdown pauses each active execution (SIGTERM the
	// agent container, save checkpoint, mark PAUSED with reason=shutdown)
	// and waits for the goroutines to wind down within the shutdown timeout.
	// Executions that finish pausing are auto-resumed by Recover() on next
	// start; nothing is lost beyond the most-recent (unfinished) step.
	//
	// ORDERING INVARIANT: the executor must quiesce before
	// shutdownHTTPWithDeadline unlinks the agent unix socket
	// (/run/user/1001/vornik/vornik.sock). If the HTTP server closes first
	// it unlinks that socket, leaving in-flight podman-run bind-mounts
	// pointing at a deleted path — which produces "statfs …/vornik.sock: no
	// such file or directory" and drives tasks to FAILED instead of PAUSED
	// (incident restart-induced in-flight FAILED, 2026-06-21, Signature A).
	//
	// Stop()'s old behaviour — cancel every context — is the fallback when
	// the shutdown budget runs out: goroutines that haven't paused by
	// ctx.Done() return Stop's err and Recover() on next start picks them
	// up via the still-RUNNING status path. Even a tight shutdown doesn't
	// lose data; it just forces a re-run of the in-flight step.
	execSD := c.testExecShutdown // test hook; nil in production
	if execSD == nil && c.Executor != nil {
		execSD = c.Executor
	}
	if execSD != nil {
		c.Logger.Info().Msg("pausing executor for graceful shutdown")
		if err := execSD.Shutdown(ctx); err != nil {
			c.Logger.Error().Err(err).Msg("executor shutdown error")
		}
	}

	// Phase 3: Stop HTTP server. Bound the graceful drain and force-close
	// stuck connections (e.g. an in-flight /api/v1/chat/completions proxy
	// request held open for the agent's whole tool-loop, write_timeout 600s)
	// so we always stop within systemd's TimeoutStopSec rather than being
	// SIGABRT-killed mid-drain (incident 2026-06-20).
	// NOTE: must run AFTER the executor quiesces — see ORDERING INVARIANT above.
	httpSrv := c.testHTTPShutdown // test hook; nil in production
	if httpSrv == nil && c.HTTPServer != nil {
		httpSrv = c.HTTPServer
	}
	if httpSrv != nil {
		c.Logger.Info().Msg("stopping HTTP server")
		shutdownHTTPWithDeadline(ctx, httpSrv, httpShutdownBudget, c.Logger)
	}

	// Stop the mTLS relay-ingress listener (Slice B, job tier only).
	if c.relayServer != nil {
		if err := c.relayServer.Shutdown(ctx); err != nil {
			c.Logger.Warn().Err(err).Msg("relay ingress shutdown error")
		}
	}

	if c.Scheduler != nil {
		c.Logger.Info().Msg("stopping scheduler")
		if err := c.Scheduler.Stop(); err != nil {
			c.Logger.Error().Err(err).Msg("scheduler shutdown error")
		}
	}

	if c.Watchdog != nil {
		c.Logger.Info().Msg("stopping watchdog")
		if err := c.Watchdog.Stop(); err != nil {
			c.Logger.Error().Err(err).Msg("watchdog shutdown error")
		}
	}

	if c.EffectiveCostMon != nil {
		c.Logger.Info().Msg("stopping effective-cost monitor")
		if err := c.EffectiveCostMon.Stop(); err != nil {
			c.Logger.Error().Err(err).Msg("effective-cost monitor shutdown error")
		}
	}

	if c.ConfigReloader != nil {
		c.Logger.Info().Msg("stopping config reloader")
		c.ConfigReloader.Stop()
	}

	// Stop Telegram bot
	if c.TelegramBot != nil {
		c.Logger.Info().Msg("stopping telegram bot")
		if err := c.TelegramBot.Stop(); err != nil {
			c.Logger.Error().Err(err).Msg("telegram bot shutdown error")
		}
	}

	// Stop autonomous manager
	if c.autonomyManager != nil {
		c.Logger.Info().Msg("stopping autonomous manager")
		c.autonomyManager.Stop()
	}

	// Stop retention sweeper and wait for the current sweep iteration to
	// return. Without the wait, an in-flight DELETE could race with the
	// DB.Close() below and surface as noisy "driver: bad connection" errors.
	if c.retentionCancel != nil {
		c.retentionCancel()
		if c.retentionDone != nil {
			select {
			case <-c.retentionDone:
			case <-ctx.Done():
				c.Logger.Warn().Msg("retention sweeper did not exit within shutdown deadline")
			}
		}
	}

	if c.mcpManager != nil {
		c.mcpManager.Close()
	}

	if c.graphWorker != nil {
		c.Logger.Info().Msg("stopping KG extraction worker")
		c.graphWorker.Stop()
	}

	// memory ingest worker + memory manager drain via
	// MemoryIngestSubsystem.Stop (registered before Run), invoked
	// from c.stopSubsystems(). Pre-fix this block ran ALONGSIDE
	// the subsystem's Stop, causing a double-stop on Manager —
	// addressed by 2026-05-29 audit pass.

	// Stop warm container pool
	if c.warmPool != nil {
		c.Logger.Info().Msg("stopping warm container pool")
		c.warmPool.Stop(ctx)
	}

	// Drain centralised log forwarding: by now every producer (HTTP
	// server, executor, scheduler, subsystems) has stopped, so the queue
	// is stable. Flush it to the sinks within the shutdown budget, then
	// close the sinks. No-op when forwarding is disabled.
	c.drainLogship(ctx)

	// Phase 3: Close database connections
	if c.backend != nil && c.backend.Close != nil {
		c.Logger.Info().Msg("closing database connection")
		if err := c.backend.Close(); err != nil {
			c.Logger.Error().Err(err).Msg("database close error")
		}
	}

	// Close artifact backend (S3 HTTP client pool, etc.). LocalBackend's
	// Close is a no-op, so this is always safe to call.
	if c.artifactBackend != nil {
		if err := c.artifactBackend.Close(); err != nil {
			c.Logger.Error().Err(err).Msg("artifact backend close error")
		}
	}

	c.Logger.Info().Msg("vornik shutdown complete")
	return nil
}

// Run initializes and starts the vornik daemon.
// This is the main entry point called from cmd/vornik.
func Run(version, buildDate, edition string, providers ProviderSet) error {
	// Create context that listens for shutdown signals
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Load configuration
	cfg, configPath, err := config.Load()
	if err != nil {
		if err == config.ErrVersionRequested {
			fmt.Println(editionpkg.BuildLine("vornik", version, buildDate, edition))
			return nil
		}
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	obsCfg := buildObservabilityConfig(cfg)

	// Always create with observability so the Prometheus registry is
	// available for /metrics on the main API server, even when the
	// dedicated metrics listener is disabled.
	container, err := NewContainerWithObservability(cfg, configPath, obsCfg, WithProviders(providers))
	if err != nil {
		return fmt.Errorf("failed to initialize container: %w", err)
	}
	container.SetVersion(version)
	container.SetEdition(edition)
	container.Logger.Info().
		Str("edition", container.Edition()).
		Msg(editionpkg.BuildLine("vornik", version, buildDate, container.Edition()))

	// SIGHUP → config reload (same path as POST /api/v1/config/reload).
	// Lets `systemctl --user reload vornik` work instead of operators having
	// to hit the HTTP endpoint with an API key.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if container.ConfigReloader == nil {
				container.Logger.Warn().Msg("SIGHUP received but config reloader is not wired — ignoring")
				continue
			}
			container.Logger.Info().Msg("SIGHUP received — reloading config")
			if err := container.ConfigReloader.Reload(); err != nil {
				container.Logger.Error().Err(err).Msg("SIGHUP reload failed")
			}
		}
	}()
	// signal.Stop stops future deliveries but does not close the channel,
	// which would leave the reader goroutine blocked forever. Close after
	// stopping so the `for range hup` loop can return.
	defer func() {
		signal.Stop(hup)
		close(hup)
	}()

	// Run until shutdown
	return container.Run(ctx)
}
