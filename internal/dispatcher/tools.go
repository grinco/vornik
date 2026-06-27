package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/idfmt"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/untrusted"
)

// filepathBase returns the basename of a path. Wrapper kept thin so
// dispatcher unit tests can stub it if a future caller needs a
// different name-derivation rule (e.g. artifact ID + extension).
func filepathBase(p string) string { return filepath.Base(p) }

// basePathProvider is the optional capability an InputArtifactStore
// implementation exposes so the dispatcher can derive its default
// allow-list of legitimate input roots without a separate config
// field. The concrete *artifacts.Store satisfies this via BasePath().
type basePathProvider interface {
	BasePath() string
}

// inputFileSourceRoots returns the set of host directories a literal
// create_task `input_files` path is allowed to live under. When an
// operator pins allowedInputRoots explicitly that set wins verbatim;
// otherwise we derive the two always-legitimate roots the dispatcher
// can see: os.TempDir() (where channel uploads — Telegram/webchat —
// land) and the artifact store base path (where prior-task artifacts
// and freshly-snapshotted inputs resolve to). Roots are symlink-
// resolved so the containment check in confineInputFileSource
// compares like-for-like, mirroring executor.allowedStagingRoots.
func (te *ToolExecutor) inputFileSourceRoots() []string {
	if len(te.allowedInputRoots) > 0 {
		roots := make([]string, 0, len(te.allowedInputRoots))
		for _, r := range te.allowedInputRoots {
			roots = append(roots, resolveRootForContainment(r))
		}
		return roots
	}
	roots := []string{resolveRootForContainment(os.TempDir())}
	if bp, ok := te.artifactStore.(basePathProvider); ok {
		if base := bp.BasePath(); base != "" {
			roots = append(roots, resolveRootForContainment(base))
		}
	}
	return roots
}

// resolveRootForContainment canonicalises a single allow-list root,
// preferring the symlink-resolved form and falling back to a Clean.
// Matches the resolution executor.allowedStagingRoots applies so the
// two trust boundaries agree on what "under" means.
func resolveRootForContainment(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// confineInputFileSource canonicalises a literal create_task input
// source (absolute + symlink resolution) and reports whether it lands
// beneath one of roots. Returns (canonicalPath, true) on accept;
// ("", false) on a path that cannot be made absolute or escapes every
// root. This is the dispatcher-side analogue of
// executor.resolveStagingSrc — it has to live here too because
// read_artifact/send_artifact deliver the snapshotted bytes back to
// the model without ever crossing the worker's container-staging
// guard, so confining only at the executor would leave an
// exfiltration path open. The returned symlink-resolved path is what
// the caller hands to StoreInput, closing the TOCTOU window between
// the gate and the subsequent read.
func confineInputFileSource(src string, roots []string) (string, bool) {
	if src == "" {
		return "", false
	}
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return "", false
	}
	cleanSrc, err := filepath.EvalSymlinks(absSrc)
	if err != nil {
		cleanSrc = filepath.Clean(absSrc)
	}
	for _, r := range roots {
		if cleanSrc == r || strings.HasPrefix(cleanSrc, r+string(filepath.Separator)) {
			return cleanSrc, true
		}
	}
	return "", false
}

// resolveInputFileSourceTracked wraps resolveInputFileSource and also
// reports whether the entry resolved FROM an artifact ID (i.e. the
// returned path is an artifact StoragePath that artifactRepo.Get
// confirmed). Such entries are trusted by construction — they name a
// real artifact row inside the store base path, so they bypass the
// literal-path allow-list gate in createTask. A return of
// (path, false) means "src was a literal filesystem path" and MUST be
// confined against inputFileSourceRoots before any read.
func (te *ToolExecutor) resolveInputFileSourceTracked(ctx context.Context, src string) (string, bool) {
	resolved := te.resolveInputFileSource(ctx, src)
	// resolveInputFileSource only rewrites when src had no path
	// separator AND matched an artifact row; in every other case it
	// returns src verbatim. So a changed value is proof of a
	// successful artifact-ID resolution.
	return resolved, resolved != src
}

// MemorySearcher is the narrow interface the dispatcher needs from the
// memory subsystem. Keeping it as an interface avoids pulling the full
// memory.Manager shape into unit tests and lets the dispatcher work
// unchanged when memory is disabled (nil implementation).
type MemorySearcher interface {
	Search(ctx context.Context, projectID, query string, limit int) ([]memory.SearchResult, error)
}

// MemoryTemporalSearcher is the optional capability surface for
// memory searchers that support from_date/to_date filtering.
// Added 2026.6.0 as the external-research retrofit — the tool
// type-asserts for this interface and falls back to plain Search
// when the implementation doesn't provide it. Keeps the legacy
// MemorySearcher contract stable.
type MemoryTemporalSearcher interface {
	SearchWithOptions(ctx context.Context, projectID, query string, opts memory.SearchOptions) ([]memory.SearchResult, error)
}

// MemoryFirewallSearcher is the optional capability surface for
// memory searchers wired through the Policy-Aware Memory
// Firewall (2026-05-29). The dispatcher's memory_search tool
// type-asserts for this interface so the firewall audit row
// carries dispatcher-side metadata (role + operator + purpose).
// Implementations that don't satisfy it fall back to plain
// Search / SearchWithOptions — those still run through the
// firewall (default-on routing) but with an empty
// RequestContext, so the audit row records the retrieval
// without per-tool attribution.
type MemoryFirewallSearcher interface {
	RecallWithContext(
		ctx context.Context,
		projectID, query string,
		opts memory.SearchOptions,
		reqCtx memoryfirewall.RequestContext,
	) ([]memory.SearchResult, error)
}

// GraphSearcher is the optional knowledge-graph read surface the
// memory_search tool blends into its result. When wired, after the
// chunk hits the tool appends a structured "entities + 1-hop
// relationships" block so the lead sees the graph layer alongside
// raw chunks (HippoRAG-style hybrid). Nil → chunk-only behaviour,
// unchanged. The interface is the narrow read slice the dispatcher
// uses; *graph.Searcher satisfies it.
//
// see https://docs.vornik.io §6.2.
type GraphSearcher interface {
	// FindEntities returns entities matching the query, scoped to
	// projectID. types nil = all types.
	FindEntities(ctx context.Context, projectID, query string, types []string, limit int) ([]*persistence.KnowledgeEntity, error)
	// GetEntity loads one entity + its outgoing/incoming 1-hop edges,
	// scoped to projectID (returns nil entity when not in project).
	GetEntity(ctx context.Context, projectID, entityID string) (*persistence.KnowledgeEntity, []*persistence.KnowledgeEdge, []*persistence.KnowledgeEdge, error)
}

// ProjectSwitch is returned when the tool executor processes a switch_project call.
// The caller applies it to the session state.
type ProjectSwitch struct {
	ProjectID string
}

// ToolResult holds the text result and an optional project-switch side effect.
type ToolResult struct {
	Content       string
	ProjectSwitch *ProjectSwitch
	// Provenance carries the trust level of the tool result's content.
	// The zero value (ProvenanceUnknown) is the fail-safe: the output
	// guard treats it as third-party and runs the full rule set.
	// Dispatcher-composed builtins set ProvenanceFirstParty so that
	// injection-class output guard rules are skipped on their results.
	Provenance outputguard.Provenance
}

// TaskWatchFunc is a callback to register a chat for task completion notifications.
type TaskWatchFunc func(taskID string, chatID int64)

// FollowupRegistrar is the bot-side hook the dispatcher calls when
// create_task is invoked with `await_completion: true`. The bot
// records the (chatID, taskID, projectID) triple and, when the
// task reaches a terminal status, automatically resumes the chat
// conversation with a synthetic user turn that injects the task's
// result. This is what makes "schedule a task to get fresh data"
// queries actually deliver the answer the user asked for —
// without depending on the dispatcher model to remember to call
// wait_for_task before its turn ends.
//
// Implementations: the production telegram.Bot satisfies this; tests
// pass nil to disable the auto-resume path.
type FollowupRegistrar interface {
	RegisterFollowup(chatID int64, taskID, projectID string)
}

// ChannelFollowupRegistrar is the per-channel follow-up surface,
// keyed by string sessionID. Lets non-Telegram channels (email
// today; future Slack, GitHub) record their own auto-resume
// targets without forcing every channel into the Telegram-shaped
// int64 chatID model. create_task picks the right registrar by
// Request.OriginatingChannel.
type ChannelFollowupRegistrar interface {
	RegisterFollowup(sessionID, taskID, projectID string)
}

// ToolExecutor executes individual tool calls on behalf of the dispatcher agent.
type ToolExecutor struct {
	registry      *registry.Registry
	taskRepo      persistence.TaskRepository
	execRepo      persistence.ExecutionRepository
	artifactRepo  persistence.ArtifactRepository
	artifactStore InputArtifactStore // nil disables input snapshotting
	// allowedInputRoots is the allow-list of host directories a
	// create_task `input_files` entry may name as a *literal*
	// filesystem path before it is snapshotted via StoreInput.
	// create_task accepts model-controlled input, so a literal path
	// is a read primitive into the project artifact store (and back
	// out via read_artifact/send_artifact). Without confinement the
	// model can name /etc/passwd or the daemon's secrets dir and
	// exfiltrate it. Entries that resolve to an artifact StoragePath
	// (via resolveInputFileSource) already live under the store base
	// path and pass naturally. Empty/nil falls back to the
	// always-legitimate roots derived in inputFileSourceRoots
	// (os.TempDir — where Telegram/webchat uploads land — plus the
	// artifact store base path). See confineInputFileSource.
	allowedInputRoots []string
	// attachmentAutoExtractor runs synchronous extraction after each
	// StoreInput so non-email channels get the same "document is
	// already in memory" trailer the email channel produces. nil
	// disables — snapshotting still works, just no extraction
	// trigger. See document-extraction-design.md §8.1.
	attachmentAutoExtractor AttachmentAutoExtractor
	// attachmentAutoExtractTimeout caps each per-attachment
	// extraction call. The dispatcher's create_task is on the
	// user's interactive critical path — a runaway extractor
	// (multi-GB scanned PDF, malformed file) must not block the
	// task creation. 60s mirrors the email channel's default and
	// covers EPUB/PDF/HTML/text comfortably; audio/video that
	// would need longer fail through the timeout and the
	// operator can re-trigger via the manual /extract endpoint.
	attachmentAutoExtractTimeout time.Duration
	watchFunc                    TaskWatchFunc
	mcpManager                   MCPExecutor
	auditRepo                    AuditRepository
	memory                       MemorySearcher                          // nil when memory subsystem is disabled
	graphSearcher                GraphSearcher                           // nil disables the KG block in memory_search (chunk-only)
	memoryCorrector              MemoryCorrector                         // nil when memory corrector isn't wired
	llmUsageRepo                 persistence.TaskLLMUsageRepository      // nil disables budget enforcement
	reservRepo                   persistence.BudgetReservationRepository // nil disables atomic budget reservation
	rateLimiter                  ratelimit.ProjectLimiter                // nil disables rate-limit enforcement
	budgetNotifier               budget.Notifier                         // nil disables telegram alerts on budget breach
	pricing                      *pricing.Table                          // nil disables forecast cold-start pricing fallback
	defaultModel                 string                                  // VORNIK_LLM_MODEL fallback; empty disables forecast for unmodelled steps
	followupRegistrar            FollowupRegistrar                       // nil disables auto-resume after task completion
	// channelFollowupRegistrars routes per-channel follow-up
	// registrations. Keyed by channel name (matches
	// conversation.Channel.Name()). Empty/nil means "no channel
	// has a follow-up sink wired" — the legacy chatID-based
	// followupRegistrar still works for Telegram. Email and any
	// future non-int64-chatID channels register here.
	channelFollowupRegistrars map[string]ChannelFollowupRegistrar
	// expanded tracks per-chat-session "I uncovered these MCP
	// tools via tool_search" sets. 2026.7.0 F12 deferred loading.
	// nil disables — every MCP tool is always visible (legacy
	// behaviour).
	expanded *expandedToolStore
	// emailSender backs the send_email tool. Nil disables the tool
	// (the dispatch returns "not configured"); production wiring
	// supplies an adapter over the service container's per-project
	// email channels. See [EmailSender] for the contract.
	emailSender EmailSender
	// reminderRepo backs the set_reminder tool. Nil disables it
	// (the tool returns a "not configured on this daemon" message).
	// reminderKicker is the optional sweeper kick — set_reminder
	// for "in 30s" feels broken without it because the next
	// heartbeat tick could be 30s away.
	reminderRepo   persistence.ReminderRepository
	reminderKicker ReminderKicker
	// adminAuditRepo writes one row per chat-initiated reminder
	// set so the admin audit log captures every reminder
	// regardless of which channel originated it. Mirrors the
	// audit shape the runner emits on fire (reminder.fired) and
	// the UI/API cancel handlers emit on cancel
	// (reminder.cancelled). Nil disables the layer cleanly.
	adminAuditRepo persistence.AdminAuditRepository
	// operatorProfiles backs the update_operator_profile tool.
	// Nil disables the tool (it returns "not configured"); the
	// dispatcher's read-path injection still works against the
	// same repo when wired. See tool_update_operator_profile.go.
	operatorProfiles persistence.OperatorProfileRepository
	// operatorIdentityLinks (optional) lets the read+write path
	// collapse a per-channel speaker id onto its canonical
	// operator id. Nil falls back to per-channel behaviour. See
	// https://docs.vornik.io
	operatorIdentityLinks persistence.OperatorIdentityLinkRepository
	logger                zerolog.Logger
}

