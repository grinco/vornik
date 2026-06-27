// Package executor provides workflow execution for vornik.
package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/replay"
	"vornik.io/vornik/internal/runtime"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/toolbudget"
	"vornik.io/vornik/internal/workspacelock"
)

// APIKeyMinter mints and revokes per-task agent keys. Implemented by
// the service layer over the api_keys repository; nil disables
// per-task keys (the static AgentLLMEnv VORNIK_API_KEY passes
// through unchanged — sqlite/dev deployments).
//
// Limitation: only the ephemeral container path (startContainer /
// executeAgentStep) uses per-task keys. Warm-pool roles keep the static
// agent key for their container lifetime because the warm container's
// env is baked at pool-start time, not at each Acquire; per-acquire key
// rotation is a tracked follow-up.
type APIKeyMinter interface {
	// MintTaskKey returns the RAW key for (project, task). The
	// implementation persists only the hash (apikey.Generate +
	// repo.Create) with name "agent:task_<taskID>", empty
	// client_kind (NON-EMPTY WOULD TRIGGER COMPANION PATH
	// CONFINEMENT — middleware.go ~382), expires_at now+48h.
	MintTaskKey(ctx context.Context, projectID, taskID string) (string, error)
	// RevokeTaskKey revokes the key minted for taskID; idempotent.
	RevokeTaskKey(ctx context.Context, taskID string) error
}

// RuntimeManager is the interface for container operations.
type RuntimeManager interface {
	StartContainer(ctx context.Context, config *runtime.ContainerConfig) (string, error)
	StopContainer(ctx context.Context, containerID string, force bool) error
	InspectContainer(ctx context.Context, containerID string) (*runtime.Container, error)
	WaitForExit(ctx context.Context, containerID string, timeout time.Duration) (int, error)
	GetContainerByTask(ctx context.Context, taskID string) (*runtime.Container, error)
	RemoveContainer(ctx context.Context, containerID string, force bool) error
	Logs(ctx context.Context, containerID string, tail int) (string, error)
}

// ExecutionRepository is the interface for execution persistence.
type ExecutionRepository interface {
	Create(ctx context.Context, execution *persistence.Execution) error
	Get(ctx context.Context, id string) (*persistence.Execution, error)
	GetByTaskID(ctx context.Context, taskID string) (*persistence.Execution, error)
	List(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error)
	Update(ctx context.Context, execution *persistence.Execution) error
	UpdateStatus(ctx context.Context, id string, status persistence.ExecutionStatus) error
	SaveStateSnapshot(ctx context.Context, id string, snapshot []byte, currentStepID string, completedSteps []string) error
	SetWorkflowSnapshot(ctx context.Context, id string, snapshot []byte) error
	GetWorkflowSnapshot(ctx context.Context, id string) ([]byte, error)
	RecordCompletion(ctx context.Context, id string, result []byte) error
	RecordFailure(ctx context.Context, id string, errorMessage, errorCode string) error
	SupersedeNonTerminalForTask(ctx context.Context, taskID string) (int64, error)
}

// ArtifactRepository is the interface for artifact operations.
type ArtifactRepository interface {
	Create(ctx context.Context, artifact *persistence.Artifact) error
	GetByHash(ctx context.Context, hash string) (*persistence.Artifact, error)
	List(ctx context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error)
}

// TaskRepository is the interface for task operations needed by executor.
type TaskRepository interface {
	Get(ctx context.Context, id string) (*persistence.Task, error)
	Create(ctx context.Context, task *persistence.Task) error
	Update(ctx context.Context, task *persistence.Task) error
	Delete(ctx context.Context, id string) error
	UpdateStatus(ctx context.Context, id string, status persistence.TaskStatus) error
	GetChildren(ctx context.Context, parentTaskID string) ([]*persistence.Task, error)
	// ReleaseLease atomically updates the task status and clears
	// the lease — used by the recovered-execution self-release
	// path to flip RUNNING→QUEUED/FAILED without waiting for the
	// scheduler's recovery-loop grace window. Mirrors the
	// scheduler's interface on the same underlying repository.
	ReleaseLease(ctx context.Context, taskID, leaseID string, newStatus persistence.TaskStatus, opts persistence.ReleaseOptions) error
	// TransitionConditional is the leaseless analog of ReleaseLease.
	// retry-from-step executions don't carry a lease (the scheduler
	// wasn't involved in dispatch), so ReleaseLease's leaseID guard
	// rejects them. releaseRecoveredTask falls back to this on the
	// no-lease path so a recovered-execution failure still
	// transitions to QUEUED (retry) or FAILED (terminal) instead of
	// leaving the row stuck in its non-terminal status.
	TransitionConditional(ctx context.Context, id string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error)
}

// ModelLimit holds output and context limits for a specific model.
// Applied automatically when a role uses that model via its Model override field.
type ModelLimit struct {
	MaxTokens   int
	ContextSize int
}

// worktreeCleanupTimeout bounds the detached context used for
// worktree removal / workspace reset after a task's own context is
// already cancelled (bug-sweep follow-up 2026-06-04). Generous for a
// local `git worktree remove` + `git branch -D`; tight enough that a
// wedged git can't pin the goroutine indefinitely.
const worktreeCleanupTimeout = 2 * time.Minute

// NamedSecret is a project-scoped credential injected into agent containers
// (mirror of config.NamedSecret). Empty AllowedProjects = every project.
type NamedSecret struct {
	Name            string
	Value           string
	AllowedProjects []string
}

// allowsProject reports whether this secret may be injected for projectID.
// An empty allowlist means all projects (the AllowedTools convention).
func (s NamedSecret) allowsProject(projectID string) bool {
	if len(s.AllowedProjects) == 0 {
		return true
	}
	for _, p := range s.AllowedProjects {
		if p == projectID {
			return true
		}
	}
	return false
}

// namedSecretEnv returns the named secrets a project's agents may receive, as
// an env map. A secret with no name/value is skipped. This is the per-secret
// allowlist enforcement point — a credential is only injected into a
// container for an allowed project.
func (e *Executor) namedSecretEnv(projectID string) map[string]string {
	if e == nil || e.config == nil || len(e.config.NamedSecrets) == 0 {
		return nil
	}
	out := make(map[string]string, len(e.config.NamedSecrets))
	for _, s := range e.config.NamedSecrets {
		if s.Name == "" || s.Value == "" {
			continue
		}
		if s.allowsProject(projectID) {
			out[s.Name] = s.Value
		}
	}
	return out
}

// Config holds executor configuration.
type Config struct {
	// DefaultTimeout is the default execution timeout.
	DefaultTimeout time.Duration

	// MaxRetries is the maximum number of retry attempts for failed tasks.
	MaxRetries int

	// RetryDelay is the delay between retry attempts.
	RetryDelay time.Duration

	// ArtifactStoragePath is the base path for artifact storage.
	ArtifactStoragePath string

	// RuntimeImage is the container image used for the current single-step path.
	RuntimeImage string

	// AgentLLMEnv holds the resolved LLM env vars to inject into agent containers.
	AgentLLMEnv map[string]string

	// NamedSecrets are operator-declared, project-scoped credentials injected
	// into agent containers — only for the projects each one allows (per-secret
	// allowlist). Populated from config.NamedSecrets. Kept as a local type so
	// the executor stays decoupled from internal/config.
	NamedSecrets []NamedSecret

	// ModelLimits holds per-model max_tokens and context_size overrides.
	// Applied when a role's Model field selects a specific model.
	ModelLimits map[string]ModelLimit

	// ProjectWorkspacePath is the base dir for per-project persistent workspaces.
	ProjectWorkspacePath string

	// LogLevel is the daemon's log level, passed to agent containers.
	LogLevel string

	// DelegationDepthLimit caps how many levels deep a delegation chain
	// may run. A task whose lineage already has this many DELEGATION-source
	// ancestors is refused further delegation. 0 falls back to the package
	// default (defaultDelegationDepthLimit).
	// See https://docs.vornik.io §3 (Depth Limit).
	DelegationDepthLimit int

	// DelegationFanOutLimit caps how many child tasks a single parent may
	// create in one delegation batch. 0 falls back to the package default
	// (defaultDelegationFanOutLimit).
	// See https://docs.vornik.io §3 (Fan-out Limit).
	DelegationFanOutLimit int

	// ToolBudget is the resolved dynamic-tool-budget config (already
	// defaulted via config.ToolBudgetConfig.Resolved()). Zero value has
	// Enabled=false, so the budget injection is a no-op and roles run on
	// their static VORNIK_MAX_TOOL_ITERATIONS. See
	// https://docs.vornik.io
	ToolBudget toolbudget.Config
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		DefaultTimeout:        30 * time.Minute,
		MaxRetries:            3,
		RetryDelay:            30 * time.Second,
		ArtifactStoragePath:   "/var/lib/vornik/artifacts",
		RuntimeImage:          "vornik-agent:latest",
		DelegationDepthLimit:  defaultDelegationDepthLimit,
		DelegationFanOutLimit: defaultDelegationFanOutLimit,
	}
}

// WarmPool is the interface for warm container pool operations.
type WarmPool interface {
	Acquire(key runtime.PoolKey) *runtime.PoolEntry
	StartWarm(ctx context.Context, key runtime.PoolKey, envOverrides map[string]string) (*runtime.PoolEntry, error)
	InjectTask(entry *runtime.PoolEntry, inputData []byte) error
	WaitForTaskDone(ctx context.Context, entry *runtime.PoolEntry, timeout time.Duration) ([]byte, error)
	Release(entry *runtime.PoolEntry, healthy bool)
}

// MemoryIndexer is the interface for ingesting task output into project memory.
type MemoryIndexer interface {
	IngestText(ctx context.Context, projectID, taskID, artifactID, sourceName, content string) error
	// PatchScopeByArtifact stamps repo_scope on every chunk produced
	// for the given artifact. The executor calls this after IngestText
	// to retag chunks when the task payload carries a repo_scope
	// (migration 75). Empty repoScope is a no-op.
	PatchScopeByArtifact(ctx context.Context, projectID, artifactID, repoScope string) error
}

// IngestQueueEnqueuer is the producer-side surface of
// project_ingest_queue. The executor enqueues per OUTPUT artifact
// at handleSuccess time; the IngestWorker drains. Nil-safe — when
// no queue repo is wired the executor falls back to the legacy
// synchronous IngestText path so deployments without the
// hardening migration keep working.
type IngestQueueEnqueuer interface {
	Enqueue(ctx context.Context, item *persistence.IngestQueueItem) error
}

// CompletionNotifier is called when a task finishes (success or failure).
type CompletionNotifier interface {
	NotifyTaskCompleted(ctx context.Context, task *persistence.Task, success bool, message string)
}

// SteeringNotifier is called when a task enters a "needs the operator" state
// (AWAITING_INPUT / AWAITING_APPROVAL) so the operator who created it from a
// chat/DM gets pushed a prompt instead of having to watch the UI inbox.
// Satisfied by *steering.Notifier; nil-safe via e.notifySteering.
type SteeringNotifier interface {
	NotifySteeringRequired(ctx context.Context, task *persistence.Task, state string)
}

