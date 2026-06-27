// Package registry provides in-memory registries for projects, swarms, and workflows.
package registry

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/forge"
)

// Project represents a project definition loaded from projects/*.yaml
type Project struct {
	// ID is the unique identifier for the project (required)
	ID string `yaml:"projectId"`
	// DisplayName is a human-readable name for the project
	DisplayName string `yaml:"displayName"`
	// Description is the operator-facing prose summary surfaced on
	// the per-project homepage (2026.6.0 SaaS-readiness). Multi-line
	// markdown is welcome — the homepage renders it as plain text.
	// Empty falls back to the DisplayName / ID so older project
	// files don't show a blank hero block.
	Description string `yaml:"description"`
	// SwarmID references the swarm definition this project uses (required)
	SwarmID string `yaml:"swarmId"`
	// DefaultWorkflowID is the default workflow for tasks in this project (required)
	DefaultWorkflowID string `yaml:"defaultWorkflowId"`
	// AdaptiveCandidateWorkflows is the menu the lead picks from when
	// running the adaptive workflow's strict-router step. The lead
	// emits {"selected_workflow": "<id>"} and the executor validates
	// the choice against this list before delegating to a child task
	// with that workflow. An empty/nil list keeps the legacy behaviour
	// (the lead's free-form plan is honoured). Listed names must each
	// resolve to a known workflow; unknown entries surface at registry-
	// load validation rather than runtime.
	AdaptiveCandidateWorkflows []string `yaml:"adaptiveCandidateWorkflows"`
	// DefaultPriority is the default priority for tasks (0-100, lower = more urgent)
	DefaultPriority int `yaml:"defaultPriority"`
	// MaxConcurrentTasks limits parallel execution within this project
	MaxConcurrentTasks int `yaml:"maxConcurrentTasks"`
	// Autonomy controls autonomous task creation for this project
	Autonomy ProjectAutonomy `yaml:"autonomy"`
	// Permissions defines project-level permissions
	Permissions ProjectPermissions `yaml:"permissions"`
	// MCP configures Model Context Protocol servers for this project
	MCP ProjectMCP `yaml:"mcp"`
	// Budget caps LLM spend for this project. Zero values disable the cap.
	Budget ProjectBudget `yaml:"budget"`
	// Chat configures per-project chat behaviour. Currently only a system
	// prompt prefix — operator-controlled copy prepended to every session
	// started on this project (useful for role guidance, compliance notes,
	// or project-specific tool-use hints without forking the default prompt).
	Chat ProjectChat `yaml:"chat"`
	// Retention caps how long historical data is kept for this project.
	// Each field is a day count; zero means "inherit global default from
	// the daemon config". project_memory_chunks is NEVER pruned regardless
	// of these values — it's the product, not operational state.
	Retention ProjectRetention `yaml:"retention"`
	// RateLimit caps task-creation frequency for the project.
	RateLimit ProjectRateLimit `yaml:"rate_limit"`
	// TradingRateLimit caps how fast this project may place orders
	// via the broker→daemon trading audit channel
	// (POST /api/v1/internal/trading-orders). Independent of
	// RateLimit (task creation) and enforced with its own sliding
	// window. Zero values disable that dimension (unlimited). A
	// PRE-LIVE blocker before onboarding a 2nd trading project.
	TradingRateLimit ProjectTradingRateLimit `yaml:"trading_rate_limit"`
	// Webhooks configures signed external event ingestion for this project.
	Webhooks ProjectWebhooks `yaml:"webhooks"`
	// GitHubApp configures the per-project GitHub App channel
	// (issues.labeled → task, @vornik mention → reply,
	// pull_request.opened → review task). Zero value disables the
	// channel for this project; the daemon still loads other
	// projects normally.
	GitHubApp ProjectGitHubApp `yaml:"github_app"`
	// GitHub configures GitHub App credentials used to mint short-lived
	// installation access tokens for agent outbound (git push / gh pr review /
	// gh pr create). Independent of the GitHubApp conversational channel: set
	// this to give a project's agents a least-privilege GH_TOKEN without
	// enabling the channel. Zero value = no token injected.
	GitHub ProjectGitHub `yaml:"github"`
	// Git controls the git-over-HTTPS workspace-access feature for this
	// project. When Enabled is true the daemon exposes the project's
	// workspace via HTTPS git (clone/push) at the path derived from
	// server.public_base_url. Default false = opt-out; operators must
	// explicitly enable per-project. See
	// https://docs.vornik.io
	Git ProjectGit `yaml:"git"`
	// Forge configures the provider-discriminated forge automation (issue →
	// change request, CR reviews) used by the forge.* system steps. Provider
	// selects github|gitlab|gitea. For GitHub the top-level `github:` block above
	// is honoured as a back-compat alias when this is unset, so existing configs
	// keep working. Zero value = forge automation disabled for this project.
	Forge ProjectForge `yaml:"forge"`
	// Email configures the per-project email ConversationChannel
	// (IMAP polling for inbound, SMTP for outbound). Zero value
	// disables the channel for this project; same one-project-wins
	// rule as GitHubApp in slice 1.
	Email ProjectEmail `yaml:"email"`
	// Slack configures the per-project Slack ConversationChannel
	// (Events API webhook inbound, chat.postMessage outbound). Zero
	// value disables the channel for this project. Mirrors
	// ProjectEmail's per-project routing — every project with a
	// configured `slack` block gets its own channel instance pinned
	// to its workspace team_id.
	Slack ProjectSlack `yaml:"slack"`
	// Voice configures per-project STT / TTS providers for the
	// async voice-message MVP (Telegram voice + Slack audio clips).
	// Zero value disables voice handling — inbound audio is treated
	// as a non-voice attachment and outbound replies stay text.
	// See https://docs.vornik.io
	Voice ProjectVoice `yaml:"voice"`
	// Firewall overrides daemon-level memory firewall settings
	// for this project. Phase D follow-on of the Policy-Aware
	// Memory Firewall (LLD: https://docs.vornik.io
	// memory-firewall-design.md § "Rollout Plan"). Zero value
	// = inherit daemon default (advisory). Per-project enforce
	// mode is the typical compliance path — strict mode on the
	// project storing personal data, advisory on the rest.
	Firewall ProjectFirewall `yaml:"firewall"`
	// Verifiers is the Phase 2 hallucination-detection list:
	// declarative outcome checks that run after each agent step
	// finalises. A failing verifier fails the step so the
	// scheduler retries it. Empty list = no verifiers configured.
	// See internal/verifier for the schema and the supported
	// types (artifact_min_entries, no_status_429_in_audit, etc.).
	// Decoded as []map[string]any here to avoid importing the
	// verifier package into registry; the executor passes them
	// to verifier.Run after type-asserting at the call site.
	Verifiers []map[string]any `yaml:"verifiers"`
	// Trading is the per-project broker policy block. Mirrors what
	// the broker MCP's safety envelope enforces, but lets the
	// operator set caps in the project YAML rather than as broker-
	// global env vars. The daemon attaches Trading.Caps to every
	// place_order call routed through the broker MCP via the
	// X-Project-Caps header; the broker reads that header to scope
	// caps per request, falling back to its env-var defaults when
	// the header is missing (back-compat for projects that haven't
	// migrated yet, and for tools/calls that the broker can route
	// without the daemon's mcp.Manager — e.g. direct curl probes).
	Trading ProjectTrading `yaml:"trading"`
	// Brief is the parsed PROJECT.md companion. Nil when no
	// PROJECT.md exists for this project; non-nil when the loader
	// found and parsed a matching `projects/<id>.md`. Not loaded
	// from YAML — populated by LoadProjects after the YAML pass.
	// See https://docs.vornik.io
	Brief *ProjectBrief `yaml:"-"`
	// Assistant is the per-project override for the prompt-
	// writing assistant (Phase 2 of the web-authoring UX work).
	// Lets operators pin a specific model for "AI Assist" calls
	// independently of the swarm's worker models. Empty fields
	// fall through to the swarm leadRole's model and then to
	// the daemon default.
	Assistant ProjectAssistant `yaml:"assistant"`
	// HallucinationJudge is the Phase 3 LLM-as-judge config. When
	// Enabled is true, every completed task gets evaluated by an
	// LLM judge after termination; the verdict is persisted in
	// task_judge_verdicts and surfaced on the task UI. Model is
	// the LLM model identifier (operators usually pick a smaller,
	// cheaper model than the worker roles). Prompt overrides the
	// default judge prompt — empty falls back to the package
	// default. Disabled by default so the layer is pure opt-in;
	// the rolllout flow is enable-on-one-project, watch verdicts
	// for a week, then enable everywhere.
	HallucinationJudge ProjectHallucinationJudge `yaml:"hallucinationJudge"`
	// Pedantic, when set true, disables the swarm-recovery flow for
	// every task under this project: on_fail routing falls straight
	// through to the configured terminal failure target instead of
	// surfacing a `decision` checkpoint with proposed alternatives.
	// Set this on trading / compliance / high-precision projects
	// where "fail loudly on any deviation" is the contract. Pointer
	// so an absent field reads as nil (recovery on, the default for
	// every other project); workflow- and task-level pedantic flags
	// can override per-execution. See
	// https://docs.vornik.io §6.
	Pedantic *bool `yaml:"pedantic,omitempty"`
	// AcceptCallsFrom gates inter-project orchestration (Phase A
	// LLD: https://docs.vornik.io
	// design.md §4.4). When another project's `call_project`
	// step targets this project, the executor checks the
	// caller's project ID against this list:
	//
	//   - empty list (default) → closed; all cross-project
	//     calls are rejected. Operators opt in explicitly.
	//   - ["*"] → wildcard, accepts from any project in the
	//     tenant. Emits a load-time warning; discouraged.
	//   - ["projectA", "team-*"] → exact + glob matches. Glob
	//     uses path.Match semantics; "team-*" matches any
	//     caller project ID with that prefix.
	//
	// See AcceptsCallsFrom() for the matcher implementation.
	AcceptCallsFrom []string `yaml:"acceptCallsFrom,omitempty"`

	// CanCallProjects is the CALLER-side outbound allowlist — the
	// complement to AcceptCallsFrom. When this project's workflow runs a
	// `call_project` step, the executor checks the TARGET project ID
	// against this list:
	//
	//   - empty / unset (default) → ["*"] semantics: may call any
	//     project (back-compatible; the callee's AcceptCallsFrom is
	//     still the binding consent gate).
	//   - ["projectA", "team-*"] → exact + glob (path.Match) matches;
	//     any other target is refused with CROSS_PROJECT_REJECTED.
	//
	// Lets operators lock down a critical caller (e.g. a trading or
	// compliance project) so a hallucinating planner can't fan calls out
	// to arbitrary projects that happen to accept from a wildcard. See
	// CanCall() for the matcher.
	CanCallProjects []string `yaml:"canCallProjects,omitempty"`

	// AllowSpawn gates Phase B's spawn_project step. When this
	// project's workflow tries to spawn a new project from a
	// template, the executor checks the requested template
	// against AllowSpawn.Templates and the per-day count
	// against AllowSpawn.MaxSpawnsPerDay. Zero-value means the
	// project cannot spawn — same secure default as
	// AcceptCallsFrom.
	AllowSpawn ProjectAllowSpawn `yaml:"allowSpawn,omitempty"`

	// MaxCallDepth caps inter-project call chain depth
	// originating from this project. The depth counter starts
	// at 0 on a task created by the operator (or the autonomy
	// loop), increments by 1 on every call_project hop, and is
	// checked at the call site: a hop that would push the depth
	// above this cap is refused with DEPTH_EXCEEDED. Defends
	// against a hallucinating LLM in a planning role fanning out
	// N levels deep before the bottom of the chain times out.
	//
	// Zero / unset falls back to DefaultMaxCallDepth (8). Set to
	// 1 to forbid any further hop, or to a higher number for
	// legitimate long orchestrator → sub-orchestrator → leaf
	// chains.
	//
	// See "Later — Inter-project cycle detection + depth limit"
	// in https://docs.vornik.io.
	MaxCallDepth int `yaml:"maxCallDepth,omitempty"`

	// Lifecycle drives the archive → scheduled-deletion flow.
	// When Status is "archived" the daemon stops dispatching
	// work for this project; once ScheduledDeleteAt elapses the
	// archived-project sweeper deletes the project YAML +
	// PROJECT.md, every project-scoped DB row, and every
	// artifact blob on disk. Zero value (omitted block) keeps
	// the project active — same default-on shape every existing
	// project file already has. See
	// https://docs.vornik.io
	Lifecycle ProjectLifecycle `yaml:"lifecycle,omitempty"`
}

