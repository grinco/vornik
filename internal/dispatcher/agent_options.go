package dispatcher

// Agent functional-options surface. All the WithXxx setters that the
// service container chains into NewAgent live here so agent.go itself
// can stay focused on struct + lifecycle + processing.

import (
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/ratelimit"
)

// AgentOption configures an Agent.
type AgentOption func(*Agent)

// WithLogger sets the logger.
func WithLogger(l zerolog.Logger) AgentOption {
	return func(a *Agent) { a.logger = l }
}

// WithMaxIterations overrides the tool iteration cap (default 10
// since 2026.4.x; was 30 historically, but runaway tool loops on
// chat turns were the dominant cost driver and 10 is plenty for
// typical tool-use turns). Operators running heavy tool-use chats
// can raise it; lighter deployments can drop it further.
func WithMaxIterations(n int) AgentOption {
	return func(a *Agent) {
		if n > 0 {
			a.maxIterations = n
		}
	}
}

// WithTaskWatchFunc sets the callback for task completion notifications.
func WithTaskWatchFunc(fn TaskWatchFunc) AgentOption {
	return func(a *Agent) { a.watchFunc = fn }
}

// WithFollowupRegistrar wires the bot-side hook that auto-resumes
// the chat conversation when a task scheduled with
// await_completion=true completes. Production passes the
// telegram.Bot here; tests can pass nil to leave the path
// unwired (in which case create_task with await_completion=true
// degrades to fire-and-forget — the LLM still gets the task ID
// back, but no auto-resume fires).
func WithFollowupRegistrar(r FollowupRegistrar) AgentOption {
	return func(a *Agent) {
		a.followupRegistrar = r
	}
}

// WithOutputGuard enables the post-tool-result scanner that
// catches prompt-injection / credential leakage / encoded
// payloads in tool output before they enter the LLM's context.
// Pairs with the intent judge on the input side — the judge
// validates intent before tool execution; the output guard
// validates content after.
//
// `redactHigh` controls whether HIGH-severity findings get
// rewritten in place with [REDACTED:<kind>] markers before the
// LLM sees the body. Default behaviour in production is true;
// false is useful for offline adversarial-testing setups where
// the operator wants to inspect the raw flagged content.
func WithOutputGuard(redactHigh bool) AgentOption {
	return func(a *Agent) {
		a.outputGuard = &outputGuardConfig{RedactHigh: redactHigh}
	}
}

// WithHallucinationDetector wires the Phase 1 chat-side detector.
// The dispatcher runs it on the final assistant text after the
// tool loop terminates; on High signals it triggers ONE in-place
// retry with a system prompt explaining the rejected claims, then
// surfaces a warning banner to the user if the retry also fails.
// Nil disables the layer.
func WithHallucinationDetector(d *hallucination.Detector) AgentOption {
	return func(a *Agent) {
		a.hallucinationDetector = d
	}
}

// WithHallucinationMetrics wires the Phase 1 Prometheus sink so
// each detector emission bumps a counter labelled by severity +
// detector name. Nil-safe.
func WithHallucinationMetrics(m *hallucination.Metrics) AgentOption {
	return func(a *Agent) {
		a.hallucinationMetrics = m
	}
}

// WithGroundingTaskRepo gives the dispatcher's hallucination
// detector a read-only handle to the task repo so it can hydrate
// KnownTaskIDs from the recent-tasks snapshot when scanning a
// reply. Distinct from the create_task path's repo so test
// stubs and partial deployments can wire one without the other.
func WithGroundingTaskRepo(repo persistence.TaskRepository) AgentOption {
	return func(a *Agent) {
		a.taskRepoForGrounding = repo
	}
}

// WithMCPManager sets the MCP tool executor for external tool integration.
func WithMCPManager(m MCPExecutor) AgentOption {
	return func(a *Agent) { a.mcpManager = m }
}

// WithLLMUsageRepository wires the per-step LLM usage repo into the
// dispatcher. When set, create_task calls run a budget check before
// scheduling new work and refuse with a user-facing message when the
// project is over its configured daily or monthly hard cap.
func WithLLMUsageRepository(repo persistence.TaskLLMUsageRepository) AgentOption {
	return func(a *Agent) { a.llmUsageRepo = repo }
}