// Executor runs workflow instances for tasks.
type Executor struct {
	config         *Config
	runtime        RuntimeManager
	warmPool       WarmPool
	execRepo       ExecutionRepository
	artifactRepo   ArtifactRepository
	artifactStore  ArtifactStore
	taskRepo       TaskRepository
	auditRepo      persistence.ToolAuditRepository
	recoveryEvents persistence.RecoveryEventRepository
	llmUsageRepo   persistence.TaskLLMUsageRepository
	reservRepo     persistence.BudgetReservationRepository
	outcomeRepo    persistence.ExecutionStepOutcomeRepository
	notifier       CompletionNotifier
	steering       SteeringNotifier
	memoryIndexer  MemoryIndexer
	// systemHandlers backs the `system` workflow step type (B-7).
	// Populated at executor construction via WithSystemHandlers.
	// Nil-safe: a missing handler surfaces the standard
	// "unknown handler" outcome instead of panicking, so daemons
	// that don't wire the RAG handlers still run agent / gate /
	// approval / plan workflows unchanged.
	systemHandlers *SystemHandlerRegistry

	// ghTokenMu guards ghTokens, the per-installation GitHub App
	// installation-token cache. Tokens are minted on demand for agent
	// outbound (git push / gh) when a task's project sets `github` creds, and
	// reused across the task's steps until near expiry. ghMintFn is the mint
	// implementation (overridable in tests; nil → defaultGitHubMint).
	ghTokenMu sync.Mutex
	ghTokens  map[int64]ghCachedToken
	ghMintFn  func(ctx context.Context, apiBase string, appID, installID int64, keyPath string) (string, time.Time, error)
	// ingestQueue (optional) routes ingestion through the
	// project_ingest_queue → IngestWorker pipeline introduced in
	// Phase 1 of the memory hardening roadmap. Nil falls back to
	// synchronous IngestText (legacy path).
	ingestQueue IngestQueueEnqueuer
	// ingestEnqueueFallbackRecorder (optional) is bumped whenever
	// the queue Enqueue fails and ingestOutputArtifacts falls back
	// to the legacy synchronous indexer. Lets the container surface
	// the fallback in a metric without making memory.* a direct
	// executor dependency.
	ingestEnqueueFallbackRecorder func(projectID string)
	// tradingOrderRepo (optional) lets the executor inject a
	// "recent activity" block into the strategist's prompt so
	// next-tick reasoning has structured context (last 24h
	// fills, cancels, refusals) without depending on the LLM
	// remembering to call memory_search. Nil-safe: a nil repo
	// just disables the injection.
	tradingOrderRepo persistence.TradingOrderRepository
	workflows        WorkflowResolver
	// circuitBreaker (optional) auto-pauses a project's autonomy
	// when the rolling failure count crosses the configured
	// threshold. Nil disables the breaker — preserves the
	// pre-feature behaviour for deployments that don't enable it.
	circuitBreaker *circuitBreaker
	metrics        *Metrics
	pricing        *pricing.Table
	// livePub broadcasts per-execution events to subscribers on
	// /api/v1/executions/{id}/live (Feature #3). Nil-safe — the
	// executor's tap helpers no-op when the publisher isn't
	// wired, so existing deployments without the live surface
	// keep their current behaviour.
	livePub livepubsub.Publisher
	// hintRepo lets the executor consume operator-injected
	// hints at step boundaries (Feature #3 Phase C). Nil-safe —
	// when not wired, hint consumption is skipped silently.
	hintRepo persistence.ExecutionHintRepository
	// cpcRepo backs the call_project step type (Phase A of
	// inter-project orchestration; LLD https://docs.vornik.io
	// inter-project-orchestration-design.md). Nil-safe — when
	// not wired, the call_project handler fails the step with
	// a CROSS_PROJECT_DISABLED message. Postgres-only in v1
	// (the SQLite repo branch leaves the field nil).
	cpcRepo persistence.CrossProjectCallRepository
	// spawnRepo backs the spawn_project step type (Phase B).
	// Same nil-safe contract as cpcRepo. Postgres-only in v1.
	spawnRepo persistence.ProjectSpawnRepository
	// templateCatalog gives the spawn handler read-only access
	// to the project-templates manifests + the renderer. Same
	// catalog the gallery handler uses; loaded once at daemon
	// boot. Nil-safe — spawn handler returns
	// CROSS_PROJECT_DISABLED when missing.
	templateCatalog spawnTemplateCatalog
	// configsDir is the daemon's writable configs root (the
	// parent of configs/projects/, configs/swarms/, etc.).
	// The spawn handler writes rendered template output below
	// this path. Empty disables spawn (handler returns
	// CROSS_PROJECT_DISABLED with the missing-dir reason).
	configsDir string
	// registryReloader triggers a registry stage-validate-
	// activate cycle so a freshly-spawned project becomes
	// resolvable immediately (e.g. a downstream call_project
	// step in the same workflow can target it). Nil disables
	// — the file watcher's 5-second poll picks up the new YAML
	// anyway; the synchronous reload is a freshness
	// optimisation, not a correctness requirement.
	registryReloader registryReloader
	// adminAuditRepo writes one row per CPC create, CPC
	// resolve, and project spawn (Phase C observability;
	// LLD §9.4). Nil disables — the lineage rows
	// (cross_project_calls + project_spawns) still serve as
	// the durable record; the audit log is the cross-cutting
	// "who did what when" view operators query from
	// /ui/admin/audit.
	adminAuditRepo persistence.AdminAuditRepository
	// callReceivedDedup tracks executions that have already
	// emitted cross_project_call_received (Phase D §9.1) so a
	// retry / scheduler recovery doesn't re-emit. Per-process
	// state — the event is informational; a daemon restart re-
	// emitting once is acceptable. Lazy-initialised at construct
	// time below.
	callReceivedDedup *liveCallReceivedTracker
	// contextSourceByExecution caches the canonical-context
	// source (dot_autonomy / plain_autonomy / mixed / "") per
	// execution so the step-outcome recorder can stamp the
	// context_source column without re-walking the workspace.
	// Populated at workspace prep; consulted at outcome write.
	// Per-process state — a daemon restart leaves rows with
	// empty context_source until the next step prep runs.
	contextSourceByExecution sync.Map
	// schemaRegistry validates result envelopes' `data` field
	// against the registered JSON-Schema body (Phase D follow-
	// on; LLD §4.2 + §5.3). Nil disables schema validation —
	// the resolve hook falls through to envelope-shape
	// validation only.
	schemaRegistry schemaValidator
	// secretsDetector scans agent output + container logs at the
	// boundary where they enter vornik's persistence + display
	// stores. Per-checkpoint action policy (redact / detect /
	// block) lives in secretsActions; resolved at construction.
	// Nil disables the layer (tests, opted-out deployments).
	secretsDetector secrets.Detector
	secretsActions  map[string]secrets.Action
	// hallucinationDetector scans the agent's result.json prose
	// after each step against a grounding context built from the
	// step's tool_audit_log + artifact list. Signals are
	// persisted on the step outcome row; High-severity findings
	// fail the step so the existing retry path picks it up.
	// Nil disables Phase 1 detection — preserves pre-feature
	// behaviour for deployments that opt out.
	hallucinationDetector *hallucination.Detector
	// hallucinationMetrics observes Phase 1 detector emissions.
	// Nil-safe; the executor still blocks on High signals when
	// metrics is unset.
	hallucinationMetrics *hallucination.Metrics
	// instinctMetrics is the instinct subsystem metrics sink, shared with
	// the instinct worker. The executor bumps vornik_instinct_applications_total
	// when it surfaces a learned recovery remediation (slice 7). Nil-safe:
	// an executor without it records the application row but emits no
	// counter. Set via SetInstinctMetrics so observability can be wired
	// after construction, in any order.
	instinctMetrics *observability.InstinctMetrics
	// judgeRunner runs the Phase 3 LLM-as-judge after a task
	// reaches its terminal status. Per-project opt-in via
	// project.HallucinationJudge.Enabled; nil disables Phase 3
	// entirely (no judge runs regardless of project config).
	//
	// Held as an interface (rather than the concrete
	// *hallucination.JudgeRunner) so handleFailure tests can
	// wire a recording stub without standing up the runner's
	// full repo dependency graph. *hallucination.JudgeRunner
	// trivially satisfies the interface.
	judgeRunner judgeRunnerInterface
	// Phase 25 — conversational task lifecycle.
	// taskMessageRepo + persistTaskRepo let the executor write
	// checkpoint / external_wait / closure_request messages and
	// flip the task to AWAITING_INPUT / AWAITING_EXTERNAL when
	// the lead emits a non-continue outcome. Both nil-safe — when
	// not wired, the executor only handles the legacy plan shape
	// (outcome=continue) and falls through to error on other kinds.
	taskMessageRepo    persistence.TaskMessageRepository
	taskScratchpadRepo persistence.TaskScratchpadRepository
	persistTaskRepo    persistence.TaskRepository // distinct from taskRepo (interface narrower)
	// instinctRepo is the (advisory) continuous-learning instinct
	// repository, Consumer A (slice 3). When wired AND
	// instinctPlaybooks is true, the lead's RECOVERY_CONTEXT block is
	// augmented with worker-mined "similar failures here resolved by …"
	// remediations and an InstinctApplication row is recorded. Both nil/
	// false-safe: with either off the recovery prompt is byte-for-byte
	// identical to today (the static alternatives table only). Read-mostly:
	// the only write is the application/feedback row, never the audit spine.
	instinctRepo      persistence.InstinctRepository
	instinctPlaybooks bool
	// instinctBudgetResolver is the contracts.InstinctBudgetResolver injected
	// from the service layer (EE wires a real impl; CE wires nil). Nil means
	// "no learned tier" — the executor falls back to its default budget exactly
	// as today (same as the ok==false path). Set via WithInstinctBudgetResolver.
	instinctBudgetResolver contracts.InstinctBudgetResolver
	// instinctToolBudget gates the Slice-4 active budget consumer: when on
	// AND instinctBudgetResolver is non-nil, LearnedTier is consulted on the
	// absent-verdict path to supply a learned complexity tier to
	// toolbudget.Resolve. An explicit planner verdict always wins — this only
	// fills the gap. Default false: budget instincts are mined and surfaced
	// advisory but never change a budget.
	instinctToolBudget bool
	// instinctAutoApply (v2) gates prompt-level auto-apply: when on, a
	// surfaced recovery remediation that clears the confidence floor (and the
	// optional error-class allowlist) is rendered as a DIRECTIVE rather than
	// advisory and its application row is recorded 'auto_applied' instead of
	// 'ignored'. Zero value = off (advisory only), so behaviour is unchanged
	// unless an operator opts in via instinct.consumers.auto_apply.
	instinctAutoApply autoApplyConfig
	// apiKeyMinter mints a scoped per-task bearer key before the
	// container starts and revokes it after the step finishes (both
	// success and failure paths). Nil disables per-task keys — the
	// static VORNIK_API_KEY from AgentLLMEnv passes through unchanged,
	// preserving the pre-feature behaviour for sqlite/dev deployments.
	apiKeyMinter APIKeyMinter
	tracer       trace.Tracer
	logger       zerolog.Logger

	// mu protects activeExecutions map.
	mu sync.Mutex

	// activeExecutions tracks currently running executions.
	activeExecutions map[string]*executionHandle

	// workspaceLock serializes git operations per-project. It is the
	// SAME shared *workspacelock.Locker the service container injects
	// into the UI artifact-delete path and the API (git-over-HTTPS)
	// server, so every workspace writer on this node is mutually
	// exclusive per project ("lock-on-mutation"). NewWithOptions sets a
	// fallback workspacelock.New() when none is injected; struct-literal
	// constructions (tests) get one lazily via wsLock(). Always reach it
	// through wsLock(), never directly.
	workspaceLock *workspacelock.Locker

	// ctx and cancel control execution lifecycle.
	ctx    context.Context
	cancel context.CancelFunc

	// wg tracks in-flight runExecution goroutines so Stop() can wait.
	wg sync.WaitGroup

	// shuttingDown signals that a graceful shutdown is in progress.
	// New ExecuteWithContext calls reject so the scheduler doesn't
	// hand us more work mid-pause. Set by Shutdown(); cleared on
	// next process start (the field is on the Executor instance,
	// which doesn't survive a restart).
	shuttingDown bool
}

// wsLock returns the per-project workspace lock, lazily allocating a private
// fallback when the executor was built without one (struct-literal
// constructions in tests bypass NewWithOptions, which otherwise installs the
// fallback / container-injected shared instance). Production always goes
// through NewWithOptions + WithWorkspaceLock, so the lazy branch is a
// test-only safety net. Allocation is guarded by e.mu so concurrent first
// calls agree on one instance.
func (e *Executor) wsLock() *workspacelock.Locker {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.workspaceLock == nil {
		e.workspaceLock = workspacelock.New()
	}
	return e.workspaceLock
}

// schemaValidator is the narrow surface the resolve hook needs
// from *schemaregistry.Registry. Decoupled so tests can inject
// a fake without touching the on-disk loader.
type schemaValidator interface {
	HasSchema(id string) bool
	Validate(id string, envelope any) error
}

// executionHandle tracks an active execution.
type executionHandle struct {
	taskID      string
	projectID   string
	containerID string
	startedAt   time.Time
	cancel      context.CancelFunc
	// ctx holds the execution context with trace span
	ctx context.Context
	// recovered is true when this execution was started by the
	// post-restart recovery path (Recover → recoverExecution),
	// not by the live scheduler dispatch (ExecuteWithContext).
	// The distinction matters at finish time: recovered
	// executions have NO scheduler.dispatchViaExecutor goroutine
	// watching them, so when their runExecution returns, nobody
	// calls scheduler.TaskCompleted to release the lease + flip
	// the task from RUNNING to QUEUED/FAILED. Without the flag
	// the task stays RUNNING in the DB until the recovery loop's
	// 90-second idle-grace window expires (observed
	// 2026-05-07: task task_20260507204558_88382830f8da1aaf
	// stuck RUNNING for 3+ minutes after its 10th attempt
	// failed during a daemon restart). cleanupExecution reads
	// this flag and self-releases when set.
	recovered bool
}

// ArtifactStore persists generated files into the artifact store
// and reads them back. Retrieve was added when the executor started
// routing artifact reads through the backend-aware Store (so S3
// deployments work as well as filesystem) — phase 4 storage
// abstraction follow-up.
type ArtifactStore interface {
	Store(ctx context.Context, projectID, executionID, taskID, name, sourcePath string) (*persistence.Artifact, error)
	Retrieve(ctx context.Context, artifactID string) ([]byte, error)
}

// WorkflowResolver provides read-only access to project, swarm, and workflow config.
type WorkflowResolver interface {
	GetProject(id string) *registry.Project
	GetSwarm(id string) *registry.Swarm
	GetWorkflow(id string) *registry.Workflow
}

// Option is a functional option for configuring the Executor.
type Option func(*Executor)

// WithWarmPool sets the warm container pool for the executor.
func WithWarmPool(pool WarmPool) Option {
	return func(e *Executor) {
		e.warmPool = pool
	}
}

// WithWorkspaceLock injects the shared per-project workspace lock. The service
// container builds ONE *workspacelock.Locker and passes the SAME instance here
// and into the UI + API servers, so every workspace writer on this node is
// mutually exclusive per project. When this option is omitted the constructor
// falls back to a private workspacelock.New() (correct in isolation; only the
// container-injected shared instance gives cross-subsystem exclusion).
func WithWorkspaceLock(l *workspacelock.Locker) Option {
	return func(e *Executor) {
		if l != nil {
			e.workspaceLock = l
		}
	}
}

// WithToolAuditRepository sets the tool audit log repository.
func WithToolAuditRepository(repo persistence.ToolAuditRepository) Option {
	return func(e *Executor) {
		e.auditRepo = repo
	}
}

// WithSystemHandlers wires the executor's SystemHandlerRegistry
// (B-7). Daemons that include the document-ingest workflow pass
// a populated registry (rag.extract + rag.index); daemons that
// don't can pass nil — the `system` step type still validates,
// but executing an unregistered handler surfaces a clear
// "unknown handler" outcome instead of panicking.
func WithSystemHandlers(reg *SystemHandlerRegistry) Option {
	return func(e *Executor) {
		e.systemHandlers = reg
	}
}