// ReminderKicker is the narrow contract the set_reminder handler
// uses to kick the heartbeat after a fresh insert. Lets the tool
// stay independent of internal/reminders (which would cause an
// import cycle through service).
type ReminderKicker interface {
	Kick()
}

// Execute dispatches a single tool call and returns the result.
// chatID and fs are forwarded to send_artifact only.
//
// allowedProjects is the session-scope whitelist. Empty/nil means
// "no restriction" (dev mode or API-key callers with unrestricted
// projects). Non-empty acts as an exact-match whitelist, with "*"
// meaning wildcard. This is what prevents a Telegram user pinned to
// project A from calling memory_search(project_id="B") or
// read_artifact on a task belonging to another operator's project.
func (te *ToolExecutor) Execute(ctx context.Context, tc chat.ToolCall, activeProject string, allowedProjects []string, chatID int64, fs FileSender) ToolResult {
	name := tc.Function.Name
	args := tc.Function.Arguments

	te.logger.Debug().Str("tool", name).Str("args", args).Msg("tool call")

	start := time.Now()
	result := te.execute(ctx, tc, activeProject, allowedProjects, chatID, fs)
	te.logAudit(ctx, name, args, result.Content, activeProject, time.Since(start))
	return result
}

func (te *ToolExecutor) logAudit(ctx context.Context, tool, input, output, projectID string, dur time.Duration) {
	if te.auditRepo == nil {
		return
	}
	if len(output) > 2000 {
		output = output[:2000] + "…"
	}
	entry := &persistence.ToolAuditEntry{
		ID:         persistence.GenerateID("ta"),
		ProjectID:  projectID,
		StepID:     "dispatcher",
		ToolName:   tool,
		ToolInput:  input,
		ToolOutput: output,
		DurationMs: persistence.ClampToolAuditDurationMs(dur.Milliseconds()),
		CreatedAt:  time.Now(),
	}
	if err := te.auditRepo.Log(ctx, entry); err != nil {
		te.logger.Warn().Err(err).Str("tool", tool).Msg("dispatcher: failed to write audit entry")
	}
}

func (te *ToolExecutor) execute(ctx context.Context, tc chat.ToolCall, activeProject string, allowedProjects []string, chatID int64, fs FileSender) ToolResult {
	name := tc.Function.Name
	args := tc.Function.Arguments

	switch name {
	case "list_projects":
		return te.listProjects(allowedProjects)
	case "switch_project":
		return te.switchProject(args, allowedProjects)
	case "list_tasks":
		return te.listTasks(ctx, args, activeProject, allowedProjects)
	case "create_task":
		return te.createTask(ctx, args, activeProject, allowedProjects, chatID)
	case "get_task_status":
		return te.getTaskStatus(ctx, args, allowedProjects)
	case "wait_for_task":
		return te.waitForTask(ctx, args, allowedProjects)
	case "cancel_task":
		return te.cancelTask(ctx, args, allowedProjects)
	case "retry_task":
		return te.retryTask(ctx, args, allowedProjects)
	case "list_executions":
		return te.listExecutions(ctx, args, activeProject, allowedProjects)
	case "list_artifacts":
		return te.listArtifacts(ctx, args, allowedProjects)
	case "send_artifact":
		return te.sendArtifact(ctx, args, allowedProjects, fs)
	case "render_document":
		return te.renderDocument(ctx, args, fs)
	case "send_email":
		return te.sendEmail(ctx, args, activeProject, allowedProjects)
	case "set_reminder":
		return te.setReminder(ctx, args, chatID, activeProject)
	case "cancel_reminder":
		return te.cancelReminderTool(ctx, args, chatID)
	case "update_reminder":
		return te.updateReminderTool(ctx, args, chatID)
	case "update_operator_profile":
		return te.updateOperatorProfile(ctx, args)
	case "memory_search":
		return te.memorySearch(ctx, args, activeProject, allowedProjects)
	case memoryCorrectName:
		return te.memoryCorrect(ctx, args, activeProject, allowedProjects)
	case "read_artifact":
		return te.readArtifact(ctx, args, allowedProjects)
	case ToolSearchName:
		return te.toolSearch(args, activeProject, chatID)
	default:
		// Route MCP tools (mcp__{server}__{tool}) to the MCP manager,
		// scoped to the active project so one project's tools can't be
		// called from another project's chat session. The active
		// project itself has already passed the allowlist check in
		// telegram/handlers.go before dispatch reaches here.
		if strings.HasPrefix(name, "mcp__") {
			if !projectAllowed(activeProject, allowedProjects) {
				return ToolResult{Content: fmt.Sprintf("Access to MCP tools for project %q is not permitted for this session.", activeProject)}
			}
			return te.executeMCPTool(ctx, activeProject, name, args)
		}
		return ToolResult{Content: fmt.Sprintf("Unknown tool: %s", name)}
	}
}