// WithBudgetReservationRepository wires the reservation ledger so the
// dispatcher's create_task path atomically reserves hard-cap budget for a
// chat/DM-created task before it's persisted (trading-hardening §1) — the
// same atomic gate the API / webhook / autonomy paths already run. nil
// disables the reservation (the read-only budget Check still applies).
func WithBudgetReservationRepository(repo persistence.BudgetReservationRepository) AgentOption {
	return func(a *Agent) { a.reservRepo = repo }
}

// WithOperatorProfileRepository wires the per-operator profile
// store. When set + the request carries an OperatorID, the
// dispatcher fetches the profile at the start of every turn
// and appends an <operator_profile> block to the system
// prompt so the model reads accumulated preferences + notes.
// Nil keeps the pre-feature behaviour — no block injected.
func WithOperatorProfileRepository(repo persistence.OperatorProfileRepository) AgentOption {
	return func(a *Agent) { a.operatorProfiles = repo }
}

// WithOperatorIdentityLinkRepository wires the cross-channel
// identity-link table. When set, the dispatcher resolves every
// inbound speaker id (tg:N / web:hash / slack:U…) to its
// canonical operator id before reading or writing the operator
// profile, so an operator who's run `/link` (or
// `vornikctl operator link`) sees one profile across every
// channel they've consolidated. Nil falls back to the
// pre-link-feature behaviour. See
// https://docs.vornik.io
func WithOperatorIdentityLinkRepository(repo persistence.OperatorIdentityLinkRepository) AgentOption {
	return func(a *Agent) { a.operatorIdentityLinks = repo }
}

// WithProfileUseAuditRepository wires the per-turn audit log of
// operator-profile use. When set, the dispatcher writes one row
// every time maybeInjectOperatorProfile produces a non-empty
// <operator_profile> block — used by `vornikctl operator audit`
// to surface "the model started citing your 'prefers Czech'
// preference on day X". Nil disables the audit; the citation
// markers in the prompt still surface profile use to the
// operator in the reply itself. See
// https://docs.vornik.io (Phase B).
func WithProfileUseAuditRepository(repo persistence.ProfileUseAuditRepository) AgentOption {
	return func(a *Agent) { a.profileUseAudit = repo }
}

// WithRateLimiter wires the shared task-creation rate limiter. When set,
// create_task runs the limiter's per-minute/per-hour Check before
// scheduling and records a successful creation for counting.
func WithRateLimiter(l ratelimit.ProjectLimiter) AgentOption {
	return func(a *Agent) { a.rateLimiter = l }
}

// WithBudgetNotifier wires a sink that receives soft- and hard-cap
// alerts when create_task trips the project budget. Optional — nil
// keeps the log-only behaviour and continues to return the same
// error envelope to the model.
func WithBudgetNotifier(n budget.Notifier) AgentOption {
	return func(a *Agent) { a.budgetNotifier = n }
}

// WithDefaultModel pins the daemon's VORNIK_LLM_MODEL fallback so the
// dispatcher's cost forecast can resolve roles that don't override the
// model. Optional — empty disables the pricing fallback for those
// steps and they contribute zero to the forecast (history-only mode).
func WithDefaultModel(model string) AgentOption {
	return func(a *Agent) { a.defaultModel = model }
}

// WithBillingProjectID overrides the per-call project attribution
// for dispatcher LLM usage rows. Without this, every chat round-
// trip's cost lands on whichever project the chat is pinned to as
// its active project — which mixes "this project's actual work
// cost" with "chat overhead while talking about this project" on
// the per-project spend dashboards. Setting this to a dedicated
// assistant project routes all chat-driven LLM cost there.
//
// MCP / memory / budget / autonomy still gate against the active
// project — only the cost attribution moves. Empty preserves the
// pre-existing behaviour for back-compat.
func WithBillingProjectID(projectID string) AgentOption {
	return func(a *Agent) { a.billingProjectID = projectID }
}

// WithPricing wires the model pricing table so dispatcher LLM calls write
// their token-attributed cost into task_llm_usage alongside the agent
// container's workflow_step rows. A nil table means cost_usd will be 0 on
// the written rows — tokens are still counted.
func WithPricing(t *pricing.Table) AgentOption {
	return func(a *Agent) { a.pricing = t }
}