// ProjectLifecycle holds the archival state for a project. An
// empty struct means "active"; only the archive flow populates
// these fields. Persisted directly in the project YAML so the
// file is the single source of truth (operators can grep
// configs/projects/*.yaml to inventory pending deletions, and
// version control captures the audit trail).
type ProjectLifecycle struct {
	// Status is one of "" / "active" (default) or "archived".
	// The sweeper / dispatch gates only check IsArchived(); any
	// other value is treated as active so a partial future
	// state ("deleting") doesn't accidentally re-activate a
	// project mid-removal.
	Status string `yaml:"status,omitempty"`

	// ArchivedAt records when the operator flipped the project
	// to archived. RFC3339 string in the file; parsed lazily.
	// Zero value when not archived.
	ArchivedAt string `yaml:"archivedAt,omitempty"`

	// ScheduledDeleteAt is when the sweeper will hard-delete
	// every trace of the project. Set at archive time as
	// archivedAt + graceDuration. RFC3339 string. Zero value
	// when not archived. Sweeper compares against time.Now() —
	// rows past their scheduledDeleteAt get wiped on the next
	// tick.
	ScheduledDeleteAt string `yaml:"scheduledDeleteAt,omitempty"`

	// Reason is the operator-supplied free-text justification
	// for the archive. Optional; surfaces in the project-detail
	// banner so a teammate auditing the archived list sees
	// *why*.
	Reason string `yaml:"reason,omitempty"`

	// ArchivedBy is the principal that triggered the archive
	// (API key label, operator email — whatever the admin
	// surface stashes in adminPrincipal). Optional. Empty when
	// archived via a path that doesn't capture identity (CLI
	// without admin gate).
	ArchivedBy string `yaml:"archivedBy,omitempty"`
}

// IsArchived reports whether the project has been flipped to
// the archived state. Treats any non-"archived" status as
// active so a typo / partial future enum doesn't accidentally
// keep work running on a project the operator believes is
// shutting down. Nil-receiver-safe (zero ProjectLifecycle is
// IsArchived=false).
func (p *Project) IsArchived() bool {
	if p == nil {
		return false
	}
	return p.Lifecycle.Status == "archived"
}