// WithConversationalLifecycle wires the repositories the executor
// needs to consume the lead's outcome envelope (Phase 25 of the
// conversational task lifecycle). Both repos must be non-nil for
// the path to activate — partial wiring leaves the legacy
// plan-only handling in place.
func WithConversationalLifecycle(
	msgRepo persistence.TaskMessageRepository,
	scratchpad persistence.TaskScratchpadRepository,
	taskPersistRepo persistence.TaskRepository,
) Option {
	return func(e *Executor) {
		e.taskMessageRepo = msgRepo
		e.taskScratchpadRepo = scratchpad
		e.persistTaskRepo = taskPersistRepo
	}
}

// WithLLMUsageRepository sets the per-step LLM usage persistence repo. A nil
// repo disables DB writes — Prometheus metrics still fire, the UI spend panel
// just shows "n/a" because there's no row to drill into.
func WithLLMUsageRepository(repo persistence.TaskLLMUsageRepository) Option {
	return func(e *Executor) {
		e.llmUsageRepo = repo
	}
}

// WithBudgetReservationRepository sets the budget-reservation ledger so the
// executor settles a task's in-flight reservation when it reaches a terminal
// state (trading-hardening §1). A nil repo disables settlement here — the
// watchdog's terminal-and-stale sweep is the backstop that still reaps the
// reservation, so this is purely a latency optimization (settle promptly vs.
// at the next sweep).
func WithBudgetReservationRepository(repo persistence.BudgetReservationRepository) Option {
	return func(e *Executor) {
		e.reservRepo = repo
	}
}

// settleBudgetReservation drops a terminated task's in-flight budget
// reservation so it stops counting against the project's hard cap. Best
// effort + idempotent: logs and moves on if it fails (the watchdog sweep is
// the backstop), and a no-op when no reservation repo is wired or the task
// never reserved.
func (e *Executor) settleBudgetReservation(ctx context.Context, taskID string) {
	if e == nil || e.reservRepo == nil || taskID == "" {
		return
	}
	if _, err := e.reservRepo.SettleByTask(ctx, taskID, time.Now().UTC()); err != nil {
		e.logger.Warn().Err(err).Str("task_id", taskID).
			Msg("budget reservation settle failed — watchdog sweep will reap it")
	}
}

// WithStepOutcomeRepository sets the per-step outcome repo. A nil repo
// disables outcome tracking — executions still run, the execution_step_outcomes
// table just stays empty for those runs. All writes in this package guard on
// e.outcomeRepo != nil so Phase-2 bring-up can roll out independent of the
// repo wiring.
func WithStepOutcomeRepository(repo persistence.ExecutionStepOutcomeRepository) Option {
	return func(e *Executor) {
		e.outcomeRepo = repo
	}
}

// WithInstinctPlaybooks wires the continuous-learning instinct
// repository so the lead's RECOVERY_CONTEXT block can be augmented with
// worker-mined recovery remediations (Consumer A, slice 3). enabled
// mirrors the instinct.consumers.failure_playbooks gate; the container
// passes false (and/or a nil repo) whenever the gate is off, so the
// recovery prompt stays byte-for-byte identical to today. Advisory only:
// the overlay never auto-pivots recovery — the operator still approves.
func WithInstinctPlaybooks(repo persistence.InstinctRepository, enabled bool) Option {
	return func(e *Executor) {
		e.instinctRepo = repo
		e.instinctPlaybooks = enabled
	}
}

// WithInstinctBudgetResolver injects the contracts.InstinctBudgetResolver used
// by the Slice-4 active budget consumer. A nil resolver means "no learned tier"
// — the executor falls back to its default budget (same as today's Community
// behaviour). EE wires a real impl; CE wires nil (or omits the call entirely).
func WithInstinctBudgetResolver(r contracts.InstinctBudgetResolver) Option {
	return func(e *Executor) {
		e.instinctBudgetResolver = r
	}
}

// WithInstinctToolBudget gates the Slice-4 active budget consumer. When
// enabled is true AND a non-nil instinctBudgetResolver is wired (via
// WithInstinctBudgetResolver), LearnedTier is consulted on the absent-verdict
// path to supply a learned complexity tier to toolbudget.Resolve. An explicit
// planner verdict always wins.
func WithInstinctToolBudget(enabled bool) Option {
	return func(e *Executor) {
		e.instinctToolBudget = enabled
	}
}

// autoApplyConfig is the executor-side view of instinct.consumers.auto_apply.
type autoApplyConfig struct {
	enabled         bool
	minConfidence   float64
	minCleanSupport int                 // 0 = off (support/contradict ignored)
	allowedClasses  map[string]struct{} // empty = all classes eligible
}

// eligible reports whether a remediation qualifies for prompt-level
// auto-apply. The clean-support gate (minCleanSupport > 0) requires at least
// minCleanSupport corroborations AND zero contradictions on top of the
// confidence floor — this lets a lowered confidence bar stay safe by demanding
// enough clean evidence, and rejects a decayed-but-now-mixed instinct even
// when its confidence still clears the floor (instinct-auto-apply supply
// design, 2026-06-23). support/contradict are the remediation's CURRENT row
// tallies; minCleanSupport == 0 preserves the pre-supply behaviour exactly.
func (c autoApplyConfig) eligible(confidence float64, support, contradict int, errorClass string) bool {
	if !c.enabled || confidence < c.minConfidence {
		return false
	}
	if c.minCleanSupport > 0 && (support < c.minCleanSupport || contradict > 0) {
		return false
	}
	if len(c.allowedClasses) == 0 {
		return true
	}
	_, ok := c.allowedClasses[errorClass]
	return ok
}

// WithInstinctAutoApply wires the v2 prompt-level auto-apply gate. enabled
// off (the default) keeps learned remediations advisory. minConfidence <= 0
// with enabled is treated as the 0.85 default. minCleanSupport > 0 adds the
// clean-evidence gate (>= minCleanSupport corroborations AND zero
// contradictions); 0 leaves it off (pre-supply behaviour). allowedClasses
// empty = every class meeting the floor is eligible.
func WithInstinctAutoApply(enabled bool, minConfidence float64, minCleanSupport int, allowedClasses []string) Option {
	return func(e *Executor) {
		if enabled && minConfidence <= 0 {
			minConfidence = 0.85
		}
		if minCleanSupport < 0 {
			minCleanSupport = 0
		}
		var set map[string]struct{}
		if len(allowedClasses) > 0 {
			set = make(map[string]struct{}, len(allowedClasses))
			for _, c := range allowedClasses {
				if c != "" {
					set[c] = struct{}{}
				}
			}
		}
		e.instinctAutoApply = autoApplyConfig{
			enabled:         enabled,
			minConfidence:   minConfidence,
			minCleanSupport: minCleanSupport,
			allowedClasses:  set,
		}
	}
}

// WithSecrets wires the secret-leak detector + the per-checkpoint
// action map. Both nil disables the secrets layer entirely
// (tests, opt-out deployments) — callers that pre-resolved
// actions hand them in here so the executor doesn't re-walk
// the operator config every step.
func WithSecrets(d secrets.Detector, actions map[string]secrets.Action) Option {
	return func(e *Executor) {
		e.secretsDetector = d
		e.secretsActions = actions
	}
}

// WithTradingOrderRepo wires the trading_orders read repo so
// the executor can build a structured "recent activity" block
// for the strategist's prompt. Nil disables the injection;
// non-trading workflows are unaffected (the block builder
// short-circuits when no recent rows exist).
func WithTradingOrderRepo(r persistence.TradingOrderRepository) Option {
	return func(e *Executor) {
		e.tradingOrderRepo = r
	}
}

// WithAPIKeyMinter wires the per-task API-key lifecycle into the
// executor. When set, startContainer mints a scoped key for each
// container run and the step teardown revokes it — so a
// prompt-injected agent holds one project's key for one task's
// lifetime only. Nil (the default) leaves the static
// AgentLLMEnv["VORNIK_API_KEY"] untouched (sqlite/dev posture).
func WithAPIKeyMinter(m APIKeyMinter) Option {
	return func(e *Executor) {
		e.apiKeyMinter = m
	}
}

// WithHallucinationDetector wires the Phase 1 claim-grounding
// detector. Nil disables the layer — preserves the pre-feature
// behaviour for opt-out deployments. The detector runs after
// verifyClaimedFiles on every agent step; signals land on the
// step's outcome row, and High-severity signals fail the step
// so the existing scheduler retry path picks it up.
func WithHallucinationDetector(d *hallucination.Detector) Option {
	return func(e *Executor) {
		e.hallucinationDetector = d
	}
}

// WithHallucinationMetrics wires the Prometheus sink so each Phase
// 1 detector emission on a step bumps a counter labelled by
// severity + detector name. Nil-safe.
func WithHallucinationMetrics(m *hallucination.Metrics) Option {
	return func(e *Executor) {
		e.hallucinationMetrics = m
	}
}

// judgeRunnerInterface is the minimal contract handleSuccess /
// handleFailure need from the judge subsystem. Allows tests to
// substitute a recording stub for the production
// *hallucination.JudgeRunner — which carries a deep repo graph
// (Verdicts, Audits, Artifacts, Executions, Pricing, LLMUsage)
// that's overkill for verifying the executor's call-site policy.
type judgeRunnerInterface interface {
	Run(ctx context.Context, task *persistence.Task) error
}

// WithJudgeRunner wires the Phase 3 LLM-as-judge runner. The
// runner fires once per task on terminal status when the
// project's HallucinationJudge.Enabled flag is set. Nil
// disables Phase 3 altogether.
func WithJudgeRunner(r *hallucination.JudgeRunner) Option {
	return func(e *Executor) {
		// Nil-on-nil-interface trap: assigning a typed-nil
		// *hallucination.JudgeRunner to a non-nil interface var
		// makes e.judgeRunner != nil even though the underlying
		// pointer is nil. fireJudgeIfEnabled's nil-check would
		// then pass and crash on the .Run call. Guard here.
		if r == nil {
			e.judgeRunner = nil
			return
		}
		e.judgeRunner = r
	}
}

// WithLogger sets the logger for the executor.
func WithLogger(l zerolog.Logger) Option {
	return func(e *Executor) {
		e.logger = l
	}
}

// WithSteeringNotifier sets the steering-notification sink (AWAITING_INPUT /
// AWAITING_APPROVAL → push to the originating chat). Nil disables it.
func WithSteeringNotifier(n SteeringNotifier) Option {
	return func(e *Executor) { e.steering = n }
}

// notifySteering is the nil-safe call site for steering notifications.
func (e *Executor) notifySteering(ctx context.Context, task *persistence.Task, state string) {
	if e == nil || e.steering == nil || task == nil {
		return
	}
	e.steering.NotifySteeringRequired(ctx, task, state)
}

// WithCompletionNotifier sets the callback for task completion notifications.
func WithCompletionNotifier(n CompletionNotifier) Option {
	return func(e *Executor) {
		e.notifier = n
	}
}

// WithCircuitBreaker wires the per-project failure-rate breaker.
// Pass a fully-built *circuitBreaker (constructed via
// newCircuitBreaker) — nil leaves the breaker disabled and the
// executor's failure path skips the trip evaluation entirely.
func WithCircuitBreaker(cb *circuitBreaker) Option {
	return func(e *Executor) {
		e.circuitBreaker = cb
	}
}

// SetCompletionNotifier sets the notifier after construction.
// Guarded by e.mu: runExecution goroutines read e.notifier
// concurrently, so unguarded writes (pre-2026-05-29) were a
// data race the audit-agent flagged. Config-reload paths that
// reach this setter mid-flight no longer race the executor.
func (e *Executor) SetCompletionNotifier(n CompletionNotifier) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notifier = n
}

// SetCircuitBreaker wires the per-project failure-rate breaker
// after construction. The container builds the breaker only once
// the notifier and registry are both wired (it depends on both),
// so this setter exists alongside the WithCircuitBreaker option
// for the late-binding case. Guarded by e.mu — same rationale as
// SetCompletionNotifier above.
func (e *Executor) SetCircuitBreaker(cb *circuitBreaker) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.circuitBreaker = cb
}

// WithMetrics sets the metrics instance for the executor.
func WithMetrics(metrics *Metrics) Option {
	return func(e *Executor) {
		e.metrics = metrics
	}
}

// WithLivePublisher wires the live-events publisher (Feature #3
// Phase A) so step boundaries broadcast to WebSocket subscribers.
// Nil disables — pre-feature deployments stay unchanged.
func WithLivePublisher(pub livepubsub.Publisher) Option {
	return func(e *Executor) {
		e.livePub = pub
	}
}

// WithRecoveryEventRepository wires the recovery-event marker store. The
// executor records one event each time an execution reaches a Recovery:true
// terminal. Nil disables — recovery markers simply aren't persisted.
func WithRecoveryEventRepository(repo persistence.RecoveryEventRepository) Option {
	return func(e *Executor) {
		e.recoveryEvents = repo
	}
}

// WithHintRepository wires the operator-hint repo so the executor
// can consume pending hints at step boundaries (Feature #3
// Phase C). nil disables — hint injection is silently a no-op.
func WithHintRepository(repo persistence.ExecutionHintRepository) Option {
	return func(e *Executor) {
		e.hintRepo = repo
	}
}

// WithCrossProjectCallRepository wires the inter-project
// orchestration repo so the executor can handle `call_project`
// steps (Phase A). Nil disables — the step handler fails with
// CROSS_PROJECT_DISABLED and the workflow's on_fail branch fires.
func WithCrossProjectCallRepository(repo persistence.CrossProjectCallRepository) Option {
	return func(e *Executor) {
		e.cpcRepo = repo
	}
}