func (te *ToolExecutor) listProjects(allowedProjects []string) ToolResult {
	if te.registry == nil {
		return ToolResult{Content: "Project registry is not available."}
	}
	projects := te.registry.ListProjects()
	// Filter by session scope so a scoped user can't enumerate
	// projects they don't have access to (which would tell them
	// whether a project name exists at all).
	visible := make([]*registry.Project, 0, len(projects))
	for _, p := range projects {
		if projectAllowed(p.ID, allowedProjects) {
			visible = append(visible, p)
		}
	}
	if len(visible) == 0 {
		return ToolResult{Content: "No projects available for this session."}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d project(s):\n", len(visible))
	for _, p := range visible {
		name := p.DisplayName
		if name == "" {
			name = p.ID
		}
		fmt.Fprintf(&b, "- %s (%s)\n", p.ID, name)
	}
	return ToolResult{Content: b.String(), Provenance: outputguard.ProvenanceFirstParty}
}

func (te *ToolExecutor) switchProject(argsJSON string, allowedProjects []string) ToolResult {
	var args struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	if args.ProjectID == "" {
		return ToolResult{Content: "project_id is required."}
	}
	// Deny with a distinct message rather than a generic "not found" so
	// operators who misconfigure their allowlist see the real reason.
	if !projectAllowed(args.ProjectID, allowedProjects) {
		return ToolResult{Content: fmt.Sprintf(
			"Access to project '%s' is not permitted for this session.", args.ProjectID)}
	}
	if te.registry != nil {
		if p := te.registry.GetProject(args.ProjectID); p == nil {
			return ToolResult{Content: fmt.Sprintf("Project '%s' not found.", args.ProjectID)}
		}
	}
	return ToolResult{
		Content:       fmt.Sprintf("Switched active project to '%s'.", args.ProjectID),
		ProjectSwitch: &ProjectSwitch{ProjectID: args.ProjectID},
		Provenance:    outputguard.ProvenanceFirstParty,
	}
}

func (te *ToolExecutor) listTasks(ctx context.Context, argsJSON string, activeProject string, allowedProjects []string) ToolResult {
	var args struct {
		ProjectID string `json:"project_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}

	project, err := resolveProjectAllowed(args.ProjectID, activeProject, allowedProjects)
	if err != nil {
		return ToolResult{Content: err.Error()}
	}
	action := chat.Action{
		Type:    chat.ActionListTasks,
		Project: project,
		Status:  args.Status,
	}
	result, err := chat.ExecuteAction(ctx, action, te.taskRepo, te.execRepo, nil)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error: %v", err)}
	}
	// ThirdParty: result.Message can embed exec.ErrorMessage / agent-derived text (chat/parser.go:496) — must be injection-scanned.
	return ToolResult{Content: result.Message, Provenance: outputguard.ProvenanceThirdParty}
}

func (te *ToolExecutor) createTask(ctx context.Context, argsJSON string, activeProject string, allowedProjects []string, chatID int64) ToolResult {
	// AwaitCompletion is parsed as a *bool so we can tell "LLM
	// explicitly opted out" (false) from "LLM didn't set it"
	// (nil → register the followup by default). Required because
	// the small dispatcher model routinely drops the field on
	// retries; without the nil default we silently lose the
	// auto-resume on those retries even when the user is waiting
	// for the answer. See the followup tests for the matrix.
	var args struct {
		ProjectID       string                 `json:"project_id"`
		Type            string                 `json:"type"`
		Prompt          string                 `json:"prompt"`
		WorkflowID      string                 `json:"workflow_id"`
		Input           map[string]interface{} `json:"input"`
		InputFiles      []string               `json:"input_files"`
		AwaitCompletion *bool                  `json:"await_completion"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}

	project, err := resolveProjectAllowed(args.ProjectID, activeProject, allowedProjects)
	if err != nil {
		return ToolResult{Content: err.Error()}
	}

	// Rate-limit gate: caps task creation frequency independently of $
	// budget. Runs first because it's the cheapest check (in-memory
	// counter); no point computing spend if the tap is closed.
	if te.rateLimiter != nil && te.registry != nil {
		if proj := te.registry.GetProject(project); proj != nil {
			if d := te.rateLimiter.Check(proj, time.Now()); d.Blocked {
				return ToolResult{Content: fmt.Sprintf(
					"Cannot create task for project %q: %s (minute=%d hour=%d). Wait a moment and retry, or raise the limit in project YAML.",
					project, d.Reason, d.MinuteCount, d.HourCount)}
			}
		}
	}

	// Task-type gate: must run regardless of workflow_id, and must
	// not get the LLM stuck in a loop. Two failure modes were
	// observed in production with the small dispatcher model
	// (zai.glm-4.7-flash):
	//   1. LLM passes type="feature" → rejected because janka's
	//      AllowedTaskTypes is [research planning writing
	//      comparison summary]; LLM drops type and retries.
	//   2. LLM retries with no type at all → rejected by
	//      executeCreateTask requiring type to be non-empty.
	// The model can't break out of this without help. Two changes
	// here: (a) auto-default type to the first AllowedTaskTypes
	// entry when the project restricts types and the LLM omitted
	// one, (b) tighten the rejection message so a smarter retry
	// has a clear instruction (use ONE of these, not just "type
	// is invalid").
	if te.registry != nil {
		if proj := te.registry.GetProject(project); proj != nil {
			allowed := proj.Autonomy.AllowedTaskTypes
			if args.Type == "" && len(allowed) > 0 {
				args.Type = allowed[0]
				te.logger.Info().
					Str("project", project).
					Str("defaulted_type", args.Type).
					Strs("allowed", allowed).
					Msg("dispatcher: create_task missing type — defaulting to project's first allowed type")
			}
			if args.Type != "" && len(allowed) > 0 {
				ok := false
				for _, t := range allowed {
					if t == args.Type {
						ok = true
						break
					}
				}
				if !ok {
					return ToolResult{Content: fmt.Sprintf(
						"Task type %q is not allowed for project %s. Retry create_task with one of these exact values: %s.",
						args.Type, project, strings.Join(allowed, ", "))}
				}
			}
		}
	}

	// Budget gate: if the resolved project has a spend cap and its daily
	// or monthly hard cap is exceeded, refuse now with a user-facing reason.
	// Soft breaches are logged but allowed through. Registry miss or missing
	// repo skips the check.
	//
	// currentBudget captures the result so the forecast gate below
	// can compose `current + forecast` against the cap without paying
	// for a second SQL aggregate.
	var currentBudget budget.Decision
	if te.llmUsageRepo != nil && te.registry != nil {
		if proj := te.registry.GetProject(project); proj != nil {
			decision, berr := budget.Check(ctx, te.llmUsageRepo, proj, time.Now().UTC())
			if berr != nil {
				te.logger.Warn().Err(berr).Str("project", project).Msg("dispatcher: budget check failed — proceeding")
			} else {
				currentBudget = decision
				if decision.Blocked {
					if te.budgetNotifier != nil {
						period, level := decision.Period()
						te.budgetNotifier.NotifyBudgetBreach(ctx, project, level, period, decision)
					}
					return ToolResult{Content: fmt.Sprintf(
						"Cannot create task for project %q: %s. Increase the budget in project YAML or wait for the period to roll over.",
						project, decision.Reason)}
				} else if decision.SoftBreached {
					te.logger.Warn().
						Str("project", project).
						Str("reason", decision.Reason).
						Msg("dispatcher: create_task proceeding despite soft budget breach")
					if te.budgetNotifier != nil {
						period, level := decision.Period()
						te.budgetNotifier.NotifyBudgetBreach(ctx, project, level, period, decision)
					}
				}
			}
		}
	}

	// Build the payload with taskType and context.prompt so that
	// buildAgentInput can find both the user prompt and the task type.
	if args.Input == nil {
		args.Input = make(map[string]interface{})
	}
	if args.Type != "" {
		args.Input["taskType"] = args.Type
	}
	if args.Prompt != "" {
		if args.Input["context"] == nil {
			args.Input["context"] = map[string]interface{}{}
		}
		if ctx, ok := args.Input["context"].(map[string]interface{}); ok {
			ctx["prompt"] = args.Prompt
		}
	}

	// Attach input files. When an artifact store is wired, snapshot
	// each input into durable artifact storage and replace the host
	// path with the artifact storage path — this is what makes
	// retries reliable across /tmp reaping and workspace cleanup.
	// The artifact IDs are recorded so we can link them to the task
	// row after creation. Without an artifact store (tests, minimal
	// deployments) we fall back to the original host path; retries
	// still work as long as the source file is still there.
	var snapshottedArtifactIDs []string
	if len(args.InputFiles) > 0 {
		if args.Input["context"] == nil {
			args.Input["context"] = map[string]interface{}{}
		}
		// Resolve any artifact-ID entries to their on-disk paths
		// BEFORE the snapshot loop runs. The email-channel
		// attachment-plumbing path surfaces artifact IDs to the LLM
		// (so it can call read_artifact), but the LLM sometimes
		// passes that same ID into create_task's input_files. Without
		// this resolution the snapshotter would try to open the ID as
		// a path, log "input snapshot failed", fall back to the
		// literal ID, and the executor's container-staging guard
		// would reject it as "outside allowed roots." See
		// tool_input_resolve.go.
		// Resolve + confine the SOURCE path before anything reads its
		// bytes. create_task input is model-controlled; a literal host
		// path here is a read primitive — StoreInput copies the file
		// into the project artifact store, and
		// read_artifact/send_artifact then hand the bytes straight back
		// to the model (bypassing the worker's container-staging guard
		// entirely, so the executor's allowed-roots gate never sees
		// these). We must confine at this trust boundary.
		//
		// Two entry shapes:
		//   - artifact ID: resolveInputFileSourceTracked rewrites it to
		//     a StoragePath that artifactRepo.Get already confirmed is a
		//     real artifact row inside the store. Trusted by
		//     construction — bypass the literal-path gate.
		//   - literal filesystem path: gate against the allow-list of
		//     legitimate input roots (os.TempDir(), where channel
		//     uploads land, + the artifact store base path). Anything
		//     else (e.g. the secrets dir, /etc/passwd) is rejected here
		//     and never reaches StoreInput.
		roots := te.inputFileSourceRoots()
		paths := make([]string, 0, len(args.InputFiles))
		for _, src := range args.InputFiles {
			resolved, fromArtifactID := te.resolveInputFileSourceTracked(ctx, src)
			if fromArtifactID {
				paths = append(paths, resolved)
				continue
			}
			canonical, ok := confineInputFileSource(resolved, roots)
			if !ok {
				te.logger.Warn().
					Str("source", resolved).
					Str("project", project).
					Strs("allowed_roots", roots).
					Msg("dispatcher: create_task input_files entry outside allowed roots — rejecting")
				return ToolResult{Content: fmt.Sprintf(
					"Cannot create task: input_files entry %q is not an allowed input. Pass an artifact ID or a file already uploaded through a channel; arbitrary host paths are not permitted.",
					src)}
			}
			paths = append(paths, canonical)
		}
		var extractionSummaries []map[string]any
		if te.artifactStore != nil {
			rewritten := make([]string, 0, len(paths))
			for _, src := range paths {
				name := filepathBase(src)
				art, err := te.artifactStore.StoreInput(ctx, project, name, src)
				if err != nil {
					te.logger.Warn().
						Err(err).
						Str("source", src).
						Str("project", project).
						Msg("dispatcher: input snapshot failed — falling back to source path")
					rewritten = append(rewritten, src)
					continue
				}
				rewritten = append(rewritten, art.StoragePath)
				snapshottedArtifactIDs = append(snapshottedArtifactIDs, art.ID)

				// Auto-extract synchronously per artifact when the
				// pipeline is wired. Best-effort: failure logs and
				// continues; the task still creates with the raw
				// path so the worker can decide what to do.
				// Without this, a Telegram/webchat/API upload of an
				// EPUB or PDF would land in input_files and the
				// researcher would file_read the raw bytes →
				// context-overflow against every Bedrock model.
				if summary := te.autoExtract(ctx, project, art); summary != nil {
					extractionSummaries = append(extractionSummaries, summary)
				}
			}
			paths = rewritten
		}
		if ctx, ok := args.Input["context"].(map[string]interface{}); ok {
			ctx["inputFiles"] = paths
			if len(snapshottedArtifactIDs) > 0 {
				ctx["inputArtifactIDs"] = snapshottedArtifactIDs
			}
			if len(extractionSummaries) > 0 {
				// Persist the per-artifact summaries on the task
				// payload so the worker's prompt builder
				// (executor/plan.go) can surface a memory-ready
				// trailer to the agent — mirroring the email
				// channel's "↳ ingested into project memory"
				// trailer in enrichUserContent.
				ctx["inputExtractions"] = extractionSummaries
			}
		}
	}

	// Sanitize LLM-generated placeholder values before resolving.
	workflowID := args.WorkflowID
	if workflowID == "-" {
		workflowID = ""
	}
	// Resolve to the project's defaultWorkflowID when the LLM didn't
	// pin a workflow explicitly. Prior behaviour matched args.Type
	// against loaded workflows and silently promoted a matching name to
	// workflow_id — but task types are user-meaningful labels ("dev",
	// "research", "assistant"), and several projects have workflows of
	// the same names, so the inference quietly overrode operator-set
	// defaults: a project pinned to "dev-workflow" but receiving a
	// type="dynamic" task would route to the standalone "dynamic"
	// workflow instead of the configured default. defaultWorkflowId is
	// the field operators already set per project for exactly this
	// purpose; honour it. autonomy/manager.go made the same change for
	// the LLM-driven create_task path — keep these two callsites
	// aligned so chat-initiated and autonomy-initiated tasks resolve
	// the same way.
	//
	// Resolve priority alongside workflow from the same project
	// lookup. Hardcoded priority=50 in chat/parser.go used to bypass
	// project.DefaultPriority for every Telegram-created task; the
	// resolution now happens here so the chat layer (which can't
	// reach the registry without a coupling) gets the project's
	// configured value.
	priority := 0
	if te.registry != nil {
		if proj := te.registry.GetProject(project); proj != nil {
			if workflowID == "" {
				workflowID = proj.DefaultWorkflowID
			} else if workflowID != proj.DefaultWorkflowID {
				// Surface LLM-driven workflow overrides so an
				// operator who set defaultWorkflowId in YAML can
				// see WHY their tasks ran on a different
				// workflow. Operators routinely report "I set
				// X but tasks run on Y" — this log line answers
				// that question without a code dive.
				te.logger.Info().
					Str("project", project).
					Str("llm_requested_workflow", workflowID).
					Str("project_default_workflow", proj.DefaultWorkflowID).
					Msg("dispatcher: create_task using LLM-supplied workflow_id, not project default")
			}
			priority = proj.DefaultPriority
		}
	}

	// Validate workflow_id against the project's swarm — every agent role
	// in the workflow must exist in the swarm. This prevents the LLM from
	// routing tasks to incompatible workflows (same check as autonomy manager).
	// AllowedTaskTypes was already enforced above, regardless of workflow_id.
	if workflowID != "" && te.registry != nil {
		proj := te.registry.GetProject(project)
		if proj != nil {
			wf := te.registry.GetWorkflow(workflowID)
			swarm := te.registry.GetSwarm(proj.SwarmID)
			if wf != nil && swarm != nil {
				swarmRoles := make(map[string]struct{}, len(swarm.Roles))
				for _, r := range swarm.Roles {
					swarmRoles[r.Name] = struct{}{}
				}
				for stepID, step := range wf.Steps {
					if step.Type == "agent" && step.Role != "" {
						if _, ok := swarmRoles[step.Role]; !ok {
							return ToolResult{Content: fmt.Sprintf("Workflow %q step %q requires role %q not present in the project's swarm. Omit workflow_id to use the project default.", workflowID, stepID, step.Role)}
						}
					}
				}

				// Forecast gate: predict the task's USD cost from the
				// workflow's step list and refuse if forecast +
				// already-spent would breach the project's hard cap.
				// Distinct from the budget.Check above (which is
				// reactive — fires AFTER spend has crossed the cap).
				// Forecast is preventive — refuses a task whose
				// expected cost would push us over.
				//
				// Only runs when the LLM usage repo is wired (no
				// history → no forecast) and the project has a hard
				// cap configured (no cap → nothing to check
				// against). Forecast errors are logged but
				// non-fatal: the gate's job is to catch obvious
				// over-budget submissions, not to block on transient
				// DB hiccups.
				if te.llmUsageRepo != nil && (proj.Budget.DailyHardUSD > 0 || proj.Budget.MonthlyHardUSD > 0) {
					forecast, ferr := budget.ForecastTask(ctx, te.llmUsageRepo, te.pricing, budget.ForecastInput{
						Workflow:     wf,
						Swarm:        swarm,
						DefaultModel: te.defaultModel,
					}, time.Now().UTC())
					if ferr != nil {
						te.logger.Warn().Err(ferr).
							Str("project", project).
							Str("workflow", workflowID).
							Msg("dispatcher: forecast failed — proceeding without preventive gate")
					} else if d := budget.CheckForecast(proj, forecast, currentBudget); d.Refused {
						te.logger.Info().
							Str("project", project).
							Str("workflow", workflowID).
							Float64("forecast_usd", forecast.USD).
							Float64("daily_spent_usd", currentBudget.DailyUSD).
							Str("reason", d.Reason).
							Msg("dispatcher: refusing create_task — forecast would breach hard cap")
						return ToolResult{Content: fmt.Sprintf(
							"Cannot create task for project %q: %s. Either pick a smaller workflow, raise the cap in project YAML, or wait for the period to roll over.",
							project, d.Reason)}
					} else if forecast.USD > 0 {
						te.logger.Debug().
							Str("project", project).
							Str("workflow", workflowID).
							Float64("forecast_usd", forecast.USD).
							Int("step_count", len(forecast.Steps)).
							Msg("dispatcher: forecast within budget — proceeding")
					}
				}
			}
		}
	}

	// In-flight dedup: if the LLM already created a task with the
	// same project + prompt in the last 30 seconds AND that task is
	// still active (QUEUED / LEASED / RUNNING / WAITING_FOR_CHILDREN
	// / PENDING), return the existing ID instead of creating a
	// duplicate. Two trigger conditions surface this in production:
	//
	//   1. The dispatcher LLM (zai.glm-4.7-flash on the chat path)
	//      hallucinates a wait_for_task with a bogus task_id, gets
	//      an error back, and re-fires create_task to "recover".
	//      Each retry creates a real task.
	//   2. The user sends two telegram messages in quick succession
	//      and the second message's tool loop runs while the first
	//      hasn't completed yet.
	//
	// Reproduced 2026-05-07 ~20:46: a single Telegram conversation
	// turn produced 7 identical "Run one trading tick…" tasks
	// inside a 90-second window.
	//
	// Best-effort: a transient DB error returns no recent tasks and
	// the create proceeds. Better to occasionally double-create
	// than to refuse a legitimate user request because Postgres was
	// briefly unreachable.
	if te.taskRepo != nil && args.Prompt != "" {
		// Same-turn dedup runs first and ignores age + status: if the
		// dispatcher already scheduled this work earlier in the
		// current turn (e.g. T-0918 in the 2026-05-21 watchlist
		// incident), a second create_task for nearly the same prompt
		// must return the existing task ID — even when the first one
		// already completed and the 30s sliding window has lapsed.
		// Empty turn id falls through to the legacy window-only path.
		if turn := ChatTurnIDFromContext(ctx); turn != "" {
			if existing := findSameTurnDuplicateTask(ctx, te.taskRepo, project, turn, args.Prompt); existing != "" {
				te.logger.Info().
					Str("project", project).
					Str("chat_turn_id", turn).
					Str("dedupe_to", existing).
					Str("prompt_preview", truncatePrompt(args.Prompt, 80)).
					Msg("dispatcher: create_task suppressed — same turn already scheduled this work")
				return ToolResult{Content: fmt.Sprintf(
					"A task for nearly the same prompt was already scheduled in this conversation turn (%s). Read its artifacts with read_artifact instead of re-creating.",
					idfmt.Short(existing))}
			}
		}
		if existing := findRecentDuplicateTask(ctx, te.taskRepo, project, args.Prompt, 30*time.Second); existing != "" {
			te.logger.Info().
				Str("project", project).
				Str("dedupe_to", existing).
				Str("prompt_preview", truncatePrompt(args.Prompt, 80)).
				Msg("dispatcher: create_task suppressed — identical prompt already in flight (returning existing task ID)")
			return ToolResult{Content: fmt.Sprintf(
				"A task with this exact prompt is already in flight for project %q (%s). Use wait_for_task or get_task_status with that ID instead of creating a duplicate.",
				project, idfmt.Short(existing))}
		}
	}

	action := chat.Action{
		Type:       chat.ActionCreateTask,
		Project:    project,
		Type_:      args.Type,
		WorkflowID: workflowID,
		Priority:   priority,
		Input:      args.Input,
		// ChatTurnID rides on the action so the created task carries
		// the dispatcher turn that spawned it. Empty when the caller
		// isn't running inside a dispatcher turn (autonomy scheduler,
		// some test harnesses) — the chat parser treats empty as
		// "leave task.ChatTurnID nil", which is the correct
		// "untrackable origin" case for API-initiated work.
		ChatTurnID: ChatTurnIDFromContext(ctx),
	}
	// Atomic hard-cap reservation gate (trading-hardening §1): the read-only
	// budget.Check above is best-effort; this makes the chat-create path
	// reserve the estimate against the cap inside executeCreateTask (which
	// owns the task ID), so chat/DM admissions can't dodge the hard cap.
	// nil-safe — skipped when budget wiring is absent.
	var gate *chat.BudgetGate
	if te.reservRepo != nil && te.llmUsageRepo != nil && te.registry != nil {
		if proj := te.registry.GetProject(project); proj != nil {
			gate = &chat.BudgetGate{Reservations: te.reservRepo, Usage: te.llmUsageRepo, Project: proj}
		}
	}
	result, err := chat.ExecuteAction(ctx, action, te.taskRepo, te.execRepo, gate)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error: %v", err)}
	}
	if te.rateLimiter != nil {
		te.rateLimiter.Record(project, time.Now())
	}

	// Best-effort: link the snapshotted INPUT artifacts to the task we
	// just created. Failure here doesn't block the task — the durable
	// file is already in the artifact store and the task payload
	// already references it. The task_id link only powers UI/audit
	// queries that filter artifacts by task. Logging is enough.
	if task, ok := result.Data.(*persistence.Task); ok && task != nil {
		// Register the originating chat for task completion notification.
		if chatID != 0 && te.watchFunc != nil {
			te.watchFunc(task.ID, chatID)
		}
		// Server-side auto-resume: hand the (chat, task, project)
		// triple to the bot so it can splice the task's result
		// back into the conversation when the task reaches a
		// terminal state. Default ON for any chat-initiated task
		// (chatID != 0) — small dispatcher models drop the
		// await_completion flag on tool-call retries, and an
		// opt-IN default leaves users stranded waiting for an
		// answer that never auto-arrives. The LLM can still opt
		// OUT for genuinely fire-and-forget work by passing
		// await_completion: false explicitly; nil (unset) means
		// register, true means register, false means skip.
		if chatID != 0 && te.followupRegistrar != nil &&
			(args.AwaitCompletion == nil || *args.AwaitCompletion) {
			te.followupRegistrar.RegisterFollowup(chatID, task.ID, task.ProjectID)
		}
		// Per-channel follow-up: when the inbound turn carries an
		// OriginatingChannel (set by ChannelReceiver), look up the
		// matching ChannelFollowupRegistrar and register the
		// sessionID-keyed follow-up so the channel's own resume
		// machinery fires when the task terminates. Telegram chat
		// IDs route through the legacy chatID path above and skip
		// this — chatID != 0 implies the bot already recorded it.
		if chatID == 0 && (args.AwaitCompletion == nil || *args.AwaitCompletion) {
			if channel, sessionID := originatingChannelFromContext(ctx); channel != "" && sessionID != "" {
				if registrar, ok := te.channelFollowupRegistrars[channel]; ok && registrar != nil {
					registrar.RegisterFollowup(sessionID, task.ID, task.ProjectID)
				}
			}
		}
		if len(snapshottedArtifactIDs) > 0 && te.artifactRepo != nil {
			for _, id := range snapshottedArtifactIDs {
				if err := te.artifactRepo.UpdateTaskID(ctx, id, task.ID); err != nil {
					te.logger.Warn().
						Err(err).
						Str("artifact_id", id).
						Str("task_id", task.ID).
						Msg("dispatcher: failed to link input artifact to task")
				}
			}
		}
	}

	// ThirdParty: result.Message can embed exec.ErrorMessage / agent-derived text (chat/parser.go:496) — must be injection-scanned.
	return ToolResult{Content: result.Message, Provenance: outputguard.ProvenanceThirdParty}
}