// ScheduledDeletion returns the parsed scheduledDeleteAt and
// true when set. Zero time + false means the project isn't
// archived (or the YAML doesn't carry a deletion deadline,
// which the archive handler is supposed to enforce — the
// sweeper treats a missing deadline as "do nothing yet").
func (p *Project) ScheduledDeletion() (time.Time, bool) {
	if p == nil || p.Lifecycle.ScheduledDeleteAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, p.Lifecycle.ScheduledDeleteAt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// ArchivedAtTime returns the parsed archivedAt timestamp and
// true when set. Mirrors ScheduledDeletion's defensive parsing.
func (p *Project) ArchivedAtTime() (time.Time, bool) {
	if p == nil || p.Lifecycle.ArchivedAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, p.Lifecycle.ArchivedAt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// DeletionDue reports whether the project is archived AND its
// scheduled-delete-at moment has elapsed relative to `now`.
// Sweeper-facing helper — calling at every tick so the sweeper
// stays declarative.
func (p *Project) DeletionDue(now time.Time) bool {
	if !p.IsArchived() {
		return false
	}
	t, ok := p.ScheduledDeletion()
	if !ok {
		return false
	}
	return !t.After(now)
}

// ProjectAllowSpawn is the spawn_project authorisation block.
// See LLD §7 ("Spawn authorization").
type ProjectAllowSpawn struct {
	// Templates is the per-template allowlist. A spawn_project
	// step whose `template:` doesn't match any entry here is
	// rejected with TEMPLATE_NOT_ALLOWED. ["*"] is the
	// permissive wildcard; ["sales-campaign", "partner-onboard"]
	// is the recommended shape (explicit list of approved
	// templates).
	Templates []string `yaml:"templates,omitempty"`
	// MaxSpawnsPerDay caps how many projects this project can
	// spawn in any rolling 24-hour window. Zero disables the
	// cap (no per-day limit). Default-suggested value is 5 —
	// stops a hallucinating LLM that loops through
	// spawn_project from materialising hundreds of empty
	// projects.
	MaxSpawnsPerDay int `yaml:"maxSpawnsPerDay,omitempty"`
}

// ProjectTrading is the per-project broker / safety-envelope
// configuration. Caps are surfaced to the broker MCP on every
// place_order call so the per-project YAML is the authoritative
// source of position / turnover / rate limits — not a broker-
// wide env var. Mode is informational on the daemon side
// (paper/live promotion is still gated at the broker process
// itself); KillSwitch when true short-circuits orders for this
// project at the broker.
type ProjectTrading struct {
	// Mode is "paper" or "live" — informational on the project
	// page; the broker enforces the live-promotion gate
	// independently via VORNIK_BROKER_LIVE_ENABLED.
	Mode string `yaml:"mode"`
	// KillSwitch, when true, makes the broker refuse any
	// place_order routed under this project's caps. Reset to
	// false to resume.
	KillSwitch bool `yaml:"killSwitch"`
	// Caps mirror the broker.Caps shape — operator-configured
	// limits the safety envelope checks before forwarding the
	// order to the underlying provider. Zero values mean
	// "unlimited" / "off" for that dimension, matching broker
	// semantics so the YAML and runtime agree.
	Caps TradingCaps `yaml:"caps"`
	// Watchlist is the symbols the daemon pre-warms quotes for
	// before the strategist runs. Pre-fix the strategist made
	// 16 sequential get_quote calls per tick — burning ~30% of
	// its tool-call budget on what's effectively a batch read.
	// Listing the symbols here lets the daemon fetch them once
	// in parallel and inject the result as a structured block
	// in the strategist's context, freeing iterations for TA
	// + news work.
	//
	// Empty list disables pre-warming — the strategist falls
	// back to per-symbol get_quote calls (back-compat).
	Watchlist []string `yaml:"watchlist"`

	// NotifyFillsChatID is the Telegram chat that receives a
	// per-fill notification when the broker reports a fill on
	// this project. Zero disables fill notifications. The bot's
	// debouncer collapses partial-fill bursts (multiple
	// `trading_fills` rows for the same order arriving within a
	// 2s window) into a single message so a 4-leg partial fill
	// doesn't spam the operator's chat.
	NotifyFillsChatID int64 `yaml:"notify_fills_chat_id"`
}

// TradingCaps is the per-project subset of broker.Caps that the
// operator owns from project YAML. Stop-loss policy
// (require_stop_loss, default_stop_loss_pct) lives broker-wide
// for now — strategy-specific stop policy can move here later
// if a project ever needs to diverge.
type TradingCaps struct {
	MaxPositionUSD            float64 `yaml:"max_position_usd"`
	MaxDailyTurnoverUSD       float64 `yaml:"max_daily_turnover_usd"`
	MaxOrdersPerHour          int     `yaml:"max_orders_per_hour"`
	MaxOrdersPerMinute        int     `yaml:"max_orders_per_minute"`
	DrawdownCircuitBreakerPct float64 `yaml:"drawdown_circuit_breaker_pct"`
	// DailyLossCircuitBreakerPct flips the broker kill switch when
	// today's equity drops by this percentage from the UTC-day-start
	// baseline. Plumbed per-project to the broker via X-Project-Caps
	// so the order-path breaker uses the project's threshold (audit
	// T4). Zero disables. 0–100.
	DailyLossCircuitBreakerPct float64 `yaml:"daily_loss_circuit_breaker_pct"`
}

// ProjectAssistant is the per-project override block for the
// web-authoring prompt-writing assistant. Today it only holds
// a Model override; future fields (max-tokens cap, separate
// budget cap) land here too without re-shaping the schema.
type ProjectAssistant struct {
	// Model overrides the assistant's default model resolution.
	// Empty falls through to swarm leadRole.model then to the
	// daemon default. Lets operators run authoring on a stronger
	// model than their worker swarm uses (or a cheaper one).
	Model string `yaml:"model"`
}

// ProjectHallucinationJudge is the per-project config for the
// Phase 3 LLM-as-judge runner.
type ProjectHallucinationJudge struct {
	Enabled bool   `yaml:"enabled"`
	Model   string `yaml:"model"`
	Prompt  string `yaml:"prompt"`
}

// ProjectWebhooks configures signed webhook sources for a project.
type ProjectWebhooks struct {
	Sources []ProjectWebhookSource `yaml:"sources"`
}

// ProjectWebhookSource maps one signed source to a task template.
type ProjectWebhookSource struct {
	Name             string `yaml:"name"`
	Secret           string `yaml:"secret"`
	SecretEnv        string `yaml:"secret_env"`
	EventIDPath      string `yaml:"event_id_path"`
	TaskTypeTemplate string `yaml:"task_type_template"`
	WorkflowID       string `yaml:"workflow_id"`
	Priority         int    `yaml:"priority"`
	// ForwardPayload, when true, places the raw verified webhook body into the
	// created task's Context as {"prompt": <body>} so the agent can act on the
	// specific event (e.g. which PR/issue). Off by default — existing sources
	// keep creating context-less tasks identified only by task_type.
	ForwardPayload bool `yaml:"forward_payload"`
	// Filter, when set, gates task creation on the event JSON. Forms:
	//   ${path}            — match when the path resolves to a non-empty value
	//   ${path}=a,b,c      — match when the path's value is one of the listed values
	// A non-matching delivery is acknowledged 200 and recorded as a "filtered"
	// webhook_event with no task created. Empty = no filter (every verified
	// delivery creates a task, the prior behaviour).
	Filter string `yaml:"filter"`
	// AllowSecrets opts out of the secret-leak Block-mode scan for
	// this source. Used when the signed payload format legitimately
	// contains long high-entropy tokens (signed JWTs, encoded delivery
	// IDs) that the detector would flag. Off by default so unconfigured
	// sources benefit from the protection.
	AllowSecrets bool `yaml:"allow_secrets"`
	// ChangeRequestWorkflowID routes deliveries the forge classifier flags as a
	// change request (an opened PR/MR) to a distinct workflow — e.g. a review
	// flow — while issues use WorkflowID. Empty = every delivery uses WorkflowID.
	// Provider-neutral: keys off the classified forge_job, not an event name.
	ChangeRequestWorkflowID string `yaml:"change_request_workflow_id"`
	// RequireForgeEvent drops (filters) a verified delivery that the forge
	// classifier does NOT recognise as an actionable forge job — issues.closed,
	// pull_request.synchronize, an unlabeled issue, etc. — so they never create
	// a task with no forge_job. Use on a single GitHub webhook source that must
	// act only on labeled issues + opened change requests.
	RequireForgeEvent bool `yaml:"require_forge_event"`
}

// ProjectGitHubApp configures the per-project GitHub App
// conversation channel (slice 4D of the ConversationChannel
// rollout). See https://docs.vornik.io
//
// Enabled at the project level when AppID, PrivateKeyPath, and
// InstallationID are all set AND at least one repo is in
// RepoAllowlist. Inbound webhook reception remains optional —
// operators who only want to receive notifications can set
// WebhookSecretEnv + RepoAllowlist without filling in the
// outbound (AppID/PrivateKeyPath/InstallationID) fields, and Send
// will return github.ErrOutboundNotConfigured.
type ProjectGitHubApp struct {
	// AppID is the GitHub App's numeric identifier. Required for
	// outbound replies; can be 0 if only inbound is needed.
	AppID int64 `yaml:"app_id"`

	// PrivateKeyPath is the filesystem path to the PEM-encoded
	// private key downloaded from the GitHub App settings page.
	// Required for outbound replies; empty disables outbound.
	PrivateKeyPath string `yaml:"private_key_path"`

	// InstallationID is the GitHub App installation ID for the
	// target org / user account. Required for outbound replies.
	InstallationID int64 `yaml:"installation_id"`

	// APIBaseURL overrides the GitHub REST endpoint. Empty
	// defaults to `https://api.github.com`. Set to
	// `https://github.example.com/api/v3` for GitHub Enterprise
	// installations.
	APIBaseURL string `yaml:"api_base_url"`

	// WebhookSecretEnv names an environment variable holding the
	// HMAC secret GitHub uses to sign every delivery. Required for
	// the inbound webhook handler (the channel rejects every
	// delivery when not configured). The env-var indirection
	// mirrors ProjectWebhookSource.SecretEnv so operators never
	// embed secrets in YAML.
	WebhookSecretEnv string `yaml:"webhook_secret_env"`

	// RepoAllowlist is the set of `owner/repo` full names this
	// channel accepts events from. Required (defensive default:
	// deny-all so a misconfigured channel rejects every
	// delivery).
	RepoAllowlist []string `yaml:"repo_allowlist"`

	// TaskLabels are the issue labels that, when applied, fire
	// the task-creation path. Empty disables that path.
	TaskLabels []string `yaml:"task_labels"`

	// PRReviewLabels are the labels that gate the `pull_request.opened`
	// review-task path. Empty means every opened PR triggers a
	// review task. Non-empty means at least one listed label must
	// be present.
	PRReviewLabels []string `yaml:"pr_review_labels"`

	// SenderAllowlist lists GitHub logins allowed to trigger the
	// @vornik reply path. Empty allows every login (dev mode).
	SenderAllowlist []string `yaml:"sender_allowlist"`

	// ReplyWorkflowID names the workflow GitHub-App-driven tasks run
	// under. Empty falls back to the project's DefaultWorkflowID. Lets
	// an operator route GitHub interactions (label tasks, PR reviews,
	// @vornik replies) through a workflow distinct from the project's
	// default without changing the project-wide default.
	// See https://docs.vornik.io (Config Surface).
	ReplyWorkflowID string `yaml:"reply_workflow_id,omitempty"`

	// PRReviewWorkflowID names the workflow that `pull_request.opened`
	// tasks run under, distinct from the issue-task router so a PR review
	// (fetch diff → review → post) and an issue→change-request flow can be
	// separate workflows. Empty falls back to EffectiveReplyWorkflowID.
	PRReviewWorkflowID string `yaml:"pr_review_workflow_id,omitempty"`
}

// EffectiveReplyWorkflowID returns the workflow GitHub-App-driven
// tasks should run under: the configured ReplyWorkflowID when set,
// otherwise the project's DefaultWorkflowID passed in by the caller.
func (g ProjectGitHubApp) EffectiveReplyWorkflowID(projectDefault string) string {
	if id := strings.TrimSpace(g.ReplyWorkflowID); id != "" {
		return id
	}
	return projectDefault
}

// EffectivePRReviewWorkflowID returns the workflow an opened-PR review task runs
// under: the configured PRReviewWorkflowID when set, otherwise the reply/router
// workflow (so a deployment that doesn't separate them keeps working).
func (g ProjectGitHubApp) EffectivePRReviewWorkflowID(projectDefault string) string {
	if id := strings.TrimSpace(g.PRReviewWorkflowID); id != "" {
		return id
	}
	return g.EffectiveReplyWorkflowID(projectDefault)
}

// ProjectGitHub holds GitHub App credentials for minting short-lived
// installation access tokens for agent outbound. All three of AppID,
// InstallationID, and PrivateKeyPath are required together (Enabled());
// validation enforces the all-or-nothing rule.
type ProjectGitHub struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	// APIBaseURL overrides the GitHub REST base (GitHub Enterprise). Empty →
	// https://api.github.com.
	APIBaseURL string `yaml:"api_base_url"`
}

// Enabled reports whether the project has complete GitHub App credentials for
// token minting.
func (g ProjectGitHub) Enabled() bool {
	return g.AppID != 0 && g.InstallationID != 0 && strings.TrimSpace(g.PrivateKeyPath) != ""
}

// ResolvedAPIBaseURL returns the configured GitHub REST base or the public
// default.
func (g ProjectGitHub) ResolvedAPIBaseURL() string {
	if s := strings.TrimSpace(g.APIBaseURL); s != "" {
		return s
	}
	return "https://api.github.com"
}

// toForge maps the registry's GitHub creds onto the provider-neutral forge
// config block.
func (g ProjectGitHub) toForge() forge.GitHubConfig {
	return forge.GitHubConfig{
		AppID:          g.AppID,
		InstallationID: g.InstallationID,
		PrivateKeyPath: g.PrivateKeyPath,
		APIBaseURL:     g.APIBaseURL,
	}
}

// ProjectForge is the provider-discriminated forge configuration. Provider picks
// the implementation; the matching credential block is populated. New providers
// add a field here (and an impl package) with no change to callers.
type ProjectForge struct {
	Provider string        `yaml:"provider"` // github | gitlab | gitea
	GitHub   ProjectGitHub `yaml:"github"`
	// GitLab ProjectGitLab `yaml:"gitlab"` // future sibling
	// Gitea  ProjectGitea  `yaml:"gitea"`  // future sibling
}

// ProjectGit controls the git-over-HTTPS workspace-access feature.
// Default (zero value) is off — operators must set enabled: true to expose
// the project workspace via HTTPS git. The clone URL is derived at runtime:
// <server.public_base_url>/api/v1/git/<projectID>.git; it is never stored here.
type ProjectGit struct {
	// Enabled opts this project into git-over-HTTPS access. When false (the
	// default) the daemon returns 404 for all git routes on this project.
	Enabled bool `yaml:"enabled"`
}

// ResolveForge returns the effective provider-neutral forge.Config for the
// project and whether forge automation is enabled. Resolution order:
//
//  1. An explicit `forge:` block (provider set) wins. For github, credentials
//     fall back to the top-level `github:` block when the nested one is empty,
//     so an operator can opt into a provider name without duplicating creds.
//  2. Otherwise the top-level `github:` block is honoured as a back-compat alias
//     (provider = github), so configs predating the forge block keep working.
//  3. Otherwise forge automation is disabled (ok = false).
func (p *Project) ResolveForge() (forge.Config, bool) {
	if prov := strings.TrimSpace(p.Forge.Provider); prov != "" {
		cfg := forge.Config{Provider: prov}
		if prov == forge.ProviderGitHub {
			gh := p.Forge.GitHub
			if !gh.Enabled() && p.GitHub.Enabled() {
				gh = p.GitHub // back-compat: creds in the top-level block
			}
			cfg.GitHub = gh.toForge()
		}
		return cfg, true
	}
	if p.GitHub.Enabled() {
		return forge.Config{Provider: forge.ProviderGitHub, GitHub: p.GitHub.toForge()}, true
	}
	return forge.Config{}, false
}

// Enabled reports whether the GitHub App channel is configured
// enough to load. WebhookSecretEnv + RepoAllowlist are the bare
// minimum; outbound creds are separate and optional at this gate.
func (g ProjectGitHubApp) Enabled() bool {
	return strings.TrimSpace(g.WebhookSecretEnv) != "" && len(g.RepoAllowlist) > 0
}

// ProjectEmail configures the per-project email
// ConversationChannel — IMAP polling for inbound, SMTP for outbound.
// Mirrors ProjectGitHubApp's shape; secrets are read from env vars
// (IMAPPasswordEnv / SMTPPasswordEnv) so YAML stays secret-free.
//
// Enabled at the project level when IMAPHost + IMAPUsername +
// IMAPPasswordEnv are all set. Outbound (SMTP*) is optional in
// slice 1 — operators who only want inbound notifications can
// leave the SMTP block empty and Channel.Send returns
// email.ErrOutboundNotConfigured.
type ProjectEmail struct {
	// IMAPHost is the IMAP server hostname (e.g. imap.gmail.com).
	// Required.
	IMAPHost string `yaml:"imap_host"`

	// IMAPPort is the IMAP server port. Zero defaults to 993 (TLS).
	IMAPPort int `yaml:"imap_port"`

	// IMAPUsername is the IMAP login username. Required.
	IMAPUsername string `yaml:"imap_username"`

	// IMAPPasswordEnv names the env var holding the IMAP login
	// password. The daemon reads it at boot and passes the
	// resolved value to the channel. Required for inbound.
	IMAPPasswordEnv string `yaml:"imap_password_env"`

	// IMAPMailbox is the IMAP folder to poll. Empty defaults to
	// "INBOX".
	IMAPMailbox string `yaml:"imap_mailbox"`

	// SMTPHost is the SMTP server hostname (e.g. smtp.gmail.com).
	// Empty disables outbound; Channel.Send returns the channel's
	// ErrOutboundNotConfigured sentinel.
	SMTPHost string `yaml:"smtp_host"`

	// SMTPPort is the SMTP server port. Zero defaults to 587
	// (STARTTLS).
	SMTPPort int `yaml:"smtp_port"`

	// SMTPUsername is the SMTP login username.
	SMTPUsername string `yaml:"smtp_username"`

	// SMTPPasswordEnv names the env var holding the SMTP login
	// password. Required for outbound.
	SMTPPasswordEnv string `yaml:"smtp_password_env"`

	// FromAddress is the From: header on outbound mail. Required
	// for outbound.
	FromAddress string `yaml:"from_address"`

	// SenderAllowlist filters inbound. Entries are either full
	// addresses ("alice@example.com") or bare domains
	// ("example.com"). Empty allows every sender (dev-mode
	// pass-through, matching GitHubApp's SenderAllowlist semantics).
	SenderAllowlist []string `yaml:"sender_allowlist"`

	// PollInterval is the IMAP poll cadence as a Go duration string
	// ("60s", "2m"). Empty defaults to "60s". Sub-second values are
	// accepted but discouraged.
	PollInterval string `yaml:"poll_interval"`

	// AttachmentSizeCapBytes refuses inbound messages whose total
	// attachment payload exceeds this byte count. Zero means
	// unlimited. Mirrors maxWebhookBodyBytes's defensive style: we
	// refuse early so the channel never has to babysit a multi-MiB
	// inbound through the rest of the pipeline. Slice-2 default
	// suggestion is 25 MiB — Gmail's per-message inbound cap.
	AttachmentSizeCapBytes int64 `yaml:"attachment_size_cap_bytes"`

	// AttachmentStoreDir is the local directory under which inbound
	// attachment bytes land (one subdir per Message-ID). Empty
	// disables attachment persistence even when an ArtifactRepository
	// is wired — the channel logs and drops attachment bytes in that
	// mode. Slice-2 wiring sets this to ~/.vornik/data/email-attachments
	// or the operator-supplied override.
	AttachmentStoreDir string `yaml:"attachment_store_dir"`

	// VerifyInboundAuth turns on the slice-3 HeaderAuthVerifier — the
	// channel consults Authentication-Results and Received-SPF
	// headers stamped by upstream MTAs and rejects mail whose SPF/
	// DKIM verdicts fail policy. Default false keeps the slice-1/2
	// pass-everything behaviour for operators not yet ready to gate
	// inbound on relay verdicts.
	VerifyInboundAuth bool `yaml:"verify_inbound_auth"`

	// AuthPolicy selects the HeaderAuthVerifier strictness when
	// VerifyInboundAuth is true. "relaxed" (default) rejects only
	// explicit SPF/DKIM fail; "strict" additionally requires at
	// least one explicit pass.
	AuthPolicy string `yaml:"auth_policy"`

	// TrustedAuthServers lists the authserv-ids whose
	// Authentication-Results headers vornik trusts (RFC 8601 §5),
	// typically the operator's terminating relay (e.g. "mx.example.com").
	// When set, A-R headers stamped by any other authserv-id are
	// ignored, closing the spoof where an attacker embeds a forged
	// `Authentication-Results: …; dkim=pass` in the message they send.
	// When empty, every A-R header is consulted (legacy behaviour).
	TrustedAuthServers []string `yaml:"trusted_auth_servers"`
}

// Enabled reports whether the email channel is configured enough
// to load. IMAP host + username + password env are the bare
// minimum; outbound (SMTP*) is optional.
func (e ProjectEmail) Enabled() bool {
	return strings.TrimSpace(e.IMAPHost) != "" &&
		strings.TrimSpace(e.IMAPUsername) != "" &&
		strings.TrimSpace(e.IMAPPasswordEnv) != ""
}

// ProjectSlack configures the per-project Slack ConversationChannel
// (Events API webhook inbound, chat.postMessage outbound). Mirrors
// ProjectGitHubApp's shape — webhook signing secret + per-workspace
// allowlists live here; outbound bot token rides via env var so
// secrets stay out of YAML.
//
// Enabled at the project level when SigningSecretEnv + TeamID are
// both set. Outbound (BotTokenEnv) is optional in v1 — operators
// who only want the bot to observe events can leave it empty and
// Channel.Send returns slack.ErrOutboundNotConfigured.
type ProjectSlack struct {
	// TeamID is the Slack workspace ID (T…). Required. Events from
	// other workspaces sharing this app are dropped (multi-workspace
	// installs land each one on its own project YAML).
	TeamID string `yaml:"team_id"`

	// SigningSecretEnv names the env var holding the Slack App's
	// signing secret. The daemon reads it at boot and passes the
	// resolved value to the channel for HMAC verification. Required
	// — an empty signing secret would accept every inbound payload,
	// which the channel rejects at construction.
	SigningSecretEnv string `yaml:"signing_secret_env"`

	// BotTokenEnv names the env var holding the workspace bot token
	// (xoxb-…). The daemon resolves it at boot and the channel uses
	// it for outbound chat.postMessage calls. Empty disables outbound
	// (Send returns slack.ErrOutboundNotConfigured); inbound webhook
	// reception still works.
	BotTokenEnv string `yaml:"bot_token_env"`

	// ChannelAllowlist filters inbound. Entries are Slack channel
	// IDs (C…). Empty allows every channel in the workspace —
	// useful in dev where the operator doesn't want to pin a specific
	// channel. Production deployments should set this so a misclick
	// in Slack's "install to channel" picker doesn't expose the bot
	// to unrelated channels.
	ChannelAllowlist []string `yaml:"channel_allowlist"`

	// SenderAllowlist filters inbound by Slack user_id (U…). Empty
	// allows every user (dev-mode pass-through). Mirrors the
	// GitHubApp + Email channels' SenderAllowlist semantics —
	// non-empty rejects unknown users without burning LLM budget.
	SenderAllowlist []string `yaml:"sender_allowlist"`

	// VerifyInboundSignature when true (the default) enforces the
	// HMAC + replay-window gate. Setting it to false disables the
	// signature check — used only for local-dev round-trips against
	// a self-issued payload. Production deployments MUST leave this
	// true; a future linter pass will warn on `false`. Pointer so a
	// missing-from-YAML field defaults to the safe value.
	//
	// Note: the channel constructor will fail loudly if signing
	// secret is empty regardless of this flag. The flag's only role
	// is to short-circuit the verifier on hot paths in test
	// fixtures, not to allow unsigned production traffic.
	VerifyInboundSignature *bool `yaml:"verify_inbound_signature,omitempty"`

	// PostMessageRPS / PostMessageBurst tune the per-(team, channel)
	// outbound rate limiter. Zero values fall back to Slack's
	// documented Tier-3 cap (1 msg/sec, burst 1). Operators on a
	// higher-tier app can raise these.
	PostMessageRPS   int `yaml:"post_message_rps"`
	PostMessageBurst int `yaml:"post_message_burst"`
}

// Enabled reports whether the Slack channel is configured enough to
// load. SigningSecretEnv + TeamID are the bare minimum; outbound
// (BotTokenEnv) is optional, mirroring ProjectGitHubApp's gate.
func (s ProjectSlack) Enabled() bool {
	return strings.TrimSpace(s.SigningSecretEnv) != "" &&
		strings.TrimSpace(s.TeamID) != ""
}

// ProjectVoice configures per-project voice-message handling
// (transcription on inbound audio attachments, synthesis on outbound
// replies when the originating message was voice). Zero value disables
// voice handling for the project — the channel adapter ignores audio
// attachments on inbound and replies with text on outbound. Mirrors the
// design-doc §"Configuration shape" structure exactly. See
// https://docs.vornik.io
//
// Slice 1-4 MVP: only the open-weight local providers (whisper.cpp +
// Piper) are wired. Hosted-provider fields (deepgram-, openai-,
// elevenlabs-, coqui-) parse but are not honoured until slice 7.
type ProjectVoice struct {
	// STT is the speech-to-text provider block.
	STT ProjectVoiceSTT `yaml:"stt"`
	// TTS is the text-to-speech provider block.
	TTS ProjectVoiceTTS `yaml:"tts"`
}

// ProjectVoiceSTT configures the speech-to-text provider.
type ProjectVoiceSTT struct {
	// Provider names the implementation. "whisper-local" is the only
	// supported value in the MVP; "deepgram" / "openai-whisper" parse
	// for forward compat and are ignored until slice 7. Empty disables
	// inbound transcription for the project.
	Provider string `yaml:"provider"`
	// Model is provider-specific. For whisper-local it's the model
	// file path or a known whisper.cpp model name ("base", "small",
	// "medium", "large-v3").
	Model string `yaml:"model"`
	// BinaryPath is the absolute path to the whisper.cpp `main` CLI
	// binary. Empty asks exec.LookPath("whisper-cpp") then
	// exec.LookPath("main").
	BinaryPath string `yaml:"binary_path"`
	// FFmpegPath is the absolute path to ffmpeg. Empty asks
	// exec.LookPath. Used to normalise inbound audio (OGG/Opus from
	// Telegram, MP4/M4A from Slack) into the 16 kHz mono PCM that
	// whisper.cpp expects.
	FFmpegPath string `yaml:"ffmpeg_path"`
	// LanguageHint is an optional BCP-47 nudge for the recogniser.
	// Empty asks whisper.cpp to auto-detect.
	LanguageHint string `yaml:"language_hint"`
}

// ProjectVoiceTTS configures the text-to-speech provider.
type ProjectVoiceTTS struct {
	// Provider names the implementation. "piper" is the only
	// supported value in the MVP; "elevenlabs" / "openai" /
	// "coqui-xtts" parse for forward compat and are ignored until
	// slice 7. Empty disables outbound synthesis for the project.
	Provider string `yaml:"provider"`
	// Voice is the voice-model identifier. For Piper it's the
	// .onnx file path (or a name the operator's wrapper resolves).
	Voice string `yaml:"voice"`
	// BinaryPath is the absolute path to the piper binary. Empty
	// asks exec.LookPath("piper").
	BinaryPath string `yaml:"binary_path"`
	// FFmpegPath is the absolute path to ffmpeg. Empty asks
	// exec.LookPath. Used to transcode Piper's WAV output into the
	// channel-native format (ogg-opus for Telegram, mp4-aac for
	// Slack).
	FFmpegPath string `yaml:"ffmpeg_path"`
	// Speed tunes the playback speed (1.0 = natural). Range
	// 0.5–2.0; values outside that are clipped at synthesis time.
	// Zero falls back to 1.0.
	Speed float64 `yaml:"speed"`
	// MaxTextRunes caps the per-call text length. Zero falls back
	// to the provider default (1500 runes ~ 90s at conversational
	// pace, which trips the 1-minute Telegram cap with a small
	// safety margin).
	MaxTextRunes int `yaml:"max_text_runes"`
}

// Enabled reports whether the voice block is configured enough to be
// wired. Either STT or TTS being non-empty is sufficient — operators
// might want transcription-only (replies stay text) or
// synthesis-only (per-session /voice on, slice 5).
func (v ProjectVoice) Enabled() bool {
	return strings.TrimSpace(v.STT.Provider) != "" ||
		strings.TrimSpace(v.TTS.Provider) != ""
}

// ProjectFirewall overrides daemon-level Memory Firewall
// settings for this project. Phase D follow-on of the
// Policy-Aware Memory Firewall (LLD:
// https://docs.vornik.io).
// Zero value means "inherit daemon default" — Mode unset is the
// signal to fall through to VORNIK_MEMORY_FIREWALL_MODE.
//
// Typical compliance shape: the project storing personal data
// runs `enforce`, every other project stays on `advisory`.
type ProjectFirewall struct {
	// Mode is "off" | "advisory" | "enforce" | "" (inherit).
	// Empty / unknown / unset falls through to the daemon
	// default; the daemon's resolver normalises operator
	// typos via memoryfirewall.NormalizeMode-equivalent logic
	// in the service container.
	Mode string `yaml:"mode"`
}

// Enabled reports whether the per-project override should be
// applied. Empty Mode = "inherit daemon default"; any
// non-empty value means the operator authored an explicit
// override.
func (f ProjectFirewall) Enabled() bool {
	return strings.TrimSpace(f.Mode) != ""
}

// ProjectRateLimit caps how fast tasks can be created for the project.
// Enforced at autonomy evaluate(), dispatcher create_task, and API
// POST /tasks — each gate consults the same shared counter so a project
// can't dodge its cap by routing through a different entry point. Zero
// values disable that dimension (unlimited).
type ProjectRateLimit struct {
	TasksPerMinute int `yaml:"tasks_per_minute"`
	TasksPerHour   int `yaml:"tasks_per_hour"`
}

// ProjectTradingRateLimit caps how fast a project may place trading
// orders through the daemon audit channel. Separate from
// ProjectRateLimit so a trading project can have a tight order cap
// without throttling its task creation (or vice versa). Zero values
// disable that dimension (unlimited).
type ProjectTradingRateLimit struct {
	OrdersPerMinute int `yaml:"orders_per_minute"`
	OrdersPerHour   int `yaml:"orders_per_hour"`
}

// ProjectRetention controls per-project pruning thresholds in days.
type ProjectRetention struct {
	TaskLLMUsageDays int `yaml:"task_llm_usage_days"` // cost history; default 90
	ToolAuditDays    int `yaml:"tool_audit_days"`     // debug data; default 30
	TasksDays        int `yaml:"tasks_days"`          // terminal tasks + cascaded executions; default 60
	ExecutionsDays   int `yaml:"executions_days"`     // terminal executions; default 60
	ArtifactsDays    int `yaml:"artifacts_days"`      // artifact DB records + files; default 60
	// TaskMessagesDays prunes rows from task_messages independently
	// of the parent task. Default 0 → no independent prune (cascade
	// from tasks still applies). Useful when operators want chat
	// history trimmed faster than terminal-task retention.
	TaskMessagesDays int `yaml:"task_messages_days"`
	// MemoryChunksDays caps project_memory_chunks regardless of
	// per-class TTL. Default 0 → forever (class TTL only).
	MemoryChunksDays int `yaml:"memory_chunks_days"`
	// MemoryIngestAuditDays bounds memory_ingest_audit for this
	// project. Zero → daemon default (90). Always-on. §7.3.
	MemoryIngestAuditDays int `yaml:"memory_ingest_audit_days"`
	// MemoryPolicyEvalAllowDays / MemoryPolicyEvalBlockDays bound the
	// firewall's memory_policy_evaluations audit trail for this
	// project. Zero → daemon defaults (allow 30, block 365).
	// Always-on. Drift-mitigation §8.3.
	MemoryPolicyEvalAllowDays int `yaml:"memory_policy_eval_allow_days"`
	MemoryPolicyEvalBlockDays int `yaml:"memory_policy_eval_block_days"`
}

// ProjectChat holds per-project chat/dispatcher configuration.
type ProjectChat struct {
	// SystemPrefix is prepended to the dispatcher's system prompt for any
	// session whose active project is this one. Plain text; newlines are
	// preserved. Empty (default) adds nothing.
	SystemPrefix string `yaml:"system_prefix"`
}

// ProjectBudget defines soft and hard spending ceilings on LLM usage. All
// values are USD. Soft caps emit a warning log + metric when crossed; hard
// caps block new autonomous work, new chat-tool-driven task creation, and
// the API's POST /tasks. Zero means no limit for that dimension.
type ProjectBudget struct {
	DailySoftUSD   float64 `yaml:"daily_soft_usd"`
	DailyHardUSD   float64 `yaml:"daily_hard_usd"`
	MonthlySoftUSD float64 `yaml:"monthly_soft_usd"`
	MonthlyHardUSD float64 `yaml:"monthly_hard_usd"`
	// Timezone controls when daily/monthly windows reset. IANA tz string
	// ("Europe/Prague", "America/New_York"). Empty defaults to UTC.
	// Invalid zones fall back to UTC with a warning so a typo in config
	// doesn't silently miscount spend.
	Timezone string `yaml:"timezone"`
	// ReservationEstimateUSD is the per-task headroom claimed against the
	// hard cap at admission, before the task's real spend lands (the
	// reservation-ledger TOCTOU fix, trading-hardening §1). Over-estimating
	// is safe-side — it refuses sooner, never later; the reservation is
	// dropped (settled) when the task terminates and its real cost is what
	// actually counts. Zero falls back to budget.DefaultReservationEstimateUSD.
	// Only meaningful when a hard cap is set.
	ReservationEstimateUSD float64 `yaml:"reservation_estimate_usd"`
}

// ProjectAutonomy controls autonomous task creation settings
type ProjectAutonomy struct {
	// Enabled allows the swarm to create tasks autonomously
	Enabled bool `yaml:"enabled"`
	// Goal is the high-level objective the swarm lead works toward.
	// The autonomous loop gives this to the LLM along with current project
	// state so it can decide what to schedule next.
	Goal string `yaml:"goal"`
	// MaxTasksPerHour limits autonomous task creation rate
	MaxTasksPerHour int `yaml:"maxTasksPerHour"`
	// AllowedTaskTypes restricts what types of tasks can be created autonomously
	AllowedTaskTypes []string `yaml:"allowedTaskTypes"`
	// RequireApproval gates autonomous tasks for manual approval
	RequireApproval bool `yaml:"requireApproval"`
	// PollInterval is how often the lead evaluates the project (default "5m")
	PollInterval string `yaml:"pollInterval"`
	// EvaluateTimeout bounds a single evaluation tick's LLM + DB work.
	// Must be long enough for the lead model to complete a plan — slow
	// local models can take several minutes. Go duration string
	// (e.g. "5m", "15m"). Default "5m". The evaluation context derives
	// from the project loop's shutdown context so SIGTERM cancels
	// in-flight HTTP calls promptly.
	EvaluateTimeout string `yaml:"evaluate_timeout"`
	// PreCheck names a deterministic pre-LLM gate that runs
	// BEFORE the autonomy LLM call. Skips the tick (records as
	// SKIPPED with the gate's reason) without burning an LLM
	// call when conditions fail. Today supports:
	//   - "trading-rth": refuses ticks outside US Eastern
	//     market hours (Mon-Fri 09:30-16:00 ET, no holidays)
	//     OR when the remaining minutes-until-close is less
	//     than the configured PreCheckWorkflowMinDuration. The
	//     latter prevents scheduling a tick whose execution
	//     would drag past close. Calls into the broker MCP's
	//     /caps endpoint to verify reachability — broker down
	//     also skips the tick.
	// Empty disables the gate (existing behaviour).
	PreCheck string `yaml:"preCheck"`
	// PreCheckWorkflowMinDuration is the wall-clock buffer the
	// trading-rth pre-check requires between "now" and market
	// close. If less remains, the tick is skipped because the
	// workflow's executor would land orders into a closed
	// market. Go duration string; default "12m" matches the
	// trading workflow's maxWallClock cap.
	PreCheckWorkflowMinDuration string `yaml:"preCheckWorkflowMinDuration"`

	// ContextFilePath is the workspace-relative path the daemon
	// reads when injecting project context into the autonomy
	// lead's prompt. Default ".autonomy/PROJECT_CONTEXT.md" — a
	// hidden namespace so the daemon's bookkeeping doesn't
	// collide with the project's own files. Override per-project
	// when an existing convention is in place.
	//
	// Empty string keeps the default; explicitly set to
	// "PROJECT_CONTEXT.md" to use the legacy root-level path.
	ContextFilePath string `yaml:"contextFilePath"`

	// UserContextFilePath is the workspace-relative path the
	// daemon stamps on agent containers running tasks created
	// with creation_source = USER. ContextFilePath above is
	// the autonomy procedure (narrow, prescriptive); this one
	// is the looser ad-hoc reference the agent should read for
	// user-initiated requests. Default empty — agents fall back
	// to ContextFilePath when no user-specific guidance is
	// configured.
	//
	// Concretely the daemon stamps two env vars on the agent
	// container: VORNIK_TASK_CREATION_SOURCE=USER and
	// VORNIK_USER_CONTEXT_PATH=<resolved path>. Role
	// systemPrompts can branch on those to read the right
	// file (e.g. "If $VORNIK_TASK_CREATION_SOURCE = USER and
	// $VORNIK_USER_CONTEXT_PATH is set, read that file; else
	// read .autonomy/PROJECT_CONTEXT.md").
	//
	// Same path-safety rules as ContextFilePath: absolute paths
	// and `..` segments are rejected.
	UserContextFilePath string `yaml:"userContextFilePath"`

	// DuplicateWindow is how long after a matching task COMPLETES
	// the autonomy scheduler refuses to re-schedule the same
	// (taskType, workflow, normalized prompt) tuple. Default
	// "24h" — appropriate for backlog-style projects where each
	// item is unique. Set to "0" for cron-style projects (e.g.
	// trading) whose autonomy goal deliberately produces the
	// same prompt every tick; the active-task check still
	// prevents two concurrent runs.
	//
	// Go duration string. "0", "0s", or empty-with-explicit-zero
	// disable the completion-window block; the default applies
	// when the field is unset.
	DuplicateWindow string `yaml:"duplicateWindow"`

	// Mode selects the autonomy tick's decision engine:
	//   - "llm" (default): the lead model evaluates state +
	//     goal and decides whether to schedule. Best for fuzzy
	//     procedures (multi-portal job hunt, ad-hoc backlogs)
	//     where the right next step isn't a pure function of
	//     a flat list.
	//   - "cron": every tick fires Goal verbatim as the task
	//     prompt. No LLM evaluation, no NO_ACTION path. Best
	//     for deterministic, time-driven loops (trading
	//     windows, scheduled scrapes) where "evaluate" is
	//     dead-weight ceremony. Pre-checks, rate limits,
	//     budgets, and duplicateWindow still apply.
	//   - "backlog": every tick reads the top non-empty,
	//     non-completed line from BacklogFilePath (default
	//     "BACKLOG.md") and fires it as the prompt. Operator
	//     edits the file with normal git workflows;
	//     completion is marked by checking the box (`- [x]`)
	//     so the next tick picks up the next item.
	//
	// Empty defaults to "llm" for backward compat. Unknown
	// values are rejected at registry-load.
	Mode string `yaml:"mode"`

	// BacklogFilePath is the workspace-relative path of the
	// BACKLOG.md-style file consumed when Mode = "backlog".
	// Empty defaults to "BACKLOG.md". Same safety rules as
	// ContextFilePath: absolute paths and `..` segments
	// rejected at resolve time.
	BacklogFilePath string `yaml:"backlogFilePath"`

	// CronTaskType is the task type assigned to tasks created
	// by Mode="cron" ticks. Empty defaults to the first entry
	// in AllowedTaskTypes (or "task" when that list is empty).
	// The cron path doesn't use an LLM to pick a type, so the
	// operator pins it here.
	CronTaskType string `yaml:"cronTaskType"`
}

// AutonomyMode* constants enumerate the legal values of
// ProjectAutonomy.Mode. Empty defaults to ModeLLM at resolve
// time; only canonical values are accepted by validation.
const (
	AutonomyModeLLM     = "llm"
	AutonomyModeCron    = "cron"
	AutonomyModeBacklog = "backlog"
)

// AcceptsCallsFrom reports whether a `call_project` step from
// the given caller project should be admitted by this project.
//
// Matching rules (see LLD §4.4):
//   - empty list (default) → false (closed; no cross-project
//     calls accepted).
//   - entry equal to "*" → true (wildcard).
//   - entry equal to callerID → true (exact match).
//   - entry containing '*' or '?' → glob match via
//     path.Match; matches like "team-*" cover any callerID
//     with that prefix.
//
// Designed to be cheap on the hot path — caller passes a single
// string, no allocation in the no-match path beyond the for-
// range itself.
//
// Returns false on nil receiver, empty callerID, or any malformed
// glob pattern (a pattern that fails to compile is treated as a
// non-match rather than crashing the runtime).
// DefaultMaxCallDepth is the fallback cap for inter-project call
// chains when a project's YAML doesn't set maxCallDepth. Eight
// hops is generous for legitimate orchestrator → sub-orchestrator
// patterns and tight enough that a hallucinating LLM fanning out
// "delegate one more time" gets stopped before burning the
// underlying budget. Per-project YAML overrides via maxCallDepth.
const DefaultMaxCallDepth = 8

// EffectiveMaxCallDepth returns the depth cap applicable to chains
// originating from this project. Falls through to
// DefaultMaxCallDepth when MaxCallDepth is zero / negative. Nil
// receiver returns the default — callers that pass an unknown
// project still get the secure-by-default cap.
func (p *Project) EffectiveMaxCallDepth() int {
	if p == nil || p.MaxCallDepth <= 0 {
		return DefaultMaxCallDepth
	}
	return p.MaxCallDepth
}

// CanCall reports whether this project (the caller) is permitted to make
// a call_project call to calleeID. Empty/unset CanCallProjects means
// allow-all (the secure consent decision lives on the callee's
// AcceptCallsFrom); a non-empty list restricts outbound calls to exact +
// glob matches. A nil receiver or empty calleeID is refused.
func (p *Project) CanCall(calleeID string) bool {
	if p == nil || calleeID == "" {
		return false
	}
	if len(p.CanCallProjects) == 0 {
		return true // default: no caller-side restriction
	}
	for _, pat := range p.CanCallProjects {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if pat == "*" || pat == calleeID {
			return true
		}
		if strings.ContainsAny(pat, "*?[") {
			if matched, err := path.Match(pat, calleeID); err == nil && matched {
				return true
			}
		}
	}
	return false
}

func (p *Project) AcceptsCallsFrom(callerID string) bool {
	if p == nil || callerID == "" {
		return false
	}
	for _, pat := range p.AcceptCallsFrom {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if pat == "*" {
			return true
		}
		if pat == callerID {
			return true
		}
		if strings.ContainsAny(pat, "*?[") {
			matched, err := path.Match(pat, callerID)
			if err == nil && matched {
				return true
			}
		}
	}
	return false
}

// AllowsSpawnTemplate reports whether this project is permitted
// to spawn a new project from the named template (LLD §7).
// Returns false on nil receiver, empty template name, or an
// empty AllowSpawn.Templates list (the secure default).
//
// Matching is identical to AcceptsCallsFrom: exact match,
// "*" wildcard, and path.Match-style globs (e.g. "sales-*").
func (p *Project) AllowsSpawnTemplate(template string) bool {
	if p == nil || strings.TrimSpace(template) == "" {
		return false
	}
	for _, pat := range p.AllowSpawn.Templates {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if pat == "*" {
			return true
		}
		if pat == template {
			return true
		}
		if strings.ContainsAny(pat, "*?[") {
			matched, err := path.Match(pat, template)
			if err == nil && matched {
				return true
			}
		}
	}
	return false
}

// NormalizedAutonomyMode returns the project's autonomy mode
// after defaulting and case-normalising. Always returns one of
// the AutonomyMode* constants for a project that passed
// registry-load validation.
func (p *Project) NormalizedAutonomyMode() string {
	if p == nil {
		return AutonomyModeLLM
	}
	m := strings.ToLower(strings.TrimSpace(p.Autonomy.Mode))
	switch m {
	case AutonomyModeCron, AutonomyModeBacklog:
		return m
	default:
		return AutonomyModeLLM
	}
}

// ResolveBacklogFilePath returns the cleaned workspace-relative
// path of the backlog file for Mode="backlog" projects, or "" if
// the configured value fails the safety check (absolute path,
// `..` traversal). Defaults to "BACKLOG.md" when unset.
func (p *Project) ResolveBacklogFilePath() string {
	if p == nil {
		return ""
	}
	v := strings.TrimSpace(p.Autonomy.BacklogFilePath)
	if v == "" {
		v = "BACKLOG.md"
	}
	if filepath.IsAbs(v) {
		return ""
	}
	cleaned := filepath.Clean(v)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return ""
	}
	return cleaned
}