// WithProjectSpawnRepository wires the spawn_project lineage
// repo (Phase B). Nil disables — same fail-soft contract as the
// call_project pair.
func WithProjectSpawnRepository(repo persistence.ProjectSpawnRepository) Option {
	return func(e *Executor) {
		e.spawnRepo = repo
	}
}

// WithProjectTemplateCatalog wires the project-templates catalog
// the gallery handler uses, so the spawn handler can resolve
// the template by slug and render it with the step's params.
// Pass *templates.Catalog from the service container's existing
// load. Nil disables spawn (fails with CROSS_PROJECT_DISABLED).
func WithProjectTemplateCatalog(catalog spawnTemplateCatalog) Option {
	return func(e *Executor) {
		e.templateCatalog = catalog
	}
}

// WithConfigsDir wires the writable configs root path so the
// spawn handler can write rendered project YAML below
// <configsDir>/projects/. Empty disables spawn.
func WithConfigsDir(dir string) Option {
	return func(e *Executor) {
		e.configsDir = dir
	}
}

// WithRegistryReloader wires a reload trigger so a spawned
// project becomes immediately resolvable (instead of waiting
// for the file watcher's next 5-second poll). Implementations
// expose a single Reload() error method — matches the shape
// of *config.ConfigReloader. Nil falls through to the watcher
// for eventual consistency.
func WithRegistryReloader(rl registryReloader) Option {
	return func(e *Executor) {
		e.registryReloader = rl
	}
}

// WithAdminAuditRepository wires the admin audit log so
// inter-project-orchestration events (CPC create, CPC resolve,
// project spawn) write one row each (LLD §9.4). Nil disables
// — the lineage tables still carry the durable record.
func WithAdminAuditRepository(repo persistence.AdminAuditRepository) Option {
	return func(e *Executor) {
		e.adminAuditRepo = repo
	}
}

// WithSchemaRegistry wires the JSON-Schema validator the
// resolve hook uses to check envelope.data against the
// registered schema body (Phase D follow-on; LLD §4.2). Nil
// disables — the resolve hook falls through to envelope-shape
// validation only.
func WithSchemaRegistry(reg schemaValidator) Option {
	return func(e *Executor) {
		e.schemaRegistry = reg
	}
}

// WithPrometheusRegistry creates metrics with the given Prometheus registry.
// This is a convenience option that creates a new Metrics instance.
func WithPrometheusRegistry(registry *prometheus.Registry) Option {
	return func(e *Executor) {
		if registry != nil {
			e.metrics = NewMetrics(registry)
		}
	}
}

// WithConfig sets the executor configuration.
func WithConfig(config *Config) Option {
	return func(e *Executor) {
		e.config = config
	}
}

// WithTracer sets the tracer for the executor.
func WithTracer(tracer trace.Tracer) Option {
	return func(e *Executor) {
		e.tracer = tracer
	}
}

// WithArtifactStore sets the artifact store used for persisted outputs.
func WithArtifactStore(store ArtifactStore) Option {
	return func(e *Executor) {
		e.artifactStore = store
	}
}

// WithWorkflowResolver sets the registry-backed config resolver.
// A nil resolver is ignored to prevent typed-nil interface panics.
func WithWorkflowResolver(resolver WorkflowResolver) Option {
	return func(e *Executor) {
		if resolver != nil {
			e.workflows = resolver
		}
	}
}

// SetWorkflowResolver updates the registry-backed config resolver after construction.
// A nil resolver is ignored to prevent typed-nil interface panics.
// Guarded by e.mu — runExecution reads e.workflows concurrently
// (resolveExecutionPlan, cleanExcludesFor); pre-2026-05-29 the
// unguarded write was a data race.
func (e *Executor) SetWorkflowResolver(resolver WorkflowResolver) {
	if e == nil || resolver == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.workflows = resolver
}

// WithMemoryIndexer sets the memory indexer used to ingest task output artifacts.
func WithMemoryIndexer(indexer MemoryIndexer) Option {
	return func(e *Executor) {
		e.memoryIndexer = indexer
	}
}

// WithIngestQueue routes memory ingestion through the
// project_ingest_queue (Phase 1 of memory hardening). When set,
// ingestOutputArtifacts enqueues per-artifact rows instead of
// calling IngestText synchronously. The IngestWorker drains the
// queue. Nil-safe: omitting this option preserves the legacy
// synchronous path.
func WithIngestQueue(q IngestQueueEnqueuer) Option {
	return func(e *Executor) {
		e.ingestQueue = q
	}
}

// WithIngestEnqueueFallbackRecorder wires a callback the executor
// invokes whenever the queue Enqueue fails and ingestOutputArtifacts
// falls back to the legacy synchronous IngestText path. Pre-fix this
// fallback was visible only in a Warn log, so an upstream queue
// outage looked silent on dashboards — the callback exists so the
// container can bump a Prometheus counter without making memory.*
// a direct dependency of the executor package. Nil-safe.
func WithIngestEnqueueFallbackRecorder(rec func(projectID string)) Option {
	return func(e *Executor) {
		e.ingestEnqueueFallbackRecorder = rec
	}
}

// WithPricing sets the model pricing table used to derive cost metrics. A nil
// table disables cost emission — tokens still go to Prometheus, just without
// the $ counter.
func WithPricing(t *pricing.Table) Option {
	return func(e *Executor) {
		e.pricing = t
	}
}

// SetPricing swaps the pricing table at runtime. Safe to call from config-reload.
// A nil table disables cost emission.
func (e *Executor) SetPricing(t *pricing.Table) {
	if e == nil {
		return
	}
	e.pricing = t
	// Keep the metrics layer's label catalog in sync so model labels stay
	// bucketed to the known-model set after a reload.
	e.metrics.SetModelCatalog(t)
}

// NewWithOptions creates a new Executor instance with functional options.
func NewWithOptions(runtime RuntimeManager, execRepo ExecutionRepository, artifactRepo ArtifactRepository, taskRepo TaskRepository, config *Config, opts ...Option) *Executor {
	if config == nil {
		config = DefaultConfig()
	}

	e := &Executor{
		config:            config,
		runtime:           runtime,
		execRepo:          execRepo,
		artifactRepo:      artifactRepo,
		taskRepo:          taskRepo,
		activeExecutions:  make(map[string]*executionHandle),
		tracer:            otel.Tracer("vornik.io/vornik/internal/executor"),
		callReceivedDedup: newLiveCallReceivedTracker(),
		systemHandlers:    NewSystemHandlerRegistry(),
	}

	// Apply functional options
	for _, opt := range opts {
		opt(e)
	}

	// Fall back to a private workspace lock when none was injected, so
	// standalone constructions (tests, callers that don't wire the
	// container) still serialise this executor's own per-project git
	// mutations. The container injects the shared instance via
	// WithWorkspaceLock for cross-subsystem exclusion.
	if e.workspaceLock == nil {
		e.workspaceLock = workspacelock.New()
	}

	// Sync the metrics label catalog with the pricing table regardless of the
	// order WithPricing / WithPrometheusRegistry were supplied in, so model
	// labels are bucketed from the first metric — including outcome counters
	// that fire before any LLM-usage call.
	if e.pricing != nil {
		e.metrics.SetModelCatalog(e.pricing)
	}

	return e
}

// Execute starts a workflow for the given task.
// This is the main entry point for task execution.
func (e *Executor) Execute(taskID string) error {
	return e.ExecuteWithContext(context.Background(), taskID)
}

// ResumePaused resumes a PAUSED execution in-process — same code
// path as the startup Recover() loop's auto-resume, but
// triggerable on demand without a daemon restart. Added 2026.6.0
// for the retry-from-step UI surface, which sets an execution to
// Paused with PauseReasonRetryFromStep after rewinding state, then
// calls this method to kick the resume immediately.
//
// Refuses if:
//   - the execution doesn't exist or isn't Paused;
//   - the pause reason isn't one of the auto-resumable set
//     (PauseReasonShutdown, PauseReasonRetryFromStep) — operator
//     pauses and awaiting-children pauses still require their own
//     explicit signal;
//   - the task is already in a terminal state (FAILED, COMPLETED,
//     CANCELLED) and not re-armed by the caller (retry-from-step
//     handler flips task to RUNNING before calling this).
//
// Idempotent: a second call while the same execution is already
// running in this process returns nil (the existing recoverExecution
// short-circuits on activeExecutions membership).
func (e *Executor) ResumePaused(execID string) error {
	if execID == "" {
		return fmt.Errorf("execution ID required")
	}
	ctx := context.Background()
	if e.ctx != nil {
		ctx = e.ctx
	}
	exec, err := e.execRepo.Get(ctx, execID)
	if err != nil {
		return fmt.Errorf("load execution %s: %w", execID, err)
	}
	if exec == nil {
		return fmt.Errorf("execution %s not found", execID)
	}
	if exec.Status != persistence.ExecutionStatusPaused {
		return fmt.Errorf("execution %s is %s, not paused", execID, exec.Status)
	}
	state := loadExecutionState(exec)
	if state.PausedReason != PauseReasonShutdown &&
		state.PausedReason != PauseReasonRetryFromStep {
		return fmt.Errorf("execution %s pause reason %q is not auto-resumable", execID, state.PausedReason)
	}
	// Flip the row to RUNNING before calling recoverExecution,
	// mirroring the Recover() loop. recoverExecution does the rest
	// of the lifecycle wiring (worktree, goroutine spawn).
	if uerr := e.execRepo.UpdateStatus(ctx, exec.ID, persistence.ExecutionStatusRunning); uerr != nil {
		return fmt.Errorf("flip paused→running: %w", uerr)
	}
	exec.Status = persistence.ExecutionStatusRunning
	e.logger.Info().
		Str("execution_id", exec.ID).
		Str("task_id", exec.TaskID).
		Str("step", state.CurrentStepID).
		Str("paused_reason", state.PausedReason).
		Msg("executor: in-process resume of paused execution")
	return e.recoverExecution(ctx, exec)
}

// Recover restarts in-flight executions left in RUNNING state after a daemon
// restart, and also sweeps PENDING executions whose tasks already reached a
// terminal state (orphans that would otherwise block config reloads forever).
// PAUSED executions are intentionally left untouched and require explicit
// operator resume.
func (e *Executor) Recover(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// recoverExecution handles three paths:
	//   - RUNNING:  daemon was killed; resume from the last
	//               checkpoint (or mark ORPHANED if the task is
	//               already terminal).
	//   - PENDING:  daemon crashed between Create() and the first
	//               status transition; same recovery path.
	//   - PAUSED with state.PausedReason == "shutdown":
	//               daemon was stopped cleanly. Resume from the
	//               checkpoint stamped during Shutdown(). Other
	//               PAUSED reasons (operator pause, awaiting
	//               children) stay paused — those need an external
	//               signal to resume.
	statuses := []persistence.ExecutionStatus{
		persistence.ExecutionStatusRunning,
		persistence.ExecutionStatusPending,
		persistence.ExecutionStatusPaused,
	}

	// Build the preserve set of task IDs whose worktrees must
	// survive the startup prune. Live evidence: T-4ae6 (2026-05-10)
	// had a RUNNING execution when the daemon restarted; the
	// startup prune deleted its worktree before recoverExecution
	// could adopt it, then the recovered goroutine's next podman
	// step hit `statfs: no such file or directory` and the task
	// terminal-failed. Pre-fix the prune ran before the recovery
	// scan and assumed every worktree was orphaned.
	preserve := make(map[string]struct{})
	for _, status := range statuses {
		s := status
		execs, err := e.execRepo.List(ctx, persistence.ExecutionFilter{
			Status:   &s,
			PageSize: 1000,
		})
		if err != nil {
			e.logger.Warn().Err(err).Str("status", string(s)).
				Msg("executor recovery: failed to enumerate executions for worktree-preserve set; pruning may delete in-flight worktrees")
			continue
		}
		for _, ex := range execs {
			if ex == nil || ex.TaskID == "" {
				continue
			}
			preserve[ex.TaskID] = struct{}{}
		}
	}
	// Prune stale worktree metadata left by a previous daemon run.
	// Worktrees for tasks in `preserve` are kept untouched.
	e.pruneAllWorktrees(ctx, preserve)
	for _, status := range statuses {
		s := status
		executions, err := e.execRepo.List(ctx, persistence.ExecutionFilter{
			Status:   &s,
			PageSize: 1000,
		})
		if err != nil {
			return fmt.Errorf("failed to list %s executions: %w", status, err)
		}
		for _, execution := range executions {
			if execution == nil {
				continue
			}
			// Orphan sweep for RUNNING rows: a SIGKILL'd daemon (or a
			// pauseWithReason path that hit a DB error before the
			// UpdateStatus(PAUSED) write) leaves the exec row at
			// RUNNING with no live container. Without this check,
			// recoverExecution below would spawn a fresh goroutine
			// trying to drive a dead container, the scheduler would
			// concurrently re-lease the same task (because the
			// executor's self-release moved the task back to QUEUED
			// during shutdown), and we'd end up with two RUNNING
			// execution rows for one task — observed 2026-05-12 on
			// task_20260512211821 (exec_20260512211822 staying RUNNING
			// while exec_20260512213142 ran as the actual retry).
			//
			// Marking the orphan FAILED with class ORPHANED here lets
			// the new execution proceed cleanly and gives operators a
			// real audit trail for the abandoned attempt.
			if status == persistence.ExecutionStatusRunning && e.runtime != nil {
				container, err := e.runtime.GetContainerByTask(ctx, execution.TaskID)
				if err == nil && (container == nil || container.ID == "") {
					if recErr := e.execRepo.RecordFailure(ctx, execution.ID,
						"container missing on daemon boot; the daemon was killed before pauseWithReason could write PAUSED. Marking ORPHANED so the scheduler can re-dispatch.",
						persistence.TaskFailureClassOrphaned); recErr != nil {
						e.logger.Warn().Err(recErr).
							Str("execution_id", execution.ID).
							Str("task_id", execution.TaskID).
							Msg("executor recovery: orphan sweep failed to record failure; recoverExecution will be tried as a fallback")
					} else {
						e.logger.Info().
							Str("execution_id", execution.ID).
							Str("task_id", execution.TaskID).
							Msg("executor recovery: marked RUNNING execution ORPHANED — no live container found")
						continue
					}
				} else if err != nil {
					// Runtime probe failed; do NOT assume orphan. A
					// transient podman socket hiccup must not cascade
					// every RUNNING row to FAILED. Log and fall through
					// to recoverExecution which has its own checks.
					e.logger.Warn().Err(err).
						Str("task_id", execution.TaskID).
						Msg("executor recovery: container liveness probe failed; falling back to recoverExecution")
				}
			}
			if status == persistence.ExecutionStatusPaused {
				// Only auto-resume the shutdown-paused and
				// retry-from-step subsets. Operator-paused /
				// awaiting-children executions stay where they are.
				state := loadExecutionState(execution)
				if state.PausedReason != PauseReasonShutdown &&
					state.PausedReason != PauseReasonRetryFromStep {
					continue
				}
				e.logger.Info().
					Str("execution_id", execution.ID).
					Str("task_id", execution.TaskID).
					Str("step", state.CurrentStepID).
					Str("paused_reason", state.PausedReason).
					Msg("executor: recovering paused execution")
				// Flip back to RUNNING before resuming so the row
				// reflects live state again. recoverExecution does
				// the rest of the lifecycle wiring.
				if uerr := e.execRepo.UpdateStatus(ctx, execution.ID, persistence.ExecutionStatusRunning); uerr != nil {
					e.logger.Warn().Err(uerr).Str("execution_id", execution.ID).
						Msg("executor: failed to flip PAUSED->RUNNING during recovery; skipping")
					continue
				}
				execution.Status = persistence.ExecutionStatusRunning
			}
			if err := e.recoverExecution(ctx, execution); err != nil {
				// Log and continue: a single bad task row (corrupt /
				// deleted / DB-unavailable) must not strand every
				// subsequent RUNNING execution. Returning here would
				// leave them all stuck in RUNNING with no goroutine
				// driving them, only resolved by the scheduler's
				// recovery loop after its 90-second idle-grace window
				// — once per task per restart.
				e.logger.Warn().Err(err).
					Str("execution_id", execution.ID).
					Str("task_id", execution.TaskID).
					Msg("executor: failed to recover one execution; continuing with the rest")
				continue
			}
		}
	}

	// Self-heal stranded WAITING_FOR_CHILDREN parents (2026-05-26):
	// re-check parents whose children have all reached terminal status
	// but who never got the "child terminated" notify — historically
	// this happened when the closure_request handoff path skipped
	// checkParentUnblock (fixed 2026-05-26, T-a8e1 evidence). Even
	// with that bug fixed, daemon restarts can drop in-flight notify
	// calls, so a convergence sweep at boot keeps state from drifting
	// over crashes. Best-effort: errors are logged, restart succeeds
	// regardless.
	e.sweepStuckWaitingForChildren(ctx)

	return nil
}