func (te *ToolExecutor) getTaskStatus(ctx context.Context, argsJSON string, allowedProjects []string) ToolResult {
	var args struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	if _, err := te.taskProjectAllowed(ctx, args.TaskID, allowedProjects); err != nil {
		return ToolResult{Content: err.Error()}
	}

	action := chat.Action{
		Type:   chat.ActionGetStatus,
		TaskID: args.TaskID,
	}
	result, err := chat.ExecuteAction(ctx, action, te.taskRepo, te.execRepo, nil)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error: %v", err)}
	}
	// ThirdParty: result.Message can embed exec.ErrorMessage / agent-derived text (chat/parser.go:496) — must be injection-scanned.
	return ToolResult{Content: result.Message, Provenance: outputguard.ProvenanceThirdParty}
}

// waitForTask polling cadence — exposed as package-level vars so
// tests can override them. Production: 2s fast, 5s slow after a
// 30s grace. Tests squeeze the window down to single-millisecond
// units so the fast→slow transition + the timeout branch both
// run inside a unit-test budget.
var (
	waitForTaskFastPoll   = 2 * time.Second
	waitForTaskSlowPoll   = 5 * time.Second
	waitForTaskFastWindow = 30 * time.Second
)

// waitForTask blocks until the named task reaches a terminal status
// (COMPLETED / FAILED / CANCELLED) or the timeout elapses, then
// returns the task's outcome plus any text artifacts so the
// dispatcher can continue the conversation with the fresh data.
//
// The dispatcher uses this immediately after create_task when the
// task was scheduled to obtain data needed to answer the user's
// question. Without it, the dispatcher would return "task X is
// running" and the user would have to re-ask after completion;
// with it, the dispatcher's tool loop naturally resumes the
// original analysis with the produced data.
//
// Polling cadence: 2s for the first 30s, then 5s. Operators
// running long workflows can pass timeout_seconds up to 1800
// (30 min) — beyond that, fail-fast is the right default since
// chat-completion-side timeouts in the user's client kick in.
func (te *ToolExecutor) waitForTask(ctx context.Context, argsJSON string, allowedProjects []string) ToolResult {
	var args struct {
		TaskID         string `json:"task_id"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	if args.TaskID == "" {
		return ToolResult{Content: "task_id is required"}
	}
	if _, err := te.taskProjectAllowed(ctx, args.TaskID, allowedProjects); err != nil {
		return ToolResult{Content: err.Error()}
	}

	timeout := 600 * time.Second // 10 minutes — covers a typical dev-pipeline run
	if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
		if timeout > 30*time.Minute {
			timeout = 30 * time.Minute
		}
	}
	deadline := time.Now().Add(timeout)
	// Package-level vars so tests can shorten the fast-poll window
	// without waiting 30 seconds. Production values: 2s fast, 5s
	// slow after a 30s grace; the constants here mirror the
	// originals when tests don't override them.
	pollInterval := waitForTaskFastPoll
	fastPollWindow := waitForTaskFastWindow
	pollStart := time.Now()

	for {
		select {
		case <-ctx.Done():
			return ToolResult{Content: fmt.Sprintf("wait_for_task cancelled: %v", ctx.Err())}
		default:
		}
		task, err := te.taskRepo.Get(ctx, args.TaskID)
		if err != nil {
			return ToolResult{Content: fmt.Sprintf("Failed to read task %s: %v", args.TaskID, err)}
		}
		if task == nil {
			return ToolResult{Content: fmt.Sprintf("Task %s not found", args.TaskID)}
		}

		switch task.Status {
		case persistence.TaskStatusCompleted, persistence.TaskStatusFailed, persistence.TaskStatusCancelled:
			return te.formatWaitResult(ctx, task)
		}

		if time.Now().After(deadline) {
			return ToolResult{Content: fmt.Sprintf(
				"wait_for_task timed out after %s; task %s is still %s. Call get_task_status later or wait_for_task again with a longer timeout.",
				timeout, args.TaskID, task.Status)}
		}
		if time.Since(pollStart) > fastPollWindow {
			pollInterval = waitForTaskSlowPoll
		}
		select {
		case <-ctx.Done():
			return ToolResult{Content: fmt.Sprintf("wait_for_task cancelled: %v", ctx.Err())}
		case <-time.After(pollInterval):
		}
	}
}

// formatWaitResult builds the dispatcher-facing summary for a
// terminal task. Includes the task's last_error / last_error_class
// on failure, the produced text artifacts on success (so the
// dispatcher can quote them in its answer), and the active
// execution ID for follow-up queries.
func (te *ToolExecutor) formatWaitResult(ctx context.Context, task *persistence.Task) ToolResult {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Task %s reached status %s.\n", idfmt.Short(task.ID), task.Status)

	if task.LastError != nil && *task.LastError != "" {
		errMsg := *task.LastError
		if len(errMsg) > 1500 {
			errMsg = errMsg[:1500] + "…"
		}
		fmt.Fprintf(&sb, "\nLast error: %s\n", errMsg)
	}
	if task.LastErrorClass != nil && *task.LastErrorClass != "" {
		fmt.Fprintf(&sb, "Failure class: %s\n", *task.LastErrorClass)
	}

	// Artifacts produced — the dispatcher uses these to answer the
	// user. Names + sizes only; the model can call read_artifact
	// next if it wants the full content.
	if te.artifactRepo != nil {
		filter := persistence.ArtifactFilter{TaskID: &task.ID, PageSize: 25}
		if arts, err := te.artifactRepo.List(ctx, filter); err == nil && len(arts) > 0 {
			fmt.Fprintf(&sb, "\nProduced %d artifact(s):\n", len(arts))
			for _, a := range arts {
				line := fmt.Sprintf("  - %s (%s", a.Name, a.ArtifactClass)
				if a.SizeBytes != nil {
					line += fmt.Sprintf(", %d bytes", *a.SizeBytes)
				}
				line += ")"
				sb.WriteString(line + "\n")
			}
			sb.WriteString("\nCall read_artifact with the task_id and one of the names above to get the content.\n")
		}
	}

	// ThirdParty: embeds task.LastError which is agent-/execution-derived text — must be injection-scanned.
	return ToolResult{Content: sb.String(), Provenance: outputguard.ProvenanceThirdParty}
}

func (te *ToolExecutor) cancelTask(ctx context.Context, argsJSON string, allowedProjects []string) ToolResult {
	var args struct {
		TaskID  string `json:"task_id"`
		Confirm bool   `json:"confirm"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	if _, err := te.taskProjectAllowed(ctx, args.TaskID, allowedProjects); err != nil {
		return ToolResult{Content: err.Error()}
	}

	action := chat.Action{
		Type:    chat.ActionCancelTask,
		TaskID:  args.TaskID,
		Confirm: args.Confirm,
	}
	result, err := chat.ExecuteAction(ctx, action, te.taskRepo, te.execRepo, nil)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error: %v", err)}
	}
	// ThirdParty: result.Message can embed exec.ErrorMessage / agent-derived text (chat/parser.go:496) — must be injection-scanned.
	return ToolResult{Content: result.Message, Provenance: outputguard.ProvenanceThirdParty}
}