// ResolveCronTaskType returns the task type to assign for
// Mode="cron" ticks. Falls back through CronTaskType →
// AllowedTaskTypes[0] → "task" so a minimally-configured cron
// project still produces well-typed work.
func (p *Project) ResolveCronTaskType() string {
	if p == nil {
		return "task"
	}
	if v := strings.TrimSpace(p.Autonomy.CronTaskType); v != "" {
		return v
	}
	for _, t := range p.Autonomy.AllowedTaskTypes {
		if v := strings.TrimSpace(t); v != "" {
			return v
		}
	}
	return "task"
}

// ResolveUserContextFilePath returns the cleaned workspace-
// relative path for USER-initiated task context, or "" when
// none is configured / the configured value fails the safety
// check (absolute path, `..` traversal). Used by the executor
// to stamp VORNIK_USER_CONTEXT_PATH on agent containers; the
// autonomy package has its own copy of this helper for the
// legacy (non-USER) path which must keep its existing default
// fallback.
func (p *Project) ResolveUserContextFilePath() string {
	if p == nil {
		return ""
	}
	v := strings.TrimSpace(p.Autonomy.UserContextFilePath)
	if v == "" {
		return ""
	}
	if filepath.IsAbs(v) {
		return ""
	}
	cleaned := filepath.Clean(v)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return ""
	}
	return cleaned
}

