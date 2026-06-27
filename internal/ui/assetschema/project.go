package assetschema

// Project schema — the curated editable surface for registry.Project.
//
// Phase 1 covers the operational tuning blocks (identity, scheduling,
// autonomy, budget, rate-limit, retention, chat, firewall, trading,
// inter-project). Credential / connector / opaque blocks (github, forge,
// email, slack, voice, mcp, webhooks, permissions, verifiers, assistant,
// hallucination-judge, allow-spawn, lifecycle) are EXPLICITLY deferred to
// ProjectDeferredPaths — still editable via the raw-YAML escape hatch, and
// tracked by the drift-guard so they can't be confused with "missing".
//
// The drift-guard test (project_test.go) asserts every registry.Project
// yaml leaf is either covered here or in ProjectDeferredPaths.

// ProjectSchema returns the curated form schema for a project asset.
func ProjectSchema() AssetSchema {
	return AssetSchema{
		Asset: "project",
		Sections: []Section{
			{
				Title: "Identity & routing",
				Fields: []Field{
					{Path: "projectId", Label: "Project ID", Kind: KindString, Required: true, ReadOnly: true, Help: "Unique identifier; matches the YAML filename. Not editable here — renaming would orphan the file."},
					{Path: "displayName", Label: "Display name", Kind: KindString},
					{Path: "description", Label: "Description", Kind: KindString, Help: "Operator-facing summary shown on the project homepage."},
					{Path: "swarmId", Label: "Swarm", Kind: KindString, Required: true, Help: "The swarm definition this project uses."},
					{Path: "defaultWorkflowId", Label: "Default workflow", Kind: KindString, Required: true},
					{Path: "adaptiveCandidateWorkflows", Label: "Adaptive candidate workflows", Kind: KindStringList, Help: "Menu the lead picks from in the adaptive router; each must resolve to a known workflow."},
				},
			},
			{
				Title: "Scheduling",
				Fields: []Field{
					{Path: "defaultPriority", Label: "Default priority", Kind: KindInt, Default: "0", Help: "0–100, lower = more urgent."},
					{Path: "maxConcurrentTasks", Label: "Max concurrent tasks", Kind: KindInt, Help: "Parallel execution cap within this project."},
				},
			},
			{
				Title: "Autonomy",
				Help:  "Autonomous task creation by the project lead.",
				Fields: []Field{
					{Path: "autonomy.enabled", Label: "Enabled", Kind: KindBool},
					{Path: "autonomy.mode", Label: "Mode", Kind: KindEnum, Enum: []string{"llm", "cron", "backlog"}, Default: "llm", Help: "llm = lead evaluates state; cron = fire Goal verbatim each tick; backlog = consume BACKLOG.md."},
					{Path: "autonomy.goal", Label: "Goal", Kind: KindString},
					{Path: "autonomy.maxTasksPerHour", Label: "Max tasks/hour", Kind: KindInt},
					{Path: "autonomy.allowedTaskTypes", Label: "Allowed task types", Kind: KindStringList},
					{Path: "autonomy.requireApproval", Label: "Require approval", Kind: KindBool},
					{Path: "autonomy.pollInterval", Label: "Poll interval", Kind: KindDuration, Default: "5m"},
					{Path: "autonomy.evaluate_timeout", Label: "Evaluate timeout", Kind: KindDuration, Advanced: true},
					{Path: "autonomy.preCheck", Label: "Pre-check", Kind: KindString, Advanced: true, Help: "Optional pre-check workflow/gate before firing."},
					{Path: "autonomy.preCheckWorkflowMinDuration", Label: "Pre-check min duration", Kind: KindDuration, Advanced: true, Default: "12m"},
					{Path: "autonomy.duplicateWindow", Label: "Duplicate window", Kind: KindDuration, Advanced: true},
					{Path: "autonomy.contextFilePath", Label: "Context file path", Kind: KindString, Advanced: true},
					{Path: "autonomy.userContextFilePath", Label: "User context file path", Kind: KindString, Advanced: true},
					{Path: "autonomy.backlogFilePath", Label: "Backlog file path", Kind: KindString, Advanced: true, Default: "BACKLOG.md"},
					{Path: "autonomy.cronTaskType", Label: "Cron task type", Kind: KindString, Advanced: true},
				},
			},
			{
				Title:    "Budget",
				Advanced: true,
				Help:     "Per-project LLM spend caps (USD). Zero disables a cap.",
				Fields: []Field{
					{Path: "budget.daily_soft_usd", Label: "Daily soft cap", Kind: KindFloat},
					{Path: "budget.daily_hard_usd", Label: "Daily hard cap", Kind: KindFloat},
					{Path: "budget.monthly_soft_usd", Label: "Monthly soft cap", Kind: KindFloat},
					{Path: "budget.monthly_hard_usd", Label: "Monthly hard cap", Kind: KindFloat},
					{Path: "budget.timezone", Label: "Budget timezone", Kind: KindString, Default: "UTC"},
					{Path: "budget.reservation_estimate_usd", Label: "Reservation estimate", Kind: KindFloat, Help: "Per-task amount reserved at admission (TOCTOU-safe)."},
				},
			},
			{
				Title:    "Rate limit",
				Advanced: true,
				Fields: []Field{
					{Path: "rate_limit.tasks_per_minute", Label: "Tasks/minute", Kind: KindInt},
					{Path: "rate_limit.tasks_per_hour", Label: "Tasks/hour", Kind: KindInt},
				},
			},
			{
				Title:    "Retention (days)",
				Advanced: true,
				Help:     "Days to keep each data class; 0 = inherit daemon default. Memory chunks are never pruned.",
				Fields: []Field{
					{Path: "retention.task_llm_usage_days", Label: "Task LLM usage", Kind: KindInt},
					{Path: "retention.tool_audit_days", Label: "Tool audit", Kind: KindInt},
					{Path: "retention.tasks_days", Label: "Tasks", Kind: KindInt},
					{Path: "retention.executions_days", Label: "Executions", Kind: KindInt},
					{Path: "retention.artifacts_days", Label: "Artifacts", Kind: KindInt},
					{Path: "retention.task_messages_days", Label: "Task messages", Kind: KindInt},
					{Path: "retention.memory_chunks_days", Label: "Memory chunks", Kind: KindInt},
					{Path: "retention.memory_ingest_audit_days", Label: "Memory ingest audit", Kind: KindInt},
					{Path: "retention.memory_policy_eval_allow_days", Label: "Policy eval (allow)", Kind: KindInt},
					{Path: "retention.memory_policy_eval_block_days", Label: "Policy eval (block)", Kind: KindInt},
				},
			},
			{
				Title: "Chat",
				Fields: []Field{
					{Path: "chat.system_prefix", Label: "System prompt prefix", Kind: KindString, Help: "Prepended to every chat session on this project."},
				},
			},
			{
				Title:    "Memory firewall",
				Advanced: true,
				Fields: []Field{
					{Path: "firewall.mode", Label: "Mode", Kind: KindEnum, Enum: []string{"off", "advisory", "enforce"}, Help: "Empty = inherit daemon default."},
				},
			},
			{
				Title:    "Trading",
				Advanced: true,
				Help:     "Per-project broker policy. Caps are enforced by the broker safety envelope.",
				Fields: []Field{
					{Path: "trading.mode", Label: "Mode", Kind: KindEnum, Enum: []string{"paper", "live"}, Help: "Informational; live promotion is gated at the broker."},
					{Path: "trading.killSwitch", Label: "Kill switch", Kind: KindBool, Help: "When on, the broker refuses any order under this project."},
					{Path: "trading.caps.max_position_usd", Label: "Max position (USD)", Kind: KindFloat},
					{Path: "trading.caps.max_daily_turnover_usd", Label: "Max daily turnover (USD)", Kind: KindFloat},
					{Path: "trading.caps.max_orders_per_hour", Label: "Max orders/hour", Kind: KindInt},
					{Path: "trading.caps.max_orders_per_minute", Label: "Max orders/minute", Kind: KindInt},
					{Path: "trading.caps.drawdown_circuit_breaker_pct", Label: "Drawdown breaker (%)", Kind: KindFloat},
					{Path: "trading.caps.daily_loss_circuit_breaker_pct", Label: "Daily-loss breaker (%)", Kind: KindFloat},
					{Path: "trading.watchlist", Label: "Watchlist", Kind: KindStringList},
					{Path: "trading.notify_fills_chat_id", Label: "Notify fills chat ID", Kind: KindString},
				},
			},
			{
				Title:    "Inter-project",
				Advanced: true,
				Fields: []Field{
					{Path: "pedantic", Label: "Pedantic mode", Kind: KindBool},
					{Path: "maxCallDepth", Label: "Max call depth", Kind: KindInt},
					{Path: "acceptCallsFrom", Label: "Accept calls from", Kind: KindStringList, Help: "Project IDs allowed to call into this project."},
					{Path: "canCallProjects", Label: "Can call projects", Kind: KindStringList, Help: "Project IDs this project may call out to."},
				},
			},
		},
	}
}