func (te *ToolExecutor) retryTask(ctx context.Context, argsJSON string, allowedProjects []string) ToolResult {
	var args struct {
		TaskID  string `json:"task_id"`
		Confirm bool   `json:"confirm"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	if _, err := te.taskProjectAllowed(ctx, args.TaskID, allowedProjects); err != nil {
		return ToolResult{Content: err.Error()}
	}

	action := chat.Action{
		Type:    chat.ActionRetryTask,
		TaskID:  args.TaskID,
		Confirm: args.Confirm,
	}
	result, err := chat.ExecuteAction(ctx, action, te.taskRepo, te.execRepo, nil)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error: %v", err)}
	}
	// ThirdParty: result.Message can embed exec.ErrorMessage / agent-derived text (chat/parser.go:496) — must be injection-scanned.
	return ToolResult{Content: result.Message, Provenance: outputguard.ProvenanceThirdParty}
}

func (te *ToolExecutor) listExecutions(ctx context.Context, argsJSON string, activeProject string, allowedProjects []string) ToolResult {
	var args struct {
		ProjectID string `json:"project_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}

	project, err := resolveProjectAllowed(args.ProjectID, activeProject, allowedProjects)
	if err != nil {
		return ToolResult{Content: err.Error()}
	}
	filter := persistence.ExecutionFilter{PageSize: 20, ProjectID: &project}
	if args.Status != "" {
		status := persistence.ExecutionStatus(strings.ToUpper(args.Status))
		filter.Status = &status
	}

	if te.execRepo == nil {
		return ToolResult{Content: "Execution repository not available."}
	}
	executions, err := te.execRepo.List(ctx, filter)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error listing executions: %v", err)}
	}
	if len(executions) == 0 {
		return ToolResult{Content: "No executions found."}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Found %d execution(s):\n", len(executions))
	for i, exec := range executions {
		if exec == nil {
			continue
		}
		fmt.Fprintf(&b, "%d. %s (task=%s, project=%s) - %s\n",
			i+1, exec.ID, exec.TaskID, exec.ProjectID, exec.Status)
	}
	return ToolResult{Content: b.String()}
}

func (te *ToolExecutor) listArtifacts(ctx context.Context, argsJSON string, allowedProjects []string) ToolResult {
	var args struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	if args.TaskID == "" {
		return ToolResult{Content: "task_id is required."}
	}
	if te.artifactRepo == nil {
		return ToolResult{Content: "Artifact repository is not available."}
	}
	if _, err := te.taskProjectAllowed(ctx, args.TaskID, allowedProjects); err != nil {
		return ToolResult{Content: err.Error()}
	}

	filter := persistence.ArtifactFilter{TaskID: &args.TaskID, PageSize: 50}
	artifacts, err := te.artifactRepo.List(ctx, filter)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Failed to list artifacts: %v", err)}
	}
	if len(artifacts) == 0 {
		return ToolResult{Content: "No artifacts found for this task."}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d artifact(s) for task %s:\n", len(artifacts), args.TaskID)
	for i, a := range artifacts {
		line := fmt.Sprintf("%d. %s (%s)", i+1, a.Name, a.ArtifactClass)
		if a.SizeBytes != nil {
			line += fmt.Sprintf(" — %d bytes", *a.SizeBytes)
		}
		if a.ExecutionID != nil {
			line += fmt.Sprintf(" [exec: %s]", *a.ExecutionID)
		}
		sb.WriteString(line + "\n")
	}
	return ToolResult{Content: sb.String(), Provenance: outputguard.ProvenanceFirstParty}
}

// resolveSendArtifactTarget lists a task's artifacts (walking children when the
// parent has none — the adaptive-routing fallback: a create_task parent ID
// often carries forward while the CHILD owns the artifacts) and selects the
// named artifact, or the first when no name is given. Returns the chosen
// artifact, or a non-empty user-facing error message when none matches.
func (te *ToolExecutor) resolveSendArtifactTarget(ctx context.Context, taskID, artifactName string) (*persistence.Artifact, string) {
	filter := persistence.ArtifactFilter{TaskID: &taskID, PageSize: 10}
	artifacts, err := te.artifactRepo.List(ctx, filter)
	if err != nil {
		return nil, fmt.Sprintf("Failed to list artifacts: %v", err)
	}
	if len(artifacts) == 0 && te.taskRepo != nil {
		if children, cerr := te.taskRepo.GetChildren(ctx, taskID); cerr == nil {
			for _, child := range children {
				if child == nil {
					continue
				}
				childID := child.ID
				cfilter := persistence.ArtifactFilter{TaskID: &childID, PageSize: 10}
				childArtifacts, lerr := te.artifactRepo.List(ctx, cfilter)
				if lerr != nil || len(childArtifacts) == 0 {
					continue
				}
				artifacts = append(artifacts, childArtifacts...)
			}
		}
	}
	if len(artifacts) == 0 {
		return nil, "No artifacts found for this task."
	}
	if artifactName == "" {
		return artifacts[0], ""
	}
	for _, a := range artifacts {
		if a.Name == artifactName {
			return a, ""
		}
	}
	names := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		names = append(names, a.Name)
	}
	return nil, fmt.Sprintf("Artifact '%s' not found. Available: %s", artifactName, strings.Join(names, ", "))
}

func (te *ToolExecutor) sendArtifact(ctx context.Context, argsJSON string, allowedProjects []string, fs FileSender) ToolResult {
	var args struct {
		TaskID       string `json:"task_id"`
		ArtifactName string `json:"artifact_name"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	if args.TaskID == "" {
		return ToolResult{Content: "task_id is required."}
	}
	if te.artifactRepo == nil || fs == nil {
		return ToolResult{Content: "Artifact sending is not configured."}
	}
	if _, err := te.taskProjectAllowed(ctx, args.TaskID, allowedProjects); err != nil {
		return ToolResult{Content: err.Error()}
	}

	target, errMsg := te.resolveSendArtifactTarget(ctx, args.TaskID, args.ArtifactName)
	if errMsg != "" {
		return ToolResult{Content: errMsg}
	}

	// Phase-4 storage abstraction: read via the backend-aware
	// artifactStore when wired, then upload via the streaming
	// SendDocumentReader. The legacy SendDocument(path) call is
	// preserved as a fallback for tests / boot paths that don't
	// wire a Store. send_artifact has a per-tool size envelope
	// (telegram bot upload limit is 50 MiB; artifacts here are
	// always user-readable result files so the in-memory buffer
	// is acceptable).
	caption := fmt.Sprintf("Artifact: %s", target.Name)
	if te.artifactStore != nil {
		data, rerr := te.artifactStore.Retrieve(ctx, target.ID)
		if rerr != nil {
			return ToolResult{Content: fmt.Sprintf("Failed to read artifact: %v", rerr)}
		}
		if err := fs.SendArtifactFile(ctx, target.Name, bytes.NewReader(data), caption); err != nil {
			return ToolResult{Content: fmt.Sprintf("Failed to send file: %v", err)}
		}
		return ToolResult{Content: fmt.Sprintf("Sent artifact '%s' to the operator.", target.Name), Provenance: outputguard.ProvenanceFirstParty}
	}
	// No backend-aware store wired: stream the file from its recorded path.
	f, oerr := os.Open(target.StoragePath)
	if oerr != nil {
		return ToolResult{Content: fmt.Sprintf("Failed to read artifact: %v", oerr)}
	}
	defer func() { _ = f.Close() }()
	if err := fs.SendArtifactFile(ctx, target.Name, f, caption); err != nil {
		return ToolResult{Content: fmt.Sprintf("Failed to send file: %v", err)}
	}
	return ToolResult{Content: fmt.Sprintf("Sent artifact '%s' to the operator.", target.Name), Provenance: outputguard.ProvenanceFirstParty}
}

// memorySearch queries the project's hybrid (semantic + keyword) memory
// store and returns the top-N chunks as a human-readable list. The
// dispatcher uses this as its first step for any factual question about
// project history — it replaces the "schedule a researcher" reflex with
// "check what we already know".
func (te *ToolExecutor) memorySearch(ctx context.Context, argsJSON, activeProject string, allowedProjects []string) ToolResult {
	var args struct {
		Query     string `json:"query"`
		ProjectID string `json:"project_id"`
		Limit     int    `json:"limit"`
		// FromDate / ToDate are optional ISO-8601 (YYYY-MM-DD or
		// full RFC3339) bounds on chunk created_at. Added 2026.6.0
		// for the external-research-inspired retrofit so the LLM
		// can answer "what did we discuss last week" without
		// dragging in every prior matching chunk.
		FromDate string `json:"from_date"`
		ToDate   string `json:"to_date"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	if args.Query == "" {
		return ToolResult{Content: "query is required."}
	}
	project, err := resolveProjectAllowed(args.ProjectID, activeProject, allowedProjects)
	if err != nil {
		return ToolResult{Content: err.Error()}
	}
	if te.memory == nil {
		return ToolResult{Content: "Project memory is not enabled on this daemon."}
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}

	fromDate, fdErr := parseMemoryDateBound(args.FromDate, false)
	if fdErr != nil {
		return ToolResult{Content: "from_date: " + fdErr.Error()}
	}
	toDate, tdErr := parseMemoryDateBound(args.ToDate, true)
	if tdErr != nil {
		return ToolResult{Content: "to_date: " + tdErr.Error()}
	}

	var results []memory.SearchResult
	// Capability cascade:
	//   1. MemoryFirewallSearcher — preferred when wired. Lets the
	//      memory firewall record dispatcher-side metadata (role +
	//      operator + purpose) on the audit row.
	//   2. MemoryTemporalSearcher — for temporal filters when the
	//      firewall capability isn't present (test mocks).
	//   3. Plain Search — last-resort fallback. Production
	//      *memory.Searcher always satisfies path 1; this branch
	//      keeps the contract for stubs.
	//
	// The fallback paths still flow through the firewall when wired
	// (default-on routing landed da87e9b4) — they just lose the
	// per-tool RequestContext attribution.
	reqCtx := memoryfirewall.RequestContext{
		Role:    "dispatcher",
		Purpose: memoryfirewall.PurposeOperational,
	}
	opts := memory.SearchOptions{
		Limit:    limit,
		FromDate: fromDate,
		ToDate:   toDate,
	}
	// The interactive memory_search tool stays single-shot RRF (no rerank) so
	// the agent's turn isn't slowed by the rerank LLM call. Scored-sufficiency
	// + reranking is scoped to the non-interactive context-assembly path
	// (the pre-delegation recall hint via RecallSufficient).
	if fw, ok := te.memory.(MemoryFirewallSearcher); ok {
		results, err = fw.RecallWithContext(ctx, project, args.Query, opts, reqCtx)
	} else if temporal, ok := te.memory.(MemoryTemporalSearcher); ok && (!fromDate.IsZero() || !toDate.IsZero()) {
		results, err = temporal.SearchWithOptions(ctx, project, args.Query, opts)
	} else {
		results, err = te.memory.Search(ctx, project, args.Query, limit)
	}
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Memory search failed: %v", err)}
	}
	if len(results) == 0 {
		return ToolResult{Content: fmt.Sprintf("No memory hits for %q in project %s.", args.Query, project), Provenance: outputguard.ProvenanceThirdParty}
	}

	// Keep the tool output compact — this string ends up in the LLM's
	// context. Include enough content (~800 chars per chunk) for the
	// model to extract the relevant fact without dumping whole
	// documents. Wrap each chunk body in untrusted_content markers —
	// memory is populated from task outputs and scraped artifacts, so
	// it can carry prompt-injection payloads. The wrapper + the
	// untrusted.Prelude in the dispatcher's system prompt are the
	// cheap mitigation.
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d memory hit(s) for %q in %s:\n\n", len(results), args.Query, project)
	for i, r := range results {
		fmt.Fprintf(&b, "[%d] source=%s  score=%.2f\n", i+1, r.SourceName, r.Score)
		// Surface the firewall's policy proof + any advisory warning so
		// the model can cite the policy decision when composing its
		// answer. RecallWithContext populates these; the legacy Search
		// paths leave them nil/empty, so this is a no-op for stubs.
		// CRITICAL: policy metadata is emitted OUTSIDE the
		// untrusted_content wrapper below — it's daemon-generated, not
		// chunk-derived, so it carries no prompt-injection risk and the
		// model can trust it. See finding #3 of
		// https://docs.vornik.io.
		// see LLD § https://docs.vornik.io
		// § "PolicyProof".
		if line := formatPolicyProofLine(r.PolicyProof); line != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
		if r.PolicyWarning != "" {
			fmt.Fprintf(&b, "    policy_warning=%s\n", r.PolicyWarning)
		}
		content := r.Content
		if len(content) > 800 {
			content = content[:800] + "…"
		}
		b.WriteString(untrusted.WrapLabeled("memory_hit", content))
		b.WriteString("\n\n")
	}

	// Knowledge-graph overlay (LLD §6.2). When a GraphSearcher is
	// wired AND the project has extracted entities for this query,
	// append a structured "entities + 1-hop relationships" block.
	// Failures here are non-fatal — the chunk answer already stands,
	// so we log nothing to the model and just skip the block. The
	// block is daemon-derived structure (entity names + predicates),
	// not raw chunk text, but the entity descriptions ARE chunk-
	// derived, so the block is wrapped in untrusted_content too.
	if graphBlock := te.graphContextBlock(ctx, project, args.Query); graphBlock != "" {
		b.WriteString(graphBlock)
	}

	return ToolResult{Content: b.String(), Provenance: outputguard.ProvenanceThirdParty}
}