// ProjectMCP configures MCP (Model Context Protocol) servers for a project.
// Tools discovered from these servers are available to the dispatcher and agents.
type ProjectMCP struct {
	// Servers lists the MCP servers to connect to for this project.
	Servers []MCPServerConfig `yaml:"servers"`
	// ToolRateLimits caps how fast individual MCP tools may be invoked
	// — keyed by either the bare tool name (e.g. "place_order") or the
	// "server.tool" form (e.g. "broker.place_order") for disambiguation
	// when two servers expose the same tool name. Empty disables
	// per-tool throttling for this project (existing back-compat
	// behaviour). Defensive ceiling: when the upstream MCP server's
	// own rate limits are slack or absent, this keeps a misbehaving
	// agent from hammering broker.place_order or scraper.web_fetch.
	//
	// Match precedence inside the daemon's MCP client:
	//   1. "server.tool" entry wins (most specific)
	//   2. bare "tool" entry wins next (server-agnostic ceiling)
	//   3. no entry → unlimited (the legacy default)
	//
	// Violations emit vornik_mcp_tool_rate_limited_total{project,
	// server,tool} and return a rate_limit_error to the agent with
	// a Retry-After hint so it can back off / re-queue.
	ToolRateLimits map[string]ToolRateLimitSpec `yaml:"toolRateLimits"`
}