// sweepStuckWaitingForChildren walks every task in WAITING_FOR_CHILDREN
// status and runs the parent-unblock sweep on each. checkParentUnblock
// handles the children-status check and either re-queues the parent
// (any children terminal, none failed) or transitions it to FAILED
// (any children failed, no retry budget remaining). Tasks whose
// children are still in-flight stay WAITING_FOR_CHILDREN — same
// outcome as if the sweep didn't fire.
//
// Why this is safe to run at startup: checkParentUnblock is
// idempotent — it short-circuits when the parent isn't actually in
// WAITING_FOR_CHILDREN, when the children fetch fails, or when not
// every child is terminal. Worst case: we no-op every entry.
//
// Why this needs to exist: notify-style state transitions are
// best-effort. If the child's terminal-transition path crashed before
// it could call checkParentUnblock (the closure_request bug), or the
// daemon was killed between transition and notify, the parent has
// no other wake source. The CPC timeout scanner covers cross-project
// timeouts; this covers in-project delegation parents.
func (e *Executor) sweepStuckWaitingForChildren(ctx context.Context) {
	if e == nil || e.persistTaskRepo == nil {
		return
	}
	// persistTaskRepo.List is the broader interface with TaskFilter;
	// the executor-narrow taskRepo doesn't expose List. We're at
	// startup so the page size is generous — most deployments have
	// at most a handful of stuck parents.
	status := persistence.TaskStatusWaitingForChildren
	stuck, err := e.persistTaskRepo.List(ctx, persistence.TaskFilter{
		Status:   &status,
		PageSize: 500,
	})
	if err != nil {
		e.logger.Warn().Err(err).
			Msg("executor recovery: failed to enumerate WAITING_FOR_CHILDREN parents for startup sweep")
		return
	}
	if len(stuck) == 0 {
		return
	}
	e.logger.Info().
		Int("count", len(stuck)).
		Msg("executor recovery: sweeping stranded WAITING_FOR_CHILDREN parents")
	for _, parent := range stuck {
		if parent == nil {
			continue
		}
		// checkParentUnblock takes a CHILD task pointer (it reads
		// child.ParentTaskID). Synthesize a stub child with the
		// parent's ID so the existing logic does its job: it'll
		// load the parent, walk children, and decide.
		stubChild := &persistence.Task{ParentTaskID: &parent.ID}
		e.checkParentUnblock(ctx, stubChild)
	}
}

// ExecuteWithContext starts a workflow for the given task with a context for trace propagation.
// This is the main entry point for task execution with distributed tracing support.
func (e *Executor) ExecuteWithContext(ctx context.Context, taskID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// During graceful shutdown reject new starts so we don't begin
	// work we'd immediately have to pause. The scheduler retries
	// leased tasks on next start; a freshly-leased task that we
	// rejected here has no checkpoint to resume from, so the
	// scheduler will simply re-lease it after the daemon comes back.
	if e.shuttingDown {
		return fmt.Errorf("executor is shutting down; not starting new task %s", taskID)
	}

	// Check if already executing
	if _, exists := e.activeExecutions[taskID]; exists {
		return fmt.Errorf("task %s is already being executed", taskID)
	}

	// Initialize context if needed
	if e.ctx == nil {
		e.ctx, e.cancel = context.WithCancel(context.Background())
	}

	// Synchronous setup uses the caller's ctx so the per-request
	// trace and cancellation deadline reach the DB. Pre-fix these
	// calls used the executor lifecycle context (e.ctx) which is
	// only cancelled on Shutdown, dropping the caller's deadline
	// silently. The background goroutine below still derives from
	// e.ctx because it outlives the caller's request.
	task, err := e.taskRepo.Get(ctx, taskID)
	if err != nil {
		return fmt.Errorf("failed to get task %s: %w", taskID, err)
	}

	// Create execution record. Use a placeholder workflow ID here;
	// resolveExecutionPlan will update it with the actual resolved
	// workflow before the execution starts.
	executionID := generateExecutionID(taskID)
	execution := &persistence.Execution{
		ID:               executionID,
		TaskID:           taskID,
		ProjectID:        task.ProjectID,
		WorkflowID:       taskWorkflowID(task),
		WorkflowRevision: "v1",
		Status:           persistence.ExecutionStatusPending,
	}

	// Failure-forensics fork-from-step lineage (Feature #1 Phase B).
	// When the task carries a fork_target envelope in its payload,
	// stamp the new execution with parent_execution_id /
	// forked_from_step_id / forked_prompt_override so the replay
	// page can render the lineage chain and the workflow loop can
	// apply the prompt override on the first iteration. Non-fork
	// tasks see target == nil and the columns stay NULL.
	if target, ferr := replay.ExtractForkTarget(task.Payload); ferr != nil {
		e.logger.Warn().Err(ferr).Str("task_id", taskID).
			Msg("fork target present but malformed; running task as a normal new execution")
	} else if target != nil {
		src := target.SourceExecutionID
		step := target.StepID
		override := target.PromptOverride
		execution.ParentExecutionID = &src
		execution.ForkedFromStepID = &step
		if override != "" {
			execution.ForkedPromptOverride = &override
		}
		execution.CurrentStepID = &step
		e.logger.Info().
			Str("task_id", taskID).
			Str("source_execution_id", src).
			Str("forked_from_step_id", step).
			Bool("has_override", override != "").
			Msg("fork: stamping execution lineage from task payload")
	}

	if err := e.execRepo.Create(ctx, execution); err != nil {
		return fmt.Errorf("failed to create execution record: %w", err)
	}

	// Record execution started
	if e.metrics != nil {
		e.metrics.RecordStarted(task.ProjectID)
	}

	// Inter-project orchestration Phase D — when this task is
	// a CPC callee, fire cross_project_call_received on the
	// callee execution's stream so an operator watching the
	// callee project sees the inbound edge. Best-effort,
	// nil-safe, and dedup'd via the in-process tracker so a
	// retry or scheduler recovery doesn't re-emit.
	e.emitCrossProjectCallReceivedIfCallee(ctx, task, execution)

	// Start execution in background with trace context
	execCtx, cancel := context.WithCancel(e.ctx)
	handle := &executionHandle{
		taskID:    taskID,
		projectID: task.ProjectID,
		startedAt: time.Now(),
		cancel:    cancel,
		ctx:       execCtx,
	}
	e.activeExecutions[taskID] = handle
	e.syncActiveGaugeLocked()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.runExecution(execCtx, task, execution)
	}()

	return nil
}

func (e *Executor) recoverExecution(ctx context.Context, execution *persistence.Execution) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, exists := e.activeExecutions[execution.TaskID]; exists {
		return nil
	}
	if e.ctx == nil {
		e.ctx, e.cancel = context.WithCancel(context.Background())
	}

	task, err := e.taskRepo.Get(ctx, execution.TaskID)
	if err != nil {
		return fmt.Errorf("failed to load task %s for execution recovery: %w", execution.TaskID, err)
	}

	// Skip recovery if the task is already in a terminal state (FAILED, COMPLETED,
	// CANCELLED). This prevents zombie executions that re-run, fail, and loop on
	// every daemon restart. Mark the orphaned execution as failed instead.
	switch task.Status {
	case persistence.TaskStatusFailed, persistence.TaskStatusCompleted, persistence.TaskStatusCancelled:
		_ = e.execRepo.RecordFailure(ctx, execution.ID, fmt.Sprintf(
			"orphaned execution: task already %s, skipping recovery", task.Status), "ORPHANED")
		return nil
	}

	// Workflow-drift guard. Historically this fired hard with
	// WORKFLOW_DRIFT when the live YAML's hash didn't match the one
	// stored on the execution row — the only safe thing to do because
	// step IDs, gate targets, and role assignments could all have
	// drifted. With workflow_snapshot pinning (see resolveExecutionPlan)
	// we now have the authoritative body of the workflow as it stood
	// at execution start, so the resume path uses the snapshot and
	// the drift becomes a logged warning rather than a terminal
	// failure. The hash check below is kept as a defensive guard for
	// rows that pre-date the snapshot column: those still take the
	// hard-fail path because we have no body to replay against.
	if execution.WorkflowRevision != "" && execution.WorkflowRevision != "v1" && e.workflows != nil {
		// If the execution has a snapshot, the snapshot is the
		// authoritative body — drift is handled by resolveExecutionPlan
		// (warning + use snapshot) and we should NOT fail here.
		hasSnapshot := false
		if e.execRepo != nil {
			if data, err := e.execRepo.GetWorkflowSnapshot(ctx, execution.ID); err == nil && len(data) > 0 {
				hasSnapshot = true
			}
		}
		if !hasSnapshot {
			if wf := e.workflows.GetWorkflow(execution.WorkflowID); wf != nil {
				if current := wf.Hash(); current != "" && current != execution.WorkflowRevision {
					msg := fmt.Sprintf("workflow %q changed since execution started (stored=%s live=%s) and no snapshot is available — manual intervention required",
						execution.WorkflowID, execution.WorkflowRevision, current)
					_ = e.execRepo.RecordFailure(ctx, execution.ID, msg, persistence.TaskFailureClassWorkflowDrift)
					// Also stamp the task so operators see the class on
					// listings, not just on the execution. This path
					// deliberately skips the scheduler's retry budget —
					// a workflow snapshot mismatch is operator-required
					// to fix, not transient. Logged so the "task
					// finalized FAILED on attempt 1" diagnostic
					// (stability item 3) shows the path explicitly.
					e.logger.Warn().
						Str("task_id", task.ID).
						Str("execution_id", execution.ID).
						Int("task_attempt", task.Attempt).
						Int("task_max_attempts", task.MaxAttempts).
						Str("workflow_id", execution.WorkflowID).
						Str("decision_path", "executor.workflowDriftBypass").
						Msg("task: terminal FAILED via workflow-drift bypass (intentional; not retryable)")
					task.Status = persistence.TaskStatusFailed
					task.LastError = &msg
					class := persistence.TaskFailureClassWorkflowDrift
					task.LastErrorClass = &class
					_ = e.taskRepo.Update(ctx, task)
					return nil
				}
			}
		}
	}

	execCtx, cancel := context.WithCancel(e.ctx)
	e.activeExecutions[task.ID] = &executionHandle{
		taskID:    task.ID,
		projectID: task.ProjectID,
		startedAt: time.Now(),
		cancel:    cancel,
		ctx:       execCtx,
		recovered: true,
	}
	e.syncActiveGaugeLocked()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.runExecution(execCtx, task, execution)
	}()
	return nil
}