// graphContextBlock builds the optional knowledge-graph context the
// memory_search tool appends after the chunk hits. Returns "" when
// no GraphSearcher is wired, the project has no matching entities,
// or any lookup errors (graph is best-effort augmentation — never a
// reason to fail the chunk answer). See LLD §6.2.
//
// Scope: every call is projectID-scoped via the GraphSearcher, which
// enforces the same project + repo_scope + cross-project guards as
// chunk retrieval (no entity, edge, or chunk from another project
// can surface here).
func (te *ToolExecutor) graphContextBlock(ctx context.Context, projectID, query string) string {
	if te.graphSearcher == nil {
		return ""
	}
	// Cap entity fan-out tight: this lands in the model's context and
	// the lead only needs the handful of entities the query is about.
	const maxEntities = 5
	const maxEdgesPerEntity = 6
	entities, err := te.graphSearcher.FindEntities(ctx, projectID, query, nil, maxEntities)
	if err != nil || len(entities) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Knowledge-graph entities related to your query (structured layer derived from the chunks above):\n")
	var inner strings.Builder
	for _, e := range entities {
		if e == nil {
			continue
		}
		fmt.Fprintf(&inner, "- %s [%s]", e.CanonicalName, e.Type)
		if e.Description != "" {
			desc := e.Description
			if len(desc) > 160 {
				desc = desc[:160] + "…"
			}
			fmt.Fprintf(&inner, ": %s", desc)
		}
		inner.WriteString("\n")
		// 1-hop edges for this entity.
		_, outgoing, incoming, gErr := te.graphSearcher.GetEntity(ctx, projectID, e.ID)
		if gErr != nil {
			continue
		}
		n := 0
		for _, edge := range outgoing {
			if n >= maxEdgesPerEntity {
				break
			}
			fmt.Fprintf(&inner, "    → %s %s\n", edge.Predicate, edge.ToEntity)
			n++
		}
		for _, edge := range incoming {
			if n >= maxEdgesPerEntity {
				break
			}
			fmt.Fprintf(&inner, "    ← %s %s\n", edge.FromEntity, edge.Predicate)
			n++
		}
	}
	if inner.Len() == 0 {
		return ""
	}
	b.WriteString(untrusted.WrapLabeled("knowledge_graph", inner.String()))
	b.WriteString("\n")
	return b.String()
}

// formatPolicyProofLine renders a compact, citable one-liner from the
// firewall's PolicyProof. Returns "" when the proof is nil (the legacy
// Search paths don't populate it, and lazy-backfilled chunks may have
// no policy block). The wire proof carries the evaluation decision,
// the policy digest, and the request purpose/role — it does NOT carry
// the chunk's sensitivity tier or provenance (those aren't propagated
// into SearchResult), so the line reports what's actually available.
func formatPolicyProofLine(p *memory.PolicyProofWire) string {
	if p == nil {
		return ""
	}
	digest := p.PolicyDigest
	if len(digest) > 12 {
		digest = digest[:12]
	}
	if digest == "" {
		digest = "none"
	}
	purpose := p.RequestContext.Purpose
	if purpose == "" {
		purpose = "unspecified"
	}
	return fmt.Sprintf("    policy=decision=%s/purpose=%s/digest=%s", p.Decision, purpose, digest)
}