// ToolRateLimitSpec is the per-MCP-tool token-bucket configuration.
// RPS is the sustained rate (tokens added per second); Burst is the
// maximum number of in-flight calls the bucket can hold. Both must
// be > 0 for the spec to be active — a zero or negative value
// disables enforcement (treated the same as "no entry").
type ToolRateLimitSpec struct {
	RPS   int `yaml:"rps"`
	Burst int `yaml:"burst"`
}

// Enabled reports whether this spec is configured strictly enough
// to actually enforce a limit. Mirrors the keybucket's "rps≤0 OR
// burst≤0 = no limit" contract so callers can pre-filter without
// reaching into the limiter.
func (s ToolRateLimitSpec) Enabled() bool {
	return s.RPS > 0 && s.Burst > 0
}

// MCPServerConfig defines a single MCP server connection.
type MCPServerConfig struct {
	// Name is a unique identifier for this server within the project.
	Name string `yaml:"name"`
	// Transport is "stdio" (subprocess) or "sse" (HTTP).
	Transport string `yaml:"transport"`
	// Command is the executable to run (stdio transport only).
	Command string `yaml:"command"`
	// Args are command-line arguments (stdio transport only).
	Args []string `yaml:"args"`
	// Env are environment variables for the subprocess (stdio only).
	// Values support ${VAR} expansion from the daemon's environment.
	Env map[string]string `yaml:"env"`
	// URL is the SSE endpoint (sse transport only).
	URL string `yaml:"url"`
	// AllowedTools optionally restricts which tools from this server are
	// exposed to the dispatcher and agents. When empty, all tools the
	// server advertises are exposed. When non-empty, only tools whose
	// names are listed are visible in Tools() and callable via Execute().
	//
	// Useful to shrink the LLM's tool catalog when a server advertises
	// many tools the project doesn't need (smaller payload = faster
	// completion and fewer spurious tool-name collisions). Also a
	// per-project scope knob when the same MCP server is used across
	// multiple projects with different intended capabilities.
	AllowedTools []string `yaml:"allowed_tools"`
}