// ExecutionResult holds the result of a task execution.
type ExecutionResult struct {
	TaskID       string
	ExecutionID  string
	Status       persistence.ExecutionStatus
	ExitCode     int
	ErrorMessage *string
	Result       []byte
	Duration     time.Duration
}

// runExecution executes a task to completion with tracing.
func (e *Executor) runExecution(ctx context.Context, task *persistence.Task, execution *persistence.Execution) {
	// Create the main execution span
	ctx, span := e.tracer.Start(ctx, "executor.run",
		trace.WithAttributes(
			attribute.String("task_id", task.ID),
			attribute.String("execution_id", execution.ID),
			attribute.String("project_id", task.ProjectID),
		),
	)
	defer span.End()

	// Update handle with context containing trace span
	e.mu.Lock()
	if h, ok := e.activeExecutions[task.ID]; ok {
		h.ctx = ctx
	}
	e.mu.Unlock()

	defer e.cleanupExecution(task.ID)

	// Update execution + task status to RUNNING.
	//
	// Both transitions belong here: LeaseTask promotes the task to LEASED
	// (a scheduler-level lock), and a separate step is needed to mark it
	// RUNNING once actual work has begun. Without the task-level update the
	// UI, API, and external observers see "LEASED" for the entire execution
	// lifetime — misleading and indistinguishable from a stuck lease.
	//
	// We also mirror the status on the in-memory execution struct so that
	// the later `execRepo.Update(ctx, execution)` call (which persists the
	// whole object to update WorkflowID) does not clobber the Running
	// status back to Pending.
	execution.Status = persistence.ExecutionStatusRunning
	if err := e.execRepo.UpdateStatus(ctx, execution.ID, persistence.ExecutionStatusRunning); err != nil {
		e.logger.Warn().
			Str("execution_id", execution.ID).
			Str("task_id", task.ID).
			Err(err).
			Msg("failed to mark execution as RUNNING — status may remain PENDING in the API")
	}
	if err := e.taskRepo.UpdateStatus(ctx, task.ID, persistence.TaskStatusRunning); err != nil {
		e.logger.Warn().
			Str("task_id", task.ID).
			Err(err).
			Msg("failed to mark task as RUNNING — status may remain LEASED in the API")
	}

	// Per-execution timeout. MaxAttempts controls retries, not timeout.
	timeout := e.config.DefaultTimeout

	// Calculate remaining attempts.
	//
	// `attempts` is the local counter used by the retry loop below.
	// The loop pre-increments (attempts++) at the top of each
	// iteration, so to have iteration N run as "attempt N" we start
	// at task.Attempt - 1. Without the offset the first iteration
	// would run at task.Attempt+1, silently losing one slot — a
	// fresh task (Attempt=1, MaxAttempts=3) would only run twice,
	// and a retried task bumped to Attempt=MaxAttempts would hit
	// the `attempts >= maxAttempts` guard below and fail instantly
	// with duration ~15 ms.
	if task.Attempt <= 0 {
		task.Attempt = 1
	}
	attempts := task.Attempt - 1
	maxAttempts := task.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = e.config.MaxRetries + 1
	}
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	plan, err := e.resolveExecutionPlan(ctx, task, execution)
	if err != nil {
		span.SetStatus(codes.Error, "failed to resolve workflow")
		span.RecordError(err)
		e.handleFailure(ctx, task, execution, err)
		return
	}

	// Per-task wall-clock hard cap. Workflow.MaxWallClock — when set —
	// bounds the entire execution including retries, container
	// startup, and post-step bookkeeping. The watchdog handles the
	// "no forward progress" failure mode reactively; this is the
	// proactive ceiling for "agent is making progress but the task
	// will simply not finish in human-reasonable time." Empty = no
	// cap (preserves the pre-feature behaviour for upgrades).
	if plan != nil && plan.workflow != nil && plan.workflow.MaxWallClock != "" {
		if d, perr := time.ParseDuration(plan.workflow.MaxWallClock); perr == nil && d > 0 {
			var cancelDeadline context.CancelFunc
			ctx, cancelDeadline = context.WithTimeout(ctx, d)
			// Wrap the original cancelation so the deadline timer
			// is released whether the goroutine exits via success,
			// failure, or shutdown.
			defer cancelDeadline()
			e.logger.Debug().
				Str("execution_id", execution.ID).
				Dur("max_wall_clock", d).
				Msg("execution wall-clock cap armed")
		} else if perr != nil {
			e.logger.Warn().
				Str("execution_id", execution.ID).
				Str("workflow_id", plan.workflow.ID).
				Str("max_wall_clock", plan.workflow.MaxWallClock).
				Err(perr).
				Msg("workflow MaxWallClock is set but unparseable — treating as unset")
		}
	}

	// Persist the resolved workflow ID — the initial execution record may
	// have a placeholder ("default-workflow") if the task had no explicit
	// workflow_id. The resolved ID from the project config is authoritative.
	span.SetAttributes(attribute.String("workflow_id", execution.WorkflowID))
	_ = e.execRepo.Update(ctx, execution)

	// Resolve the project directory and set up workspace isolation.
	var projectDir string
	if e.config.ProjectWorkspacePath != "" && task.ProjectID != "" {
		projectDir = filepath.Join(e.config.ProjectWorkspacePath, task.ProjectID)
	}

	// Bootstrap: ensure the project workspace is a git repo with at least
	// one commit. This lets us unconditionally use worktree isolation +
	// auto-commit-on-merge for every project, regardless of whether an
	// operator ever ran `git init` by hand. Failure here falls through to
	// the legacy non-worktree path below — the task still runs, it just
	// loses the per-task isolation and the merge-time persistence
	// guarantee, same as it did before this bootstrap existed.
	// Per-project lock guards the git+worktree setup so two tasks
	// on the same project can't race each other (worktree branch
	// collision, snapshotWorkspaceRef tearing). Lock window is
	// intentionally short — releases before the actual task run
	// kicks off so parallel executions across projects aren't
	// serialised. Wrapped in IIFE so a panic anywhere in the
	// setup block still unlocks (pre-2026-05-29 the inline unlock
	// would leak the lock on panic, deadlocking every subsequent
	// task on the same project).
	var wsRef string
	var useWorktrees bool
	func() {
		unlockProject := e.wsLock().Lock(task.ProjectID)
		defer unlockProject()
		if err := ensureGitRepo(ctx, projectDir, e.logger); err != nil {
			e.logger.Warn().
				Err(err).
				Str("project_dir", projectDir).
				Msg("bootstrap: failed to initialize project git repo — falling back to shared workspace")
		}

		// Deterministic pre-work rebase for forge tasks: reset the project clone
		// to current upstream before branching the worktree, so the agent's code
		// work starts from HEAD of the default branch. Best-effort + no-op for
		// non-forge tasks (see rebaseProjectToOrigin).
		//
		// CRITICAL: skip the rebase when the task already has children — that's a
		// delegating parent (e.g. github-router) RESUMING after its dev-pipeline
		// child merged a fix into this clone. A reset --hard to origin here would
		// discard the child's merged commits, leaving the publish step with
		// nothing to open a change request from (incident 2026-06-13). The child's
		// own first run already rebased before doing the code work.
		if spec, ok := forgeCheckoutSpec(task.Payload); ok {
			// Keep vornik-internal bookkeeping (.autonomy/, CURRENT_TASK.md, …)
			// out of the customer's repo for every forge task (parent + child).
			excludeVornikInternalPaths(projectDir, e.logger)
			hasChildren := false
			if e.taskRepo != nil {
				if children, gErr := e.taskRepo.GetChildren(ctx, task.ID); gErr == nil && len(children) > 0 {
					hasChildren = true
				}
			}
			switch {
			case hasChildren:
				e.logger.Debug().Str("task_id", task.ID).
					Msg("pre-work checkout: skipped — task has children (resuming delegator; clone holds the child's merged work)")
			case spec.IsChangeRequest && spec.HeadRef != "":
				// PR-review task (github-review): materialize the change request's
				// head so the reviewer's working tree holds the PR's actual files,
				// not the base branch (incident 2026-06-13: reviewer "couldn't
				// locate any new files"). Falls back to the default-branch rebase
				// internally if the head fetch fails.
				checkoutForgeChangeRequest(ctx, projectDir, spec.HeadRef, spec.DefaultBranch, e.logger)
			default:
				// issue-fix and other forge tasks: start code work from HEAD of the
				// default branch.
				rebaseProjectToOrigin(ctx, projectDir, spec.DefaultBranch, e.logger)
			}
		}

		// When the project is a git repository, create an isolated worktree for this
		// task so parallel executions don't share the working tree. Each attempt gets
		// a fresh worktree branched from HEAD; on success the branch is merged back.
		// Fall back to the legacy snapshot/reset approach when git is not available.
		useWorktrees = isGitRepo(projectDir)
		if useWorktrees {
			wt, wtErr := createWorktree(ctx, projectDir, task.ID, e.logger)
			if wtErr != nil {
				// Branch or directory may exist from a paused/interrupted prior run.
				// Remove it and retry once before falling back to shared workspace.
				removeWorktree(ctx, projectDir, worktreePath(projectDir, task.ID), task.ID, e.logger)
				wt, wtErr = createWorktree(ctx, projectDir, task.ID, e.logger)
			}
			if wtErr != nil {
				e.logger.Warn().Err(wtErr).Str("project_dir", projectDir).
					Msg("worktree creation failed — falling back to shared workspace")
				useWorktrees = false
			} else {
				plan.worktreeDir = wt
			}
		}
		if !useWorktrees {
			wsRef = snapshotWorkspaceRef(projectDir)
		}
	}()

	// cleanupWorktree returns a non-nil error only on the success path
	// when the merge-back itself fails. A merge failure means the
	// worktree has commits that never landed on master — the task's
	// real output is inaccessible, so the task must be marked FAILED
	// rather than silently COMPLETED (the behaviour that hid
	// task_20260419194623_6b319f72664e632b's lost PROJECT_CONTEXT.md
	// update behind a misleading success message).
	cleanupWorktree := func(success bool) error {
		if !useWorktrees || plan.worktreeDir == "" {
			return nil
		}
		// Run the git operations on a context that survives task
		// cancellation (bug-sweep follow-up 2026-06-04).
		// removeWorktree already self-detaches internally, but
		// mergeWorktree runs every git command on the ctx it is
		// handed — a cancel racing retry exhaustion (the
		// cleanupWorktree(true) call sites below) would fail the
		// merge-back instantly and strand the task's committed work
		// in the worktree. Same detached-context pattern container
		// cleanup uses (container.go RemoveContainer).
		cleanupCtx := ctx
		if ctx.Err() != nil {
			var cancelCleanup context.CancelFunc
			cleanupCtx, cancelCleanup = context.WithTimeout(context.Background(), worktreeCleanupTimeout)
			defer cancelCleanup()
		}
		var mergeErr error
		unlockCleanup := e.wsLock().Lock(task.ProjectID)
		if success {
			mergeErr = mergeWorktree(cleanupCtx, projectDir, plan.worktreeDir, task.ID, e.logger)
		} else {
			removeWorktree(cleanupCtx, projectDir, plan.worktreeDir, task.ID, e.logger)
		}
		unlockCleanup()
		// Clear the pointer only when we actually removed the worktree.
		// On merge failure we deliberately leave the worktree in place
		// so the operator can salvage the commits.
		if mergeErr == nil {
			plan.worktreeDir = ""
		}
		return mergeErr
	}

	var lastErr error
	if attempts >= maxAttempts {
		lastErr = fmt.Errorf("task has no remaining attempts (%d/%d)", attempts, maxAttempts)
	}
