package service

// Scheduler + lifecycle-monitor wiring extracted from container.go
// as part of the 2026-05-16 service-package split. Owns:
//   - initScheduler   (the task queue's lease loop)
//   - initWatchdog    (stuck-execution detector)
//   - initEffectiveCostMonitor ($/success drift alarm)
//   - rebuildSchedulerMetrics  (called by observabilityRegistry to
//     re-attach Prometheus sinks after init)

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/executor/agenthealth"
	forgeh "vornik.io/vornik/internal/executor/handlers/forge"
	"vornik.io/vornik/internal/executor/handlers/rag"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/runtime"
	"vornik.io/vornik/internal/scheduler"
	"vornik.io/vornik/internal/schemaregistry"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/templates"
	"vornik.io/vornik/internal/toolbudget"
	"vornik.io/vornik/internal/verifier"
	"vornik.io/vornik/internal/watchdog"
)

// taskKeyMinter implements executor.APIKeyMinter over the api_keys
// repository. One struct per daemon; the executor calls it on every
// container start.
type taskKeyMinter struct{ repo persistence.APIKeyRepository }

// MintTaskKey generates a fresh project-scoped key, persists only the
// hash (never the raw key), and returns the raw key to the executor for
// injection into the container's VORNIK_API_KEY env var.
//
// Key invariants:
//   - client_kind is intentionally EMPTY — a non-empty client_kind would
//     route the key through the companion-path allowlist in middleware.go
//     (~line 382), blocking every internal agent call. Agent task keys
//     are identified by Name alone: "agent:task_<taskID>".
//   - expires_at is now+48h as belt-and-braces; the primary lifecycle is
//     the RevokeTaskKey call at step teardown.
func (m *taskKeyMinter) MintTaskKey(ctx context.Context, projectID, taskID string) (string, error) {
	raw, err := apikey.Generate(projectID)
	if err != nil {
		return "", err
	}
	exp := time.Now().UTC().Add(48 * time.Hour)
	err = m.repo.Create(ctx, &persistence.APIKey{
		ID:        persistence.GenerateID("key"),
		ProjectID: projectID,
		Name:      persistence.TaskKeyNamePrefix + taskID,
		KeyHash:   apikey.Hash(raw),
		KeyPrefix: apikey.DisplayPrefix(raw),
		ExpiresAt: &exp,
		CreatedBy: "executor",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

// RevokeTaskKey revokes the key named "agent:task_<taskID>". Uses
// RevokeByName so the caller doesn't need the key ID. Idempotent —
// zero-row UPDATE is not an error.
func (m *taskKeyMinter) RevokeTaskKey(ctx context.Context, taskID string) error {
	return m.repo.RevokeByName(ctx, persistence.TaskKeyNamePrefix+taskID)
}

// warmAgentKeyNamePrefix is the reserved APIKey.Name prefix for
// project-scoped warm-pool agent credentials (Finding B1(b)). The
// (project, role) tuple is appended so each pool gets a stable,
// distinguishable key. Deliberately NOT persistence.TaskKeyNamePrefix:
// warm keys are project-scoped, not task-scoped, so they must not match
// persistence.TaskIDFromKeyName (which would otherwise make the audit
// handlers and CallMCPTool treat a warm key as bound to a bogus task ID).
const warmAgentKeyNamePrefix = "agent:warm_"

// MintProjectScopedKey generates a fresh PROJECT-scoped key for a
// warm-pool container (Finding B1(b)) and persists only the hash. Unlike
// MintTaskKey the key is not bound to any task — warm containers are
// baked once and reused across tasks, so a project-scoped credential
// (pools are keyed per project/role) is the tightest scope achievable
// without restarting the container per task. expires_at is now+48h;
// warm keys are reused for the pool's lifetime and a stale key simply
// expires (a new one is minted on the next cold start).
//
// Invariants match MintTaskKey: empty client_kind (a non-empty value
// routes through the companion-path allowlist in middleware.go and
// blocks every internal agent call).
func (m *taskKeyMinter) MintProjectScopedKey(ctx context.Context, projectID, role string) (string, error) {
	raw, err := apikey.Generate(projectID)
	if err != nil {
		return "", err
	}
	exp := time.Now().UTC().Add(48 * time.Hour)
	err = m.repo.Create(ctx, &persistence.APIKey{
		ID:        persistence.GenerateID("key"),
		ProjectID: projectID,
		Name:      warmAgentKeyNamePrefix + projectID + ":" + role,
		KeyHash:   apikey.Hash(raw),
		KeyPrefix: apikey.DisplayPrefix(raw),
		ExpiresAt: &exp,
		CreatedBy: "executor-warm",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

// warnAgentLLMDirectRoute logs a startup breakage warning when agents would
// route their minted per-task key direct to the upstream (topology 1).
// Extracted from initScheduler so it's unit-testable without standing up the
// whole scheduler (podman runtime manager, executor wiring, repos, ...).
//
// Regression: fresh-install "Invalid API key" (2026-07-25, design F2b) — an
// empty agent_llm.endpoint makes agents fall back to chat.endpoint (the real
// upstream) and send their minted per-task sk-vornik-* key there, where it's
// rejected outright. This used to log at Info, which fresh-install operators
// never see before every task starts failing; it must WARN and name the
// breakage. The `vornikctl doctor` agent_llm_topology check (internal/api)
// guards the same failure mode at boot/on-demand.
func warnAgentLLMDirectRoute(logger zerolog.Logger, resolvedEndpoint string) {
	logger.Warn().
		Str("resolved_endpoint", resolvedEndpoint).
		Msg("agent_llm.endpoint is empty: agents send their minted per-task key " +
			"DIRECT to the upstream, which rejects it as 'Invalid API key' — every " +
			"task will fail. Set AGENT_LLM_ENDPOINT to the daemon /api/v1 proxy. " +
			"Run `vornikctl doctor` (agent_llm_topology).")
}

// initScheduler initializes the task scheduler.
func (c *Container) initScheduler() error {
	// Resolve the FileBackend named by Config.Storage.Backend
	// (filesystem | s3). The local filesystem default is preserved
	// when nothing is configured. OpenArtifactBackend is the single
	// dispatch point — keeping the picker out of artifacts.New means
	// the artifacts package stays free of YAML-config coupling.
	storeOpts := []artifacts.StoreOption{
		artifacts.WithBasePath(c.Config.Storage.ArtifactsPath),
		artifacts.WithRepository(c.repos.Artifacts),
	}
	backend, err := storage.OpenArtifactBackend(context.Background(), c.Config.Storage)
	if err != nil {
		return fmt.Errorf("failed to open artifact backend: %w", err)
	}
	storeOpts = append(storeOpts, artifacts.WithBackend(backend))
	artifactStore, err := artifacts.New(storeOpts...)
	if err != nil {
		_ = backend.Close()
		return fmt.Errorf("failed to initialize artifact store: %w", err)
	}
	c.artifactStore = artifactStore
	c.artifactBackend = backend
	// Wire the verifier's package-global blob reader so verifiers
	// that inspect artifact content (artifact_min_entries and
	// friends) read through the same backend the rest of the
	// daemon does. Package-global is the existing seam — see
	// verifier.SetBlobReader docstring.
	verifier.SetBlobReader(artifactStore)

	// Everything below — the podman runtime manager, the executor, and the
	// task scheduler — is worker machinery. Only a RunWorkers node executes
	// tasks, and only it has podman. A ui/webhook node serves and reads
	// artifacts (built above) but runs nothing, so stop here. Without this
	// gate a thin webhook node crash-loops on "runtime manager: podman not
	// available" (incident 2026-06-12); the artifact store is kept because a
	// ui node (ServeUI, !RunWorkers) still needs it for downloads.
	if c.skipNonWorker("scheduler") {
		return nil
	}

	taskRepo := c.repos.Tasks
	execRepo := c.repos.Executions

	runtimeOptions := []runtime.ManagerOption{
		runtime.WithLogger(c.Logger),
		runtime.WithAllowHostUserns(c.Config.Runtime.AllowHostUserns),
	}
	if c.Config.Runtime.UserNSMode != "" {
		runtimeOptions = append(runtimeOptions, runtime.WithUserNSMode(c.Config.Runtime.UserNSMode))
	}
	if c.Config.Runtime.RunAsUser != "" {
		runtimeOptions = append(runtimeOptions, runtime.WithRunAsUser(c.Config.Runtime.RunAsUser))
	}
	if c.Config.Server.UnixSocket != "" {
		// daemon-only agent roles bind-mount this socket to reach the
		// daemon (MCP + LLM) under --network=none. See Step B.
		runtimeOptions = append(runtimeOptions, runtime.WithDaemonSocketPath(c.Config.Server.UnixSocket))
	}
	if c.Config.Runtime.DefaultNetwork != "" {
		// Network policy for roles that don't set runtime.network. New
		// installs ship "daemon-only" (zero-egress by default); roles
		// needing egress opt out with network:host.
		runtimeOptions = append(runtimeOptions, runtime.WithDefaultNetwork(c.Config.Runtime.DefaultNetwork))
	}
	if registry := c.observabilityRegistry(); registry != nil {
		runtimeOptions = append(runtimeOptions, runtime.WithPrometheusRegistry(registry))
	}

	runtimeManager, err := runtime.New(runtimeOptions...)
	if err != nil {
		return fmt.Errorf("failed to initialize runtime manager: %w", err)
	}
	c.runtimeManager = runtimeManager

	executorConfig := executor.DefaultConfig()
	executorConfig.ArtifactStoragePath = c.Config.Storage.ArtifactsPath

	// Default project workspace from config, then VORNIK_DATA_DIR.
	// Shared with initDispatcher's WithProjectWorkspacePath — the two
	// trust boundaries must resolve the same root.
	executorConfig.ProjectWorkspacePath = resolveProjectWorkspacePath(c.Config.Runtime.ProjectWorkspacePath)

	executorConfig.LogLevel = c.Config.Logging.Level

	// Dynamic per-role tool budget: resolve the daemon tool_budget block
	// (defaults applied) into the executor's pre-resolved config. Disabled
	// by default, in which case the budget injection is a no-op. See
	// https://docs.vornik.io
	executorConfig.ToolBudget = c.Config.ToolBudget.Resolved()

	// Producer-success gate (LLD 2026-07-12-rag-ingest-producer-success-gate):
	// default-on (nil → on); explicit false → passthrough + startup WARN.
	executorConfig.RequireProducerSuccess = c.Config.Memory.IngestRequireProducerSuccess
	if c.Config.Memory.IngestRequireProducerSuccess != nil && !*c.Config.Memory.IngestRequireProducerSuccess {
		c.Logger.Warn().Msg("memory.ingest_require_producer_success=false: failed/parked-task outputs WILL be ingested (re-introduces the recall-pollution failure mode)")
	}

	// N4 delegation guards — operator-tunable, fall back to executor
	// defaults (5 / 20) when unset.
	// See https://docs.vornik.io §3.
	if c.Config.Runtime.DelegationDepthLimit > 0 {
		executorConfig.DelegationDepthLimit = c.Config.Runtime.DelegationDepthLimit
	}
	if c.Config.Runtime.DelegationFanOutLimit > 0 {
		executorConfig.DelegationFanOutLimit = c.Config.Runtime.DelegationFanOutLimit
	}

	// Resolve LLM config for agent containers (AgentLLM with ChatConfig fallback).
	llm := c.Config.ResolvedAgentLLM()
	// Hard misconfiguration, not a latency nudge (LLD 2026-07-12-agent-llm-health-breaker
	// §8, tightened 2026-07-25 per design F2b): when the operator leaves
	// runtime.agent_llm.endpoint empty, agents inherit chat.endpoint (the
	// upstream gateway) and curl it DIRECTLY (topology 1) with their minted
	// per-task sk-vornik-* key, which the upstream rejects outright as
	// "Invalid API key" — every task fails, not just a degraded fail-over
	// path. Set runtime.agent_llm.endpoint to the daemon's /api/v1 URL so
	// agents route through the daemon proxy (topology 2), which both
	// accepts the minted key and gives sub-second fail-over via the
	// chat-router breaker.
	if c.Config.Runtime.AgentLLM.Endpoint == "" && llm.Endpoint != "" {
		warnAgentLLMDirectRoute(c.Logger, llm.Endpoint)
	}
	if llm.Endpoint != "" {
		executorConfig.AgentLLMEnv = map[string]string{
			"VORNIK_LLM_ENDPOINT": llm.Endpoint,
			"VORNIK_LLM_MODEL":    llm.Model,
			"VORNIK_LLM_API_KEY":  llm.APIKey,
			// VORNIK_API_KEY is the credential the agent entrypoint
			// + mcp-bridge use for internal daemon callbacks
			// (/api/v1/internal/llm-usage, /api/v1/internal/tool-audit,
			// /api/v1/projects/{id}/mcp/tools, /api/v1/projects/{id}/mcp/tools/call).
			// Defaults to the same value as VORNIK_LLM_API_KEY so a
			// single config knob (agent_llm.api_key) covers every
			// daemon-internal caller. Without this, flipping
			// api.auth_enabled=true would 401 every llm-usage /
			// tool-audit stream and break mcp-bridge tool discovery
			// (observed 2026-05-28 on task_20260528111635 — every
			// internal call returned "Missing API key"). See
			// https://docs.vornik.io §3.1.
			"VORNIK_API_KEY": llm.APIKey,
		}
		if llm.ContextSize > 0 {
			executorConfig.AgentLLMEnv["VORNIK_LLM_CONTEXT_SIZE"] = strconv.Itoa(llm.ContextSize)
		}
		if llm.MaxTokens > 0 {
			executorConfig.AgentLLMEnv["VORNIK_LLM_MAX_TOKENS"] = strconv.Itoa(llm.MaxTokens)
		}
		if llm.ToolResultMaxBytes > 0 {
			executorConfig.AgentLLMEnv["VORNIK_TOOL_RESULT_MAX_BYTES"] = strconv.Itoa(llm.ToolResultMaxBytes)
		}
		// Per-call timeout, forwarded to curl --max-time in the agent
		// entrypoint. We accept the same duration format as chat.timeout
		// (e.g. "300s", "5m") and round down to whole seconds because curl
		// only accepts integer seconds. Invalid values fall through to the
		// entrypoint's own 300s default.
		if llm.Timeout != "" {
			if d, err := time.ParseDuration(llm.Timeout); err == nil && d > 0 {
				executorConfig.AgentLLMEnv["VORNIK_LLM_TIMEOUT"] = strconv.Itoa(int(d.Seconds()))
			} else {
				c.Logger.Warn().Str("timeout", llm.Timeout).Err(err).
					Msg("agent_llm.timeout (or chat.timeout) is not a valid Go duration — using agent default")
			}
		}
		if len(llm.ModelLimits) > 0 {
			executorConfig.ModelLimits = make(map[string]executor.ModelLimit, len(llm.ModelLimits))
			for model, limit := range llm.ModelLimits {
				executorConfig.ModelLimits[model] = executor.ModelLimit{
					MaxTokens:   limit.MaxTokens,
					ContextSize: limit.ContextSize,
				}
			}
		}
	}
	// Raw media staged alongside a lossy extraction is bounded by an
	// operator-set size; 0 = unbounded (see MediaConfig).
	// see LLD § https://docs.vornik.io §4.2
	executorConfig.MediaStageMaxBytes = c.Config.Media.StageMaxBytes
	if executorConfig.AgentLLMEnv == nil {
		executorConfig.AgentLLMEnv = make(map[string]string)
	}
	// Agent-side helpers call back to the daemon for project-scoped MCP
	// proxying. Keep this independent from memory so MCP does not fall
	// back to credentials-bearing /app/input/mcp.json when memory is off.
	executorConfig.AgentLLMEnv["VORNIK_API_URL"] = agentCallbackURL(c.Config.Server.Address)

	// Project-scoped named secrets: map the daemon config's per-secret
	// allowlist into the executor (values are already ${VAR}-expanded at
	// config load). The executor injects each only for its allowed projects.
	for _, s := range c.Config.NamedSecrets {
		executorConfig.NamedSecrets = append(executorConfig.NamedSecrets, executor.NamedSecret{
			Name:            s.Name,
			Value:           s.Value,
			AllowedProjects: append([]string(nil), s.AllowedProjects...),
		})
	}

	// Phase B inter-project orchestration: load the project-
	// templates catalog + configs root so the spawn handler
	// can materialise new projects from templates. Resolves to
	// the same dir + catalog the API gallery handler uses
	// (container_http.go) — both surfaces share the load.
	// Failure to load the catalog is logged as a warning but
	// doesn't fail daemon boot — the spawn handler nil-checks
	// and short-circuits when the catalog isn't wired.
	var spawnCatalog *templates.Catalog
	var spawnConfigsDir string
	var schemaReg *schemaregistry.Registry
	if configsDir := resolveRegistryConfigDir(c.ConfigPath); configsDir != "" {
		templatesDir := filepath.Join(configsDir, "project-templates")
		if templatesEnv := os.Getenv("VORNIK_TEMPLATES_DIR"); templatesEnv != "" {
			templatesDir = templatesEnv
		}
		if cat, terr := templates.Load(templatesDir); terr != nil {
			c.Logger.Warn().Err(terr).Str("dir", templatesDir).
				Msg("spawn_project: template catalog load failed; spawn handler will return CROSS_PROJECT_DISABLED")
		} else {
			spawnCatalog = cat
			spawnConfigsDir = configsDir
		}
		// JSON-Schema registry — loads every *.json under
		// configs/schemas/. Missing dir is not an error; the
		// resolve hook falls through to envelope-shape
		// validation when no schema is registered for the
		// call's expected_schema id.
		schemasDir := filepath.Join(configsDir, "schemas")
		if schemasEnv := os.Getenv("VORNIK_SCHEMAS_DIR"); schemasEnv != "" {
			schemasDir = schemasEnv
		}
		if reg, sferr := schemaregistry.Load(schemasDir); sferr != nil {
			c.Logger.Warn().Err(sferr).Str("dir", schemasDir).
				Msg("schemaregistry: load reported errors; partially-loaded registry will be wired anyway")
			schemaReg = reg
		} else {
			schemaReg = reg
		}
		if schemaReg != nil {
			c.Logger.Info().Str("dir", schemasDir).Int("schemas", schemaReg.Count()).
				Msg("schemaregistry: loaded")
		}
	}

	executorOpts := []executor.Option{}
	// Inject the shared per-project workspace lock so the executor's
	// repo-mutation sites + startup prune serialise with the UI
	// artifact-delete path and the git-over-HTTPS handler (same
	// instance, injected below into the api/ui servers).
	if c.WorkspaceLock != nil {
		executorOpts = append(executorOpts, executor.WithWorkspaceLock(c.WorkspaceLock))
	}
	if spawnCatalog != nil {
		// Pass the concrete *templates.Catalog only when non-
		// nil — wrapping a typed nil in the spawnTemplateCatalog
		// interface would make the handler's nil-check miss and
		// crash on dereference.
		executorOpts = append(executorOpts, executor.WithProjectTemplateCatalog(spawnCatalog))
	}
	if schemaReg != nil {
		// Same typed-nil pitfall: only pass the registry when
		// non-nil so the resolve hook's HasSchema short-circuit
		// works correctly on disabled deployments.
		executorOpts = append(executorOpts, executor.WithSchemaRegistry(schemaReg))
	}
	executorOpts = append(executorOpts,
		executor.WithArtifactStore(artifactStore),
		executor.WithToolAuditRepository(c.repos.ToolAudit),
		executor.WithRecoveryEventRepository(c.repos.RecoveryEvents),
		executor.WithSkillRepository(c.repos.Skills),
		executor.WithExecutionInjectedSkillRepository(c.repos.ExecInjectedSkills),
		executor.WithSkillDistiller(c.ChatClient),
		// Phase 25 — conversational task lifecycle. The executor
		// uses these to write checkpoint / external_wait /
		// closure_request task_messages and flip the task status
		// when the lead emits a non-continue outcome.
		executor.WithConversationalLifecycle(
			c.repos.Messages,
			c.repos.Scratchpads,
			c.repos.Tasks,
		),
		executor.WithLLMUsageRepository(c.repos.LLMUsage),
		executor.WithSpend(c.llmSpend(persistence.TaskLLMUsageSourceWorkflowStep, "worker")),
		executor.WithBudgetReservationRepository(c.repos.BudgetReservations),
		executor.WithSteeringNotifier(c.combinedSteeringNotifier()),
		executor.WithStepOutcomeRepository(c.repos.StepOutcomes),
		// Per-task scoped API keys: mint on container start, revoke on
		// teardown. Wired whenever the API-key repo is available —
		// BOTH sqlite and postgres provide one (storage.go wires
		// sqlite.NewAPIKeyRepository / postgres.NewAPIKeyRepository),
		// so the minter is active on dev too. Only the legacy
		// static-key-only path (no repo at all) leaves this nil.
		executor.WithAPIKeyMinter(func() executor.APIKeyMinter {
			if c.repos != nil && c.repos.APIKeys != nil {
				return &taskKeyMinter{repo: c.repos.APIKeys}
			}
			return nil
		}()),
		// Feature #3 Phase A — live observation publisher. Wired
		// at executor construction so step boundary hooks broadcast
		// to WebSocket subscribers once Phase B lands. nil-safe
		// when livePub isn't constructed (test paths).
		executor.WithLivePublisher(c.livePub),
		// Feature #3 Phase C — operator-hint repo. Executor reads
		// pending hints at step boundaries.
		executor.WithHintRepository(c.repos.ExecutionHints),
		// Inter-project orchestration Phase A — cross-project call
		// ledger. nil-safe (sqlite branch leaves this nil; handler
		// returns errCrossProjectDisabled which the workflow's
		// on_fail branch handles cleanly).
		executor.WithCrossProjectCallRepository(c.crossProjectCallRepo()),
		// Inter-project orchestration Phase B — spawn_project
		// lineage + template catalog + configs root + the
		// registry-reload trigger. All optional individually —
		// the handler short-circuits to errSpawnDisabled when any
		// required dep is nil. The catalog + configsDir are
		// resolved the same way container_http.go resolves them
		// for the gallery handler so both surfaces see the same
		// templates.
		executor.WithProjectSpawnRepository(c.repos.ProjectSpawns),
		executor.WithRegistryReloader(c.ConfigReloader),
		executor.WithConfigsDir(spawnConfigsDir),
		// Phase C — admin audit log writes for every CPC create,
		// CPC resolve, and project spawn. nil-safe.
		executor.WithAdminAuditRepository(c.repos.AdminAudit),
		executor.WithHallucinationDetector(hallucination.NewDefault()),
		executor.WithJudgeRunner(c.storeJudgeRunner(c.buildJudgeRunner())),
		executor.WithTradingOrderRepo(c.repos.TradingOrders),
		// Continuous-learning Consumer A (slice 3): surface worker-mined
		// recovery remediations in the lead's RECOVERY_CONTEXT block. Double-
		// gated — the master instinct.enabled AND
		// instinct.consumers.failure_playbooks must both be on. Advisory
		// only; with either off the recovery prompt is byte-for-byte
		// unchanged. nil-safe when the instinct repo isn't wired.
		executor.WithInstinctPlaybooks(
			c.repos.Instincts,
			c.Config != nil && c.Config.Instinct.Enabled && c.Config.Instinct.Consumers.FailurePlaybooks,
		),
		// Slice-4 active budget consumer (default off): supplies a learned
		// complexity tier to toolbudget.Resolve on the absent-verdict path.
		// Double-gated — instinct.enabled AND consumers.tool_budget must both be
		// on. An explicit planner verdict always wins; Resolve's caps still bind.
		executor.WithInstinctToolBudget(
			c.Config != nil && c.Config.Instinct.Enabled && c.Config.Instinct.Consumers.ToolBudget,
		),
		// InstinctBudget resolver — Phase 1c seam. In Community, providers.InstinctBudget
		// is nil (Community has no learned-tier EE engine); WithInstinctBudgetResolver(nil)
		// is safe — the executor nil-guards before calling LearnedTier. In EE the
		// enterprise provider aggregator marks the edition by setting providers.Instinct
		// non-nil (Task 5); the container upgrades the field with a real DB-backed
		// resolver when the instinct repo is available.
		executor.WithInstinctBudgetResolver(c.instinctBudgetResolver()),
		// v2 prompt-level auto-apply (default off): only active when the master
		// instinct gate, failure_playbooks, AND auto_apply.enabled are all on —
		// so it can never fire without the advisory overlay it builds on.
		executor.WithInstinctAutoApply(
			c.Config != nil && c.Config.Instinct.Enabled &&
				c.Config.Instinct.Consumers.FailurePlaybooks &&
				c.Config.Instinct.Consumers.AutoApply.Enabled,
			instinctAutoApplyMinConfidence(c.Config),
			instinctAutoApplyMinCleanSupport(c.Config),
			instinctAutoApplyAllowedClasses(c.Config),
		),
		executor.WithLogger(c.Logger.With().Str("component", "executor").Logger()),
		executor.WithPricing(c.pricingTable),
	)
	if c.Registry != nil {
		executorOpts = append(executorOpts, executor.WithWorkflowResolver(c.Registry))
	}
	// Per-task cost governor breach notifier (LLD 2026-07-24 §3.5/§3.6, impl
	// review I1): the executor is the subsystem that fires soft-breach + hard-
	// park alerts, so it must receive the same budget notifier every other
	// subsystem gets (dispatcher/autonomy/api/ui) — without this the executor's
	// soft AND hard alerts are dead no-ops.
	if c.TelegramBot != nil {
		executorOpts = append(executorOpts, executor.WithBudgetNotifier(c.TelegramBot))
	}
	if registry := c.observabilityRegistry(); registry != nil {
		executorOpts = append(executorOpts, executor.WithPrometheusRegistry(registry))
	}

	// Build the secret-leak detector once and share it across
	// every executor consumer that needs it. Disabled config or
	// a construction error degrades to a nil detector — the
	// executor's per-checkpoint helpers no-op cleanly. A
	// startup error here would be a fail-closed posture, but
	// secrets-disabled deployments don't want that, so we only
	// log + carry on.
	if !c.Config.Secrets.Enabled {
		// secrets.enabled=false removes the detector entirely, so the
		// non-disableable memory-checkpoint floor (ResolveAction) can't fire —
		// there's nothing to scan with. Surface the bypass loudly: chunks may
		// carry plaintext credentials into durable, searchable memory.
		c.Logger.Warn().Msg("secrets: scanning DISABLED (secrets.enabled=false) — memory/artifacts/logs may persist plaintext credentials; enable it unless you have a deliberate reason")
	}
	if c.Config.Secrets.Enabled {
		detector, actions, err := buildSecretsDetector(c.Config.Secrets)
		if err != nil {
			c.Logger.Error().Err(err).Msg("secrets: detector failed to construct — continuing without secret-leak protection")
		} else {
			executorOpts = append(executorOpts, executor.WithSecrets(detector, actions))
			// Phase 3: durably record redaction events for the task-detail
			// badge + scan-history CLI. Best-effort at the sinks; nil repo
			// (SQLite branch may leave it unset) simply skips recording.
			if c.repos != nil && c.repos.SecretRedaction != nil {
				executorOpts = append(executorOpts, executor.WithSecretRedactionAudit(c.repos.SecretRedaction))
			}
			if len(c.Config.Secrets.TrustedOutputTools) > 0 {
				executorOpts = append(executorOpts, executor.WithSecretsTrustedOutputTools(c.Config.Secrets.TrustedOutputTools))
				c.Logger.Info().Strs("trusted_output_tools", c.Config.Secrets.TrustedOutputTools).
					Msg("secrets: tool-audit OUTPUT exempt from heuristic redaction for these provenance-trusted tools")
			}
			// Credential carryover: capture operator-facing access credentials
			// from trusted tools' output into the store, for code-formatted +
			// copyable surfacing. Requires both the mappings and the store.
			if tcs := c.Config.Secrets.NormalizedToolCredentials(); len(tcs) > 0 && c.repos != nil && c.repos.TaskCredentials != nil {
				mappings := make([]executor.ToolCredentialMapping, 0, len(tcs))
				for _, tc := range tcs {
					m := executor.ToolCredentialMapping{
						Tool:            tc.Tool,
						CredentialField: tc.CredentialField,
						ArtifactField:   tc.ArtifactField,
						Label:           tc.Label,
					}
					// Text-mode patterns take precedence; a bad regexp drops
					// only that mapping (loud, not fatal).
					if tc.CredentialPattern != "" {
						re, err := regexp.Compile(tc.CredentialPattern)
						if err != nil {
							c.Logger.Warn().Err(err).Str("tool", tc.Tool).
								Msg("secrets: invalid credential_pattern — skipping this tool_credentials mapping")
							continue
						}
						m.CredRE = re
						if tc.ArtifactPattern != "" {
							if are, err := regexp.Compile(tc.ArtifactPattern); err != nil {
								c.Logger.Warn().Err(err).Str("tool", tc.Tool).
									Msg("secrets: invalid artifact_pattern — capturing credential without artifact URL")
							} else {
								m.ArtRE = are
							}
						}
					}
					mappings = append(mappings, m)
				}
				if len(mappings) > 0 {
					executorOpts = append(executorOpts, executor.WithToolCredentials(mappings, c.repos.TaskCredentials))
					c.Logger.Info().Int("tool_credentials", len(mappings)).
						Msg("secrets: capturing operator-facing credentials from these tools' output (credential carryover)")
				}
			}
			c.secretsDetector = detector
			c.secretsActions = actions
			// Phase 2: artifact store was constructed in initScheduler
			// before the detector existed; wire it now.
			if c.artifactStore != nil {
				c.artifactStore.SetSecrets(detector, actions)
				c.artifactStore.SetLogger(c.Logger.With().Str("component", "artifacts").Logger())
			}
			c.Logger.Info().Int("patterns", len(c.Config.Secrets.Patterns.Custom)).
				Msg("secrets: detector wired into executor")
			// Phase 2: result_json now enforces Block (step fails
			// with SECRET_LEAK). tool_audit and container_logs
			// still degrade to Redact — refusing the audit row
			// loses observability, refusing the failure-log
			// display loses operator visibility, and both
			// trade-offs were the original Phase 1 reasoning. Log
			// the residual degradation so operators who configure
			// block on those two checkpoints aren't surprised.
			redactDegradedCheckpoints := []string{
				secrets.CheckpointToolAudit,
				secrets.CheckpointContainerLogs,
			}
			for _, cp := range redactDegradedCheckpoints {
				if actions[cp] == secrets.ActionBlock {
					c.Logger.Warn().
						Str("checkpoint", cp).
						Msg("secrets: 'block' configured but this checkpoint still degrades to redact (audit fidelity / display visibility); use 'detect' or 'redact' explicitly to silence this warning")
				}
			}
		}
	} else {
		c.Logger.Info().Msg("secrets: layer disabled by config")
	}

	// Shared rate-limiter instance — same state seen by autonomy loop,
	// dispatcher's create_task, and API POST /tasks so a project can't
	// burst past its cap by routing through a different entry point.
	// Backend selector (hardening sub-item 5).
	c.initProjectRateLimiter()
	// Per-API-key request-rate limiter. Single shared instance —
	// the api router and the UI subtree both get it via
	// service options so a key's bucket isn't double-debited
	// when the same caller hits both surfaces.
	c.apiKeyLimiter = ratelimit.NewAPIKeyLimiter()

	// Per-IP backstop (hardening sub-item 2). Allocated only when
	// the daemon config carries a non-zero rps + burst — operators
	// who haven't tuned the block stay on the legacy "no per-IP
	// gate" path. Client-IP resolution (trusted proxies) is now
	// centralised in internal/httpx/realip and applied as the
	// outermost middleware; the limiter reads the resolved IP from
	// the request context.
	if c.Config.API.RateLimit.PerIP.RPS > 0 && c.Config.API.RateLimit.PerIP.Burst > 0 {
		c.perIPLimiter = ratelimit.NewPerIPLimiter()
	}

	// Warm container pool
	if c.Config.Runtime.WarmPool.Enabled {
		poolCfg := runtime.DefaultPoolConfig()
		if c.Config.Runtime.WarmPool.MaxPerRole > 0 {
			poolCfg.MaxPerRole = c.Config.Runtime.WarmPool.MaxPerRole
		}
		if c.Config.Runtime.WarmPool.IdleTimeout != "" {
			if d, err := time.ParseDuration(c.Config.Runtime.WarmPool.IdleTimeout); err == nil && d > 0 {
				poolCfg.IdleTimeout = d
			}
		}
		poolOpts := []runtime.PoolOption{
			runtime.WithPoolLogger(c.Logger),
			runtime.WithPoolEnvVars(executorConfig.AgentLLMEnv),
		}
		if executorConfig.ProjectWorkspacePath != "" {
			poolOpts = append(poolOpts, runtime.WithPoolProjectWorkspacePath(executorConfig.ProjectWorkspacePath))
		}
		pool := runtime.NewWarmPool(runtimeManager, poolCfg, poolOpts...)
		pool.Start()
		c.warmPool = pool
		executorOpts = append(executorOpts, executor.WithWarmPool(pool))
	}

	// System-step handler registry, shared across subsystems. Forge handlers
	// (forge.open_change_request / forge.post_review) wire here independent of
	// memory — they need only the project registry + workspace path. The RAG
	// handlers register into the same registry inside the memory block below.
	// The WithSystemHandlers option is appended once, before the executor is
	// constructed (just above NewWithOptions).
	sysHandlers := executor.NewSystemHandlerRegistry()
	if forgeResolver := c.newForgeResolver(); forgeResolver != nil {
		forgeSrc := &forgePublishSource{workspacePath: executorConfig.ProjectWorkspacePath}
		// c.AIDisclosure is built in initDatabase (container.go:747), which runs
		// before initScheduler (908) — so it is non-nil here. Both handlers
		// refuse to publish if it ever isn't: a pull request and a review comment
		// each reach a human outside the channel chokepoint, so both carry the
		// Art 50(1) notice (G6 findings A and C).
		sysHandlers.Register(forgeh.NewOpenChangeRequestHandler(forgeResolver, forgeSrc, c.artifactStore, c.newTaintReviewer(), c.AIDisclosure))
		sysHandlers.Register(forgeh.NewPostReviewHandler(forgeResolver, c.AIDisclosure))
		sysHandlers.Register(forgeh.NewFetchDiffHandler(forgeResolver))
		c.Logger.Info().Msg("forge system handlers registered (forge.open_change_request, forge.post_review, forge.fetch_diff)")
		// Boot-time push-permission check for every forge-configured project
		// (channel + generic-webhook paths). Non-blocking: a network probe per
		// project shouldn't gate daemon start.
		go c.verifyForgePermissions(context.Background(), forgeResolver)
	}

	// Memory subsystem (must be initialized before executor so we can
	// pass WithMemoryIndexer to the executor options).
	if c.Config.Memory.Enabled {
		memCfg := memory.Config{
			Enabled:               true,
			EmbeddingProvider:     c.Config.Memory.EmbeddingProvider,
			EmbeddingModel:        c.Config.Memory.EmbeddingModel,
			BedrockRegion:         c.Config.Memory.Bedrock.Region,
			EmbeddingDimension:    c.Config.Memory.EmbeddingDimension,
			ChunkTokens:           c.Config.Memory.ChunkTokens,
			ChunkOverlap:          c.Config.Memory.ChunkOverlap,
			WorkerConcurrency:     c.Config.Memory.WorkerConcurrency,
			EmbeddingCacheEnabled: c.Config.Memory.EmbeddingCacheEnabled,
			ResponseCacheEnabled:  c.Config.Memory.ResponseCacheEnabled,
			RetrievalRouting:      retrievalRoutingConfig(c.Config.Memory.RetrievalRouting),
		}
		// Wire pricing so the Phase E cache's CacheStats can surface
		// a TotalSavingsUSD headline on /ui/spend. nil pricing leaves
		// the cache still functional — savings just render as $0.
		if c.pricingTable != nil {
			memCfg.PricingFunc = c.pricingTable.CostUSD
		}
		if strings.EqualFold(strings.TrimSpace(memCfg.EmbeddingProvider), "bedrock") {
			if memCfg.BedrockRegion == "" {
				memCfg.BedrockRegion = c.Config.Chat.Router.Bedrock.Region
			}
		} else {
			// Resolve embedding endpoint/key from memory config, falling back to
			// the resolved agent LLM config so a single chat: section is sufficient.
			llmResolved := c.Config.ResolvedAgentLLM()
			memCfg.EmbeddingEndpoint = c.Config.Memory.EmbeddingEndpoint
			if memCfg.EmbeddingEndpoint == "" {
				memCfg.EmbeddingEndpoint = llmResolved.Endpoint
			}
			memCfg.EmbeddingAPIKey = c.Config.Memory.EmbeddingAPIKey
			if memCfg.EmbeddingAPIKey == "" {
				memCfg.EmbeddingAPIKey = llmResolved.APIKey
			}
		}

		mgr, err := memory.New(memCfg, c.DB,
			c.Logger.With().Str("component", "memory").Logger())
		if err != nil {
			c.Logger.Warn().Err(err).Msg("memory manager initialization failed — continuing without memory")
		} else {
			c.memoryManager = mgr
			executorOpts = append(executorOpts, executor.WithMemoryIndexer(mgr.Indexer))

			// B-7: wire the document-ingest workflow's system
			// handlers. Both depend on the extractor pipeline +
			// the memory indexer being live, which is exactly
			// this branch. Without these the workflow validates
			// (system step type is allowed) but each step would
			// hit "unknown handler" at dispatch.
			extReg := c.ExtractorRegistry()
			extRunner := c.ExtractorRunner()
			if extReg != nil && extRunner != nil && c.repos != nil && c.repos.Artifacts != nil && c.repos.ExtractedDocuments != nil {
				sysHandlers.Register(rag.NewExtractHandler(extReg, extRunner, c.repos.Artifacts))
				// WithProjectScope: when a task payload carries no repo_scope —
				// every creation path except a companion delegate() — fall back to
				// the project's configured default instead of landing the chunks
				// NULL-scoped (the 2026-07-30 census found five projects at 100%
				// NULL for exactly this reason). Registry-nil safe; a project with
				// no default keeps NULL, which is correct for one that is not
				// repo-bound.
				idx := rag.NewIndexHandler(c.repos.ExtractedDocuments, mgr.Indexer).
					WithProjectScope(func(projectID string) string {
						if c.Registry == nil {
							return ""
						}
						if p := c.Registry.GetProject(projectID); p != nil {
							return p.RepoScope
						}
						return ""
					})
				sysHandlers.Register(idx)
				c.Logger.Info().
					Strs("handlers", sysHandlers.Names()).
					Msg("rag system handlers registered")
			} else {
				c.Logger.Warn().Msg("system handlers NOT registered (extractor pipeline / artifacts / extracted_documents missing) — document-ingest workflow will be unrunnable")
			}

			// Phase 1 (memory hardening): wire project_ingest_queue.
			// Producer-side enqueue happens in
			// executor.ingestOutputArtifacts; the IngestWorker
			// drains. Both nil-safe — omitting either preserves
			// the legacy synchronous ingest path.
			ingestQueueRepo := c.repos.IngestQueue
			executorOpts = append(executorOpts, executor.WithIngestQueue(ingestQueueRepo))
			// Surface the enqueue→sync fallback in Prometheus. The
			// closure reads c.memoryMetrics lazily because the metrics
			// sink is registered later in the boot sequence than the
			// executor is constructed; an early-bound pointer would be
			// nil. Nil-safe on the read side — if metrics aren't wired
			// (tests, memory-disabled deployments) the bump silently
			// skips and the Warn log remains the only signal.
			executorOpts = append(executorOpts, executor.WithIngestEnqueueFallbackRecorder(func(projectID string) {
				if c.memoryMetrics != nil && c.memoryMetrics.IngestEnqueueFallbackTotal != nil {
					c.memoryMetrics.IngestEnqueueFallbackTotal.WithLabelValues(projectID).Inc()
				}
			}))
			c.ingestQueueRepo = ingestQueueRepo
			c.ingestWorker = memory.NewIngestWorker(
				ingestQueueRepo,
				c.repos.Artifacts,
				mgr.Indexer,
				c.Logger.With().Str("component", "memory").Str("worker", "ingest").Logger(),
				memory.IngestWorkerConfig{
					// Defaults from the design doc; overridable
					// via Config.Memory.IngestWorker if operators
					// need to tune later.
					PollInterval:            5 * time.Second,
					MaxBatchPerProject:      16,
					MaxAttempts:             3,
					MaxProjectsPerTick:      64,
					CircuitBreakerThreshold: 5,
					CircuitBreakerPause:     60 * time.Second,
				},
			)
			// Route memory-ingest blob reads through the backend-aware
			// Store so the worker reads via S3 when configured (instead
			// of os.ReadFile on a StoragePath that doesn't exist on
			// disk under the S3 backend).
			c.ingestWorker.SetArtifactBlobReader(artifactStore)

			// Phase 2 (memory hardening): wire the gate-and-
			// quarantine pipeline into the worker. Replaces the
			// direct IngestText call with the policy-aware path:
			// gates → quarantine for failures, indexer for
			// allowed candidates, plus per-class metadata stamping.
			c.memoryQuarantineRepo = c.repos.MemoryQuarantine
			memRepoForChunkExists := mgr.Repository()
			// Phase 3: epoch repo + stamp hook. Each ingest run
			// becomes one epoch; chunks carry epoch_id; search
			// joins through corpus_epochs_active.
			c.corpusEpochRepo = c.repos.CorpusEpochs
			// Phase 17: claim_audit_overlap gate looks up extracted
			// claims against tool_audit_log scoped to the candidate's
			// execution_id. Build the adapter once and reuse — no
			// per-candidate construction cost.
			auditLookup := newAuditLookupFunc(c.repos.ToolAudit)
			artifactsRepo := c.repos.Artifacts
			memIngestAuditRepo := c.repos.MemoryIngestAudit
			pipeline := memory.NewPipeline(mgr.Indexer, memory.PipelineConfig{
				Quarantine: c.memoryQuarantineRepo,
				Epochs:     c.corpusEpochRepo,
				ChunkExists: func(ctx context.Context, projectID, hash string) (bool, error) {
					return memRepoForChunkExists.ChunkExistsByHash(ctx, projectID, hash)
				},
				StampEpoch: func(ctx context.Context, projectID, artifactID, epochID string) error {
					return memRepoForChunkExists.StampEpochByArtifact(ctx, projectID, artifactID, epochID)
				},
				CreateCompanionArtifact: func(ctx context.Context, projectID, artifactID, sourceName string, sizeBytes int64) error {
					size := sizeBytes
					return artifactsRepo.Create(ctx, &persistence.Artifact{
						ID:            artifactID,
						ProjectID:     projectID,
						Name:          sourceName,
						ArtifactClass: persistence.ArtifactClassMetadata,
						StoragePath:   "companion://inline",
						SizeBytes:     &size,
						Origin:        persistence.ArtifactOriginUnknown,
					})
				},
				CreateChatMemoryArtifact: func(ctx context.Context, projectID, artifactID, sourceName string, sizeBytes int64) error {
					// Chat memory-write slice 5: same FK precondition as the
					// companion path — the synthetic artifacts row must exist
					// before the chunk upsert (project_memory_chunks.artifact_id).
					size := sizeBytes
					return artifactsRepo.Create(ctx, &persistence.Artifact{
						ID:            artifactID,
						ProjectID:     projectID,
						Name:          sourceName,
						ArtifactClass: persistence.ArtifactClassMetadata,
						StoragePath:   "chat://inline",
						SizeBytes:     &size,
						Origin:        persistence.ArtifactOriginUnknown,
					})
				},
				DeleteChatMemoryArtifact: func(ctx context.Context, _, artifactID string) error {
					// D4.1a compensation: remove the synthetic artifact row.
					return artifactsRepo.Delete(ctx, artifactID)
				},
				DeleteChatMemorySubjectLinks: func(ctx context.Context, projectID, artifactID string) error {
					// D4.1a compensation: data_subject_links are polymorphic
					// (table_name,row_id) with no FK to project_memory_chunks, so
					// they do not cascade — remove any that reference this
					// artifact's chunks before the chunks go.
					if c.DB == nil {
						return nil
					}
					_, err := c.DB.ExecContext(ctx, `
						DELETE FROM data_subject_links
						WHERE table_name = 'project_memory_chunks'
						  AND row_id IN (
							SELECT id FROM project_memory_chunks
							WHERE project_id = $1 AND artifact_id = $2
						  )`, projectID, artifactID)
					return err
				},
				RecordCompanionIngest: func(ctx context.Context, ev memory.CompanionIngestAuditEvent) error {
					if memIngestAuditRepo == nil {
						return nil
					}
					row := &persistence.MemoryIngestAudit{
						ProjectID:      ev.ProjectID,
						SourceName:     ev.SourceName,
						ContentHash:    ev.ContentHash,
						ContentBytes:   ev.ContentBytes,
						Decision:       ev.Decision,
						ChunksAdmitted: ev.ChunksAdmitted,
					}
					if ev.ActorKind != "" {
						s := ev.ActorKind
						row.ActorKind = &s
					}
					if ev.ActorID != "" {
						s := ev.ActorID
						row.ActorID = &s
					}
					if ev.ProposedClass != "" {
						s := ev.ProposedClass
						row.ProposedClass = &s
					}
					if ev.GateFailed != "" {
						s := ev.GateFailed
						row.GateFailed = &s
					}
					if ev.RepoScope != "" {
						s := ev.RepoScope
						row.RepoScope = &s
					}
					return memIngestAuditRepo.Record(ctx, row)
				},
				RecordAgentIngest: func(ctx context.Context, ev memory.AgentIngestAuditEvent) error {
					// Path B (queue-drained agent) audit — finding #4 /
					// mitigation plan §7.3. Maps onto the same
					// memory_ingest_audit row as the companion path.
					if memIngestAuditRepo == nil {
						return nil
					}
					row := &persistence.MemoryIngestAudit{
						ProjectID:      ev.ProjectID,
						SourceName:     ev.SourceName,
						ContentHash:    ev.ContentHash,
						ContentBytes:   ev.ContentBytes,
						Decision:       ev.Decision,
						ChunksAdmitted: ev.ChunksAdmitted,
					}
					if ev.ActorKind != "" {
						s := ev.ActorKind
						row.ActorKind = &s
					}
					if ev.ActorID != "" {
						s := ev.ActorID
						row.ActorID = &s
					}
					if ev.ProposedClass != "" {
						s := ev.ProposedClass
						row.ProposedClass = &s
					}
					if ev.GateFailed != "" {
						s := ev.GateFailed
						row.GateFailed = &s
					}
					if ev.RepoScope != "" {
						s := ev.RepoScope
						row.RepoScope = &s
					}
					return memIngestAuditRepo.Record(ctx, row)
				},
				SecretsDetector:            c.secretsDetector,
				SecretsActions:             c.secretsActions,
				AuditLookup:                auditLookup,
				PromptInjectionAction:      c.Config.Memory.PromptInjectionScan,
				ClaimAuditDisabledProjects: c.Config.Memory.ClaimAuditDisabledProjects,
				DenyPatterns:               c.Config.Memory.DenyPatterns,
				Logger:                     c.Logger.With().Str("component", "memory").Str("worker", "ingest").Str("pipeline", "v1").Logger(),
			})
			c.ingestWorker.SetPipeline(pipeline)
			c.memoryPipeline = pipeline
			// Wire the searcher's epoch filter so reads honour
			// the active set + legacy fallback.
			mgr.Searcher.SetEpochSource(func(ctx context.Context, projectID string) ([]string, error) {
				return c.corpusEpochRepo.ListActive(ctx, projectID)
			})

			// Secret-leak scan at memory ingest. Mirrors the executor's
			// result.json / tool_audit / container_logs checkpoints —
			// memory chunks live forever, so a leak here is the most
			// permanent class of leak.
			if c.secretsDetector != nil {
				mgr.SetSecrets(c.secretsDetector, c.secretsActions)
			}

			// Wire the retrieval audit repo so each Search call writes
			// a row to memory_retrieval_audit. Powers the "feedback
			// loop" CLI surface — `vornikctl memory feedback` and the
			// auto-prune candidate list.
			memAuditRepo := c.repos.MemoryRetrievalAudit
			mgr.Searcher.SetAuditRepo(memAuditRepo)
			mgr.Searcher.SetLogger(c.Logger.With().Str("component", "memory").Str("audit", "retrieval").Logger())

			// Confidence-based retrieval routing (P3) trace sink: persist
			// the trust_verdict stage row per Routing-on search. Nil-safe —
			// SQLite / unwired deployments skip the trace.
			if c.repos.MemorySearchStage != nil {
				mgr.Searcher.SetTraceSink(c.repos.MemorySearchStage)
			}

			// Scored-sufficiency iterative retrieval. Inert unless
			// enabled AND a real reranker is wired (gated inside the
			// searcher); defaults applied for unset knobs.
			sufCfg := memory.SufficiencyConfig{
				Enabled:    c.Config.Memory.Sufficiency.Enabled,
				MinHighRel: c.Config.Memory.Sufficiency.MinHighRel,
				ScoreFloor: c.Config.Memory.Sufficiency.ScoreFloor,
				MaxRounds:  c.Config.Memory.Sufficiency.MaxRounds,
			}
			if sufCfg.MinHighRel <= 0 {
				sufCfg.MinHighRel = 3
			}
			if sufCfg.ScoreFloor <= 0 {
				sufCfg.ScoreFloor = 0.6
			}
			if sufCfg.MaxRounds <= 0 {
				sufCfg.MaxRounds = 3
			}
			mgr.Searcher.SetSufficiency(sufCfg)

			// LLM reranker. Re-orders every recall by relevance AND
			// activates scored-sufficiency (its absolute floor needs
			// calibrated reranker scores). Disabled / no chat client →
			// NoopReranker (RRF ordering), so this is a no-op unless the
			// operator opts in. One extra LLM call per recall when on,
			// bounded by the timeout and degrading to RRF on failure.
			rr := c.Config.Memory.Reranker
			// Cost accounting: the reranker fires on EVERY recall, so
			// unwired it was the daemon's largest source of
			// unattributed LLM spend (~1,637 calls / $3.49 per day on
			// the instance where the gap was found). Wired 2026-07-31.
			mgr.Searcher.SetReranker(memory.NewConfiguredReranker(
				rr.Enabled, c.ChatClient, rr.Model,
				rr.MaxCandidates, rr.TimeoutSeconds, rr.MaxSnippetBytes,
				c.Logger.With().Str("component", "memory").Str("sub", "reranker").Logger(),
				memory.WithRerankerSpend(c.llmSpend(
					persistence.TaskLLMUsageSourceMemoryReranker, "memory_reranker")),
			))
			// Report the RESOLVED state, not the configured intent.
			//
			// This was invisible before, and the gap was expensive: reranker.enabled
			// was true, this line ran, and across 151,818 production LLM-usage rows
			// not one reranker call was ever made. A feature that can be configured
			// on and still be inert has to announce which gate closed.
			if active, reason := mgr.Searcher.RerankerStatus(); active {
				c.Logger.Info().
					Str("component", "memory").Str("sub", "reranker").
					Str("model", rr.Model).
					Msg("memory reranker ACTIVE — context-assembly recall will rerank")
			} else {
				c.Logger.Warn().
					Str("component", "memory").Str("sub", "reranker").
					Bool("config_enabled", rr.Enabled).
					Bool("chat_client", c.ChatClient != nil).
					Str("model", rr.Model).
					Str("reason", reason).
					Msg("memory reranker INERT — recall stays RRF-ordered and " +
						"scored-sufficiency cannot activate")
			}

			// Phase B of the Policy-Aware Memory Firewall: wire
			// the evaluator + non-blocking audit writer into the
			// Searcher so RecallWithContext can run policy
			// decisions per chunk. Postgres-only — when the
			// MemoryPolicyEvaluations repo is nil (SQLite
			// deployments) the audit writer is built with a
			// no-op sink and the firewall runs without writing
			// rows. Enforcement mode defaults to "off" here
			// (Phase B initial-rollout posture); Phase D wires
			// the daemon-level config toggle.
			//
			// Edition gate: providers.MemoryFirewall + repo availability
			// are checked via memoryFirewallEditionGatePasses, ensuring
			// the tested predicate IS the production gate (single source
			// of truth).
			if c.memoryFirewallEditionGatePasses() {
				c.memoryFirewallWriter = memoryfirewall.NewAuditWriter(
					c.repos.MemoryPolicyEvaluations,
					c.Logger.With().Str("component", "memoryfirewall").Logger(),
				)
				// Resolver reads ProjectFirewall.Mode from the
				// registry (in-memory; no DB call per recall).
				// Empty / unknown values fall through to the
				// daemon-level default via the boolean second
				// return. See c.memoryFirewallModeForProject.
				mgr.Searcher.SetFirewall(&memory.FirewallDeps{
					Evaluator:       memoryfirewall.NewEvaluator(),
					Writer:          c.memoryFirewallWriter,
					EnforcementMode: c.memoryFirewallMode(),
					ModeForProject:  c.memoryFirewallModeForProject,
				})
				c.Logger.Info().
					Str("capability", "memory-firewall").
					Bool("edition_gate", true).
					Str("enforcement", string(c.memoryFirewallMode())).
					Msg("capability registered")
			} else if !c.providers.MemoryFirewall {
				c.Logger.Info().
					Str("capability", "memory-firewall").
					Bool("edition_gate", false).
					Msg("capability omitted by edition")
			}

			// Inject VORNIK_MEM_URL into agent containers so they can call back.
			memURL := agentCallbackURL(c.Config.Server.Address)
			executorConfig.AgentLLMEnv["VORNIK_MEM_URL"] = memURL
			c.Logger.Info().Str("mem_url", memURL).Msg("memory manager initialized")

			// Phase 50/51 (KG memory): construct the four-stage
			// extraction pipeline and the worker that drives it.
			// Per-stage models pin to cost-efficient Bedrock IDs
			// (LLD §4.4a) — gpt-oss-20b for the cheap stages,
			// gpt-oss-120b for the reasoning-heavy relationship
			// extractor. The chat router routes by string prefix
			// so each WithModel call lands on the bedrock
			// sub-provider automatically.
			if c.Config.Memory.Graph.Enabled && c.ChatClient != nil {
				c.startGraphWorker(mgr)
			} else if c.Config.Memory.Graph.Enabled {
				c.Logger.Warn().Msg("memory.graph.enabled but ChatClient nil — KG worker NOT started")
			}

			// Per-chunk topic labels for the vector-cloud UI.
			// Display-only — runs after embedding, failures fall
			// back to markdown heading / filename. Routes through
			// the chat router via ModelOverridable like the KG
			// pipeline.
			if c.Config.Memory.Titler.Enabled && c.ChatClient != nil {
				// Embedding spend attribution (slice 2 of
				// 2026-08-12-embed-spend-attribution-design.md). Wired
				// post-construction like the titler/classifier below, because
				// memory.NewManager builds the Embedder from config alone and has
				// no usage repo.
				//
				// Until this line existed the embed path wrote NO usage row on any
				// provider: re-embedding this corpus is ~1.29M tokens, roughly
				// $0.60-0.75 per pass on a hosted embedder, and vornik reported
				// $0.00. Local embedding is why that was free here, not why it was
				// invisible.
				if mgr.Embedder != nil {
					mgr.Embedder.SetSpend(c.llmSpend(
						persistence.TaskLLMUsageSourceMemoryEmbedder, memory.RoleEmbedder))
				}

				// Billing is now a constructor argument, so this component cannot be
				// built without a decision about its spend
				// (2026-08-12-ledger-completeness-design.md §5).
				titler := memory.NewTitler(c.ChatClient, c.Config.Memory.Titler.Model,
					c.llmSpend(persistence.TaskLLMUsageSourceMemoryTitler, memory.RoleTitler))
				if secs := c.Config.Memory.Titler.TimeoutSeconds; secs > 0 {
					titler.Timeout = time.Duration(secs) * time.Second
				}
				if mb := c.Config.Memory.Titler.MaxPreviewBytes; mb > 0 {
					titler.MaxPreviewBytes = mb
				}
				// Phase E — share the manager's response cache so
				// titler reruns over identical chunks skip the LLM.
				// Nil when Memory.ResponseCacheEnabled is false.
				titler.Cache = mgr.ResponseCache
				mgr.SetTitler(titler)
				c.memoryTitler = titler
				c.memoryTitleBackfiller = &memory.TitleBackfiller{
					Repo:   mgr.Repository(),
					Titler: titler,
					Logger: c.Logger.With().Str("component", "memory").Str("worker", "title-backfill").Logger(),
					// Metrics is wired later in observabilityRegistry()
					// once memory.NewMetrics has registered the sinks;
					// the auto-loop reads this field on every tick so a
					// late binding still flows through.
				}
				c.Logger.Info().
					Str("model", c.Config.Memory.Titler.Model).
					Msg("memory titler wired into embed worker")
			} else if c.Config.Memory.Titler.Enabled {
				c.Logger.Warn().Msg("memory.titler.enabled but ChatClient nil — titler NOT wired")
			}

			// Classifier wiring. Independent toggle from the titler
			// — operators can enable backfill-driven classification
			// without paying for inline title generation. Shares the
			// chat client and pricing table.
			if c.Config.Memory.Classifier.Enabled && c.ChatClient != nil {
				classifier := memory.NewClassifier(c.ChatClient, c.Config.Memory.Classifier.Model,
					c.llmSpend(persistence.TaskLLMUsageSourceMemoryClassifier, memory.RoleClassifier))
				if secs := c.Config.Memory.Classifier.TimeoutSeconds; secs > 0 {
					classifier.Timeout = time.Duration(secs) * time.Second
				}
				if mb := c.Config.Memory.Classifier.MaxPreviewBytes; mb > 0 {
					classifier.MaxPreviewBytes = mb
				}
				// Phase E — share the manager's response cache so
				// `vornikctl memory reclassify --use-llm` reruns over
				// the same chunks skip the LLM.
				classifier.Cache = mgr.ResponseCache
				c.memoryClassifyBackfiller = &memory.ClassifyBackfiller{
					Repo:       mgr.Repository(),
					Classifier: classifier,
					Logger:     c.Logger.With().Str("component", "memory").Str("worker", "classify-backfill").Logger(),
				}

				// Periodic LLM-free per-project gist worker. The library
				// is sub-ms per kilobyte; the cadence is bounded by
				// Memory.ConsolidateIntervalSeconds (default 10 min). The
				// registry-to-IDs adapter keeps the memory package
				// dependency-free of internal/registry.
				cons := memory.NewConsolidator(mgr.Repository())
				if n := c.Config.Memory.ConsolidateMinTokenLength; n > 0 {
					cons.MinTokenLength = n
				}
				if n := c.Config.Memory.ConsolidateTopN; n > 0 {
					cons.TopN = n
				}
				c.memoryConsolidateWorker = &memory.ConsolidateWorker{
					Consolid:  cons,
					Repo:      mgr.Repository(),
					Projects:  &registryProjectIDsAdapter{registry: c.Registry},
					ScanLimit: c.Config.Memory.ConsolidateScanLimit,
					Logger:    c.Logger.With().Str("component", "memory").Str("worker", "consolidate").Logger(),
					// Metrics is wired later in observabilityRegistry().
				}

				// Consumer C (instinct layer, LLD slice 5): feed
				// retrieval-domain boost/prune HINTS to the consolidate
				// sweeper. Double-gated — instinct.enabled AND
				// instinct.consumers.memory_hygiene — and DEFAULT FALSE, so
				// with either gate off the worker's Hygiene stays nil and
				// behaviour is byte-for-byte identical to today. Advisory
				// only: the worker logs/counts candidates, never deletes.
				if c.Config.Instinct.Enabled &&
					c.Config.Instinct.Consumers.MemoryHygiene &&
					c.repos != nil && c.repos.Instincts != nil {
					deadDays := c.Config.Instinct.DeadDays
					if deadDays <= 0 {
						deadDays = 60
					}
					c.memoryConsolidateWorker.Hygiene = &memory.RetrievalHygiene{
						Enabled:   true,
						Instincts: c.repos.Instincts,
						Policies:  mgr.Repository(),
						Logger:    c.Logger.With().Str("component", "memory").Str("consumer", "instinct-hygiene").Logger(),
					}
					c.Logger.Info().
						Int("dead_days", deadDays).
						Msg("instinct memory-hygiene consumer wired (advisory; no auto-delete)")
				}

				// LLM-tier narrative pass (opt-in via
				// Memory.LLMConsolidateEnabled). Sits on top of the
				// LLM-free term loop above; the worker skips
				// projects without an existing gist row so order
				// of arrival is irrelevant.
				if c.Config.Memory.LLMConsolidateEnabled && c.ChatClient != nil {
					nw := memory.NewNarrativeWriter(c.ChatClient, c.Config.Memory.LLMConsolidateModel,
						c.llmSpend(persistence.TaskLLMUsageSourceMemoryNarrative, memory.RoleNarrative))
					c.memoryLLMConsolidateWorker = &memory.LLMConsolidateWorker{
						Writer:     nw,
						Repo:       mgr.Repository(),
						Projects:   &registryProjectIDsAdapter{registry: c.Registry},
						SampleSize: c.Config.Memory.LLMConsolidateSampleSize,
						Logger:     c.Logger.With().Str("component", "memory").Str("worker", "llm-consolidate").Logger(),
						// Metrics wired in observabilityRegistry().
					}
					c.Logger.Info().
						Str("model", c.Config.Memory.LLMConsolidateModel).
						Int("sample_size", c.Config.Memory.LLMConsolidateSampleSize).
						Msg("memory llm-consolidate worker wired")
				} else if c.Config.Memory.LLMConsolidateEnabled {
					c.Logger.Warn().Msg("memory.llm_consolidate_enabled but ChatClient nil — narrative worker NOT wired")
				}
				// Measure 3 (2026-05-15): inline classifier fallback at
				// ingest. Pipeline was built earlier in the wiring
				// sequence (chat client / Classifier come up later), so
				// we attach via the post-construction setter. The
				// in-pipeline gate is `ClassifierInlineFallback`; when
				// off, the Classifier sits idle here and is exercised
				// only via the backfiller above.
				if c.memoryPipeline != nil {
					c.memoryPipeline.SetClassifier(classifier, c.Config.Memory.Classifier.InlineFallbackEnabled)
				}
				c.Logger.Info().
					Str("model", c.Config.Memory.Classifier.Model).
					Bool("inline_fallback", c.Config.Memory.Classifier.InlineFallbackEnabled).
					Msg("memory classifier wired")
			} else if c.Config.Memory.Classifier.Enabled {
				c.Logger.Warn().Msg("memory.classifier.enabled but ChatClient nil — classifier NOT wired")
			}
		}
	}

	// Wire the shared system-handler registry (forge + rag handlers, registered
	// above) exactly once, if any handler landed.
	if len(sysHandlers.Names()) > 0 {
		executorOpts = append(executorOpts, executor.WithSystemHandlers(sysHandlers))
	}
	// Snapshot the registered handler names for the role-library doctor
	// check (composer task 1.1b concern-2) — see Container.systemHandlerNames.
	c.systemHandlerNames = sysHandlers.Names()

	// Agent-LLM health breaker (LLD 2026-07-12-agent-llm-health-breaker).
	// Default-on (nil Enabled → ON with agent defaults, MinSamples=3);
	// explicit enabled:false → nil registry → passthrough. Metrics wired
	// after the executor (below) so the gauge/trips land on the same
	// registerer. Topology-1 (agents curl the upstream directly) gets the
	// breaker this way; topology-2 (agents via the daemon chat proxy) is
	// already covered by the chat-router HealthGatedProvider, and the
	// agent breaker abstains on a chat-breaker MODEL_UNHEALTHY reject to
	// avoid double-counting.
	agentHealthReg := func() *agenthealth.Registry {
		hcfg, ok := resolveAgentHealthConfig(c.Config.Runtime.AgentLLM.Health)
		if !ok {
			return nil
		}
		return agenthealth.NewRegistry(agenthealth.Config{Health: hcfg, Enabled: true})
	}()
	c.agentHealth = agentHealthReg
	if agentHealthReg != nil {
		executorOpts = append(executorOpts, executor.WithAgentHealth(agentHealthReg))
	}

	c.Executor = executor.NewWithOptions(
		runtimeManager,
		execRepo,
		c.repos.Artifacts,
		taskRepo,
		executorConfig,
		executorOpts...,
	)

	// Item 2 (dynamic-tool-budget follow-ups §2, 2026-07-18): surface tool-budget
	// config foot-guns ONCE at startup when the feature is enabled — a warm role
	// (its budgets stay static by design) or a role with no base
	// VORNIK_MAX_TOOL_ITERATIONS (falls back to the daemon default before
	// scaling). Advisory WARN only; never fatal. Deduped across swarms.
	if c.Config != nil && c.Config.ToolBudget.Enabled && c.Registry != nil {
		var views []toolbudget.RoleLintView
		for _, sw := range c.Registry.ListSwarms() {
			for _, role := range sw.Roles {
				views = append(views, toolbudget.RoleLintView{
					Name:          role.Name,
					RuntimePolicy: role.RuntimePolicy,
					HasBaseLimit:  strings.TrimSpace(role.Runtime.EnvVars["VORNIK_MAX_TOOL_ITERATIONS"]) != "",
				})
			}
		}
		seen := map[string]bool{}
		for _, w := range toolbudget.LintRoles(toolbudget.Config{Enabled: true}, views) {
			if seen[w] {
				continue
			}
			seen[w] = true
			c.Logger.Warn().Msg(w)
		}
	}

	// Startup visibility for the instinct auto-apply gate (the "is it armed?"
	// kill-switch signal). A single INFO line is the right surface for this
	// journald-centric, single-operator deployment — see the supply design's
	// observability note (a dashboard gauge was considered and dropped to
	// avoid Option-ordering fragility for a low-value metric).
	if c.Config != nil && c.Config.Instinct.Enabled &&
		c.Config.Instinct.Consumers.FailurePlaybooks &&
		c.Config.Instinct.Consumers.AutoApply.Enabled {
		c.Logger.Info().
			Float64("min_confidence", instinctAutoApplyMinConfidence(c.Config)).
			Int("min_clean_support", instinctAutoApplyMinCleanSupport(c.Config)).
			Strs("allowed_error_classes", instinctAutoApplyAllowedClasses(c.Config)).
			Msg("instinct auto-apply ARMED (recovery remediations may be surfaced as prompt-level directives)")
	}

	cfg := scheduler.DefaultConfig()
	cfg.MaxConcurrency = c.Config.Scheduler.MaxConcurrentTasks
	if c.Config.Scheduler.LeaseTimeout != "" {
		if leaseTimeout, err := time.ParseDuration(c.Config.Scheduler.LeaseTimeout); err == nil && leaseTimeout > 0 {
			cfg.LeaseDurationSeconds = int(leaseTimeout.Seconds())
		}
	}

	schedulerOptions := []scheduler.Option{
		scheduler.WithLogger(c.Logger),
		scheduler.WithRuntimeManager(runtimeManager),
		scheduler.WithExecutionRepository(execRepo),
		scheduler.WithExecutor(c.Executor),
		scheduler.WithArtifactStore(artifactStore),
	}
	if registry := c.observabilityRegistry(); registry != nil {
		schedulerOptions = append(schedulerOptions, scheduler.WithPrometheusRegistry(registry))
	}
	if c.Registry != nil {
		schedulerOptions = append(schedulerOptions, scheduler.WithProjectRegistry(c.Registry))
	}

	c.Scheduler = scheduler.NewWithOptions(taskRepo, cfg, schedulerOptions...)
	return nil
}

// initWatchdog wires the stuck-execution watchdog. Lives alongside
// the scheduler — the scheduler picks tasks up; the watchdog is the
// safety net for executions that started but never advanced.
// Bad-config values (unparseable durations, unknown action) fall
// back to compiled defaults inside the watchdog package: a typo
// must surface as warn-only logging, not as a silently-disabled
// safety net.
func (c *Container) initWatchdog() error {
	// The watchdog scans for stuck executions, which only a worker node
	// produces. Skip it on ui/webhook nodes (incident 2026-06-12 family).
	if c.skipNonWorker("watchdog") {
		return nil
	}

	wcfg := c.Config.Watchdog

	cfg := watchdog.DefaultConfig()
	// Only override the package default when the operator explicitly
	// set the key. WatchdogConfig.Enabled is a pointer-bool precisely
	// so a missing `watchdog:` block (or a watchdog block with no
	// `enabled:` key) keeps the safety net on — Go's zero value for
	// a plain bool would silently flip the default to false here and
	// disable the detector. See the 2026-05-18 stuck-execution
	// incident.
	if wcfg.Enabled != nil {
		cfg.Enabled = *wcfg.Enabled
	}
	if wcfg.Interval != "" {
		if d, err := time.ParseDuration(wcfg.Interval); err == nil && d > 0 {
			cfg.Interval = d
		} else {
			c.Logger.Warn().Str("value", wcfg.Interval).Msg("watchdog: invalid interval, using default")
		}
	}
	if wcfg.StuckThreshold != "" {
		if d, err := time.ParseDuration(wcfg.StuckThreshold); err == nil && d > 0 {
			cfg.StuckThreshold = d
		} else {
			c.Logger.Warn().Str("value", wcfg.StuckThreshold).Msg("watchdog: invalid stuck_threshold, using default")
		}
	}
	if wcfg.Action != "" {
		cfg.Action = watchdog.Action(wcfg.Action)
	}
	// Approval-timeout sweep runs on the watchdog loop. Sourced from
	// autonomy.approval_timeout_hours (default 96; 0 disables).
	if h := c.Config.Autonomy.ApprovalTimeoutHours; h > 0 {
		cfg.ApprovalTimeout = time.Duration(h) * time.Hour
	}

	var metrics *watchdog.Metrics
	if reg := c.observabilityRegistry(); reg != nil {
		metrics = watchdog.NewMetrics(reg)
	}

	c.Watchdog = watchdog.New(cfg, c.repos.Executions, c.repos.Tasks, c.Logger, metrics)
	// Budget-reservation sweep backstop (trading-hardening §1): settle
	// reservations whose task went terminal or that have gone stale, so a
	// leaked reservation can't block a project's hard cap forever.
	if c.Watchdog != nil && c.repos.BudgetReservations != nil {
		c.Watchdog.SetReservationSweeper(c.repos.BudgetReservations)
	}
	return nil
}

// initEffectiveCostMonitor wires the $/success drift detector. Pairs
// with the cost forecast (preventive — refuses tasks before they
// run) and the budget breach gate (reactive — fires when total spend
// crosses a cap): this signal fires when the *quality* of spend
// degrades, regardless of whether total spend is healthy.
//
// Optional: nil notifier means logs-only; Telegram bot serves as the
// notifier when configured. Bad-config values fall back to compiled
// defaults inside the budget package — same pattern as the watchdog.
func (c *Container) initEffectiveCostMonitor() error {
	// The cost-drift monitor's scan loop reads LLM-usage / step-outcome rows
	// that only a worker node produces. Skip it on ui/webhook nodes.
	if c.skipNonWorker("effective_cost_monitor") {
		return nil
	}

	wcfg := c.Config.EffectiveCost

	cfg := budget.DefaultEffectiveCostConfig()
	cfg.Enabled = wcfg.Enabled
	if wcfg.Interval != "" {
		if d, err := time.ParseDuration(wcfg.Interval); err == nil && d > 0 {
			cfg.Interval = d
		} else {
			c.Logger.Warn().Str("value", wcfg.Interval).Msg("effective_cost: invalid interval, using default")
		}
	}
	if wcfg.CurrentWindow != "" {
		if d, err := time.ParseDuration(wcfg.CurrentWindow); err == nil && d > 0 {
			cfg.CurrentWindow = d
		} else {
			c.Logger.Warn().Str("value", wcfg.CurrentWindow).Msg("effective_cost: invalid current_window, using default")
		}
	}
	if wcfg.BaselineWindow != "" {
		if d, err := time.ParseDuration(wcfg.BaselineWindow); err == nil && d > 0 {
			cfg.BaselineWindow = d
		} else {
			c.Logger.Warn().Str("value", wcfg.BaselineWindow).Msg("effective_cost: invalid baseline_window, using default")
		}
	}
	if wcfg.RatioThreshold > 0 {
		cfg.RatioThreshold = wcfg.RatioThreshold
	}
	if wcfg.MinCurrentSpendUSD > 0 {
		cfg.MinCurrentSpendUSD = wcfg.MinCurrentSpendUSD
	}
	if wcfg.MinBaselineOks > 0 {
		cfg.MinBaselineOks = wcfg.MinBaselineOks
	}
	if wcfg.Cooldown != "" {
		if d, err := time.ParseDuration(wcfg.Cooldown); err == nil && d > 0 {
			cfg.Cooldown = d
		} else {
			c.Logger.Warn().Str("value", wcfg.Cooldown).Msg("effective_cost: invalid cooldown, using default")
		}
	}

	llmRepo := c.repos.LLMUsage
	outcomeRepo := c.repos.StepOutcomes

	// Notifier is left nil here — the Telegram bot isn't constructed
	// yet at this point in the startup sequence. The post-bot wiring
	// step calls EffectiveCostMon.SetNotifier(c.TelegramBot), which
	// is safe because the monitor only consults its notifier inside
	// scanOnce. Without a notifier, the monitor still runs and logs
	// alerts; just no Telegram delivery.
	c.EffectiveCostMon = budget.NewEffectiveCostMonitor(cfg, llmRepo, outcomeRepo, nil, c.Logger)
	return nil
}

// initProjectRateLimiter selects the per-project rate-limit backend
// (hardening sub-item 5). "memory" (default) is the legacy in-process
// limiter; "postgres" persists counters in the ratelimit_counters
// table created by migration 42 so multi-daemon SaaS deployments
// enforce one combined cap. Unknown values warn and fall back to
// memory so a typo doesn't refuse every task.
//
// Side effects: populates c.rateLimiter (interface) and, when
// postgres is selected, c.rateLimiterPostgres (concrete) so the
// startup-sweep + future periodic sweeper goroutine can reach the
// SweepExpired method.
func (c *Container) initProjectRateLimiter() {
	switch strings.ToLower(strings.TrimSpace(c.Config.API.RateLimit.Backend)) {
	case "postgres":
		c.rateLimiterPostgres = ratelimit.NewPostgresProjectLimiter(c.instrumentedDB())
		c.rateLimiter = c.rateLimiterPostgres
		retention := 24 * time.Hour
		if raw := strings.TrimSpace(c.Config.API.RateLimit.CounterRetention); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d > 0 {
				retention = d
			}
		}
		c.rateLimiterRetention = retention
		// Best-effort startup sweep so a long-uninstalled daemon
		// doesn't carry a giant counter table forward into the
		// new lifetime. The periodic sweeper (started in Start)
		// keeps the table bounded thereafter.
		if n, err := c.rateLimiterPostgres.SweepExpired(context.Background(), retention); err != nil {
			c.Logger.Warn().Err(err).Msg("ratelimit: initial sweep failed; counter table may grow until next sweep")
		} else if n > 0 {
			c.Logger.Info().Int64("rows_swept", n).Msg("ratelimit: initial counter sweep")
		}
		c.Logger.Info().Str("retention", retention.String()).Msg("ratelimit: postgres backend selected (sub-item 5)")
	case "", "memory":
		c.rateLimiter = ratelimit.New()
	default:
		c.Logger.Warn().
			Str("backend", c.Config.API.RateLimit.Backend).
			Msg("ratelimit: unknown backend — falling back to in-process memory")
		c.rateLimiter = ratelimit.New()
	}
}

// runRateLimiterCounterSweep is the periodic janitor for the
// postgres ratelimit_counters table. Without it, a long-running
// daemon's counter rows pile up indefinitely (one row per
// (scope, key, window-start) tuple). Cadence is retention/4 with
// a 30-minute floor so the table never holds much more than
// retention-worth of rows.
//
// Leader-gated when c.ratelimitCounterSweepElector is non-nil so
// multi-replica deployments only sweep once per cadence globally.
// SweepExpired is idempotent (DELETE ... WHERE window_start <
// cutoff) so a missed gate just wastes a query, not correctness.
func (c *Container) runRateLimiterCounterSweep(ctx context.Context) {
	if c.rateLimiterPostgres == nil {
		return
	}
	retention := c.rateLimiterRetention
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	cadence := retention / 4
	if cadence < 30*time.Minute {
		cadence = 30 * time.Minute
	}
	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if c.ratelimitCounterSweepElector != nil && !c.ratelimitCounterSweepElector.IsLeader() {
				continue
			}
			n, err := c.rateLimiterPostgres.SweepExpired(ctx, retention)
			if err != nil {
				c.Logger.Warn().Err(err).Msg("ratelimit: periodic counter sweep failed")
				continue
			}
			if n > 0 {
				c.Logger.Info().Int64("rows_swept", n).Msg("ratelimit: periodic counter sweep")
			}
		}
	}
}

// rebuildSchedulerMetrics wires fresh Prometheus metrics onto the existing
// runtime manager, executor, and scheduler without re-creating any of them.
// Called after observability is initialised so that metrics are registered
// against the dedicated Prometheus registry instead of the default one.
func (c *Container) rebuildSchedulerMetrics() {
	reg := c.observabilityRegistry()
	if reg == nil {
		return
	}

	if c.runtimeManager != nil {
		c.runtimeManager.SetMetrics(runtime.NewMetrics(reg))
		c.Logger.Info().Msg("runtime manager metrics wired")
	}

	if c.Executor != nil {
		c.Executor.SetMetrics(executor.NewMetrics(reg))
		c.Logger.Info().Msg("executor metrics wired")
	}

	if c.agentHealth != nil {
		c.agentHealth.SetMetrics(agenthealth.NewMetrics(reg))
		c.Logger.Info().Msg("agent LLM health breaker metrics wired")
	}

	if c.Scheduler != nil {
		c.Scheduler.SetMetrics(scheduler.NewMetrics(reg))
		c.Logger.Info().Msg("scheduler metrics wired")
	}

	// scorecard_floor verifier's rejection counter. Registered here on
	// the served observability registry (not the default registerer the
	// verifier package can't reach) so vornik_trading_floor_rejected_total
	// is visible on /metrics — avoids the 2026-06-06 invisible-metric class.
	verifier.RegisterFloorMetrics(reg)
	c.Logger.Info().Msg("trading floor metrics wired")

	// Memory firewall metrics (LLD § Observability / drift-mitigation
	// §8.3). The six promised series were registered nowhere before
	// this — operators' Grafana panels read flat zero. Wire the audit-
	// writer hooks + the recall-side decision/eval collectors here,
	// after observability boots (the registry isn't available at
	// firewall-wiring time in startMemoryManager). No-op when the
	// firewall wasn't wired (SQLite / firewall-disabled).
	if c.memoryFirewallWriter != nil {
		fwMetrics := memoryfirewall.NewMetrics(reg)
		c.memoryFirewallWriter.SetMetrics(fwMetrics.AuditMetrics())
		if c.memoryManager != nil && c.memoryManager.Searcher != nil {
			c.memoryManager.Searcher.SetFirewallMetrics(fwMetrics)
		}
		c.Logger.Info().Msg("memory firewall metrics wired")
	}
}

// instinctAutoApplyMinConfidence reads instinct.consumers.auto_apply.min_confidence
// nil-safely (0 → WithInstinctAutoApply substitutes the 0.85 default).
func instinctAutoApplyMinConfidence(cfg *config.Config) float64 {
	if cfg == nil {
		return 0
	}
	return cfg.Instinct.Consumers.AutoApply.MinConfidence
}

// instinctAutoApplyMinCleanSupport reads instinct.consumers.auto_apply.min_clean_support
// nil-safely (0 = the clean-evidence gate is off).
func instinctAutoApplyMinCleanSupport(cfg *config.Config) int {
	if cfg == nil {
		return 0
	}
	return cfg.Instinct.Consumers.AutoApply.MinCleanSupport
}

// instinctAutoApplyAllowedClasses reads instinct.consumers.auto_apply.allowed_error_classes
// nil-safely (empty = every class meeting the confidence floor is eligible).
func instinctAutoApplyAllowedClasses(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	return cfg.Instinct.Consumers.AutoApply.AllowedErrorClasses
}

// retrievalRoutingConfig maps the config-package routing block onto the
// memory-package RetrievalRoutingConfig. Zero-valued knobs are left zero so
// the searcher's applyDefaults fills them from the shipped §3.2 defaults;
// only explicitly-set operator values are carried through. no_ttl_age_cap is
// expressed in days at the config surface and converted to a duration here.
func retrievalRoutingConfig(rr config.MemoryRetrievalRoutingConfig) memory.RetrievalRoutingConfig {
	out := memory.RetrievalRoutingConfig{
		K:                      rr.K,
		MinResults:             rr.MinResults,
		HighThreshold:          rr.HighThreshold,
		LowThreshold:           rr.LowThreshold,
		WStatus:                rr.WStatus,
		WConf:                  rr.WConf,
		WFresh:                 rr.WFresh,
		UnverifiedConfDiscount: rr.UnverifiedConfDiscount,
		NoTTLStaleFreshness:    rr.NoTTLStaleFreshness,
		MaxRounds:              rr.MaxRounds,
	}
	if rr.NoTTLAgeCapDays > 0 {
		out.NoTTLAgeCap = time.Duration(rr.NoTTLAgeCapDays) * 24 * time.Hour
	}
	if rr.WidenEnabled != nil {
		out.SetWidenEnabled(*rr.WidenEnabled)
	}
	if rr.Enabled != nil {
		out.SetEnabled(*rr.Enabled)
	}
	return out
}