// ProjectPermissions defines project-level access and permissions
type ProjectPermissions struct {
	// Secrets lists secret names this project can access
	Secrets []string `yaml:"secrets"`
	// AllowedTools restricts which tools agents can use
	AllowedTools []string `yaml:"allowedTools"`
}

// ProjectValidationError represents a validation error for a project
type ProjectValidationError struct {
	File    string
	Field   string
	Message string
}

func (e ProjectValidationError) Error() string {
	return fmt.Sprintf("project validation error in %s: %s - %s", e.File, e.Field, e.Message)
}

// Validate validates a Project struct
func (p *Project) Validate(filename string) error {
	if p.ID == "" {
		return ProjectValidationError{File: filename, Field: "projectId", Message: "projectId is required"}
	}
	if p.SwarmID == "" {
		return ProjectValidationError{File: filename, Field: "swarmId", Message: "swarmId is required"}
	}
	if p.DefaultWorkflowID == "" {
		return ProjectValidationError{File: filename, Field: "defaultWorkflowId", Message: "defaultWorkflowId is required"}
	}
	if p.DefaultPriority < 0 || p.DefaultPriority > 100 {
		return ProjectValidationError{File: filename, Field: "defaultPriority", Message: "must be between 0 and 100"}
	}
	if p.MaxConcurrentTasks < 0 {
		return ProjectValidationError{File: filename, Field: "maxConcurrentTasks", Message: "cannot be negative"}
	}
	if p.Autonomy.MaxTasksPerHour < 0 {
		return ProjectValidationError{File: filename, Field: "autonomy.maxTasksPerHour", Message: "cannot be negative"}
	}
	if p.RateLimit.TasksPerMinute < 0 {
		return ProjectValidationError{File: filename, Field: "rate_limit.tasks_per_minute", Message: "cannot be negative"}
	}
	if p.RateLimit.TasksPerHour < 0 {
		return ProjectValidationError{File: filename, Field: "rate_limit.tasks_per_hour", Message: "cannot be negative"}
	}
	if p.Budget.DailySoftUSD < 0 {
		return ProjectValidationError{File: filename, Field: "budget.daily_soft_usd", Message: "cannot be negative"}
	}
	if p.Budget.DailyHardUSD < 0 {
		return ProjectValidationError{File: filename, Field: "budget.daily_hard_usd", Message: "cannot be negative"}
	}
	if p.Budget.MonthlySoftUSD < 0 {
		return ProjectValidationError{File: filename, Field: "budget.monthly_soft_usd", Message: "cannot be negative"}
	}
	if p.Budget.MonthlyHardUSD < 0 {
		return ProjectValidationError{File: filename, Field: "budget.monthly_hard_usd", Message: "cannot be negative"}
	}
	if p.Budget.DailySoftUSD > 0 && p.Budget.DailyHardUSD > 0 && p.Budget.DailySoftUSD > p.Budget.DailyHardUSD {
		return ProjectValidationError{File: filename, Field: "budget.daily_soft_usd", Message: "cannot exceed budget.daily_hard_usd"}
	}
	if p.Budget.MonthlySoftUSD > 0 && p.Budget.MonthlyHardUSD > 0 && p.Budget.MonthlySoftUSD > p.Budget.MonthlyHardUSD {
		return ProjectValidationError{File: filename, Field: "budget.monthly_soft_usd", Message: "cannot exceed budget.monthly_hard_usd"}
	}
	if p.Trading.Caps.MaxPositionUSD < 0 {
		return ProjectValidationError{File: filename, Field: "trading.caps.max_position_usd", Message: "cannot be negative"}
	}
	if p.Trading.Caps.MaxDailyTurnoverUSD < 0 {
		return ProjectValidationError{File: filename, Field: "trading.caps.max_daily_turnover_usd", Message: "cannot be negative"}
	}
	if p.Trading.Caps.MaxOrdersPerHour < 0 {
		return ProjectValidationError{File: filename, Field: "trading.caps.max_orders_per_hour", Message: "cannot be negative"}
	}
	if p.Trading.Caps.MaxOrdersPerMinute < 0 {
		return ProjectValidationError{File: filename, Field: "trading.caps.max_orders_per_minute", Message: "cannot be negative"}
	}
	if p.Trading.Caps.DrawdownCircuitBreakerPct < 0 || p.Trading.Caps.DrawdownCircuitBreakerPct > 100 {
		return ProjectValidationError{File: filename, Field: "trading.caps.drawdown_circuit_breaker_pct", Message: "must be between 0 and 100"}
	}
	if p.Trading.Caps.DailyLossCircuitBreakerPct < 0 || p.Trading.Caps.DailyLossCircuitBreakerPct > 100 {
		return ProjectValidationError{File: filename, Field: "trading.caps.daily_loss_circuit_breaker_pct", Message: "must be between 0 and 100"}
	}
	if p.Trading.Mode != "" && p.Trading.Mode != "paper" && p.Trading.Mode != "live" {
		return ProjectValidationError{File: filename, Field: "trading.mode", Message: "must be 'paper' or 'live' (or unset)"}
	}
	// Webhook sources require a name + a secret. The auth middleware
	// admits unauthenticated webhook requests only when they bear an
	// HMAC signature header; verifying that signature requires a
	// per-source secret, so a source without one would silently
	// degrade the route to "anyone can POST" — matching the audit
	// finding that flagged the previous "all webhooks public" behaviour.
	// GitHub App config: validate the cross-field requirements so a
	// half-filled block fails fast at boot rather than at first
	// delivery. WebhookSecretEnv-without-repos and outbound-creds-
	// without-AppID are the two common misconfig shapes.
	if g := p.GitHubApp; g.WebhookSecretEnv != "" || g.AppID != 0 || g.PrivateKeyPath != "" || g.InstallationID != 0 || len(g.RepoAllowlist) > 0 {
		if strings.TrimSpace(g.WebhookSecretEnv) == "" {
			return ProjectValidationError{File: filename, Field: "github_app.webhook_secret_env", Message: "required when any github_app field is set"}
		}
		if len(g.RepoAllowlist) == 0 {
			return ProjectValidationError{File: filename, Field: "github_app.repo_allowlist", Message: "must contain at least one repo (defensive deny-all default)"}
		}
		// Outbound is all-or-nothing: setting one of (AppID,
		// PrivateKeyPath, InstallationID) requires all three.
		set := 0
		if g.AppID != 0 {
			set++
		}
		if strings.TrimSpace(g.PrivateKeyPath) != "" {
			set++
		}
		if g.InstallationID != 0 {
			set++
		}
		if set != 0 && set != 3 {
			return ProjectValidationError{
				File:    filename,
				Field:   "github_app",
				Message: "app_id, private_key_path, and installation_id must all be set together (or all be empty for inbound-only mode)",
			}
		}
	}
	// Forge config: a known provider, and (for an explicit github forge block)
	// the App credential trio all-or-nothing — so a half-filled block fails fast
	// at boot rather than at first publish. The back-compat path (no forge block,
	// top-level github:) is already validated by the github_app rules above.
	if prov := strings.TrimSpace(p.Forge.Provider); prov != "" {
		switch prov {
		case forge.ProviderGitHub, forge.ProviderGitLab, forge.ProviderGitea:
		default:
			return ProjectValidationError{File: filename, Field: "forge.provider", Message: fmt.Sprintf("unknown provider %q (want github|gitlab|gitea)", prov)}
		}
		if prov == forge.ProviderGitHub {
			g := p.Forge.GitHub
			// An explicit nested github block must be complete; if it's empty we
			// fall back to the top-level github: block at resolve time, so only a
			// partially-filled nested block is an error here.
			set := 0
			if g.AppID != 0 {
				set++
			}
			if strings.TrimSpace(g.PrivateKeyPath) != "" {
				set++
			}
			if g.InstallationID != 0 {
				set++
			}
			if set != 0 && set != 3 {
				return ProjectValidationError{File: filename, Field: "forge.github", Message: "app_id, private_key_path, and installation_id must all be set together"}
			}
		}
	}
	// Email config: same all-or-nothing posture as GitHubApp.
	// When any IMAP* field is set, the trio (host/username/password
	// env) must all be present. Outbound SMTP* is independent — its
	// trio (host/username/password env) is all-or-nothing on its own.
	if e := p.Email; e.IMAPHost != "" || e.IMAPUsername != "" || e.IMAPPasswordEnv != "" ||
		e.SMTPHost != "" || e.SMTPUsername != "" || e.SMTPPasswordEnv != "" || e.FromAddress != "" {
		if strings.TrimSpace(e.IMAPHost) == "" {
			return ProjectValidationError{File: filename, Field: "email.imap_host", Message: "required when any email field is set"}
		}
		if strings.TrimSpace(e.IMAPUsername) == "" {
			return ProjectValidationError{File: filename, Field: "email.imap_username", Message: "required when any email field is set"}
		}
		if strings.TrimSpace(e.IMAPPasswordEnv) == "" {
			return ProjectValidationError{File: filename, Field: "email.imap_password_env", Message: "required when any email field is set"}
		}
		// SMTP all-or-nothing.
		smtpSet := 0
		if strings.TrimSpace(e.SMTPHost) != "" {
			smtpSet++
		}
		if strings.TrimSpace(e.SMTPUsername) != "" {
			smtpSet++
		}
		if strings.TrimSpace(e.SMTPPasswordEnv) != "" {
			smtpSet++
		}
		if strings.TrimSpace(e.FromAddress) != "" {
			smtpSet++
		}
		if smtpSet != 0 && smtpSet != 4 {
			return ProjectValidationError{
				File:    filename,
				Field:   "email",
				Message: "smtp_host, smtp_username, smtp_password_env, and from_address must all be set together (or all be empty for inbound-only mode)",
			}
		}
	}
	// ToolRateLimits: negative values are nonsensical and almost
	// certainly a typo; fail at boot rather than silently treating
	// them as "no limit" (which is the runtime behaviour for ≤0,
	// but that's the bypass-by-design path, not what an operator
	// who explicitly wrote -1 meant).
	for name, spec := range p.MCP.ToolRateLimits {
		if spec.RPS < 0 {
			return ProjectValidationError{
				File:    filename,
				Field:   fmt.Sprintf("mcp.toolRateLimits[%q].rps", name),
				Message: "cannot be negative",
			}
		}
		if spec.Burst < 0 {
			return ProjectValidationError{
				File:    filename,
				Field:   fmt.Sprintf("mcp.toolRateLimits[%q].burst", name),
				Message: "cannot be negative",
			}
		}
	}
	for i, src := range p.Webhooks.Sources {
		if strings.TrimSpace(src.Name) == "" {
			return ProjectValidationError{
				File:    filename,
				Field:   fmt.Sprintf("webhooks.sources[%d].name", i),
				Message: "name is required",
			}
		}
		if strings.TrimSpace(src.Secret) == "" && strings.TrimSpace(src.SecretEnv) == "" {
			return ProjectValidationError{
				File:    filename,
				Field:   fmt.Sprintf("webhooks.sources[%d].secret", i),
				Message: "secret or secret_env is required so HMAC verification has something to compare against",
			}
		}
		if f := strings.TrimSpace(src.Filter); f != "" && !strings.Contains(f, "${") {
			return ProjectValidationError{
				File:    filename,
				Field:   fmt.Sprintf("webhooks.sources[%d].filter", i),
				Message: `filter must reference an event field, e.g. "${action}=opened,labeled"`,
			}
		}
	}
	// GitHub App token-minting credentials are all-or-nothing.
	if g := p.GitHub; g.AppID != 0 || g.InstallationID != 0 || strings.TrimSpace(g.PrivateKeyPath) != "" {
		if g.AppID == 0 || g.InstallationID == 0 || strings.TrimSpace(g.PrivateKeyPath) == "" {
			return ProjectValidationError{
				File:    filename,
				Field:   "github",
				Message: "app_id, installation_id, and private_key_path are all required when any github field is set",
			}
		}
	}
	return nil
}