// parseMemoryDateBound parses a memory_search temporal bound. Accepts
// either a date-only form (YYYY-MM-DD) or a full RFC3339 timestamp.
// Empty input returns a zero time.Time without error so the caller
// can pass it straight through to SearchOptions (zero = no bound).
//
// The two-format support is deliberate: LLM-emitted args are
// inconsistent, and asking the model to remember which format we
// take buys us nothing. Both are unambiguous and lib/pq treats
// time.Time identically once parsed.
func parseMemoryDateBound(s string, endOfDay bool) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	// Try RFC3339 first — it's the more specific format.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Date-only — anchor to UTC midnight so the bound is
	// inclusive at the start of the day. Postgres comparisons
	// will then naturally include all chunks created on that
	// date.
	if t, err := time.Parse("2006-01-02", s); err == nil {
		if endOfDay {
			return t.AddDate(0, 0, 1).Add(-time.Nanosecond), nil
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("must be YYYY-MM-DD or RFC3339 (got %q)", s)
}

// readArtifactMaxBytes bounds how much of an artifact body we stream back
// into the LLM context. Past this size the dispatcher should use
// send_artifact to deliver the full file to the user directly.
const readArtifactMaxBytes = 4 * 1024

// readArtifact fetches the content of a stored task artifact (preferably
// a prior task's output) and returns up to readArtifactMaxBytes of it.
// Pairs with memory_search as the "grab the whole thing, not just a
// chunk" follow-up when the user wants the full document.
func (te *ToolExecutor) readArtifact(ctx context.Context, argsJSON string, allowedProjects []string) ToolResult {
	var args struct {
		TaskID       string `json:"task_id"`
		ArtifactName string `json:"artifact_name"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	if args.TaskID == "" || args.ArtifactName == "" {
		return ToolResult{Content: "task_id and artifact_name are both required."}
	}
	if te.artifactRepo == nil {
		return ToolResult{Content: "Artifact repository is not available."}
	}
	if _, err := te.taskProjectAllowed(ctx, args.TaskID, allowedProjects); err != nil {
		return ToolResult{Content: err.Error()}
	}

	list, err := te.artifactRepo.List(ctx, persistence.ArtifactFilter{
		TaskID:   &args.TaskID,
		PageSize: 50,
	})
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Failed to list artifacts: %v", err)}
	}

	var target *persistence.Artifact
	for _, a := range list {
		if a != nil && a.Name == args.ArtifactName {
			target = a
			break
		}
	}
	if target == nil {
		names := make([]string, 0, len(list))
		for _, a := range list {
			if a != nil {
				names = append(names, a.Name)
			}
		}
		return ToolResult{Content: fmt.Sprintf("Artifact %q not found for task %s. Available: %s",
			args.ArtifactName, args.TaskID, strings.Join(names, ", "))}
	}
	if target.StoragePath == "" {
		return ToolResult{Content: fmt.Sprintf("Artifact %q has no storage path.", args.ArtifactName)}
	}

	// Route through the backend-aware artifactStore when wired so
	// this works on S3 deployments. The legacy direct-disk fallback
	// covers test paths that don't wire a Store.
	var data []byte
	if te.artifactStore != nil {
		data, err = te.artifactStore.Retrieve(ctx, target.ID)
	} else {
		data, err = os.ReadFile(target.StoragePath)
	}
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Failed to read artifact: %v", err)}
	}
	truncated := false
	if len(data) > readArtifactMaxBytes {
		data = data[:readArtifactMaxBytes]
		truncated = true
	}
	totalBytes := int64(len(data))
	if target.SizeBytes != nil {
		totalBytes = *target.SizeBytes
	}
	// Artifact bodies are agent-authored or scraped; wrap as untrusted.
	var b strings.Builder
	fmt.Fprintf(&b, "Artifact %s (%d bytes total) for task %s:\n\n",
		target.Name, totalBytes, args.TaskID)
	b.WriteString(untrusted.WrapLabeled("artifact_"+target.Name, string(data)))
	if truncated {
		fmt.Fprintf(&b, "\n\n(truncated to first %d bytes — use send_artifact to deliver the full file to the user)",
			readArtifactMaxBytes)
	}
	// Provenance: task_output artifacts are agent-authored (first-party);
	// all other origins (web_scrape, upload, unknown) are third-party so
	// injection-class output guard rules still run on them.
	prov := outputguard.ProvenanceThirdParty
	if target.Origin == persistence.ArtifactOriginTaskOutput {
		prov = outputguard.ProvenanceFirstParty
	}
	return ToolResult{Content: b.String(), Provenance: prov}
}

// mcpToolResultMaxBytes bounds how much of an MCP tool's output we
// pass back to the dispatcher LLM. The dispatcher is a real-time
// orchestrator running in one shared context window; an unbounded
// result (scraped HTML, large API bodies, indexed-search dumps)
// accumulating across the in-turn tool loop crosses the model's
// context limit on the next iteration and the whole turn 400s.
//
// Past failure: a Telegram message that triggered ~20 web_fetch
// calls returning full HTML bodies accumulated to 186k input
// tokens, busted the 202k bedrock window, and failed the turn
// with no recovery path (400 is not Retryable, and the existing
// prune only drops earlier turns — not the bloated current one).
//
// 50 KiB ≈ 12.5k tokens per call, leaving headroom for ~10 such
// calls in a 200k dispatcher context (after system prompt, tools
// schema, and prior history). The truncation footer below routes
// the model to a task worker (create_task) when it needs the
// complete payload — that worker runs in a fresh context budget
// separate from the chat session.
const mcpToolResultMaxBytes = 50 * 1024

func (te *ToolExecutor) executeMCPTool(ctx context.Context, projectID, qualifiedName, argsJSON string) ToolResult {
	if te.mcpManager == nil {
		return ToolResult{Content: "MCP is not configured for this project."}
	}
	if projectID == "" {
		return ToolResult{Content: "MCP tool calls require an active project (use switch_project first)."}
	}
	result, err := te.mcpManager.Execute(ctx, projectID, qualifiedName, argsJSON)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("MCP error: %v", err)}
	}
	if len(result) > mcpToolResultMaxBytes {
		origSize := len(result)
		result = result[:mcpToolResultMaxBytes] + fmt.Sprintf(
			"\n\n[truncated: kept first %d of %d bytes. The dispatcher caps MCP results so the chat-session "+
				"context window doesn't overflow across iterations of this tool loop. "+
				"If you need the full content, schedule a task with create_task — the task worker has its own "+
				"context budget and can process the complete body, then return a summary or artifact you can read "+
				"back. For narrower data on the next call, refine the MCP arguments (e.g. set max_bytes/limit if the "+
				"tool exposes one, pass a tighter selector, or request only the section you need).]",
			mcpToolResultMaxBytes, origSize)
	}
	// MCP tool output is by definition external data — scraped pages,
	// remote API bodies, third-party indexes. Wrap as untrusted so the
	// prelude in the system prompt is authoritative over any "ignore
	// prior instructions" payload embedded in the response body.
	return ToolResult{Content: untrusted.WrapLabeled(qualifiedName, result), Provenance: outputguard.ProvenanceThirdParty}
}

// resolveProject prefers the caller-supplied explicit project over the
// session's active project. Does NOT enforce any allowlist — use
// resolveProjectAllowed for any tool that returns project-scoped data.
func resolveProject(explicit, active string) string {
	if explicit != "" {
		return explicit
	}
	return active
}

// projectAllowed reports whether the given project is within the
// session's scope. Empty/nil allowed list means "no restriction"
// (preserves backward compatibility for callers that haven't wired
// the scope through yet, and the dev-mode single-operator path).
// A "*" entry is an explicit wildcard.
func projectAllowed(projectID string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == "*" || a == projectID {
			return true
		}
	}
	return false
}

// resolveProjectAllowed resolves the effective project for a tool
// call (explicit arg wins, else session-active) and verifies it is
// within the caller's allowlist. Returns an operator-readable error
// string when the resolved project isn't permitted or can't be
// resolved at all. The empty-return + error path is what prevents a
// user from reading another project's memory by passing project_id
// in a tool argument.
func resolveProjectAllowed(explicit, active string, allowed []string) (string, error) {
	resolved := resolveProject(explicit, active)
	if resolved == "" {
		return "", fmt.Errorf("project_id is required (use switch_project first or pass project_id)")
	}
	if !projectAllowed(resolved, allowed) {
		return "", fmt.Errorf("access to project %q is not permitted for this session", resolved)
	}
	return resolved, nil
}

// taskProjectAllowed loads the task for taskID and verifies its
// project is within the caller's allowlist. Returns the resolved
// ProjectID on success. Used by tools that accept a task_id (rather
// than a project_id) to ensure a user can't read/modify/cancel a
// task belonging to a project they aren't scoped for.
func (te *ToolExecutor) taskProjectAllowed(ctx context.Context, taskID string, allowed []string) (string, error) {
	if te.taskRepo == nil {
		return "", fmt.Errorf("task repository is not available")
	}
	task, err := te.taskRepo.Get(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("failed to load task: %w", err)
	}
	if task == nil {
		return "", fmt.Errorf("task not found")
	}
	if !projectAllowed(task.ProjectID, allowed) {
		return "", fmt.Errorf("access to task %q is not permitted for this session", taskID)
	}
	return task.ProjectID, nil
}

// DispatcherTools returns the tool definitions available to the dispatcher agent.
func DispatcherTools() []chat.Tool {
	return []chat.Tool{
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "list_projects",
				Description: "List all registered projects.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "switch_project",
				Description: "Set the active project for this conversation. Subsequent task operations default to this project.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"project_id":{"type":"string","description":"Project ID to switch to"}
					},
					"required":["project_id"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "list_tasks",
				Description: "List tasks, optionally filtered by project and status.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"project_id":{"type":"string","description":"Project ID (uses active project if omitted)"},
						"status":{"type":"string","description":"Filter by status: PENDING, QUEUED, RUNNING, COMPLETED, FAILED, CANCELLED"}
					}
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "create_task",
				Description: "Create and schedule a new task. The prompt is the complete instruction the worker agent will execute. Omit workflow_id to use the project's default workflow (usually dev-pipeline). The bot AUTOMATICALLY resumes this conversation when the task reaches a terminal status (completed/failed) — you can simply return a brief 'task X scheduled, will continue when it completes' message and end your turn; the result will be spliced back in as a fresh user turn so you can answer using the produced artifacts. Pass await_completion=false ONLY for fire-and-forget tasks the user explicitly does NOT want follow-up on.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"project_id":{"type":"string","description":"Project ID (uses active project if omitted)"},
						"type":{"type":"string","description":"Task type: feature, bug-fix, refactor, test-coverage, or roadmap-revision"},
						"prompt":{"type":"string","description":"The instruction or goal for the agent to execute. Make it specific and self-contained."},
						"workflow_id":{"type":"string","description":"Override the project default workflow. Almost always omit this — the project default is correct for development tasks."},
						"input":{"type":"object","description":"Optional additional task input payload"},
						"input_files":{"type":"array","items":{"type":"string"},"description":"Files the worker needs as inputs. Accepts either host file paths or artifact IDs. When the user message ends with an [Attached files] block (inbound email/webchat attachments), pass each artifact_id=... value here verbatim — the dispatcher resolves it to the durable artifact and stages the bytes inside the worker container."},
						"await_completion":{"type":"boolean","description":"Defaults to true. When the task completes, the bot resumes this conversation and you continue with the produced artifacts. Pass false ONLY for explicit fire-and-forget work where the user has said they don't want a follow-up."}
					},
					"required":["type","prompt"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "get_task_status",
				Description: "Get detailed status of a task including its current execution and output.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"task_id":{"type":"string","description":"Task ID to look up"}
					},
					"required":["task_id"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "wait_for_task",
				Description: "Block until a task reaches a terminal status (COMPLETED / FAILED / CANCELLED) and return its result + produced artifacts. Use IMMEDIATELY after create_task when the task was scheduled to obtain data needed to answer the user's current question — call wait_for_task on the new task_id, then continue the analysis with the artifacts produced. Times out after timeout_seconds (default 600 = 10 min, max 1800 = 30 min); on timeout the task keeps running and the operator can call wait_for_task again with a longer budget.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"task_id":{"type":"string","description":"Task ID to wait on (typically the one create_task just returned)"},
						"timeout_seconds":{"type":"integer","description":"Max wait in seconds; default 600, capped at 1800."}
					},
					"required":["task_id"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name: "cancel_task",
				Description: "Cancel a running or queued task. DESTRUCTIVE: confirm with the user first. " +
					"Call WITHOUT confirm to get a confirmation prompt to relay; only call with confirm=true after the user explicitly agrees.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"task_id":{"type":"string","description":"Task ID to cancel"},
						"confirm":{"type":"boolean","description":"Set true ONLY after the user has explicitly confirmed the cancellation."}
					},
					"required":["task_id"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name: "retry_task",
				Description: "Retry a failed task (resets it to QUEUED). DESTRUCTIVE: re-runs the task and spends additional budget — " +
					"confirm with the user first. Call WITHOUT confirm to get a confirmation prompt to relay; only call with confirm=true after the user explicitly agrees.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"task_id":{"type":"string","description":"Task ID to retry"},
						"confirm":{"type":"boolean","description":"Set true ONLY after the user has explicitly confirmed the retry."}
					},
					"required":["task_id"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "list_executions",
				Description: "List workflow executions, optionally filtered by project and status.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"project_id":{"type":"string","description":"Project ID (uses active project if omitted)"},
						"status":{"type":"string","description":"Filter by status: PENDING, RUNNING, COMPLETED, FAILED"}
					}
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "list_artifacts",
				Description: "List artifacts (input files, output files, logs) for a task.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"task_id":{"type":"string","description":"Task ID to list artifacts for"}
					},
					"required":["task_id"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "send_artifact",
				Description: "Send a completed task's output artifact back to the user as a file download. Use when the user wants the actual document (CV, patch, report) rather than a summary.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"task_id":{"type":"string","description":"Task ID to retrieve artifacts from"},
						"artifact_name":{"type":"string","description":"Specific artifact name (optional — sends first artifact if omitted)"}
					},
					"required":["task_id"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "send_email",
				Description: "Send a fresh outbound email via the active project's email channel. Use when the user asks you to email someone (e.g. 'send the summary to alice@example.com'). The project must have its `email.smtp_*` fields configured and a from_address that the SMTP user is allowed to send as. Returns the Message-ID on success. For replying to an inbound thread, pass in_reply_to with that thread's original Message-ID.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"to":{"type":"string","description":"Recipient email address (RFC 5322 mailbox form)."},
						"subject":{"type":"string","description":"Subject line."},
						"body":{"type":"string","description":"Plain-text body. Newlines preserved verbatim."},
						"in_reply_to":{"type":"string","description":"Optional. The Message-ID of an inbound email to thread this reply onto (no angle brackets)."}
					},
					"required":["to","subject","body"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "render_document",
				Description: "Render user-supplied markdown into the requested formats (md / html / pdf) and deliver each file directly to the chat. Deterministic — runs pandoc/weasyprint on the daemon host, no agent container, no LLM. Use this WHENEVER the user supplies the content themselves (CV text, report draft, README) and just wants it rendered + delivered. Faster and far more reliable than create_task → adaptive workflow for transforms-only work.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"content":{"type":"string","description":"Markdown source. Will be rendered verbatim — do not pre-process."},
						"name":{"type":"string","description":"Base filename, no extension (e.g. 'CV-Senior-Lead-AI-MSD-20260518-en'). Used for every requested format."},
						"formats":{"type":"array","items":{"type":"string","enum":["md","html","pdf"]},"description":"Formats to render. Default is all three (md + html + pdf)."}
					},
					"required":["content","name"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "cancel_reminder",
				Description: "Cancel a pending reminder the operator no longer wants. Pass the reminder_id returned by set_reminder + a short rationale. Refuses if the reminder is no longer pending (already fired / cancelled) or belongs to a different operator. Audit row recorded.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"reminder_id":{"type":"string","description":"The reminder id (rem_…) returned by set_reminder."},
						"rationale":{"type":"string","description":"Short reason — persisted in the audit log."}
					},
					"required":["reminder_id","rationale"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "update_reminder",
				Description: "Reschedule a pending reminder or change its body. Pass the reminder_id + either fire_at (RFC3339) OR fire_in_seconds for a new time, and/or content to change the body. Omit both time fields to leave the existing fire_at intact and only update content. Refuses if the reminder is no longer pending or belongs to a different operator. Audit row recorded.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"reminder_id":{"type":"string","description":"The reminder id (rem_…) returned by set_reminder."},
						"fire_at":{"type":"string","description":"Absolute new fire time in RFC3339. Use when you know the target timezone."},
						"fire_in_seconds":{"type":"integer","description":"Positive integer seconds-from-now for the new fire time."},
						"content":{"type":"string","description":"Optional new message body (≤ 2000 chars). Empty leaves the existing body."},
						"rationale":{"type":"string","description":"Short reason — persisted in the audit log."}
					},
					"required":["reminder_id","rationale"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "set_reminder",
				Description: "Schedule a future reminder. The bot will message the operator at the specified time with the supplied content. Supply EITHER fire_at (RFC3339 timestamp, e.g. '2026-05-25T09:00:00+02:00') OR fire_in_seconds (positive integer offset from now). For relative times the operator asks for ('in 2 hours', 'tomorrow 9am'), compute the offset in seconds or the absolute RFC3339 string yourself before calling — the tool does NOT parse natural language. v1 fires on Telegram only.\n\nSTRONG PREFERENCE: use fire_at, not fire_in_seconds. fire_at is RFC3339 with explicit timezone; the model + the tool agree on exactly which wall-clock moment will fire. fire_in_seconds requires the model to add an offset to CURRENT TIME (in the system prompt) and is the source of every 'why did it fire at the wrong time?' bug — most commonly when the user says 'tomorrow' near midnight and the model confuses calendar days. Use fire_in_seconds ONLY for explicit durations the user said ('remind me in 30 minutes', 'check back in 2 hours').\n\nWHEN THE USER ASKS FOR A SPECIFIC TIME ('tomorrow at 9am', 'next Tuesday 14:00', 'this Friday morning'): construct fire_at by taking CURRENT TIME from the system prompt + applying the user's words. 'tomorrow' means CURRENT_DATE + 1 day in the operator's timezone. 'morning' means 08:00 unless the user specified otherwise. If the result is already past, ASK the user instead of guessing — don't silently shift to the next day. Confirm the resolved date back to the user in your reply ('Set for Tuesday 26 May at 09:00 CEST') so they can spot a mis-parse.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"fire_at":{"type":"string","description":"Absolute fire time in RFC3339 (e.g. '2026-05-24T09:00:00+02:00'). Use this when you know the target timezone."},
						"fire_in_seconds":{"type":"integer","description":"Positive integer seconds-from-now offset. Use this when the operator said 'in 2 hours' / 'in 30 minutes' — easier than computing the absolute timestamp."},
						"content":{"type":"string","description":"The message body the reminder will deliver. ≤ 2000 chars. The bot prepends a small '⏰ Reminder:' marker on its own."}
					},
					"required":["content"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "memory_search",
				Description: "Search project memory for past research, task outputs, and notes. USE THIS FIRST whenever the user asks about a topic, person, or project that might already have work done on it — scheduling a new task should be the last resort, not the first. Returns top chunks with source file names and relevance scores. Pass from_date / to_date to narrow by created-at when the user asks for recent or time-bounded context.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"query":{"type":"string","description":"Natural-language query (e.g. 'Vadim Grinco background', 'Linux audio projects', 'sensor integration notes')"},
						"project_id":{"type":"string","description":"Project to search (optional — uses active project if omitted)"},
						"limit":{"type":"integer","description":"Max chunks to return. Default 5, max 20."},
						"from_date":{"type":"string","description":"Optional lower bound on chunk created_at. ISO date (YYYY-MM-DD) or full RFC3339. Empty = no bound."},
						"to_date":{"type":"string","description":"Optional upper bound on chunk created_at. ISO date (YYYY-MM-DD) or full RFC3339. Empty = no bound."}
					},
					"required":["query"]
				}`),
			},
		},
		memoryCorrectDescriptor(),
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "update_operator_profile",
				Description: "Persist a durable preference the operator just stated about themselves. Call this ONLY when the operator makes an EXPLICIT, DURABLE preference statement that you should respect across future conversations (e.g. 'I prefer concise replies', 'always cc me on github', 'use my local time'). Do NOT infer preferences from tone or content — wait for an explicit ask. Each call carries a key + value + short rationale; the rationale persists in the audit log so operators can review later. Allowed keys: tone, verbosity, time_zone, communication_style, preferred_channel, notes. Setting an empty value removes the key.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"key":{"type":"string","description":"One of: tone, verbosity, time_zone, communication_style, preferred_channel, notes."},
						"value":{"type":"string","description":"The new value. Empty string removes a structured key. For 'notes', replaces the existing notes column."},
						"rationale":{"type":"string","description":"Short explanation of the operator's exact wording / context — persisted in the audit log so a future operator can review why this preference was set."}
					},
					"required":["key","value","rationale"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "read_artifact",
				Description: "Fetch the content of a specific task artifact (up to 4KB) into context. Useful after memory_search identifies a source file and you need the full text to answer the user. Beyond 4KB, use send_artifact instead to deliver the file directly.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"task_id":{"type":"string","description":"Task ID that produced the artifact"},
						"artifact_name":{"type":"string","description":"Exact artifact filename (e.g. 'vadim-grinco-cv.md')"}
					},
					"required":["task_id","artifact_name"]
				}`),
			},
		},
	}
}

// findSameTurnDuplicateTask returns the ID of any task previously
// created within the SAME dispatcher turn (same chat_turn_id) whose
// prompt is sufficiently similar to the incoming one. Unlike
// findRecentDuplicateTask, this ignores age and status — even a
// task that already completed in this turn is a dedup hit, because
// the dispatcher should be re-reading its artifacts rather than
// re-scheduling the work.
//
// Live evidence: 2026-05-21 17:08 turn produced T-0918 ("Add high-
// volatility symbols to watchlist") at 17:10:26, T-0918 COMPLETED at
// 17:18:14, and the same turn fired create_task again at 17:18:52
// for T-8e47 ("Update watchlist to add high-volatility symbols") —
// 38 s after T-0918 closed, so the 30 s window-based dedup never
// fired. With this guard, the dispatcher gets T-0918's ID back and
// can read_artifact rather than re-running the strategist.
//
// Returns "" on any DB error so a transient Postgres hiccup doesn't
// break a legitimate user request. Empty turn id is rejected up
// front so the legacy window-based path retains its behaviour.
func findSameTurnDuplicateTask(ctx context.Context, repo persistence.TaskRepository, projectID, turnID, prompt string) string {
	if repo == nil || projectID == "" || turnID == "" || prompt == "" {
		return ""
	}
	pid := projectID
	// PageSize cap mirrors findRecentDuplicateTask. A single turn
	// rarely spawns more than a handful of tasks; 20 is comfortable
	// headroom even when an agent is unusually chatty.
	tasks, err := repo.List(ctx, persistence.TaskFilter{
		ProjectID: &pid,
		PageSize:  20,
	})
	if err != nil || len(tasks) == 0 {
		return ""
	}
	normalised := normalisePromptForDedup(prompt)
	for _, t := range tasks {
		if t == nil || t.ChatTurnID == nil || *t.ChatTurnID != turnID {
			continue
		}
		existingPrompt := extractTaskPrompt(t.Payload)
		existingNormalised := normalisePromptForDedup(existingPrompt)
		if existingNormalised == normalised {
			return t.ID
		}
		if jaccardTokenSimilarity(normalised, existingNormalised) >= 0.85 {
			return t.ID
		}
	}
	return ""
}

// findRecentDuplicateTask returns the ID of an active task with the
// same (project, normalised prompt) that was created within
// `window` of now, or "" if no such task exists. Used by the
// dispatcher's create_task path to suppress the LLM-hallucinated
// "fire create_task several times in one turn" pattern observed in
// production (see tools.go:createTask for the incident note).
//
// "Active" = QUEUED / PENDING / LEASED / RUNNING /
// WAITING_FOR_CHILDREN. Completed and failed tasks fall outside
// the dedup so a user re-asking the same question minutes later
// gets a fresh task.
//
// Returns "" on any DB error so a transient Postgres hiccup
// doesn't break a legitimate user request. The cost of an
// occasional double-create is much lower than the cost of
// silently swallowing real intent.
func findRecentDuplicateTask(ctx context.Context, repo persistence.TaskRepository, projectID, prompt string, window time.Duration) string {
	if repo == nil || projectID == "" || prompt == "" || window <= 0 {
		return ""
	}
	pid := projectID
	tasks, err := repo.List(ctx, persistence.TaskFilter{
		ProjectID: &pid,
		PageSize:  20,
	})
	if err != nil || len(tasks) == 0 {
		return ""
	}
	normalised := normalisePromptForDedup(prompt)
	cutoff := time.Now().Add(-window)
	for _, t := range tasks {
		if t == nil {
			continue
		}
		if t.CreatedAt.Before(cutoff) {
			// List returns newest-first; once we are past the
			// window there is nothing more to check.
			return ""
		}
		switch t.Status {
		case persistence.TaskStatusQueued,
			persistence.TaskStatusPending,
			persistence.TaskStatusLeased,
			persistence.TaskStatusRunning,
			persistence.TaskStatusWaitingForChildren:
			// active — candidate
		default:
			continue
		}
		existingPrompt := extractTaskPrompt(t.Payload)
		existingNormalised := normalisePromptForDedup(existingPrompt)
		// Exact match (post-normalisation) — catches the
		// retry-on-error pattern where the LLM re-fires the same
		// prompt verbatim.
		if existingNormalised == normalised {
			return t.ID
		}
		// Fuzzy match — token-set Jaccard ≥ 0.85 collapses two
		// prompts that share nearly all of their content but
		// differ in trivial phrasing. Live evidence: T-af29 vs
		// T-6e62 (2026-05-10) had identical "ingest Toby
		// Sheldon's CV …" prompts; the second dropped a "(if
		// any)" parenthetical the first one carried, so the
		// strict-equality path missed the dup. The threshold is
		// conservative — 0.85 needs ~90%+ word overlap to
		// suppress, so genuinely-different requests still get
		// their own tasks.
		if jaccardTokenSimilarity(normalised, existingNormalised) >= 0.85 {
			return t.ID
		}
	}
	return ""
}

// jaccardTokenSimilarity returns the size of the intersection
// over the size of the union of word sets in a and b. Operates
// on lower-case whitespace-tokenised strings; punctuation is
// not stripped further because normalisePromptForDedup already
// folded the most volatile punctuation. Returns 0 when either
// input is empty so callers don't trip on a "100% match"
// against the empty token set.
//
// Both token sets are guaranteed non-empty after the guards
// above, so union = |A| + |B| - intersect ≥ max(|A|,|B|) ≥ 1.
// No defensive `if union == 0` branch is needed.
func jaccardTokenSimilarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	tokensA := tokenSet(a)
	tokensB := tokenSet(b)
	if len(tokensA) == 0 || len(tokensB) == 0 {
		return 0
	}
	var intersect int
	for tok := range tokensA {
		if _, ok := tokensB[tok]; ok {
			intersect++
		}
	}
	union := len(tokensA) + len(tokensB) - intersect
	return float64(intersect) / float64(union)
}

// tokenSet splits s on whitespace, strips leading + trailing
// ASCII punctuation from each token, and returns each distinct
// non-empty result. Punctuation stripping is what makes
// "memory" and "memory." (or "(if" and "if") collapse to the
// same token, so the Jaccard metric isn't fooled by trivial
// punctuation differences in two paraphrased prompts. Caller
// is responsible for casing + whitespace normalisation
// (normalisePromptForDedup handles those).
func tokenSet(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, t := range strings.Fields(s) {
		t = strings.Trim(t, ".,;:!?()[]{}\"'`")
		if t == "" {
			continue
		}
		out[t] = struct{}{}
	}
	return out
}

// normalisePromptForDedup folds whitespace, strips surrounding
// punctuation, and lower-cases so trivial reformatting does not
// defeat the dedup. The dispatcher LLM occasionally tweaks
// punctuation between retries (e.g. trailing periods, capitalised
// first words); without normalisation those slip through.
func normalisePromptForDedup(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.TrimSpace(strings.ToLower(s)) {
		if r == '\n' || r == '\t' || r == ' ' {
			if prevSpace {
				continue
			}
			b.WriteByte(' ')
			prevSpace = true
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return b.String()
}

// extractTaskPrompt pulls the agent-facing prompt out of a Task
// payload. Mirrors the autonomy package's extractPrompt but is
// inlined here to avoid the dispatcher importing autonomy (which
// would create a cycle).
func extractTaskPrompt(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		Context struct {
			Prompt string `json:"prompt"`
		} `json:"context"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	if p.Context.Prompt != "" {
		return p.Context.Prompt
	}
	return p.Prompt
}

// truncatePrompt is a tiny helper for log output so a 5KB prompt
// preview does not blow up the daemon journal.
func truncatePrompt(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
