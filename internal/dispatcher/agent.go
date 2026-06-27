// Package dispatcher implements the vornik orchestration dispatcher — the
// LLM-backed agent that processes user requests and drives project/task management.
//
// The dispatcher is protocol-agnostic. The Telegram bot, CLI, or any other
// interface submits a Request and receives a Result. Tool execution, system
// prompt construction, and the LLM tool-calling loop all live here.
package dispatcher

import (
	"context"
	"io"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

// AuditRepository is the subset of persistence.ToolAuditRepository used by the dispatcher.
type AuditRepository interface {
	Log(ctx context.Context, entry *persistence.ToolAuditEntry) error
}

// FileSender delivers a file to the operator on a session's own
// channel/destination. It is constructed per-session and bound to that
// destination, so the dispatcher's file tools (send_artifact,
// render_document) deliver without knowing channel specifics — the Telegram
// adapter binds the chat id, the email adapter binds the reply thread, etc.
// Nil disables the file tools on a channel that has no file-delivery surface.
type FileSender interface {
	// SendArtifactFile delivers one file. fileName is the operator-visible
	// name (the artifact's recorded Name, not a storage key); content streams
	// the bytes; caption is an optional one-line description.
	SendArtifactFile(ctx context.Context, fileName string, content io.Reader, caption string) error
}

// Request is one turn of conversation submitted to the dispatcher.
type Request struct {
	// ChatID is an opaque identifier for the conversation session (e.g. Telegram chat ID).
	// Used only by tools that send files back to the user.
	ChatID int64

	// OriginatingChannel is the conversation.Channel.Name() of the
	// channel that produced this turn (e.g. "telegram", "email",
	// "slack"). Empty when the turn was synthesised internally
	// (autonomy loops, retry-from-step, post-mortem builders).
	// The create_task tool reads this to register a follow-up
	// with the right per-channel registrar so the auto-resume on
	// task completion lands on the originating channel — Telegram
	// chats resume via the Bot, email threads via the email
	// channel, etc.
	OriginatingChannel string

	// OriginatingSessionID is the channel-specific session identifier
	// (Telegram chat ID stringified, email thread root Message-ID,
	// etc.) for the conversation that produced this turn. Pairs
	// with OriginatingChannel for the per-channel follow-up
	// registration. Empty when the turn was synthesised internally.
	OriginatingSessionID string

	// OperatorID is the stable per-operator identifier for the
	// human driving this turn — typically "<channel>:<speaker_id>"
	// (e.g. "telegram:42", "webchat:abc123hash"). Empty for
	// synthesised internal turns (autonomy loops, retry-from-step,
	// post-mortem builders).
	//
	// When non-empty AND a profile repo is wired, the dispatcher
	// fetches the operator's profile + appends an
	// <operator_profile> block to the system prompt so the model
	// reads tone / verbosity / notes the operator has built up
	// across projects and channels. See appendOperatorProfileBlock.
	OperatorID string

	// Messages is the full conversation history including the current user message.
	// The dispatcher prepends the system prompt internally on each LLM call.
	Messages []chat.Message

	// Project is the active project ID for this session.
	Project string

	// Projects is a snapshot of available projects, used to populate the system prompt.
	Projects []*registry.Project

	// LeadSystemPrompt, when non-empty, replaces the default dispatcher system
	// prompt with a project-specific lead agent persona. Set by the Telegram bot
	// when the user has selected a project via /project.
	LeadSystemPrompt string

	// AllowedProjects scopes which project IDs the dispatcher's tools
	// may route to. Empty or nil means "no restriction" — every tool
	// call that accepts project_id sees every project. Non-empty acts
	// as a whitelist: a tool argument like project_id="assistant" is
	// rejected if "assistant" isn't in this list. Contains "*" for
	// unrestricted wildcard users. Same shape as the API's
	// projectIDKey context value, so downstream auth reasoning is
	// consistent across surfaces.
	AllowedProjects []string

	// FileSender delivers output files back to the caller (optional).
	FileSender FileSender

	// ContextTier signals the session's context-budget headroom
	// (PEAK / GOOD / DEGRADING / POOR). Zero-value (TierPeak) means
	// "no degradation signal" — defaults apply. When the caller
	// computes a DEGRADING-or-worse tier (75%+ of context window
	// consumed), the dispatcher forces deferred tool loading
	// regardless of the configured catalog-size threshold, shrinking
	// the visible schema to the bare minimum + tool_search. Lets
	// chat sessions degrade gracefully before context exhaustion
	// turns into a hard truncation or a runaway tool loop.
	ContextTier chat.ContextTier

	// ContextHeadroomPct is the remaining-budget percentage that
	// produced ContextTier — [0, 100]. The dispatcher records it
	// against the vornik_chat_context_headroom_pct histogram so
	// operators can tune tier thresholds off the observed
	// distribution. Zero means "no signal" by convention — the
	// channel didn't compute headroom (e.g. tests, sub-agent
	// paths). Callers that compute the tier from a real (used,
	// limit) pair should set this via chat.HeadroomPct so the
	// histogram is meaningful.
	ContextHeadroomPct float64
}

// Result is returned by Process after the tool-calling loop completes.
type Result struct {
	// Text is the final assistant response.
	Text string

	// NewProject is non-empty when switch_project was invoked during processing.
	// The caller should update the session's active project accordingly.
	NewProject string

	// Messages contains the full updated conversation history (without system prompt).
	// The caller should replace the session's conversation state with this slice.
	Messages []chat.Message

	// GuardWarnings is the list of output-guard findings recorded
	// on tool RESULTS during this Process call. Empty when the
	// guard is disabled or saw no findings. The dispatcher does
	// not block on findings — operators retain agency; it surfaces
	// them so the UI / Telegram bot can render a non-jargon banner
	// alongside the assistant reply.
	GuardWarnings []GuardWarning

	// Err is non-nil when the dispatcher encountered a fatal error.
	Err error
}

// Agent is the dispatcher. It owns the tool-calling loop and system prompt.
type Agent struct {
	chatClient      chat.Provider
	toolExecutor    *ToolExecutor
	logger          zerolog.Logger
	metrics         *Metrics
	mcpManager      MCPExecutor
	maxIterations   int
	watchFunc       TaskWatchFunc
	auditRepo       AuditRepository
	memory          MemorySearcher
	graphSearcher   GraphSearcher
	memoryCorrector MemoryCorrector
	llmUsageRepo    persistence.TaskLLMUsageRepository
	reservRepo      persistence.BudgetReservationRepository
	// operatorProfiles (optional) DB-backs the per-operator
	// preferences + notes the dispatcher injects into the
	// system prompt at the start of every turn. Nil keeps the
	// pre-feature behaviour (no <operator_profile> block).
	operatorProfiles persistence.OperatorProfileRepository
	// operatorIdentityLinks (optional) collapses a channel-
	// specific speaker id (tg:N / web:hash / slack:U…) onto a
	// canonical operator id so an operator who's linked their
	// Telegram + webchat identities sees one profile in both
	// surfaces. Nil falls back to "speaker id IS canonical id"
	// (the pre-link-feature behaviour).
	// See https://docs.vornik.io
	operatorIdentityLinks persistence.OperatorIdentityLinkRepository
	// profileUseAudit (optional) writes one row per turn whose
	// dispatcher injected a non-empty <operator_profile> block,
	// recording which keys + whether notes influenced the
	// prompt. Powers `vornikctl operator audit`. Nil disables
	// the audit; the read-side citation markers still tell the
	// operator which key shaped which reply.
	profileUseAudit persistence.ProfileUseAuditRepository
	pricing         *pricing.Table
	rateLimiter     ratelimit.ProjectLimiter
	budgetNotifier  budget.Notifier
	artifactStore   InputArtifactStore
	// attachmentAutoExtractor is the seam between the dispatcher's
	// input-file snapshot path and the document-extraction
	// pipeline. When wired, every snapshotted input artifact runs
	// through the extractor on create_task so the worker container
	// sees the extracted text via document_* tools instead of
	// blowing context on raw bytes. nil disables — covers tests +
	// deployments without the extractor wiring; behaviour matches
	// pre-Phase-3 (raw path passed through).
	attachmentAutoExtractor AttachmentAutoExtractor
	// followupRegistrar receives RegisterFollowup calls when the
	// dispatcher schedules a task with await_completion=true. The
	// bot uses these to auto-resume the chat conversation when
	// the task reaches a terminal status. Optional — nil disables
	// the auto-resume path (tests, deployments without a chat).
	followupRegistrar FollowupRegistrar
	// outputGuard scans tool-call RESULTS for adversarial content
	// (prompt injection, credential leakage, encoded payloads)
	// before they enter conversation context. HIGH-severity
	// findings are redacted in place; lower severities pass
	// through with a Result.GuardWarnings entry so the UI /
	// Telegram bot can render a banner. nil disables the layer.
	// The dispatcher passes scanned + maybe-redacted content
	// onward to the LLM, not the raw tool output.
	outputGuard *outputGuardConfig
	// intentJudge wires the two-tier risk judge: a sub-ms
	// heuristic verdict fires before every tool call; an async
	// LLM refiner re-evaluates and persists the refined verdict
	// for later calibration. nil disables both tiers.
	intentJudge *intentJudgeConfig
	// hallucinationDetector scans the dispatcher's final reply text
	// against a chat-side grounding context (recent tool-call
	// outputs from THIS turn + registry / task snapshots). On
	// High signals, the agent appends a system retry prompt and
	// re-runs the tool loop ONCE; if the retry also hallucinates,
	// the offending claims are surfaced to the user via a warning
	// banner in the reply. nil disables the layer.
	hallucinationDetector *hallucination.Detector
	// hallucinationMetrics observes Phase 1 detector emissions for
	// the dispatcher's chat-side scan. Nil disables — the detector
	// still fires; signals just don't bump counters.
	hallucinationMetrics *hallucination.Metrics
	// taskRepoForGrounding lets the detector hydrate KnownTaskIDs
	// from the recent-tasks snapshot. Distinct from the
	// ToolExecutor's taskRepo because we want a small, read-only
	// surface here — unrelated to task creation.
	taskRepoForGrounding persistence.TaskRepository
	// defaultModel is the daemon's VORNIK_LLM_MODEL fallback,
	// used by cost-forecasting when neither the role nor the swarm
	// pins a model. Optional — empty string disables the
	// forecast's pricing-fallback path for unmodelled steps.
	defaultModel string
	// billingProjectID, when non-empty, overrides the per-call
	// project attribution in recordLLMUsage. Without this, every
	// dispatcher call lands on whichever project the chat is
	// pinned to (the active project) — meaning chat overhead
	// pollutes per-project cost dashboards on projects that may
	// have no automation enabled. Setting this to a dedicated
	// "assistant" project routes all chat-driven LLM cost to
	// that project regardless of which project the conversation
	// was about.
	//
	// MCP / memory / budget / autonomy decisions still flow from
	// the active project — only the cost attribution moves.
	// Empty preserves the legacy behaviour for back-compat.
	billingProjectID string
	// chatAuditRepo persists one row per dispatcher turn so
	// operators can answer "why did the bot do (or not do) X
	// this turn?" without grepping journald. nil disables the
	// layer cleanly — every turn still runs; just no audit row.
	// See https://docs.vornik.io "Chat-layer observability" for the
	// surface design.
	chatAuditRepo persistence.ChatAuditRepository

	// emailSender backs the send_email tool. nil disables the
	// tool cleanly (it returns "not configured"). The service
	// container builds an adapter over the per-project email
	// channels and supplies it via WithEmailSender at construction
	// or SetEmailSender afterwards — the latter is needed because
	// the email channels are wired after the dispatcher agent is
	// built (chicken-and-egg: dispatcher feeds the channel
	// receiver, channels feed the email sender).
	emailSender EmailSender

	// reminderRepo + reminderKicker back the set_reminder tool
	// (2026.7.0 — scheduled-reminders LLD). Both nil-safe; the
	// tool handler degrades to a "not configured on this daemon"
	// message when the repo isn't wired, and skips the kick when
	// the kicker is nil.
	reminderRepo   persistence.ReminderRepository
	reminderKicker ReminderKicker

	// adminAuditRepo wires the set_reminder admin-audit emit
	// (Phase B). Mirrors the runner's reminder.fired audit so
	// operators see the full set→fire→(cancel?) chain in
	// /ui/admin/audit. nil disables.
	adminAuditRepo persistence.AdminAuditRepository
}

// SetEmailSender wires (or replaces) the email-sender after Agent
// construction. The service container uses this because the
// per-project email channels are built after the dispatcher agent
// itself, and we can't bind in advance. Safe to call before any
// concurrent Process call lands — wiring happens during boot, well
// ahead of inbound dispatch. Nil clears the sender; the send_email
// tool then reverts to "not configured."
func (a *Agent) SetEmailSender(es EmailSender) {
	if a == nil {
		return
	}
	a.emailSender = es
	if a.toolExecutor != nil {
		a.toolExecutor.emailSender = es
	}
}

// SetChannelFollowupRegistrar wires (or replaces) the per-channel
// follow-up registrar for the named channel. Same late-binding
// pattern as SetEmailSender — channels are constructed after the
// dispatcher agent, so we can't pass them via WithChannelFollowupRegistrar
// at construction time.
//
// Pass nil registrar to unwire the channel (turns off auto-resume
// for that channel; tasks created from its sessions complete
// silently). The create_task tool consults this map via
// Request.OriginatingChannel.
func (a *Agent) SetChannelFollowupRegistrar(channelName string, registrar ChannelFollowupRegistrar) {
	if a == nil || channelName == "" {
		return
	}
	if a.toolExecutor == nil {
		return
	}
	if a.toolExecutor.channelFollowupRegistrars == nil {
		a.toolExecutor.channelFollowupRegistrars = map[string]ChannelFollowupRegistrar{}
	}
	if registrar == nil {
		delete(a.toolExecutor.channelFollowupRegistrars, channelName)
		return
	}
	a.toolExecutor.channelFollowupRegistrars[channelName] = registrar
}

// InputArtifactStore is the narrow interface the dispatcher needs to
// snapshot user-supplied input files (Telegram uploads, API
// attachments) into durable storage. Defined as an interface — not a
// concrete *artifacts.Store dependency — so unit tests can stub it
// without dragging the full filesystem-backed store into the test
// binary, and so a deployment that disables the artifacts subsystem
// gets a clean nil-check rather than a runtime panic.
//
// Retrieve was added when the read_artifact tool started routing
// through the backend-aware Store (phase-4 storage abstraction
// follow-up). The same interface now covers both write and read
// because the dispatcher's two artifact paths sit close enough
// that a separate interface added more noise than value.
type InputArtifactStore interface {
	StoreInput(ctx context.Context, projectID, name, sourcePath string) (*persistence.Artifact, error)
	Retrieve(ctx context.Context, artifactID string) ([]byte, error)
}

// AttachmentAutoExtractor is the dispatcher-side seam to the
// document-extraction pipeline. createTask invokes this per
// snapshotted input artifact so non-email channels (Telegram,
// webchat, API) get the same extraction-at-arrival behaviour the
// email channel already enjoys. Same shape as
// email.AttachmentAutoExtractor by intent — the service container
// builds one adapter and wires it to both seams. Defining the
// interface here (rather than importing email's) keeps the
// dispatcher package free of an email-package dependency.
type AttachmentAutoExtractor interface {
	AutoExtract(ctx context.Context, in AutoExtractRequest) (*AttachmentExtraction, error)
}

// AutoExtractRequest mirrors email.AutoExtractRequest. Kept here
// so dispatcher consumers don't import the email package.
type AutoExtractRequest struct {
	ProjectID   string
	ArtifactID  string
	Name        string
	MimeType    string
	StoragePath string
}

// AttachmentExtraction is the dispatcher-facing summary of a
// successful extraction. The dispatcher folds this into the task
// payload context so the worker's enrich step sees the same
// "↳ ingested into project memory" trailer the email channel
// produces.
type AttachmentExtraction struct {
	ExtractedDocumentID string
	Title               string
	Author              string
	SectionCount        int
	ChunksIngested      int
}

// MCPExecutor routes tool calls to MCP servers, scoped per project.
// Implemented by mcp.Manager. Tools(projectID) returns only the tools
// available to that project — the operator's gmail MCP is not visible
// to a different project even if both happen to declare a server named
// "gmail".
type MCPExecutor interface {
	Tools(projectID string) []chat.Tool
	Execute(ctx context.Context, projectID, qualifiedName, argsJSON string) (string, error)
}

// SetMetrics sets the Prometheus metrics for the dispatcher.
func (a *Agent) SetMetrics(m *Metrics) {
	if a != nil {
		a.metrics = m
	}
}

// SetHallucinationMetrics wires the Phase 1 Prometheus sink so the
// dispatcher's chat-side scan bumps a counter per emitted signal.
// Nil-safe.
func (a *Agent) SetHallucinationMetrics(m *hallucination.Metrics) {
	if a != nil {
		a.hallucinationMetrics = m
	}
}

// NewAgent creates a dispatcher Agent.
// The repos and registry are used by the tool executor; all may be nil (tools
// will return appropriate errors when nil).
func NewAgent(
	chatClient chat.Provider,
	taskRepo persistence.TaskRepository,
	execRepo persistence.ExecutionRepository,
	artifactRepo persistence.ArtifactRepository,
	reg *registry.Registry,
	opts ...AgentOption,
) *Agent {
	a := &Agent{
		chatClient: chatClient,
		// Default 10. Was 30 historically, but a tool-using chat
		// turn that fires 30 LLM calls is almost always a runaway
		// loop, not legitimate work — the dispatcher's tool set
		// is small and most useful turns finish in 3-5
		// iterations. Operators can raise via WithMaxIterations.
		maxIterations: 10,
		logger:        zerolog.Nop(),
	}
	for _, opt := range opts {
		opt(a)
	}
	a.toolExecutor = &ToolExecutor{
		registry:                     reg,
		taskRepo:                     taskRepo,
		execRepo:                     execRepo,
		artifactRepo:                 artifactRepo,
		artifactStore:                a.artifactStore,
		attachmentAutoExtractor:      a.attachmentAutoExtractor,
		attachmentAutoExtractTimeout: 60 * time.Second,
		watchFunc:                    a.watchFunc,
		mcpManager:                   a.mcpManager,
		auditRepo:                    a.auditRepo,
		memory:                       a.memory,
		graphSearcher:                a.graphSearcher,
		memoryCorrector:              a.memoryCorrector,
		llmUsageRepo:                 a.llmUsageRepo,
		reservRepo:                   a.reservRepo,
		rateLimiter:                  a.rateLimiter,
		budgetNotifier:               a.budgetNotifier,
		pricing:                      a.pricing,
		defaultModel:                 a.defaultModel,
		followupRegistrar:            a.followupRegistrar,
		// 2026.7.0 F12 — per-chat expanded-tool store. Always
		// allocated so deferred loading kicks in automatically
		// once the MCP catalog grows past the threshold; tests
		// can short-circuit via chatID=0.
		expanded:              newExpandedToolStore(),
		emailSender:           a.emailSender,
		reminderRepo:          a.reminderRepo,
		reminderKicker:        a.reminderKicker,
		adminAuditRepo:        a.adminAuditRepo,
		operatorProfiles:      a.operatorProfiles,
		operatorIdentityLinks: a.operatorIdentityLinks,
		logger:                a.logger,
	}
	return a
}