// WithAuditRepository enables tool-call audit logging for the dispatcher.
// When set, every tool invocation from Telegram or the CLI is recorded in the
// same audit table as agent container tool calls.
func WithAuditRepository(repo AuditRepository) AgentOption {
	return func(a *Agent) { a.auditRepo = repo }
}

// WithMemorySearcher gives the dispatcher direct access to project memory.
// Required for the memory_search tool; without it the dispatcher can still
// schedule research tasks but cannot answer questions from memory.
func WithMemorySearcher(m MemorySearcher) AgentOption {
	return func(a *Agent) { a.memory = m }
}

// WithGraphSearcher wires the knowledge-graph read surface so the
// memory_search tool appends an "entities + 1-hop relationships"
// block alongside the chunk hits (LLD §6.2). nil-safe / opt-in:
// without it (or against a project with no extracted entities) the
// tool's behaviour is unchanged — chunk-only. The searcher enforces
// the same project + repo_scope + cross-project isolation as chunk
// retrieval.
//
// see https://docs.vornik.io §6.
func WithGraphSearcher(g GraphSearcher) AgentOption {
	return func(a *Agent) { a.graphSearcher = g }
}

// WithMemoryCorrector enables the memory_correct tool. Without it
// the tool descriptor is still registered (so the LLM knows it
// COULD exist in some deployments) but execution returns a clear
// "not enabled on this daemon" message rather than 500-ing.
func WithMemoryCorrector(c MemoryCorrector) AgentOption {
	return func(a *Agent) { a.memoryCorrector = c }
}

// WithInputArtifactStore wires a store for snapshotting user-supplied
// input_files into durable storage on create_task. Without it the
// dispatcher passes the raw host path through to the task payload
// (current behaviour, fine for retries while the source file lives
// but loses uploads when /tmp gets reaped or the workspace is cleaned).
func WithInputArtifactStore(s InputArtifactStore) AgentOption {
	return func(a *Agent) { a.artifactStore = s }
}

// WithAttachmentAutoExtractor wires the document-extraction
// pipeline into the dispatcher's create_task snapshot loop. Each
// snapshotted input artifact gets a synchronous extraction pass;
// the resulting summary lands in the task payload context so the
// worker sees a memory-ready trailer instead of a raw host path.
// nil disables — the dispatcher falls back to passing the raw
// snapshot path through verbatim (pre-Phase-3 behaviour).
func WithAttachmentAutoExtractor(x AttachmentAutoExtractor) AgentOption {
	return func(a *Agent) { a.attachmentAutoExtractor = x }
}

// WithChatAuditRepo enables per-turn audit logging — one
// chat_audit_log row per Process/ProcessStreaming call capturing
// model, system-prompt hash, tool calls, response excerpt, cost.
// nil disables the layer cleanly (turns still run; no row).
func WithChatAuditRepo(r persistence.ChatAuditRepository) AgentOption {
	return func(a *Agent) { a.chatAuditRepo = r }
}

// WithEmailSender enables the send_email tool. nil disables the
// tool (it returns a "not configured" message). Production wiring
// supplies an adapter that routes by projectID to the right
// email.Channel; see internal/service.
func WithEmailSender(s EmailSender) AgentOption {
	return func(a *Agent) { a.emailSender = s }
}

// WithReminderRepository enables the set_reminder tool. nil leaves
// the tool registered (so the LLM knows it exists) but each call
// returns a "not configured on this daemon" message rather than
// 500ing. Production wiring passes the postgres.ReminderRepository
// from internal/storage.
func WithReminderRepository(r persistence.ReminderRepository) AgentOption {
	return func(a *Agent) { a.reminderRepo = r }
}

// WithReminderKicker wires the reminders.Runner's Kick method so a
// fresh set_reminder call for "in 30s" fires within seconds rather
// than waiting up to one full poll interval. Optional — when nil
// the reminder still lands on the next regular heartbeat tick.
func WithReminderKicker(k ReminderKicker) AgentOption {
	return func(a *Agent) { a.reminderKicker = k }
}

// WithAdminAuditRepository wires the admin-audit repo so the
// set_reminder tool writes a reminder.set row on success.
// Optional — nil leaves the chat-set audit silent (the runner's
// reminder.fired row still records the eventual fire).
func WithAdminAuditRepository(repo persistence.AdminAuditRepository) AgentOption {
	return func(a *Agent) { a.adminAuditRepo = repo }
}