// ProjectDeferredPaths are registry.Project yaml leaves intentionally NOT
// yet given a structured form (Phase 1) — credentials, connector configs,
// opaque blocks, and lifecycle state managed by dedicated flows. They stay
// editable via the raw-YAML editor. The drift-guard treats these as
// consciously-deferred (not missing); shrink this set as forms are added.
var ProjectDeferredPaths = []string{
	// Permissions — security-sensitive (tool/secret allowlists).
	"permissions.secrets",
	"permissions.allowedTools",
	// MCP server wiring — complex nested blocks.
	"mcp.servers",
	"mcp.toolRateLimits",
	// Signed webhook sources.
	"webhooks.sources",
	// Trading per-project order rate-limit (pre-live ops knob; YAML-only like the other *_rps limits).
	"trading_rate_limit.orders_per_minute", "trading_rate_limit.orders_per_hour",
	// GitHub App channel + outbound credentials.
	"github_app.app_id", "github_app.private_key_path", "github_app.installation_id",
	"github_app.api_base_url", "github_app.webhook_secret_env", "github_app.repo_allowlist",
	"github_app.task_labels", "github_app.pr_review_labels", "github_app.sender_allowlist",
	"github_app.reply_workflow_id", "github_app.pr_review_workflow_id",
	"github.app_id", "github.installation_id", "github.private_key_path", "github.api_base_url",
	// Forge automation.
	"forge.provider", "forge.github.app_id", "forge.github.installation_id",
	"forge.github.private_key_path", "forge.github.api_base_url",
	// Email connector (IMAP/SMTP, secrets via env).
	"email.imap_host", "email.imap_port", "email.imap_username", "email.imap_password_env",
	"email.imap_mailbox", "email.smtp_host", "email.smtp_port", "email.smtp_username",
	"email.smtp_password_env", "email.from_address", "email.sender_allowlist",
	"email.poll_interval", "email.attachment_size_cap_bytes", "email.attachment_store_dir",
	"email.verify_inbound_auth", "email.auth_policy", "email.trusted_auth_servers",
	// Slack connector.
	"slack.team_id", "slack.signing_secret_env", "slack.bot_token_env",
	"slack.channel_allowlist", "slack.sender_allowlist", "slack.verify_inbound_signature",
	"slack.post_message_rps", "slack.post_message_burst",
	// Voice STT/TTS providers.
	"voice.stt.provider", "voice.stt.model", "voice.stt.binary_path", "voice.stt.ffmpeg_path",
	"voice.stt.language_hint", "voice.tts.provider", "voice.tts.voice", "voice.tts.binary_path",
	"voice.tts.ffmpeg_path", "voice.tts.speed", "voice.tts.max_text_runes",
	// Opaque / specialized blocks.
	"verifiers",
	"assistant.model",
	"hallucinationJudge.enabled", "hallucinationJudge.model", "hallucinationJudge.prompt",
	"allowSpawn.templates", "allowSpawn.maxSpawnsPerDay",
	// Lifecycle — managed by archive/restore actions, not free editing.
	"lifecycle.status", "lifecycle.archivedAt", "lifecycle.scheduledDeleteAt",
	"lifecycle.reason", "lifecycle.archivedBy",
	// Git-over-HTTPS workspace access (Task 3.2). Surfaced in the
	// project-detail Git-access panel; set via raw YAML.
	"git.enabled",
}