retryLoop:
	for attempts < maxAttempts {
		attempts++

		// Reset the per-step visit counter + iteration counter at
		// the start of every retry attempt. Without this, a
		// workflow with maxStepVisits=1 (e.g. companion-rag-ingest)
		// is guaranteed to fail on attempt 2: attempt 1's
		// saveCheckpoint persisted visit_counts={step: 1} into
		// state, then attempt 2's first visit increments to 2 and
		// trips the "infinite rework loop" guard 35ms in — before
		// the agent has done anything. Observed 2026-05-28 on
		// retry-from-step + companion-rag-ingest, B-9.
		//
		// CompletedSteps is intentionally preserved: a retry that
		// genuinely advanced through some steps before failing
		// should resume from where it got, not restart. Only the
		// loop-protection counters reset.
		if attempts > 1 {
			retryState := loadExecutionState(execution)
			retryState.VisitCounts = nil
			retryState.Iterations = 0
			if err := e.saveExecutionState(ctx, execution, retryState); err != nil {
				e.logger.Warn().
					Err(err).
					Str("task_id", task.ID).
					Str("execution_id", execution.ID).
					Int("attempt", attempts).
					Msg("retry: failed to reset visit counter; attempt may fail immediately if step's maxVisits is tight")
			}
		}

		containerID, resultBytes, completedSteps, err := e.executeWorkflowAttempt(ctx, task, execution, plan, timeout)
		if err != nil {
			lastErr = err
			if errors.Is(err, errExecutionPaused) {
				span.SetStatus(codes.Ok, "execution paused")
				return
			}
			if IsLeadHandoff(err) {
				// Phase 25 — lead emitted checkpoint /
				// external_wait / closure_request. The task is
				// already in AWAITING_INPUT / AWAITING_EXTERNAL
				// (or stays COMPLETED for closure_request). The
				// execution itself succeeded; we record completion
				// + clean up the worktree, but skip handleSuccess
				// so its UpdateStatus(COMPLETED) doesn't overwrite
				// the conversational status the handoff stamped.
				span.SetStatus(codes.Ok, "lead handed off to operator")
				execution.CompletedSteps = completedSteps
				_ = e.execRepo.RecordCompletion(ctx, execution.ID, resultBytes)
				_ = cleanupWorktree(true)
				e.handleLeadHandoffFinalization(ctx, task, execution, containerID, resultBytes)
				return
			}
			// Graceful-shutdown bailout. When Shutdown(ctx) ran
			// pauseWithReason on this execution, it stopped the
			// agent container (which made waitForCompletion
			// return an error) and cancelled our ctx. The error
			// would otherwise look like a normal container
			// failure and walk into the retry loop — which would
			// then try to spawn a new container against a
			// runtime that's also being torn down, exhaust
			// attempts, and overwrite the row's PAUSED status
			// with FAILED. Bail out cleanly: the pause path
			// already wrote the right state to the DB, and
			// Recover() on next start will resume from the
			// checkpoint.
			if e.isShuttingDown() {
				e.logger.Info().
					Str("task_id", task.ID).
					Str("execution_id", execution.ID).
					Str("error", truncateStr(err.Error(), 200)).
					Msg("execution paused for shutdown — exiting goroutine cleanly")
				span.SetStatus(codes.Ok, "execution paused for shutdown")
				return
			}
			if errors.Is(err, context.Canceled) && e.isTaskCancelled(ctx, task.ID) {
				span.SetStatus(codes.Ok, "execution cancelled")
				_ = cleanupWorktree(false)
				// ctx is cancelled in this branch — run the workspace
				// reset / clean on a detached context so the git
				// commands actually execute (bug-sweep follow-up
				// 2026-06-04; same rationale as cleanupWorktree).
				resetCtx, cancelReset := context.WithTimeout(context.Background(), worktreeCleanupTimeout)
				unlockReset := e.wsLock().Lock(task.ProjectID)
				if !useWorktrees {
					if resetErr := resetWorkspace(resetCtx, projectDir, wsRef, e.logger); resetErr != nil {
						e.logger.Warn().Err(resetErr).Msg("workspace reset failed after cancellation")
					}
				} else {
					cleanProjectDir(resetCtx, projectDir, e.logger, e.cleanExcludesFor(task.ProjectID)...)
				}
				unlockReset()
				cancelReset()
				e.handleCancelled(ctx, task, execution)
				return
			}
			span.AddEvent("container_start_failed", trace.WithAttributes(
				attribute.String("error", err.Error()),
				attribute.Int("attempt", attempts),
			))
			if !e.shouldRetry(err) {
				break
			}
			if attempts >= maxAttempts {
				break
			}
			// Prepare a clean workspace for the retry attempt.
			unlockRetry := e.wsLock().Lock(task.ProjectID)
			if useWorktrees {
				// Remove the failed worktree and create a fresh one from HEAD.
				removeWorktree(ctx, projectDir, plan.worktreeDir, task.ID, e.logger)
				plan.worktreeDir = ""
				wt, wtErr := createWorktree(ctx, projectDir, task.ID, e.logger)
				if wtErr != nil {
					e.logger.Warn().Err(wtErr).Msg("worktree re-creation failed before retry")
					// Retry will use the main project dir — clean it so the next
					// attempt doesn't inherit files from the failed one.
					cleanProjectDir(ctx, projectDir, e.logger, e.cleanExcludesFor(task.ProjectID)...)
				} else {
					plan.worktreeDir = wt
				}
			} else {
				if resetErr := resetWorkspace(ctx, projectDir, wsRef, e.logger); resetErr != nil {
					e.logger.Warn().Err(resetErr).Msg("workspace reset failed before retry")
				}
			}
			unlockRetry()
			if e.metrics != nil {
				e.metrics.RecordRetried(task.ProjectID)
			}
			if e.config.RetryDelay > 0 {
				// Use a labeled break so a cancelled ctx during the
				// retry-delay sleep exits the for-loop directly. A bare
				// `break` would only exit the select; the previous
				// follow-up `if ctx.Err() != nil { break }` worked but
				// was a refactor-trap (one cleanup pass and the bug
				// surfaces as spurious retry attempts after cancel).
				select {
				case <-ctx.Done():
					lastErr = ctx.Err()
					break retryLoop
				case <-time.After(e.config.RetryDelay):
				}
			}
			continue
		}

		span.AddEvent("container_started", trace.WithAttributes(
			attribute.String("container_id", containerID),
		))

		// Update handle with container ID
		e.mu.Lock()
		if h, ok := e.activeExecutions[task.ID]; ok {
			h.containerID = containerID
		}
		e.mu.Unlock()

		// Success path — merge worktree back. If the merge itself fails,
		// the task has NOT actually succeeded from the operator's point
		// of view (output never reached master), so flip to the failure
		// handler and preserve the worktree branch for manual salvage.
		if mergeErr := cleanupWorktree(true); mergeErr != nil {
			span.SetStatus(codes.Error, "worktree merge failed")
			span.RecordError(mergeErr)
			// P1: Append MERGE_FAILED so the UI shows where it actually died
			// and avoid "done" status when changes are orphaned.
			execution.CompletedSteps = append(completedSteps, "MERGE_FAILED")
			e.handleFailure(ctx, task, execution, fmt.Errorf(
				"agent steps succeeded but changes could not be merged to master: %w. "+
					"worktree preserved at %s for manual recovery",
				mergeErr, plan.worktreeDir,
			))
			return
		}
		span.SetStatus(codes.Ok, "execution completed successfully")
		span.SetAttributes(attribute.Int("exit_code", 0))
		execution.CompletedSteps = completedSteps
		e.handleSuccess(ctx, task, execution, containerID, resultBytes)
		return
	}

	// All retries exhausted — decide whether to preserve the worktree.
	// A tool-iteration-limit failure is a partial-progress signal, not
	// a hard fail: the agent did real work before running out of
	// iterations, and the operator wants that work merged so a
	// follow-up CHECKPOINT task can continue from there. For every
	// other failure class the worktree is removed so a future task
	// starts from a known-clean main branch.
	errorClass := ClassifyExecutionFailure(lastErr, "")
	preserveWorkspace := errorClass == persistence.TaskFailureClassToolIterationLimit
	mergeFailed := false
	if preserveWorkspace {
		if mergeErr := cleanupWorktree(true); mergeErr != nil {
			// Merge of the partial work failed — fall back to the
			// hard-failure path so the operator sees the merge error
			// alongside the iteration-limit message.
			e.logger.Warn().Err(mergeErr).Str("task_id", task.ID).
				Msg("checkpoint: worktree merge failed after iteration-limit; treating as terminal failure")
			preserveWorkspace = false
			mergeFailed = true
			// cleanupWorktree(true) on merge failure leaves the worktree
			// in place for manual recovery — do not call removeWorktree
			// here.
		}
	} else {
		_ = cleanupWorktree(false)
	}
	unlockFinal := e.wsLock().Lock(task.ProjectID)
	if !useWorktrees {
		if resetErr := resetWorkspace(ctx, projectDir, wsRef, e.logger); resetErr != nil {
			e.logger.Warn().Err(resetErr).Msg("workspace reset failed after execution failure")
		}
	} else if !preserveWorkspace {
		// Skip cleanProjectDir when we just merged the worktree —
		// cleanup would discard the freshly-merged untracked files.
		cleanProjectDir(ctx, projectDir, e.logger, e.cleanExcludesFor(task.ProjectID)...)
	}
	unlockFinal()
	span.SetStatus(codes.Error, "execution failed")
	span.RecordError(lastErr)
	// Sync task.Attempt to the local in-execution counter before
	// handleFailure runs. Pre-fix the local `attempts` counter
	// drifted away from task.Attempt on every internal-retry loop
	// iteration: a fresh task.Attempt=1 task that ran 3 internal
	// tries would still have task.Attempt=1 in the DB, so
	// handleFailure's taskWillRetry check (task.Attempt < MaxAttempts
	// → 1 < 3 → true) wrongly concluded "more retries available"
	// even though the budget was already spent. Without this sync,
	// the conditional-FAILED logic in handleFailure leaves the task
	// LEASED forever when the scheduler isn't around to observe and
	// re-queue (testing path), and creates a confusing
	// LEASED→FAILED→QUEUED bounce in production. Syncing here
	// makes task.Attempt the authoritative "tries done so far"
	// count for both layers (in-execution + scheduler) to read.
	if attempts > task.Attempt {
		task.Attempt = attempts
	}
	e.handleFailure(ctx, task, execution, lastErr)

	// After the parent is marked FAILED, schedule the follow-up
	// CHECKPOINT task. Done after handleFailure so the parent's
	// LastErrorClass is already TOOL_ITERATION_LIMIT in the DB when
	// the operator pivots to the child task — the dashboard chain
	// (parent FAILED:TOOL_ITERATION_LIMIT → child QUEUED:CHECKPOINT)
	// reads correctly.
	if preserveWorkspace && !mergeFailed {
		stepID := ""
		if execution.CurrentStepID != nil {
			stepID = *execution.CurrentStepID
		}
		errMsg := ""
		if lastErr != nil {
			errMsg = lastErr.Error()
		}
		if childID, err := e.scheduleCheckpointFollowUp(ctx, task, stepID, errMsg); err != nil {
			e.logger.Warn().Err(err).Str("task_id", task.ID).
				Msg("checkpoint: follow-up task creation skipped — partial work is merged but the operator will need to reschedule manually")
		} else {
			e.logger.Info().
				Str("task_id", task.ID).
				Str("checkpoint_task_id", childID).
				Msg("checkpoint: continuation task scheduled — partial work merged to project")
		}
	}
}

// Cancel stops a running execution.
func (e *Executor) Cancel(taskID string) error {
	// Snapshot the handle's mutable fields under the lock. Pre-fix
	// the read of handle.containerID happened after Unlock, racing
	// with runExecution's lock-protected write — a lost race meant
	// containerID was zero, the StopContainer call was skipped, and
	// the agent container kept running (and billing) past the
	// operator's cancel.
	e.mu.Lock()
	handle, exists := e.activeExecutions[taskID]
	if !exists {
		e.mu.Unlock()
		return fmt.Errorf("no active execution for task %s", taskID)
	}
	containerID := handle.containerID
	cancelFn := handle.cancel
	startedAt := handle.startedAt
	projectID := handle.projectID
	e.mu.Unlock()

	// Cancel can be invoked during daemon shutdown as well as during normal
	// operation. Derive a detached context with a short deadline so DB /
	// container operations can't hang the caller if the backing services are
	// themselves draining.
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dbCancel()
	_ = e.taskRepo.UpdateStatus(dbCtx, taskID, persistence.TaskStatusCancelled)
	if exec, err := e.execRepo.GetByTaskID(dbCtx, taskID); err == nil && exec != nil {
		_ = e.execRepo.UpdateStatus(dbCtx, exec.ID, persistence.ExecutionStatusCancelled)
	}
	// Adaptive-route flows leave a PAUSED first execution that never
	// finalises after the second execution drives the task to a
	// terminal state — sweep it now so config reload + cleanup
	// scanners don't see an orphan.
	e.cascadeOrphanExecutions(dbCtx, taskID)

	if cancelFn != nil {
		cancelFn()
	}

	// runExecution writes containerID under e.mu after the container
	// is started. If we snapshotted a zero-string, the container may
	// have been started just after our snapshot — re-read once under
	// the lock to catch that race.
	if containerID == "" {
		e.mu.Lock()
		if h, ok := e.activeExecutions[taskID]; ok {
			containerID = h.containerID
		}
		e.mu.Unlock()
	}

	if containerID != "" {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = e.runtime.StopContainer(stopCtx, containerID, true)
		stopCancel()
	}

	if e.metrics != nil {
		duration := time.Since(startedAt).Seconds()
		e.metrics.RecordCancelled(projectID, duration)
	}

	return nil
}

// Stop cancels all in-flight executions and waits for their goroutines to
// exit, bounded by the supplied context. It is safe to call Stop multiple
// times and before any execution has been started. Callers should run Stop
// before closing the database the executor depends on.
func (e *Executor) Stop(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	if e.cancel != nil {
		e.cancel()
	}
	// Per-handle cancel so an execution blocked on a runtime call unwinds
	// promptly even if it's using a derived context.
	for _, h := range e.activeExecutions {
		if h != nil && h.cancel != nil {
			h.cancel()
		}
	}
	// Reset e.ctx / e.cancel so a subsequent ExecuteWithContext or
	// recoverExecution call lazy-initializes a fresh context. Without
	// this reset, callers see e.ctx != nil and derive a child of an
	// already-cancelled context, which causes the new execution to
	// fail immediately with context.Canceled — looks like a real
	// failure to the operator and burns retry budget.
	e.ctx = nil
	e.cancel = nil
	// Also reset shuttingDown — 2026-05-29 audit fix. Pre-fix
	// Shutdown set the flag and Stop never cleared it, so a
	// Stop()+Recover() cycle on the same Executor instance (used
	// in tests + the graceful-restart path) silently rejected
	// every recoverExecution call with "executor is shutting
	// down". Stop is the canonical "back to drain-then-restart-
	// able" state — clearing the flag here matches that contract.
	e.shuttingDown = false
	e.mu.Unlock()

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown is the graceful counterpart to Stop. Where Stop just
// cancels every running execution (the agent's last few seconds of
// work since the last checkpoint are lost), Shutdown:
//
//  1. Sets the shuttingDown flag so the scheduler can't lease new
//     tasks into us mid-drain.
//  2. Lists every active execution and calls pauseWithReason on
//     each. That stops the agent container with SIGTERM (giving it
//     a chance to flush result.json), stamps state.PausedReason =
//     "shutdown" in the snapshot, and marks the execution PAUSED
//     in the DB.
//  3. Waits for the per-execution goroutines to wind down within
//     ctx's deadline.
//
// On the next daemon start, Recover() finds the PAUSED-with-reason-
// shutdown executions and re-runs them from the last checkpoint.
// Each step's defer-recorded outcome and the saveCheckpoint between
// steps mean the resumed run picks up at the next step boundary —
// at most one step's worth of work is repeated, never lost.
//
// Hard kill (SIGKILL on the daemon, OOM, host shutdown) bypasses
// this entirely; in that case the existing Recover() path still
// rescues RUNNING executions but without the clean SIGTERM hand-off
// to the agent container, so the agent's in-flight LLM call is
// abandoned. That's the boundary of "graceful".
func (e *Executor) Shutdown(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	e.shuttingDown = true
	taskIDs := make([]string, 0, len(e.activeExecutions))
	for id := range e.activeExecutions {
		taskIDs = append(taskIDs, id)
	}
	e.mu.Unlock()

	if len(taskIDs) == 0 {
		// No active executions — Stop() is the right cleanup path
		// (still cancels e.ctx, releases timers, etc).
		return e.Stop(ctx)
	}

	e.logger.Info().Int("active_executions", len(taskIDs)).
		Msg("executor: shutdown — pausing active executions for resume on next start")

	for _, taskID := range taskIDs {
		if _, err := e.pauseWithReason(taskID, PauseReasonShutdown); err != nil {
			// Don't abort the loop on one bad pause — we want to do
			// the best we can across every active execution. The
			// per-execution log message is enough; an aggregate
			// failure would just hide the per-task ones.
			e.logger.Warn().Err(err).Str("task_id", taskID).
				Msg("executor: shutdown: pause failed (execution will be recovered as RUNNING)")
		}
	}

	// Stop() drains the wg and respects ctx for the wait budget.
	return e.Stop(ctx)
}

func (e *Executor) isShuttingDown() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shuttingDown
}