// LoadProjects loads all project YAML files from the specified directory
// and attaches any PROJECT.md brief companions found alongside.
func LoadProjects(dir string) (map[string]*Project, error) {
	projects := make(map[string]*Project)

	projectsDir := filepath.Join(dir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return projects, nil // No projects directory is ok
		}
		return nil, fmt.Errorf("failed to read projects directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}

		path := filepath.Join(projectsDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read project file %s: %w", name, err)
		}

		var project Project
		if err := yaml.Unmarshal(data, &project); err != nil {
			// Skip files with syntax errors rather than failing the entire
			// registry. A corrupted or hand-edited file should not prevent
			// other projects from loading. The error is printed to stderr so
			// it appears in the daemon log.
			fmt.Fprintf(os.Stderr, "vornik/registry: skipping %s: yaml parse error: %v\n", name, err)
			continue
		}

		// Validate the project
		if err := project.Validate(name); err != nil {
			return nil, err
		}

		// Check for duplicate IDs
		if _, exists := projects[project.ID]; exists {
			return nil, ProjectValidationError{
				File:    name,
				Field:   "projectId",
				Message: fmt.Sprintf("duplicate projectId: %s", project.ID),
			}
		}

		projects[project.ID] = &project
	}

	if err := attachProjectBriefs(projectsDir, projects); err != nil {
		return nil, err
	}

	return projects, nil
}

// attachProjectBriefs scans projectsDir for `*.md` PROJECT.md
// companions, parses each one, and merges the brief into the
// matching *Project. Orphan briefs (PROJECT.md whose projectId
// doesn't match any loaded project) are a hard error so a typo
// surfaces at boot.
//
// Conflict resolution (see web-authoring-ux-design.md):
//   - displayName: brief frontmatter wins when set; else project.yaml.
//   - description: brief preamble wins when non-empty; else project.yaml.
//   - autonomy.goal: project.yaml wins when set; brief fills only when
//     YAML is empty (operational source of truth stays in YAML).
func attachProjectBriefs(projectsDir string, projects map[string]*Project) error {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to re-read projects directory for briefs: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		path := filepath.Join(projectsDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read project brief %s: %w", name, err)
		}
		brief, err := ParseProjectMarkdown(data, name)
		if err != nil {
			return err
		}
		project, ok := projects[brief.ProjectID]
		if !ok {
			return ProjectValidationError{
				File:    name,
				Field:   "projectId",
				Message: fmt.Sprintf("PROJECT.md references projectId %q but no project.yaml with that ID was loaded", brief.ProjectID),
			}
		}
		if project.Brief != nil {
			return ProjectValidationError{
				File:    name,
				Field:   "projectId",
				Message: fmt.Sprintf("duplicate PROJECT.md for projectId %q", brief.ProjectID),
			}
		}
		project.Brief = brief
		if brief.DisplayName != "" {
			project.DisplayName = brief.DisplayName
		}
		if brief.Description != "" {
			project.Description = brief.Description
		}
		if strings.TrimSpace(project.Autonomy.Goal) == "" && brief.Goal != "" {
			project.Autonomy.Goal = brief.Goal
		}
	}
	return nil
}