// shuttingDownOrCancelled returns true when the daemon is tearing down or
// the context has been cancelled. Callers that are about to route a failed
// step to on_fail must check this first: routing on_fail during shutdown
// walks the failure graph while the runtime is being torn down — it starts
// more containers against a closing socket, exhausts retry budget, and
// overwrites the PAUSED status with FAILED (Signature B, restart-induced
// in-flight FAILED, 2026-06-21). Return the local error upward so the
// existing isShuttingDown() bail-out arm in runExecution handles it cleanly
// → PAUSED-for-resume, NOT FAILED.
func (e *Executor) shuttingDownOrCancelled(ctx context.Context) bool {
	return e.isShuttingDown() || ctx.Err() != nil
}

// ActiveCount returns the number of currently active executions.
func (e *Executor) ActiveCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.activeExecutions)
}

// IsExecuting returns whether a task is currently being executed.
func (e *Executor) IsExecuting(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, exists := e.activeExecutions[taskID]
	return exists
}

// TaskLogs returns best-effort logs for a running task. Ephemeral
// containers are removed after step completion, so completed tasks fall
// back to execution/task persisted errors at the API layer.
func (e *Executor) TaskLogs(ctx context.Context, taskID string, tail int) (string, error) {
	if taskID == "" {
		return "", fmt.Errorf("task_id is required")
	}

	e.mu.Lock()
	handle := e.activeExecutions[taskID]
	e.mu.Unlock()
	if handle != nil && handle.containerID != "" {
		return e.runtime.Logs(ctx, handle.containerID, tail)
	}

	container, err := e.runtime.GetContainerByTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if container == nil || container.ID == "" {
		return "", fmt.Errorf("no container found for task %s", taskID)
	}
	return e.runtime.Logs(ctx, container.ID, tail)
}

// cleanupExecution removes the execution from active tracking and
// syncs the active-execution gauge so it reflects reality.
//
// For RECOVERED executions (those started by Recover after a daemon
// restart, not by the live scheduler dispatch), this method is also
// responsible for releasing the task's lease and updating its
// status. The recovery path has no scheduler.dispatchViaExecutor
// goroutine watching, so without an explicit self-release the task
// would sit in RUNNING state until the scheduler's recovery loop
// noticed the orphan and waited out its 90-second idle-grace
// window. Reproduced 2026-05-07 — task
// task_20260507204558_88382830f8da1aaf was stuck in RUNNING for
// 3+ minutes after its post-restart execution failed, only
// transitioning when the operator cancelled it manually.
//
// The release is best-effort: a transient DB failure logs and
// continues. The recovery loop's idle-grace path is still a
// fallback for the rare case where the self-release also fails.
func (e *Executor) cleanupExecution(taskID string) {
	e.mu.Lock()
	handle := e.activeExecutions[taskID]
	delete(e.activeExecutions, taskID)
	e.syncActiveGaugeLocked()
	e.mu.Unlock()
	if handle == nil {
		return
	}
	// Release the per-execution context.WithCancel child of e.ctx.
	// Without this every completed execution leaks one entry in the
	// parent context's child-list — slow but unbounded growth.
	if handle.cancel != nil {
		handle.cancel()
	}
	if !handle.recovered {
		return
	}
	e.releaseRecoveredTask(taskID)
}

// releaseRecoveredTask reads the task's current state and releases
// the lease with the right terminal status. Mirrors the decision
// logic in scheduler.completeTask: terminal task statuses (FAILED,
// COMPLETED, CANCELLED) flow through unchanged; anything else
// gets re-queued for retry (when budget remains) or marked FAILED
// (when exhausted).
//
// Uses a 5-second background context so a daemon shutting down
// concurrently doesn't strand the release on the cancelled execution
// context.
func (e *Executor) releaseRecoveredTask(taskID string) {
	if e.taskRepo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	task, err := e.taskRepo.Get(ctx, taskID)
	if err != nil || task == nil {
		e.logger.Warn().Err(err).Str("task_id", taskID).
			Msg("recovered execution: failed to load task for self-release; relying on scheduler recovery loop")
		return
	}
	// Already terminal — but the lease columns may still be set
	// because handleSuccess/handleFailure use UpdateStatus / Update
	// (which only writes status, not the lease columns). We need to
	// call ReleaseLease to atomically clear lease_id/leased_by/
	// leased_at/lease_expires_at while preserving the terminal
	// status. Postgres' WHERE lease_id=$2 guard makes this a no-op
	// if the lease is already cleared.
	switch task.Status {
	case persistence.TaskStatusCompleted, persistence.TaskStatusFailed, persistence.TaskStatusCancelled:
		if task.LeaseID == nil || *task.LeaseID == "" {
			return
		}
		leaseID := *task.LeaseID
		if err := e.taskRepo.ReleaseLease(ctx, taskID, leaseID, task.Status, persistence.ReleaseOptions{}); err != nil {
			e.logger.Warn().Err(err).Str("task_id", taskID).Str("status", string(task.Status)).
				Msg("recovered execution: terminal lease cleanup failed")
		}
		return
	}
	// Decide retry vs terminal using the same logic as
	// scheduler.TaskCompleted. The handleFailure path has already
	// stamped LastError + LastErrorClass; we just need the status
	// + lease release here.
	maxAttempts := task.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	attempt := task.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	newStatus := persistence.TaskStatusFailed
	releaseOpts := persistence.ReleaseOptions{
		Attempt:     attempt,
		MaxAttempts: maxAttempts,
	}
	if task.LastError != nil {
		releaseOpts.Error = *task.LastError
	}
	if attempt < maxAttempts {
		newStatus = persistence.TaskStatusQueued
		releaseOpts.Attempt = attempt + 1
	}

	// 2026-05-16 fix. retry-from-step initiated executions don't
	// carry a leaseID (the scheduler wasn't involved in dispatching
	// them), so ReleaseLease — which requires a non-empty leaseID
	// for safety since the 2025.12 lease-misuse guard — fails with
	// "leaseID required". Operator-observed dead-end on
	// task_20260516015931_2c9658cb6a103380: the warn log was
	// emitted, no fallback fired, the task stayed in its non-
	// terminal status forever because the scheduler's recovery
	// loop only picks up tasks with expired leases (LEASED/RUNNING
	// with lease_expires_at < now). A leaseless RUNNING task with
	// no lease columns doesn't match — operator's only escape was
	// a manual UI Retry click.
	//
	// Use TransitionConditional as the leaseless analog:
	//   - matches the live non-terminal status as the WHERE gate
	//     so a concurrent scheduler-side requeue can't race us
	//   - clears any half-set lease columns
	//   - persists attempt + max_attempts + last_error on the
	//     same UPDATE so the row is consistent after the call
	leaseID := ""
	if task.LeaseID != nil {
		leaseID = *task.LeaseID
	}
	if leaseID == "" {
		fromStatuses := []persistence.TaskStatus{
			persistence.TaskStatusLeased,
			persistence.TaskStatusRunning,
			persistence.TaskStatusPending,
			persistence.TaskStatusWaitingForChildren,
		}
		transOpts := persistence.TransitionOpts{
			ClearLease:  true,
			Attempt:     releaseOpts.Attempt,
			MaxAttempts: releaseOpts.MaxAttempts,
		}
		if task.LastError != nil {
			transOpts.LastError = task.LastError
		}
		if task.LastErrorClass != nil {
			transOpts.LastErrorClass = task.LastErrorClass
		}
		moved, err := e.taskRepo.TransitionConditional(ctx, taskID, fromStatuses, newStatus, transOpts)
		if err != nil {
			e.logger.Warn().Err(err).
				Str("task_id", taskID).
				Str("new_status", string(newStatus)).
				Msg("recovered execution: leaseless transition failed; scheduler recovery loop will pick up after grace window")
			return
		}
		if !moved {
			e.logger.Info().
				Str("task_id", taskID).
				Str("current_status", string(task.Status)).
				Str("new_status", string(newStatus)).
				Msg("recovered execution: leaseless transition lost race; another caller already moved the row")
			return
		}
		e.logger.Info().
			Str("task_id", taskID).
			Str("new_status", string(newStatus)).
			Int("attempt", releaseOpts.Attempt).
			Int("max_attempts", maxAttempts).
			Msg("recovered execution: leaseless self-release; task transitioned to retry/terminal without waiting for scheduler grace window")
		return
	}

	if err := e.taskRepo.ReleaseLease(ctx, taskID, leaseID, newStatus, releaseOpts); err != nil {
		e.logger.Warn().Err(err).
			Str("task_id", taskID).
			Str("new_status", string(newStatus)).
			Msg("recovered execution: ReleaseLease failed; scheduler recovery loop will pick up after grace window")
		return
	}
	e.logger.Info().
		Str("task_id", taskID).
		Str("new_status", string(newStatus)).
		Int("attempt", releaseOpts.Attempt).
		Int("max_attempts", maxAttempts).
		Msg("recovered execution: self-released; task moved to terminal state without waiting for scheduler grace window")
}

// syncActiveGauge updates the vornik_executor_active gauge from the
// actual activeExecutions map. Must be called while e.mu is held.
func (e *Executor) syncActiveGaugeLocked() {
	if e.metrics == nil {
		return
	}
	counts := make(map[string]int)
	for _, h := range e.activeExecutions {
		counts[h.projectID]++
	}
	e.metrics.SetActiveGauge(counts)
}

// getStartTime returns the start time for an execution.
func (e *Executor) getStartTime(taskID string) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	if h, ok := e.activeExecutions[taskID]; ok {
		return h.startedAt
	}
	return time.Now()
}

func (e *Executor) isTaskCancelled(ctx context.Context, taskID string) bool {
	task, err := e.taskRepo.Get(ctx, taskID)
	return err == nil && task != nil && task.Status == persistence.TaskStatusCancelled
}

// pruneAllWorktrees runs `git worktree prune` for every project
// directory under ProjectWorkspacePath. Called once at startup to
// clean up stale worktree metadata left by a previous daemon run
// (e.g. after a crash). Worktrees for task IDs in `preserve` are
// retained — those have an in-flight execution row in the DB that
// the recovery loop will adopt. Pruning their worktree out from
// under the recovered goroutine is the bug T-4ae6 demonstrated.

// cleanExcludesFor returns the extra git-clean exclusions for a
// project's main workspace cleanup. Default excludes (.worktrees/
// + .autonomy/) are applied unconditionally inside cleanProjectDir;
// this helper covers the case where an operator pointed
// ProjectAutonomy.ContextFilePath or .UserContextFilePath outside
// the default .autonomy/ namespace and would otherwise see their
// untracked operator doc wiped on the next cleanup pass.
//
// Returns nil for unknown projects or when both autonomy paths sit
// inside .autonomy/ (the blanket default already covers them).
func (e *Executor) cleanExcludesFor(projectID string) []string {
	if e.workflows == nil || projectID == "" {
		return nil
	}
	proj := e.workflows.GetProject(projectID)
	if proj == nil {
		return nil
	}
	var out []string
	if dir := projectCleanExcludeDir(proj.Autonomy.ContextFilePath); dir != "" {
		out = append(out, dir)
	}
	if dir := projectCleanExcludeDir(proj.Autonomy.UserContextFilePath); dir != "" {
		out = append(out, dir)
	}
	return out
}

func (e *Executor) pruneAllWorktrees(ctx context.Context, preserve map[string]struct{}) {
	if e.config.ProjectWorkspacePath == "" {
		return
	}
	entries, err := os.ReadDir(e.config.ProjectWorkspacePath)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projectDir := filepath.Join(e.config.ProjectWorkspacePath, entry.Name())
		// Lock-on-mutation: prune is a workspace writer. Hold the
		// per-project workspace lock for THIS project only, releasing
		// before the next — never one hold across all projects (that
		// would needlessly serialise unrelated projects and risk
		// blocking a concurrent writer for an unrelated project). The
		// directory name is the project ID, matching the key the 5
		// executor mutation sites and the UI/API writers use.
		func() {
			unlock := e.wsLock().Lock(entry.Name())
			defer unlock()
			pruneWorktrees(ctx, projectDir, e.logger, preserve)
		}()
	}
}
