// Package config provides configuration loading and validation for vornik.
package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Config represents the top-level vornik configuration.
// NamedSecret is an operator-declared credential injected into agent
// containers, gated by project. Value is ${VAR}-expanded at config load.
type NamedSecret struct {
	// Name is the environment variable name the agent sees (e.g. BROKER_API_KEY).
	Name string `yaml:"name" json:"name"`
	// Value is the secret. Use ${VAR} to pull it from the daemon environment
	// rather than committing plaintext to YAML.
	Value string `yaml:"value" json:"value"`
	// AllowedProjects scopes injection. Empty = every project (all-access,
	// matching the AllowedTools convention); otherwise only the listed
	// projects' agents receive the value.
	AllowedProjects []string `yaml:"allowed_projects" json:"allowed_projects"`
}

// SteeringOperatorAlertConfig names a single operator chat recipient to alert
// when an ownerless autonomy task enters a steering state (the chat/DM
// notifier can only reach chat-originated tasks). Empty Channel disables it.
type SteeringOperatorAlertConfig struct {
	// Channel is the conversation channel to alert ("telegram"/"slack"/"email").
	Channel string `yaml:"channel" json:"channel"`
	// Session is the native session id within that channel (e.g. a Telegram chat_id).
	Session string `yaml:"session" json:"session"`
	// Address is the email recipient, required only when Channel == "email".
	Address string `yaml:"address" json:"address"`
}

// ClusterConfig holds cluster-diagnostics knobs. ExpectedEndpoints declares
// externals the daemon can't infer from its own profile (notably the public
// webhook ingress URL) so the cluster monitor + `vornikctl cluster check` can
// validate they're reachable and correctly configured. Empty = monitor only
// the config-derived endpoints.
type ClusterConfig struct {
	ExpectedEndpoints []ExpectedEndpointConfig `yaml:"expected_endpoints" json:"expected_endpoints" doc:"Endpoints the cluster should expose, validated by the cluster monitor and cluster check."`
	// MonitorIntervalSeconds is the continuous-monitor probe cadence. 0 → 30s.
	MonitorIntervalSeconds int `yaml:"monitor_interval_seconds" json:"monitor_interval_seconds" doc:"Cluster endpoint monitor probe interval in seconds. 0 means 30."`
}

// ExpectedEndpointConfig is one operator-declared endpoint to validate.
type ExpectedEndpointConfig struct {
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url" json:"url"`
	Kind string `yaml:"kind" json:"kind" doc:"https | readyz | mtls-relay | db"`
}

type Config struct {
	// SteeringNotificationsEnabled pushes a prompt to the originating
	// chat/DM when a task that was created there enters a steering state
	// (AWAITING_INPUT / AWAITING_APPROVAL), so the operator doesn't have to
	// watch the UI inbox. Default true; set false to silence. See
	// https://docs.vornik.io
	SteeringNotificationsEnabled bool `yaml:"steering_notifications_enabled" json:"steering_notifications_enabled" doc:"Push a chat/DM prompt when a task needs operator steering (input/approval). Default true."`

	// SteeringOperatorAlert is the fallback recipient for steering on
	// ownerless autonomy tasks — those the chat/DM notifier can't reach
	// because no chat originated them. Without it, an autonomy task that
	// parks at AWAITING_APPROVAL notifies nobody and stalls silently. Unset
	// (empty channel) by default, so behaviour is unchanged unless an
	// operator opts in. Gated by SteeringNotificationsEnabled.
	SteeringOperatorAlert SteeringOperatorAlertConfig `yaml:"steering_operator_alert" json:"steering_operator_alert" doc:"Fallback chat recipient alerted when an ownerless autonomy task needs steering (channel/session[/address])."`

	// Cluster holds cluster-diagnostics knobs (expected endpoints to validate,
	// monitor cadence). Optional; empty = config-derived endpoints only.
	Cluster ClusterConfig `yaml:"cluster" json:"cluster" doc:"Cluster-diagnostics: expected endpoints + monitor interval."`

	// NamedSecrets are operator-declared secrets injected into agent
	// containers ONLY for the projects each one allows — a per-secret
	// allowlist so one project's swarm can't bind another's credential. The
	// `value` is ${VAR}-expanded from the daemon environment at config load
	// (so the plaintext stays out of YAML). An empty allowed_projects means
	// "all projects" (mirrors the AllowedTools empty-means-all convention).
	// Distinct from `secrets:` (SecretsConfig) below, which is the leak
	// DETECTOR, not a store.
	NamedSecrets []NamedSecret `yaml:"named_secrets" json:"named_secrets" doc:"Per-secret allowlist of env credentials injected into agent containers, scoped by project."`

	Server    ServerConfig    `yaml:"server"`
	Database  DatabaseConfig  `yaml:"database"`
	Storage   StorageConfig   `yaml:"storage"`
	Artifacts StorageConfig   `yaml:"artifacts"`
	Runtime   RuntimeConfig   `yaml:"runtime"`
	Scheduler SchedulerConfig `yaml:"scheduler"`
	Watchdog  WatchdogConfig  `yaml:"watchdog"`
	// EffectiveCost configures the $/success drift monitor.
	EffectiveCost EffectiveCostConfig `yaml:"effective_cost"`
	// Secrets configures the secret-leak detector.
	Secrets   SecretsConfig   `yaml:"secrets"`
	Retention RetentionConfig `yaml:"retention"`
	Metrics   MetricsConfig   `yaml:"metrics"`
	Tracing   TracingConfig   `yaml:"tracing"`
	Logging   LoggingConfig   `yaml:"logging"`
	API       APIConfig       `yaml:"api"`
	// Trading configures the daemon side of the broker→daemon
	// trading audit channel — specifically the HMAC request-auth
	// guarding the /api/v1/internal/trading-* endpoints. Empty
	// (Auth.Enabled=false) keeps today's bearer-only behaviour.
	Trading  TradingConfig  `yaml:"trading" json:"trading"`
	Chat     ChatConfig     `yaml:"chat"`
	Autonomy AutonomyConfig `yaml:"autonomy"`
	Telegram TelegramConfig `yaml:"telegram"`
	Memory   MemoryConfig   `yaml:"memory"`
	// Instinct configures the continuous-learning instinct layer
	// (migrations 85/86). Opt-in, off by default — with it disabled
	// the daemon never constructs the extraction worker and behaviour
	// is byte-for-byte today's. See
	// https://docs.vornik.io
	Instinct InstinctConfig `yaml:"instinct"`
	// Intentjudge configures the two-tier risk judge. Empty leaves
	// both tiers wired with sensible defaults — operators only
	// need to override when pinning a specific LLM refiner model.
	Intentjudge IntentjudgeConfig `yaml:"intentjudge"`
	// MCP declares MCP (Model Context Protocol) servers visible at the
	// daemon level. This block is PURELY a discovery surface: the
	// servers listed here populate /api/v1/mcp/servers and /ui/mcp so
	// operators can see what's installed without pulling each
	// project's YAML. Daemon-level entries do NOT silently get
	// granted to projects — agents still see only the MCP servers
	// their PROJECT's mcp.servers block declares. The separation is
	// intentional: operators who want a daemon-level server exposed
	// to a given project must add it to that project's mcp.servers
	// list explicitly. Same shape as the per-project block, so
	// operators can copy entries either direction.
	MCP MCPConfig `yaml:"mcp"`
	// Admin gates the daemon-level admin UI surface (`/ui/admin/*`).
	// Disabled by default — slice 1 of admin-ui-design.md ships the
	// gate dark so the new routes return 404 (not 403) until an
	// operator opts in.
	Auth  AuthSettings `yaml:"auth"`
	Admin AdminConfig  `yaml:"admin"`
	// Voice configures the local STT + TTS providers used by the
	// Telegram and Slack channel adapters for voice-message round-
	// trip. Empty disables voice across all channels — inbound voice
	// drops through to the legacy attachment path and outbound
	// replies stay text. See https://docs.vornik.io
	Voice VoiceConfig `yaml:"voice"`
	// ToolBudget configures dynamic per-role tool-use limits: a planner
	// emits a complexity tier and the daemon scales each role's static
	// VORNIK_MAX_TOOL_ITERATIONS by a config-capped factor. Opt-in, off
	// by default — disabled, Resolve is a passthrough and behaviour is
	// byte-for-byte today's. See
	// https://docs.vornik.io
	ToolBudget ToolBudgetConfig `yaml:"tool_budget"`
	// Node configures role-specialized deployment. Empty / absent →
	// every capability is on, exactly today's single-process behavior.
	// See https://docs.vornik.io §1.
	Node NodeConfig `yaml:"node"`
	// Blackbox configures the Autonomy Black Box counterfactual replay
	// engine. Empty → the replay-safe classifier seeds from the curated
	// built-in default (blackbox.DefaultReplaySafeTools). See
	// https://docs.vornik.io § Phase C.
	Blackbox BlackboxConfig `yaml:"blackbox"`
}

// BlackboxConfig tunes the counterfactual replay engine.
type BlackboxConfig struct {
	// ReplaySafeTools is the deny-by-default allow-list of tool names
	// that may execute LIVE during a counterfactual replay (read-only /
	// idempotent tools — broker/account reads, market-data + TA
	// queries, file reads). Every tool NOT on this list is stubbed with
	// a synthesized "skipped" envelope so a replay can't fire orders,
	// send messages, or write files. Nil / absent → seed from
	// blackbox.DefaultReplaySafeTools; an explicit empty list stubs
	// EVERY tool. Names may be bare (broker_get_orders) or fully
	// qualified (mcp__broker__get_orders) — both match.
	ReplaySafeTools []string `yaml:"replay_safe_tools"`
}

// MCPConfig is the daemon-level MCP block. Mirrors the per-project
// shape in registry.ProjectMCP so operators write one and the same
// fields either at the daemon scope (discovery only) or per-project
// (active wiring). See the Config.MCP field comment for why the two
// scopes are separate.
type MCPConfig struct {
	// Servers lists MCP servers the daemon connects to at startup
	// for discovery purposes only — tools/list is invoked, the
	// catalog is cached with a 5-minute TTL, and the result is
	// exposed via /api/v1/mcp/servers and /ui/mcp. Servers that
	// fail to connect appear in the surface with reachable=false
	// + an error message so operators can diagnose.
	Servers []MCPServerConfig `yaml:"servers"`
}

// MCPServerConfig defines one daemon-level MCP server. Field set
// matches registry.MCPServerConfig (the per-project shape) so YAML
// snippets are portable between the two scopes.
type MCPServerConfig struct {
	// Name is the unique identifier for this server within the
	// daemon-level catalog.
	Name string `yaml:"name" doc:"Unique server name in the catalog."`
	// Transport is "stdio" (subprocess), "sse" (legacy POST-JSON), or "streamable-http".
	Transport string `yaml:"transport" doc:"stdio, sse, or streamable-http."`
	// Command is the executable to run (stdio transport only).
	Command string `yaml:"command" doc:"Subprocess executable (stdio transport)."`
	// Args are command-line arguments (stdio transport only).
	Args []string `yaml:"args" doc:"Subprocess arguments (stdio transport)."`
	// Env are environment variables for the subprocess (stdio only).
	// Values support ${VAR} expansion from the daemon's environment.
	Env map[string]string `yaml:"env" doc:"Subprocess environment variables (stdio transport)."`
	// URL is the SSE endpoint (sse transport only).
	URL string `yaml:"url" doc:"Endpoint URL (sse transport)."`
	// AllowedTools optionally restricts which tools from this
	// server appear in the discovery catalog. Empty = all advertised
	// tools.
	AllowedTools []string `yaml:"allowed_tools" doc:"Restrict which advertised tools appear."`
}

// AuthSettings holds daemon-wide authentication settings that sit
// above the legacy api block. Introduced by pluggable-auth slice 2;
// the OIDC + identity-core work (oidc-identity-permissions-design.md)
// extends this block in later phases.
type AuthSettings struct {
	// BootstrapAdmins lists channel identities ("channel:external_id",
	// e.g. "google:vadim@vornik.io") that are auto-granted admin on
	// FIRST provision — only consulted for identities with no
	// existing binding; removing an entry later does not demote the
	// user (group membership is the source of truth). Solves the
	// empty-DB chicken-and-egg for the identity core
	// (oidc-identity-permissions-design.md §3.3). The external_id
	// half may itself contain colons; the split is on the first.
	// Entries must be in exact canonical form — whitespace around the
	// entry or either side of the colon is a startup validation error
	// (a padded entry would silently never match at login).
	BootstrapAdmins []string `yaml:"bootstrap_admins" json:"bootstrap_admins" doc:"Channel identities (channel:external_id) auto-granted admin on first login."`

	// ExternalBaseURL is the public origin of this daemon as
	// browsers reach it (scheme://host[:port], no path). OAuth
	// callbacks are ExternalBaseURL + "/auth/<provider>/callback"
	// and MUST byte-match the URL registered with the provider.
	// Required when any provider is configured.
	// A trailing "/" is accepted and trimmed by consumers (the callback builder uses strings.TrimRight(base, "/") + path).
	ExternalBaseURL string `yaml:"external_base_url" json:"external_base_url" doc:"Public origin of the daemon (scheme://host[:port]). Required when a login provider is configured."`

	// Session controls browser-session policy (design §4.3).
	Session SessionSettings `yaml:"session" json:"session"`

	// Providers configures browser login providers. Absent block =
	// no provider button; none at all = /ui/login shows only the
	// break-glass key path.
	Providers ProviderSettings `yaml:"providers" json:"providers"`
}

// SessionSettings — browser session TTLs.
type SessionSettings struct {
	// Lifetime is the fixed session expiry from creation.
	// Defaulted to "168h" (7d) at load when empty.
	Lifetime string `yaml:"lifetime" json:"lifetime" doc:"Fixed session expiry."`
	// IdleTimeout cuts sessions idle longer than this. Empty/0 =
	// disabled.
	IdleTimeout string `yaml:"idle_timeout" json:"idle_timeout" doc:"Cut sessions idle longer than this."`
}

// ProviderSettings — one optional block per login provider.
type ProviderSettings struct {
	GitHub *GitHubProviderSettings `yaml:"github" json:"github"`
}

// GitHubProviderSettings — GitHub OAuth App credentials. GitHub
// issues no ID tokens; this is plain OAuth2 + userinfo.
type GitHubProviderSettings struct {
	ClientID string `yaml:"client_id" json:"client_id" doc:"GitHub OAuth App client ID."`
	// ClientSecret inline, OR ClientSecretFile (preferred — the
	// secrets dir). Exactly one. The loader resolves the file
	// into ClientSecret after parse; downstream code reads only
	// ClientSecret.
	// json:"-": the resolved secret must never serialize outward (mirrors storage SecretAccessKey at config.go:527); the API redaction layer is defense-in-depth, not the only line.
	ClientSecret     string `yaml:"client_secret" json:"-"`
	ClientSecretFile string `yaml:"client_secret_file" json:"client_secret_file" doc:"Path to the GitHub OAuth client secret (preferred over inline)."`
	// Org enables the membership check (read:org scope). NON-members
	// still provision as awaiting-access (SOFT gate, agreed 2026-06-05)
	// with their status surfaced at login. Empty = no org check. By
	// itself this grants members NOTHING — see OrgMemberRole.
	Org string `yaml:"org" json:"org" doc:"Restrict/soft-gate login to a GitHub org."`
	// OrgMemberRole, when set, auto-grants a verified org member this
	// role on login (github-org-member-default-access-design.md). "" =
	// disabled (members land awaiting; an admin approves them).
	OrgMemberRole string `yaml:"org_member_role" json:"org_member_role" doc:"Auto-grant role for GitHub org members on login: empty (default, no auto-grant), 'user', or 'admin'. Requires 'org' set."`
	// OrgMemberProjects scopes the auto-granted 'user' role. Required
	// when OrgMemberRole is 'user'; '*' = all projects. Ignored for
	// 'admin' (instance-wide).
	OrgMemberProjects []string `yaml:"org_member_projects" json:"org_member_projects" doc:"Projects an auto-granted org member (user role) receives; '*' = all projects."`
}

// AdminConfig governs the daemon-level admin UI + admin CLI gate.
// Per admin-ui-design.md §10 the gate fails closed on every axis:
// when Enabled is false, every `/ui/admin/*` URL must return 404
// (not 403) so an attacker can't probe whether the surface exists.
// When Enabled is true, only keys in AllowedKeys get through.
//
// TODO(pluggable-auth-slice-2): the AllowedKeys list is a temporary
// shim until the auth refactor's Principal carries a role claim.
// Once that lands, swap the membership check for a "principal has
// admin role" check.
type AdminConfig struct {
	// Enabled gates the entire admin surface. When false, every
	// `/ui/admin/*` URL returns 404 NOT 403 (per design doc §10 —
	// hiding the surface from probes is the whole point).
	Enabled bool `yaml:"enabled"`
	// AllowedKeys lists the `sk-vornik-*` keys that have admin
	// scope. Static-keys map values (legacy) are matched against
	// this list verbatim; DB-backed keys are matched against the
	// key string the caller presented. Membership in this list is
	// what flips the rendered "Admin" nav link on for an operator.
	AllowedKeys []string `yaml:"allowed_keys"`
}

// IsAdminKey reports whether the supplied API key is in the
// allow-list. Returns false when Enabled is false so callers can
// use a single check at the call site without re-reading the
// Enabled flag (the admin middleware uses Enabled separately for
// the 404 vs 403 vs 200 decision).
func (a AdminConfig) IsAdminKey(key string) bool {
	if !a.Enabled || key == "" {
		return false
	}
	// Constant-time comparison. A naive `k == key` short-circuits on the
	// first differing byte and returns early on the first match, leaking
	// both per-byte and match-position timing that lets an attacker
	// recover a valid admin key byte-by-byte. SHA-256 both sides to a
	// fixed 32-byte length (so ConstantTimeCompare can't leak length via
	// its length-mismatch early-out) and scan the whole slice without an
	// early return, so total time is independent of which entry matched
	// or how many bytes are correct.
	presented := sha256.Sum256([]byte(key))
	found := 0
	for _, k := range a.AllowedKeys {
		stored := sha256.Sum256([]byte(k))
		if subtle.ConstantTimeCompare(stored[:], presented[:]) == 1 {
			found = 1
		}
	}
	return found == 1
}

// IntentjudgeConfig configures the two-tier risk judge for
// dispatcher tool calls. The heuristic tier (sub-ms rule table)
// always runs; the LLM refiner (async, seconds) re-evaluates
// medium+ risk verdicts when wired.
type IntentjudgeConfig struct {
	// RefinerModel pins the LLM model id the async refiner uses.
	// Empty leaves the chat router's default in place — recommend
	// a small OSS classifier like gpt-oss-20b or gemma-4-26b. The
	// refiner doesn't need a reasoning model.
	RefinerModel string `yaml:"refiner_model"`
}

// SecretsConfig declares the secret-leak detection layer. Empty/
// missing fields fall to compiled defaults inside internal/secrets:
// curated pattern list + default allowlist + entropy detector at
// 40-char/4.5-bit thresholds. The map of checkpoint → action lets
// operators tune per-channel policy without touching code.
//
// Disabled (Enabled: false) entirely no-ops the layer — useful for
// load testing or when running against a corpus where the
// false-positive rate is unbearable. Default is on.
type SecretsConfig struct {
	Enabled bool `yaml:"enabled" doc:"Turn detection on."`
	// Patterns lets operators add custom regexes alongside the
	// curated list and disable specific defaults.
	Patterns SecretsPatternsConfig `yaml:"patterns"`
	// Allowlist regex strings appended to the default allowlist.
	// Operators add narrow allow rules ("this specific token
	// shape is fine in our codebase") without disabling whole
	// patterns.
	Allowlist []string `yaml:"allowlist" doc:"Extra regexes appended to the default allowlist."`
	// Entropy controls the entropy-fallback detector.
	Entropy SecretsEntropyConfig `yaml:"entropy"`
	// Checkpoints maps a channel name (result_json, tool_audit,
	// container_logs, artifacts, telegram, webhook, memory) to an
	// action (detect / redact / block). Unset checkpoints inherit
	// per-channel compiled defaults — see internal/secrets.
	Checkpoints map[string]string `yaml:"checkpoints" doc:"Map a channel to an action: detect, redact, or block."`
}

// SecretsPatternsConfig is the patterns substructure: a list of
// custom regexes + a list of default-pattern names to disable.
type SecretsPatternsConfig struct {
	Custom  []SecretsPatternConfig `yaml:"custom"`
	Disable []string               `yaml:"disable"`
}

// SecretsPatternConfig is one operator-supplied pattern. Name
// must be unique across the (default + custom) set; conflict at
// load time is a fatal startup error.
type SecretsPatternConfig struct {
	Name        string `yaml:"name"`
	Regex       string `yaml:"regex"`
	Description string `yaml:"description"`
}

// SecretsEntropyConfig overrides the entropy-detector defaults.
// Disabled=true skips the entropy pass entirely — operators with
// high false-positive rates fall back to regex-only detection.
type SecretsEntropyConfig struct {
	Disabled bool    `yaml:"disabled"`
	MinLen   int     `yaml:"min_len"`
	MinBits  float64 `yaml:"min_bits"`
}

// EffectiveCostConfig mirrors budget.EffectiveCostConfig at the
// daemon-config layer: empty fields fall to compiled defaults
// inside the budget package (1h tick, 24h/7d windows, 2× ratio,
// 12h cooldown). Operators tune the threshold up for noisy
// projects or down for tighter spend governance.
type EffectiveCostConfig struct {
	Enabled            bool    `yaml:"enabled" doc:"Turn the drift monitor on."`
	Interval           string  `yaml:"interval" doc:"Evaluation cadence."`                                        // "1h"
	CurrentWindow      string  `yaml:"current_window"`                                                            // "24h"
	BaselineWindow     string  `yaml:"baseline_window"`                                                           // "168h" (7d)
	RatioThreshold     float64 `yaml:"ratio_threshold" doc:"Current/baseline cost ratio that triggers an alert."` // 2.0
	MinCurrentSpendUSD float64 `yaml:"min_current_spend_usd"`                                                     // 0.10
	MinBaselineOks     int64   `yaml:"min_baseline_oks"`                                                          // 5
	Cooldown           string  `yaml:"cooldown"`                                                                  // "12h"
}

// WatchdogConfig holds the stuck-execution watchdog tunables.
// Zero-valued fields fall back to compiled defaults inside the
// watchdog package (60s interval, 30m stuck threshold, warn action).
type WatchdogConfig struct {
	// Enabled gates the periodic scan. Default true. Switching to
	// false ships the watchdog dark.
	//
	// Pointer-bool so a config.yaml that omits the entire `watchdog:`
	// block — or omits just the `enabled:` key — preserves the
	// compiled DefaultConfig() value (true) rather than letting Go's
	// zero value (false) silently disable the safety net. An operator
	// who *explicitly* writes `enabled: false` still wins; only the
	// absent case is rescued. See the 2026-05-18 stuck-execution
	// incident where a YAML without a watchdog block ran with the
	// detector off for two hours.
	Enabled *bool `yaml:"enabled,omitempty" doc:"Periodic scan for stuck executions."`
	// Interval is the gap between scans, as a Go duration string
	// (e.g. "60s", "2m"). Empty falls to default.
	Interval string `yaml:"interval" doc:"Gap between scans."`
	// StuckThreshold is how long an execution can have its
	// updated_at idle before the watchdog flags it. Empty falls to
	// default.
	StuckThreshold string `yaml:"stuck_threshold" doc:"Idle time before an execution is flagged."`
	// Action is "warn" or "fail". "warn" only logs + counter;
	// "fail" additionally marks both the execution and task rows
	// terminal with class STUCK_EXECUTION. Default "fail" since
	// 2026-05-13 — the prior warn-only default left ghost RUNNING
	// rows in place after the scheduler had moved to a retry,
	// confusing operators (cancelled live tasks thinking two were
	// running in parallel). Operators with legitimately long steps
	// can revert to "warn" in the project YAML.
	Action string `yaml:"action" doc:"warn (log only) or fail (mark the task terminal)."`
}

// AutonomyConfig holds the daemon-wide defaults for the autonomous task manager.
type AutonomyConfig struct {
	// DefaultEvaluateTimeout bounds a single evaluation tick's LLM + DB work
	// when the project doesn't specify its own autonomy.evaluate_timeout.
	// Go duration string (e.g. "5m", "15m"). Default "5m".
	DefaultEvaluateTimeout string `yaml:"default_evaluate_timeout" doc:"Bound on one evaluation tick when a project does not set its own."`

	// CircuitBreaker auto-pauses autonomy on a project when the
	// project starts failing consistently. Catches stuck loops
	// before they burn the daily budget — a project autonomously
	// scheduling tasks every hour can rack up dozens of dollars
	// before an operator notices the failure pattern.
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`

	// ApprovalTimeoutHours cancels tasks parked in AWAITING_APPROVAL longer
	// than this many hours, so the operator-action inbox can't accumulate
	// stale approvals forever. The watchdog runs the sweep on its scan loop.
	// 0 disables (approvals never expire). Default 96 (4 days).
	ApprovalTimeoutHours int `yaml:"approval_timeout_hours" doc:"Cancel tasks awaiting approval after this many hours (0 = never; default 96)."`
}

// CircuitBreakerConfig governs the per-project failure-rate breaker
// applied in the executor's terminal-failure path. When a project
// accumulates `Threshold` task failures of a class in `Window`
// rolling time, autonomy.enabled flips to false on that project and
// the operator gets a Telegram alert.
type CircuitBreakerConfig struct {
	// Enabled switches the breaker on. Default false so existing
	// deployments don't get a behavior change on upgrade.
	Enabled bool `yaml:"enabled" doc:"Auto-pause autonomy on a project that fails repeatedly."`
	// Threshold is the failure count that trips the breaker. Default
	// 5 — chosen so a transient outage burst doesn't pause a
	// project but a sustained problem does within a tick or two.
	Threshold int `yaml:"threshold" doc:"Failure count that trips the breaker."`
	// Window is the rolling time window the count is measured over.
	// Go duration string. Default "2h".
	Window string `yaml:"window" doc:"Rolling window the count is measured over."`
	// SkipClasses lists failure classes that don't count toward the
	// breaker. Defaults to ["CANCELLED","BUDGET_BLOCKED","RATE_LIMITED"]:
	// CANCELLED is operator-initiated, BUDGET_BLOCKED is operator
	// policy fired correctly, RATE_LIMITED is upstream-transient and
	// already retried. Tripping the breaker on these would just
	// punish operators for healthy enforcement.
	SkipClasses []string `yaml:"skip_classes"`
}

// RetentionConfig holds the daemon-wide defaults for the retention sweeper.
// Per-project values (on each project YAML) override these when non-zero.
// Zero here means "use the compiled-in default" — see internal/retention.
type RetentionConfig struct {
	// Enabled switches on the background sweeper. Off by default so
	// existing deployments don't delete anything on upgrade; an operator
	// has to opt in explicitly.
	Enabled bool `yaml:"enabled" doc:"Turn the background sweeper on."`
	// Interval is how often the sweeper runs (e.g. "1h", "6h"). Default "6h".
	Interval string `yaml:"interval" doc:"How often the sweeper runs."`
	// Default retention windows in days. Zero → compiled default.
	TaskLLMUsageDays int `yaml:"task_llm_usage_days" doc:"Days to keep per-task LLM usage rows."`
	ToolAuditDays    int `yaml:"tool_audit_days"`
	TasksDays        int `yaml:"tasks_days" doc:"Days to keep task rows."`
	ExecutionsDays   int `yaml:"executions_days" doc:"Days to keep execution rows."`
	ArtifactsDays    int `yaml:"artifacts_days" doc:"Days to keep artifacts."`
	// TaskMessagesDays prunes rows from task_messages by created_at.
	// Tasks cascade-delete their messages via FK, so this field only
	// has an effect when operators want messages pruned *earlier*
	// than their parent task. Default 0 → cascade-only (no
	// independent prune).
	TaskMessagesDays int `yaml:"task_messages_days"`
	// MemoryChunksDays is the operator-level escape hatch for the
	// project_memory_chunks table. The class taxonomy's per-class
	// TTL handles ordinary retention; this field caps every chunk
	// regardless of class. Default 0 → forever; class TTL only.
	// SaaS-readiness design § 3.13.
	MemoryChunksDays int `yaml:"memory_chunks_days"`
	// MemoryIngestAuditDays bounds the memory_ingest_audit table (both
	// companion + agent deposit trails). Always-on; zero → compiled
	// default of 90 days. Without a sweep the table grows unbounded.
	// Mitigation plan §7.3.
	MemoryIngestAuditDays int `yaml:"memory_ingest_audit_days"`
	// MemoryPolicyEvalAllowDays / MemoryPolicyEvalBlockDays bound the
	// memory_policy_evaluations table (Policy-Aware Memory Firewall
	// audit trail). Always-on; zero → compiled defaults (allow 30d,
	// block 365d). Allow rows are dense (one per chunk per recall) and
	// swept aggressively; block rows are the compliance trail and live
	// a year. Without a sweep the table grows unbounded
	// (drift-mitigation §8.3 / firewall LLD § Retention).
	MemoryPolicyEvalAllowDays int `yaml:"memory_policy_eval_allow_days"`
	MemoryPolicyEvalBlockDays int `yaml:"memory_policy_eval_block_days"`
	// ResponseCacheDays evicts rows from llm_response_cache (LLM
	// caching Phase E) whose last_hit_at is older than the window.
	// Global table — not scoped by project — so this sweep runs
	// once per cycle, before the per-project loop. Default 0 →
	// keep forever (which grows unbounded on busy deployments —
	// 30d is the recommended setting).
	ResponseCacheDays int `yaml:"response_cache_days" doc:"Days to keep cached LLM responses (30d recommended on busy deployments)."`
}

// ServerConfig holds HTTP server configuration.
type ServerConfig struct {
	Address      string `yaml:"address" doc:"TCP listen address for the HTTP API."`
	ReadTimeout  string `yaml:"read_timeout" doc:"Max time to read a request."`
	WriteTimeout string `yaml:"write_timeout" doc:"Max time to write a response."`
	// UnixSocket, when set, makes the daemon ALSO serve the HTTP API on
	// this unix socket path (alongside the TCP Address). Required for
	// NetworkDaemonOnly agent roles: the daemon socket is bind-mounted
	// into those containers (which run --network=none) so they reach the
	// daemon for MCP + LLM with zero internet egress. Empty = TCP only.
	UnixSocket string `yaml:"unix_socket" doc:"Optional Unix socket path the API is also served on, alongside the TCP address. Required for zero-egress agents."`

	// RealIP configures trusted-proxy client-IP resolution. Required when
	// the daemon sits behind a Cloudflare Zero Trust tunnel (cloudflared on
	// a separate host): without it every caller collapses to the
	// cloudflared IP, and the leftmost-X-Forwarded-For hop a client can
	// append is attacker-controlled (forge/rotate to trip or evade the
	// per-IP lockout). Fail-safe: disabled / empty trust list ⇒ all
	// consumers key on RemoteAddr, so enabling the tunnel REQUIRES
	// configuring this block.
	// see LLD § https://docs.vornik.io
	RealIP RealIPConfig `yaml:"real_ip" json:"real_ip"`
	// PublicBaseURL is the externally-reachable origin (scheme://host[:port])
	// used to render git clone/push HTTPS URLs on the project page.
	// The clone URL is derived at runtime — it is never stored in the DB.
	// Example: https://vornik.example.com
	// Required only when any project enables git.enabled. Empty = no clone URL shown.
	PublicBaseURL string `yaml:"public_base_url" doc:"Public origin (scheme://host[:port]) for git clone/push HTTPS URLs; used only to render the clone URL on the project page."`
}

// RealIPConfig is the trusted-proxy real-client-IP policy consumed by the
// internal/httpx/realip middleware that wraps both the API and UI handler
// chains. The middleware reads Header (only) from sources whose IP is in
// TrustedProxies and stores the result in the request context; all six
// historical client-IP derivations now read that one vetted value.
type RealIPConfig struct {
	// Enabled gates header honouring. When false the resolver always keys
	// on RemoteAddr — the fail-safe default.
	Enabled bool `yaml:"enabled" json:"enabled" doc:"Honour the trusted client-IP header from trusted_proxies. Off ⇒ always key on RemoteAddr."`
	// TrustedProxies lists the CIDR blocks / explicit IPs allowed to set
	// the trusted header. For a Cloudflare tunnel this is ONLY the
	// cloudflared host (e.g. ["10.0.0.5/32"]); never a broad range.
	TrustedProxies []string `yaml:"trusted_proxies" json:"trusted_proxies" doc:"CIDRs/IPs trusted to set the real-IP header. List ONLY the cloudflared host."`
	// Header is the trusted client-IP header name. Defaults to
	// CF-Connecting-IP — Cloudflare-set, single-value, NOT client-appendable
	// (unlike X-Forwarded-For).
	Header string `yaml:"header" json:"header" doc:"Trusted client-IP header. Default CF-Connecting-IP."`
}

// DatabaseConfig holds database connection configuration. Driver
// selects the backend; the remaining fields are interpreted per-driver
// (the postgres driver uses Host/Port/User/Password/SSLMode; the
// upcoming sqlite driver will read Path instead).
type DatabaseConfig struct {
	Driver   string `yaml:"driver"`                                                                             // Backend driver: "postgres" (default) or "sqlite"
	Host     string `yaml:"host" doc:"PostgreSQL host."`                                                        // PostgreSQL host
	Port     int    `yaml:"port" doc:"PostgreSQL port."`                                                        // PostgreSQL port
	Name     string `yaml:"name" doc:"Database name."`                                                          // PostgreSQL database name
	User     string `yaml:"user" doc:"Database user."`                                                          // PostgreSQL user
	Password string `yaml:"password" doc:"Password. Prefer the VORNIK_DATABASE_PASSWORD environment variable."` // PostgreSQL password
	SSLMode  string `yaml:"sslmode" doc:"One of disable, require, verify-ca, verify-full."`                     // PostgreSQL SSL mode (disable, require, verify-ca, verify-full)
	Path     string `yaml:"path"`                                                                               // SQLite database file path (driver="sqlite")
}

// StorageConfig holds artifact storage configuration.
//
// Backend selects the artifact backend:
//   - "" / "filesystem" / "local" — write blobs under ArtifactsPath
//     on the local filesystem (default; behaviour unchanged from
//     pre-phase-4 releases)
//   - "s3" — write blobs to an S3-compatible object store, configured
//     under S3. MinIO and Ceph RGW both work; only the standard
//     S3 v2/v4 protocol is required.
//
// The S3 block is ignored when Backend is not "s3"; this lets operators
// pre-populate credentials and flip the switch without re-editing the
// rest of the config.
type StorageConfig struct {
	ArtifactsPath string          `yaml:"artifacts_path" json:"artifacts_path" doc:"Local directory for artifacts when using the filesystem backend."`
	Backend       string          `yaml:"backend" json:"backend" doc:"filesystem (local disk) or s3 (S3-compatible object store, including MinIO)."`
	S3            S3StorageConfig `yaml:"s3" json:"s3"`
}

// S3StorageConfig holds S3 / S3-compatible (MinIO, Ceph RGW) settings
// for the artifact backend. AccessKeyID / SecretAccessKey may be left
// empty in YAML and supplied via VORNIK_STORAGE_S3_ACCESS_KEY_ID /
// VORNIK_STORAGE_S3_SECRET_ACCESS_KEY env vars (preferred for production
// — same pattern as VORNIK_DATABASE_PASSWORD).
//
// UsePathStyle defaults to false (virtual-host addressing); flip to
// true for MinIO localhost or older S3 deployments. ForceSSL defaults
// to true and should only be disabled for MinIO localhost development.
type S3StorageConfig struct {
	// Endpoint is the S3 endpoint URL. Leave empty for AWS S3 with the
	// SDK's default resolver. Set explicitly for MinIO (e.g.
	// "http://localhost:9000") or non-AWS S3.
	Endpoint string `yaml:"endpoint" json:"endpoint" doc:"S3 endpoint URL. Leave empty for AWS S3; set for MinIO/non-AWS."`
	// Region is the AWS region (e.g. "us-east-1"). Required even for
	// MinIO — the SDK refuses to sign without one. MinIO accepts any
	// non-empty value.
	Region string `yaml:"region" json:"region" doc:"AWS region. Required when backend is s3."`
	// Bucket is the S3 bucket that holds all vornik artifacts. Must
	// exist before the daemon starts; vornik does not auto-create
	// buckets (operator concern: lifecycle policies, versioning,
	// object-lock retention).
	Bucket string `yaml:"bucket" json:"bucket" doc:"Bucket holding artifacts. Required when backend is s3; must already exist."`
	// Prefix optionally namespaces keys (e.g. "vornik/prod") so one
	// bucket can host multiple deployments. Leading/trailing slashes
	// are normalised by the backend; empty means keys live at the
	// bucket root.
	Prefix string `yaml:"prefix" json:"prefix" doc:"Optional key prefix to namespace one bucket across deployments."`
	// AccessKeyID overrides the SDK's default credential chain. Leave
	// empty to let the SDK discover credentials (IAM role, ~/.aws,
	// AWS_ACCESS_KEY_ID). For MinIO you usually set both explicitly.
	AccessKeyID string `yaml:"access_key_id" json:"access_key_id" doc:"Access key. Prefer VORNIK_STORAGE_S3_ACCESS_KEY_ID."`
	// SecretAccessKey pairs with AccessKeyID.
	SecretAccessKey string `yaml:"secret_access_key" json:"-" doc:"Secret key. Prefer VORNIK_STORAGE_S3_SECRET_ACCESS_KEY."` // never serialise back
	// UsePathStyle forces path-style addressing (http://host/bucket/key
	// instead of http://bucket.host/key). Required for MinIO; harmless
	// for AWS S3 in regions that support it. Default false.
	UsePathStyle bool `yaml:"use_path_style" json:"use_path_style" doc:"Force path-style addressing. Set true for MinIO."`
	// ForceSSL requires the endpoint to use HTTPS. Default true.
	// Set false only for MinIO localhost dev (the operator-docs stub
	// explains the trade-off).
	ForceSSL *bool `yaml:"force_ssl" json:"force_ssl" doc:"Require HTTPS. Set false only for local MinIO development."`
}

// UnmarshalYAML accepts both the current snake_case key and the guide's
// camelCase key for backward compatibility.
func (s *StorageConfig) UnmarshalYAML(value *yaml.Node) error {
	type storageAlias struct {
		ArtifactsPath  string          `yaml:"artifacts_path" json:"artifacts_path"`
		StoragePath    string          `yaml:"storage_path"`
		StoragePathAlt string          `yaml:"storagePath"`
		Backend        string          `yaml:"backend" json:"backend"`
		S3             S3StorageConfig `yaml:"s3" json:"s3"`
	}

	var raw storageAlias
	if err := value.Decode(&raw); err != nil {
		return err
	}

	switch {
	case raw.ArtifactsPath != "":
		s.ArtifactsPath = raw.ArtifactsPath
	case raw.StoragePath != "":
		s.ArtifactsPath = raw.StoragePath
	default:
		s.ArtifactsPath = raw.StoragePathAlt
	}

	s.Backend = raw.Backend
	s.S3 = raw.S3

	return nil
}

// ResolveForceSSL returns the effective ForceSSL setting, defaulting
// to true when unset.
func (s S3StorageConfig) ResolveForceSSL() bool {
	if s.ForceSSL == nil {
		return true
	}
	return *s.ForceSSL
}

// NormalizedBackend returns the canonical backend name ("filesystem"
// or "s3"). Empty / "local" / "filesystem" all normalize to
// "filesystem"; "s3" stays "s3". Anything else stays as-given so the
// factory can reject it loudly.
func (s StorageConfig) NormalizedBackend() string {
	switch s.Backend {
	case "", "local", "filesystem":
		return "filesystem"
	default:
		return s.Backend
	}
}

// Validate checks the storage block. Filesystem backend has no extra
// requirements; S3 backend requires Region + Bucket. Credentials are
// optional in YAML — the SDK's default chain (env, ~/.aws, IAM role)
// fills the gap, with env-var overrides honored in the loader.
func (s StorageConfig) Validate() error {
	switch s.NormalizedBackend() {
	case "filesystem":
		return nil
	case "s3":
		if s.S3.Bucket == "" {
			return fmt.Errorf("storage.s3.bucket is required when backend=s3")
		}
		if s.S3.Region == "" {
			return fmt.Errorf("storage.s3.region is required when backend=s3")
		}
		return nil
	default:
		return fmt.Errorf("storage.backend %q is not supported (want \"filesystem\" or \"s3\")", s.Backend)
	}
}

// SchedulerConfig holds scheduler configuration.
type SchedulerConfig struct {
	MaxConcurrentTasks int    `yaml:"max_concurrent_tasks" doc:"Maximum tasks running at once across all projects."`
	LeaseTimeout       string `yaml:"lease_timeout" doc:"How long a task lease is held before it is considered stale."`
}

// RuntimeConfig holds container runtime configuration.
type RuntimeConfig struct {
	// UserNSMode controls the Podman user namespace mode for agent containers.
	// Empty uses the Podman default.
	UserNSMode string `yaml:"userns_mode"`

	// AllowHostUserns controls whether vornik is allowed to fall back to --userns=host
	// if rootless podman namespace setup fails. Allowing this reduces isolation.
	AllowHostUserns bool `yaml:"allow_host_userns"`

	// RunAsUser overrides the container's USER directive, passed to podman as
	// --user. Accepts "uid", "uid:gid" or "user:group" forms. Leave empty to
	// trust the image's own USER. Set to something like "1000:1000" to force
	// non-root execution on images that still default to root.
	RunAsUser string `yaml:"run_as_user" doc:"Override the container user (uid, uid:gid, or user:group). Use to force non-root on root-default images."`

	// DefaultNetwork is the network policy applied to agent roles that
	// don't set runtime.network explicitly. Empty = historical permissive
	// default (rootless slirp4netns egress). "daemon-only" makes
	// zero-egress the default for all roles (the daemon socket is
	// bind-mounted in; requires server.unix_socket), with network:host as
	// the per-role opt-out for roles that need real egress (e.g. build
	// roles that fetch dependencies). New installations ship "daemon-only".
	// Values: "" | host | none | daemon-only.
	DefaultNetwork string `yaml:"default_network" doc:"Default network policy for agent roles: host, none, or daemon-only (zero egress; requires server.unix_socket)."`

	// AgentLLM configures the LLM endpoint injected into agent containers
	// as environment variables. When empty, values fall back to the chat.*
	// section so that agents use the same LLM as the Telegram bot.
	AgentLLM AgentLLMConfig `yaml:"agent_llm"`

	// WarmPool configures warm container reuse for roles with runtimePolicy=warm.
	WarmPool WarmPoolConfig `yaml:"warm_pool"`

	// ProjectWorkspacePath is the base dir for per-project persistent workspaces.
	// Each project gets {path}/{projectID}/ mounted at /app/workspace/project/ in containers.
	// Default: derived from VORNIK_DATA_DIR or /var/lib/vornik/workspaces.
	ProjectWorkspacePath string `yaml:"project_workspace_path" doc:"Base directory for per-project persistent workspaces."`

	// DelegationDepthLimit caps how many levels deep a delegation chain may
	// run before the engine refuses further delegation. 0 uses the executor
	// default (5). See https://docs.vornik.io §3.
	DelegationDepthLimit int `yaml:"delegation_depth_limit" doc:"Max nesting depth for delegation chains (0 = default)."`

	// DelegationFanOutLimit caps how many child tasks a single parent may
	// create in one delegation batch. 0 uses the executor default (20).
	// See https://docs.vornik.io §3.
	DelegationFanOutLimit int `yaml:"delegation_fanout_limit" doc:"Max child tasks one parent may spawn in a batch (0 = default)."`
}

// AgentLLMConfig holds the LLM connection details passed to agent containers.
type AgentLLMConfig struct {
	Endpoint    string `yaml:"endpoint" doc:"OpenAI-compatible base URL for the LLM agents call. Falls back to the chat section when empty."` // OpenAI-compatible base URL
	APIKey      string `yaml:"api_key" doc:"API key for the agent LLM endpoint."`
	Model       string `yaml:"model" doc:"Default model for agents."`
	ContextSize int    `yaml:"context_size" doc:"Context-window tokens."`    // context window tokens (Ollama num_ctx); 0 = provider default
	MaxTokens   int    `yaml:"max_tokens" doc:"Max output tokens per call."` // max output tokens per call; 0 = omit (use provider default)
	// Timeout bounds a single LLM HTTP call from inside an agent container.
	// Go duration string (e.g. "300s", "5m"). Empty inherits chat.timeout.
	// Prevents a stalled gateway connection from pinning a task for the
	// default 30-minute curl timeout.
	Timeout string `yaml:"timeout" doc:"Bound on a single LLM call from inside an agent."`
	// ModelLimits sets per-model max_tokens and context_size overrides.
	// Keyed by model name; applied automatically when a role uses that model.
	// Example: {"claude-haiku-4-5": {MaxTokens: 4096}, "claude-opus-4-6": {MaxTokens: 32768}}
	ModelLimits map[string]ModelLimitConfig `yaml:"model_limits"`
}

// ModelLimitConfig holds output and context limits for a specific model.
// Both fields are optional; zero means inherit the global AgentLLMConfig value.
type ModelLimitConfig struct {
	MaxTokens   int `yaml:"max_tokens"`
	ContextSize int `yaml:"context_size"`
}

// WarmPoolConfig configures the warm container pool.
type WarmPoolConfig struct {
	// Enabled turns on warm container reuse.
	Enabled bool `yaml:"enabled" doc:"Reuse warm containers for faster task start."`
	// IdleTimeout is how long an idle container stays in the pool (default "10m").
	IdleTimeout string `yaml:"idle_timeout" doc:"How long an idle warm container is kept."`
	// MaxPerRole is the maximum warm containers per project+role (default 2).
	MaxPerRole int `yaml:"max_per_role" doc:"Max warm containers per project and role."`
}

// MetricsConfig holds Prometheus metrics server configuration.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled" doc:"Expose a Prometheus metrics endpoint."`
	Addr    string `yaml:"addr" doc:"Listen address for the dedicated metrics server."`
	// RequireAdmin gates the MAIN API port's /metrics endpoint behind
	// the admin key when api.auth_enabled is true (audit A5). The
	// Prometheus labels carry project / api-key IDs, so an open
	// /metrics on the LAN-reachable API port lets an unauthenticated
	// scraper enumerate tenant structure. With this set, point the
	// scraper at the dedicated loopback metrics listener (addr) — or
	// give it the scrape token / admin bearer. Default false preserves
	// the open scrape for single-tenant / trusted-network deployments.
	RequireAdmin bool `yaml:"require_admin" doc:"Require the admin key on the main port metrics endpoint."`
	// ScrapeToken is a dedicated READ-ONLY credential the gated
	// /metrics accepts in addition to the admin key (audit A5). Lets a
	// (possibly containerized) Prometheus authenticate WITHOUT being
	// handed the full admin key — a compromised scraper then can't act
	// as admin. Empty disables the token path (admin key only).
	ScrapeToken string `yaml:"scrape_token"`
}

// TracingConfig holds OpenTelemetry tracing configuration.
type TracingConfig struct {
	Enabled  bool   `yaml:"enabled" doc:"Enable OpenTelemetry tracing."`
	Endpoint string `yaml:"endpoint" doc:"OTLP gRPC endpoint for trace export."`
}

// LoggingConfig holds logging configuration.
type LoggingConfig struct {
	Level  string `yaml:"level" doc:"Log level (e.g. info, debug)."`
	Format string `yaml:"format" doc:"Log format (e.g. json, text)."`
	// Forward configures centralised log forwarding (log shipping) to
	// remote sinks. Disabled by default — when off the daemon's logging
	// is local-only with zero overhead. See
	// https://docs.vornik.io
	Forward LogForwardConfig `yaml:"forward"`
}

// LogForwardConfig is the per-instance log-forwarding block (LLD §4). A
// scope-filtered fan-out of application + audit logs to an HTTP webhook
// and/or syslog sink. Best-effort async; never blocks or crashes the
// daemon.
type LogForwardConfig struct {
	// Enabled turns forwarding on. When false every other field is
	// ignored and the root logger is left untouched.
	Enabled bool `yaml:"enabled" doc:"Enable centralised log forwarding to remote sinks."`
	// Scopes is the allowlist of log categories to ship (e.g. http, llm,
	// memory, executor, audit.admin, audit.tool). Empty while enabled =
	// ship all scopes.
	Scopes []string `yaml:"scopes" doc:"Allowlist of log categories to forward. Empty ships all scopes."`
	// QueueSize bounds the in-memory event queue; overflow is dropped +
	// counted (best-effort). 0 uses the built-in default (10000).
	QueueSize int `yaml:"queue_size" doc:"Bounded forward queue size; overflow is dropped + counted."`
	// BatchSize is the max events shipped per batch. 0 defaults to 100.
	BatchSize int `yaml:"batch_size" doc:"Max events per shipped batch."`
	// FlushInterval forces a partial batch to ship after this long.
	// Empty defaults to 5s.
	FlushInterval string `yaml:"flush_interval" doc:"Max time a partial batch waits before shipping (e.g. 5s)."`
	// HTTP is the bearer-token NDJSON webhook sink.
	HTTP LogForwardHTTPConfig `yaml:"http"`
	// Syslog is the RFC 5424 syslog sink.
	Syslog LogForwardSyslogConfig `yaml:"syslog"`
}

// LogForwardHTTPConfig configures the HTTP webhook sink. The bearer token
// is referenced by env-var NAME (bearer_token_env), resolved at boot — never
// inline in YAML (matches the email/slack *_env secret pattern).
type LogForwardHTTPConfig struct {
	Enabled bool `yaml:"enabled" doc:"Enable the HTTP webhook log sink."`
	// URL is the collector ingest endpoint (POST NDJSON).
	URL string `yaml:"url" doc:"Collector ingest URL the HTTP sink POSTs NDJSON to."`
	// BearerTokenEnv names the environment variable holding the bearer
	// token. Required (and must resolve) when the HTTP sink is enabled.
	BearerTokenEnv string `yaml:"bearer_token_env" doc:"Env-var NAME holding the HTTP sink bearer token (never the token itself)."`
	// Timeout is the per-request HTTP timeout. Empty defaults to 5s.
	Timeout string `yaml:"timeout" doc:"Per-request HTTP sink timeout (e.g. 5s)."`
	// MaxRetries bounds the retry count on 5xx / transport error.
	MaxRetries int `yaml:"max_retries" doc:"Max HTTP sink retries on 5xx/transport error."`
}

// LogForwardSyslogConfig configures the syslog sink (RFC 5424).
type LogForwardSyslogConfig struct {
	Enabled bool `yaml:"enabled" doc:"Enable the syslog log sink."`
	// Address is host:port of the syslog collector.
	Address string `yaml:"address" doc:"host:port of the syslog collector."`
	// Protocol selects the transport: udp | tcp | tls.
	Protocol string `yaml:"protocol" doc:"Syslog transport: udp | tcp | tls."`
	// CAFile is an optional PEM CA bundle for the tls protocol (system
	// roots otherwise).
	CAFile string `yaml:"ca_file" doc:"Optional PEM CA bundle for syslog tls (system roots otherwise)."`
}

// APIConfig holds API configuration.
type APIConfig struct {
	AuthEnabled bool `yaml:"auth_enabled" json:"auth_enabled" doc:"Require a valid API key on every request. Turn this on for any network-reachable deployment."`
	// AuthDryRun makes AuthMiddleware EVALUATE the would-be auth
	// verdict (log + metric per would-be denial) while admitting
	// every request exactly as auth_enabled=false does today.
	// Evaluation mode only: setting it together with
	// auth_enabled: true is a config error. Used to soak-test the
	// auth flip (2026-05-28 rollback context) before enforcing.
	AuthDryRun bool     `yaml:"auth_dry_run" json:"auth_dry_run" doc:"Evaluate and log auth verdicts without enforcing. Cannot be set together with auth_enabled: true."`
	APIKeys    []string `yaml:"api_keys" json:"api_keys" doc:"Static bearer keys. Prefer per-project DB-backed keys via vornikctl key."`
	// ExternalAPIBillingProjectID, when set, attributes every cost
	// row from the third-party OpenAI- + Ollama-compatible proxy
	// surfaces (/v1/chat/completions, /api/chat, /api/generate) to
	// this project unless the caller supplies a
	// X-Vornik-Project-ID header. Empty means rows land under the
	// literal "_external" — visible on the spend dashboard but not
	// rolled into any real project's panel. SaaS deployments
	// typically pin this to a dedicated billing project.
	ExternalAPIBillingProjectID string `yaml:"external_api_billing_project_id" json:"external_api_billing_project_id"`

	// SingleTenantOperatorID is the operator identity stamped on
	// auth-off requests that need a non-empty operator (wizard
	// sessions, UI drafts banner). Empty falls back to the
	// daemon-wide default `local:dev`. Auth-enabled deployments
	// ignore this — the verified API-key principal wins.
	SingleTenantOperatorID string `yaml:"single_tenant_operator_id" json:"single_tenant_operator_id"`

	// RateLimit holds the data-plane rate-limit primitives. Each
	// nested block is optional; a zero block keeps the daemon's
	// pre-2026.6.0 behaviour (no enforcement of that dimension).
	// See internal/ratelimit for the token-bucket primitives.
	RateLimit APIRateLimitConfig `yaml:"rate_limit" json:"rate_limit"`
}

// APIRateLimitConfig holds the data-plane rate-limit configuration
// blocks the AuthMiddleware consumes. Each nested struct is one
// layer in the defence-in-depth stack: PerIP is the unauthenticated
// backstop; per-API-key limits live on the api_keys table rows.
type APIRateLimitConfig struct {
	// PerIP is the unauthenticated backstop applied BEFORE auth.
	// Disabled when RPS or Burst is zero (the legacy default).
	// Honours X-Forwarded-For when the caller's RemoteAddr appears
	// in TrustedProxies; otherwise falls back to RemoteAddr's IP.
	PerIP PerIPRateLimit `yaml:"per_ip" json:"per_ip"`

	// Backend selects the per-project rate-limiter storage —
	// "memory" (default, legacy single-daemon) or "postgres"
	// (multi-daemon SaaS; counters live in the ratelimit_counters
	// table created by migration 42). Unknown values fall back to
	// "memory" with a startup warning. Per-API-key + per-IP +
	// per-route limiters remain in-process — those primitives are
	// SaaS-acceptable at single-daemon scale and the storage
	// abstraction adds latency to the auth-hot path.
	Backend string `yaml:"backend" json:"backend"`

	// CounterRetention is the lookback the per-table sweeper keeps
	// in ratelimit_counters. Empty / 0 defaults to 24h — covers
	// the trailing-hour cap by a comfortable margin and keeps the
	// table size bounded under sustained traffic. Only consulted
	// when Backend == "postgres".
	CounterRetention string `yaml:"counter_retention" json:"counter_retention"`
}

// PerIPRateLimit is the per-client-IP backstop token-bucket. Sits
// in front of AuthMiddleware so an unauthenticated flood can't
// even reach the auth path. RPS == 0 OR Burst == 0 disables.
type PerIPRateLimit struct {
	// RPS is the sustained per-IP refill rate. Sensible defaults
	// (when both fields are non-zero in the daemon config) are 20
	// rps + 40 burst — high enough that one operator with a
	// reload-script doesn't trip it, low enough that a runaway
	// loop stops after ~2 s.
	RPS int `yaml:"rps" json:"rps" doc:"Per-IP sustained request rate (unauthenticated backstop)."`
	// Burst is the bucket capacity (max instantaneous concurrent
	// requests). When zero, the limiter never allocates a bucket
	// — the layer is effectively off.
	Burst int `yaml:"burst" json:"burst" doc:"Per-IP burst capacity."`
	// TrustedProxies lists the CIDR blocks / explicit IPs the
	// daemon trusts to set X-Forwarded-For.
	//
	// Deprecated: superseded by server.real_ip.trusted_proxies, which
	// drives ALL client-IP consumers (not just the per-IP limiter) and
	// honours the spoof-safe CF-Connecting-IP header instead of
	// leftmost-X-Forwarded-For. When server.real_ip.trusted_proxies is
	// empty but this field is set, the daemon falls back to this list and
	// logs a one-time startup WARNING. Migrate to server.real_ip.
	TrustedProxies []string `yaml:"trusted_proxies" json:"trusted_proxies" doc:"Deprecated: use server.real_ip.trusted_proxies. CIDRs/IPs trusted to set X-Forwarded-For."`
}

// TradingConfig holds the daemon-side trading-channel configuration.
type TradingConfig struct {
	// Auth gates HMAC request-authentication on the
	// /api/v1/internal/trading-* endpoints.
	Auth TradingAuthConfig `yaml:"auth" json:"auth"`
	// CrossCheck configures the periodic broker-vs-vornik equity
	// cross-checker (the source-of-truth-agreement half of the trading
	// time-series validation — the DB-only checks ship as the
	// trading-series feature-doctor check).
	CrossCheck TradingCrossCheckConfig `yaml:"cross_check" json:"cross_check"`
}

// TradingCrossCheckConfig configures the periodic equity cross-checker:
// a leader-gated ticker that fetches each trading project's LIVE broker
// equity (/caps) and compares it to vornik's most recent persisted
// snapshot, emitting vornik_trading_equity_drift_usd +
// vornik_trading_equity_crosscheck_anomalies. Observational only — it
// places no orders and changes no trading behaviour. Opt-in (default off)
// because it adds a live broker fetch to the daemon's background load.
// See trading-report-perf-first-and-series-validation-design.md.
type TradingCrossCheckConfig struct {
	// Enabled turns the cross-checker on. Default false.
	Enabled bool `yaml:"enabled" json:"enabled" doc:"Run the periodic broker-vs-vornik equity cross-checker."`
	// Interval is the tick cadence (e.g. "15m"). Empty / 0 defaults to
	// 15m — coarser than the 5m sampler since this is a health check,
	// not a data collector.
	Interval string `yaml:"interval" json:"interval" doc:"Equity cross-check cadence (default 15m)."`
	// TolerancePct flags drift when |live-recorded|/live*100 exceeds it.
	// Empty / 0 defaults to 1.0.
	TolerancePct float64 `yaml:"tolerance_pct" json:"tolerance_pct" doc:"Equity drift tolerance in percent (default 1.0)."`
	// MaxSnapshotAge flags a stalled sampler when the newest snapshot is
	// older than this (e.g. "15m"). Empty / 0 defaults to 15m.
	MaxSnapshotAge string `yaml:"max_snapshot_age" json:"max_snapshot_age" doc:"Snapshot staleness bound (default 15m)."`
}

// TradingAuthConfig configures HMAC request authentication for the
// broker→daemon trading channel. The same shared secret is configured
// on the daemon (verify) and the broker (sign,
// VORNIK_BROKER_TRADING_HMAC_SECRET). See
// https://docs.vornik.io
//
// Rollout is two-phase to avoid a flag-day: deploy the secret to BOTH
// sides with Enabled=false first (the daemon ignores any signature,
// the broker stamps one anyway), confirm signed traffic in logs, then
// flip Enabled=true to start rejecting unsigned/invalid requests.
type TradingAuthConfig struct {
	// Enabled turns on fail-closed HMAC verification. When false the
	// endpoints behave exactly as before (bearer auth only) —
	// backward-compatible. Default false.
	Enabled bool `yaml:"enabled" json:"enabled" doc:"Require a valid HMAC signature on the broker-to-daemon trading-channel endpoints."`
	// Secret is the shared HMAC key. Prefer SecretFile (keeps the
	// plaintext out of YAML); ${VAR} expansion also works. Required
	// when Enabled is true.
	Secret string `yaml:"secret" json:"secret" doc:"Shared HMAC secret for trading-channel auth (prefer secret_file)."`
	// SecretFile is a path read at load time into Secret (whitespace
	// trimmed), then cleared — mirrors auth.providers.github
	// .client_secret_file. Preferred over inline Secret.
	SecretFile string `yaml:"secret_file" json:"secret_file" doc:"Path to the trading HMAC secret (preferred over inline secret)."`
	// ClockSkew is the timestamp freshness window (e.g. "5m"). A
	// request whose signing timestamp is more than this far from the
	// daemon's clock, in either direction, is rejected. Empty / 0
	// defaults to 5m.
	ClockSkew string `yaml:"clock_skew" json:"clock_skew" doc:"Trading-auth timestamp freshness window (default 5m)."`
}

// ChatConfig holds chat client configuration. Supported providers:
//   - "http" (default): OpenAI-compatible HTTP endpoint. Requires
//     endpoint, api_key, and model.
//   - "claude-cli": shells out to the `claude` CLI (Claude Code),
//     reusing whatever auth it's already configured with. endpoint and
//     api_key are ignored for this provider; model is passed through
//     to `--model` and may be left empty to let Claude Code pick.
//   - "codex-cli": shells out to the `codex exec` CLI (OpenAI Codex),
//     reusing its existing auth. Under ChatGPT-account auth, leave
//     model empty — the CLI rejects per-request model selection in
//     that mode. Under OpenAI API-key auth, model is honored.
//   - "codex-subscription": available only as a router sub-provider;
//     talks directly to the ChatGPT-subscription Codex Responses API
//     using auth from `codex login`.
//   - "router": composes multiple providers and dispatches each
//     request to the one whose prefix matches the requested model.
//     See ChatRouterConfig below.

// ChatCompactionConfig governs read-path conversation compaction. When
// Enabled, AddMessage stops token-trimming (the message-count cap still
// bounds storage) and the send path emits a deterministic topic gist of the
// overflow turns instead of dropping them. Absent block → disabled → legacy
// truncation, byte-for-byte.
type ChatCompactionConfig struct {
	Enabled      bool `yaml:"enabled" doc:"Summarize overflow turns into a topic gist instead of dropping them."`
	MaxGistTerms int  `yaml:"max_gist_terms" doc:"Topics retained in the compaction gist (0 = default 24)."`
}

type ChatConfig struct {
	Enabled bool `yaml:"enabled" doc:"Turn the chat client on."`
	// Provider selects the backend. Unknown values fall back to HTTP
	// with a warning at startup (see service container initChat).
	Provider string `yaml:"provider" doc:"http (OpenAI-compatible) or router (multi-provider)."`
	// CLIBinary overrides the CLI binary path when
	// Provider is a *-cli backend. Empty resolves via PATH.
	// (For Provider=="router", each sub-provider in router.* has its
	// own binary/endpoint fields; this field is ignored.)
	CLIBinary string `yaml:"cli_binary"`
	// Router holds multi-provider config used when Provider=="router".
	Router   ChatRouterConfig `yaml:"router"`
	Endpoint string           `yaml:"endpoint" doc:"OpenAI-compatible base URL (single-provider mode)."`
	APIKey   string           `yaml:"api_key" doc:"API key for the endpoint. Prefer an environment variable."`
	// Model is the default model identifier used when the caller doesn't
	// pin one per request. Semantics depend on Provider:
	//
	//   - "http" / "claude-cli" / "codex-cli": Model is the only model
	//     the configured client will run; per-request overrides via the
	//     proxy's chat.ModelOverridable path swap it out per call.
	//   - "router": Model is promoted to the router's fallback
	//     sub-provider as its effective default — used for callers that
	//     hit Router.Complete (autonomy lead, telegram free-form chat,
	//     dispatcher) without a per-request model. Per-route requests
	//     still pin via WithModel(req.Model) and ignore this. Set
	//     router.<sub>.model when you need a different per-sub-provider
	//     default; leave it unset to inherit Model.
	Model string `yaml:"model" doc:"Default model identifier when a request does not pin one."`
	// WizardModel pins the model the conversational project-setup
	// wizard (internal/projectwizard) uses, independent of the
	// dispatcher/autonomy default in Model. Empty inherits Model (the
	// historical behaviour). Set this when the default chat model is a
	// poor fit for the wizard's per-turn load — it sends the full
	// transcript + template-gallery priors + the envelope JSON-schema
	// every turn, so a small-context model (or one that's rate-limited
	// / quota-capped on this deployment, e.g. a Vertex preview) hits
	// token/limit errors partway through and the wizard can't finish.
	// Resolved through the same router prefix-routing as any pinned
	// model, so a "google/" or "anthropic." prefix routes correctly.
	WizardModel string `yaml:"wizard_model" doc:"Model for the project-setup wizard."`
	Timeout     string `yaml:"timeout" doc:"Bound on a single LLM round-trip."`
	// DispatchTimeout caps one complete interactive turn (multi-LLM-call,
	// multi-tool-call) for the dispatcher. Semantically different from
	// Timeout, which limits a single LLM round-trip. A turn that calls
	// the LLM N times with tool calls in between needs at least N ×
	// Timeout plus tool-execution overhead, so binding the two 1:1
	// would cause every multi-step request to time out on call #2.
	// Empty / 0 defaults to max(Timeout × 3, 15m) so project-scanning
	// or multi-file-edit turns have room to run.
	DispatchTimeout  string `yaml:"dispatch_timeout" doc:"Bound on one complete interactive (multi-call) turn."`
	MaxHistory       int    `yaml:"max_history" doc:"Max conversation messages kept."`
	MaxHistoryTokens int    `yaml:"max_history_tokens" doc:"Soft token budget for history trimming."` // soft token budget for conversation trim; 0 = derive from context_size
	// Compaction replaces blind history truncation with a read-path gist:
	// when enabled, older turns that don't fit the per-payload token budget
	// are condensed into one deterministic topic-summary message instead of
	// being dropped silently. Default off → legacy truncation.
	Compaction            ChatCompactionConfig `yaml:"compaction"`
	MaxToolIterations     int                  `yaml:"max_tool_iterations" doc:"Tool-call loop cap per dispatcher turn."`              // dispatcher tool-call loop cap; 0 = default (30)
	MaxConcurrentRequests int                  `yaml:"max_concurrent_requests" doc:"Max in-flight chat backend calls; excess queues."` // max in-flight chat backend calls; less-urgent requests queue
	SystemPrompt          string               `yaml:"system_prompt"`
	ContextSize           int                  `yaml:"context_size" doc:"Context-window tokens."`    // context window tokens; 0 = provider default
	MaxTokens             int                  `yaml:"max_tokens" doc:"Max output tokens per call."` // max output tokens per call; 0 = provider default
	// Thinking/reasoning is configured PER SUBPROVIDER under the router
	// (claude_subscription.thinking_budget, codex_subscription.effort_level,
	// claude_cli.effort_level), not as a top-level chat knob — a top-level
	// flag was removed 2026-06-23 because initChatRouter never read it (it
	// only worked on the legacy single-`http` provider path).

	// PromptCacheMode turns on provider-native prompt-prefix
	// caching. Default empty / "off". Recognised values:
	//
	//   - "off"    — no annotations (default).
	//   - "auto"   — when a request doesn't carry an explicit
	//                CacheStrategy, the chat proxy stamps one with
	//                mode=auto so Bedrock + Anthropic converters
	//                auto-mark the system block. Recommended.
	//   - "prefix" — same as auto but requires callers to mark
	//                specific messages with CachePrefix=true; the
	//                proxy stamps mode=prefix and the converter
	//                inserts cache pragmas only at the marked
	//                positions.
	//
	// Sub-providers without native cache support (OpenAI-compat,
	// Vertex, Ollama) ignore this knob — no cost, no behavior
	// change. See https://docs.vornik.io
	PromptCacheMode string `yaml:"prompt_cache_mode" doc:"Provider-native prompt caching: off, auto (recommended), or prefix."`
}

// ChatRouterConfig composes multiple providers behind a single
// chat.Provider. Each sub-provider is optional — an absent/disabled
// block means that kind isn't available, so no routes can point at it.
// The "default" kind picks which one handles requests that don't match
// any explicit route (and serves as the dispatcher's fallback too).
type ChatRouterConfig struct {
	// Default names the sub-provider used when no route prefix
	// matches. Must be one of "claude-cli", "codex-cli",
	// "codex-subscription", or "http".
	// Required when Provider=="router".
	Default string `yaml:"default" doc:"Sub-provider used when no route matches. Required for router."`

	// ClaudeCLI configures the subprocess-backed Claude Code sub-provider.
	// Deprecated: prefer ClaudeSubscription — the CLI adds ~200ms
	// subprocess startup and uses a tool-call prompt-engineering shim
	// instead of native tool_use blocks.
	ClaudeCLI ChatCLISubConfig `yaml:"claude_cli"`
	// ClaudeSubscription configures the direct-API claude-subscription
	// sub-provider. Reads ~/.claude/.credentials.json (Claude Code
	// login) and POSTs to https://api.anthropic.com/v1/messages with
	// the OAuth beta unlock header. No subprocess, native tool_use
	// blocks, real per-delta streaming.
	ClaudeSubscription ChatClaudeSubscriptionSubConfig `yaml:"claude_subscription"`
	// CodexCLI configures the Codex CLI subprocess sub-provider.
	// Deprecated: prefer CodexSubscription — the CLI forces built-in
	// tools we can't disable.
	CodexCLI ChatCLISubConfig `yaml:"codex_cli"`
	// CodexSubscription configures the direct-API codex-subscription
	// sub-provider. Reads ~/.codex/auth.json (Codex CLI login) and
	// posts to https://chatgpt.com/backend-api/codex/responses. No
	// subprocess, so no forced built-in tools; our shim is the only
	// way the model can call anything.
	CodexSubscription ChatCodexSubscriptionSubConfig `yaml:"codex_subscription"`
	// HTTP configures the OpenAI-compatible HTTP sub-provider.
	HTTP ChatHTTPSubConfig `yaml:"http"`
	// Vertex configures Google Vertex AI (Gemini) accessed via API key
	// against Vertex's OpenAI-compat endpoint. Separate from HTTP because
	// the auth header is different (`X-Goog-Api-Key` rather than
	// `Authorization: Bearer`) and the endpoint URL is built from
	// project_id + location rather than supplied whole.
	Vertex ChatVertexSubConfig `yaml:"vertex"`
	// OpenRouter configures the OpenRouter sub-provider — an
	// OpenAI-compatible gateway aimed primarily at OpenRouter's free-tier
	// models (model IDs ending in ":free", billed at $0). Separate from
	// HTTP because it carries app-attribution headers (HTTP-Referer /
	// X-Title), bakes in the openrouter.ai endpoint, and can hard-guard
	// against accidental paid spend via free_only. Route ":free" models
	// here with a suffix route — see ChatRouteConfig.Suffix.
	OpenRouter ChatOpenRouterSubConfig `yaml:"openrouter"`
	// Bedrock configures the AWS Bedrock Converse API as a native
	// sub-provider. Separate from HTTP because Bedrock isn't
	// OpenAI-compat: the wire shape is Converse (system + messages
	// split, different content-block schema, SigV4 auth via the AWS
	// SDK credential chain). Use this instead of routing
	// `<provider>.<model>` IDs (zai.*, qwen.*, anthropic.*, etc.)
	// through an OpenAI-compat proxy. See the migration plan in
	// commit history if you're moving roles off the proxy.
	Bedrock ChatBedrockSubConfig `yaml:"bedrock"`

	// Routes lists model-prefix → sub-provider mappings. First-match
	// wins, so order matters; more specific prefixes go first. When
	// empty, a sensible default table is applied at init time:
	//   claude-  → claude_cli
	//   gpt-     → codex_subscription
	//   o3-      → codex_subscription
	//   o4-      → codex_subscription
	//   codex    → codex_subscription
	//   gemini-  → vertex
	//   google/  → vertex
	Routes []ChatRouteConfig `yaml:"routes"`
}

// ChatCLISubConfig describes one CLI-backed sub-provider.
type ChatCLISubConfig struct {
	// Enabled gates the sub-provider. When false, routes pointing at
	// this kind are dropped at init time with a warning.
	Enabled bool `yaml:"enabled"`
	// Binary is the CLI binary's absolute path. Empty resolves via
	// PATH — usually fine interactively but systemd user services
	// inherit a minimal PATH, so setting an absolute path is safer.
	Binary string `yaml:"binary"`
	// Model is the default --model argument. Leave empty to let the
	// CLI pick (required for Codex under ChatGPT-account auth).
	Model string `yaml:"model"`
	// EffortLevel controls CLAUDE_CODE_EFFORT_LEVEL for the claude
	// CLI subprocess (Sonnet 4.6 / Opus 4.6 / Opus 4.7 adaptive
	// reasoning). Values: "low", "medium", "high", "xhigh", "max".
	// Empty = use the CLI's default. Ignored for codex-cli.
	// Defaults to "low" when unset — the proxy's tool-calling use
	// case rarely benefits from deep reasoning and higher levels
	// can produce multi-minute turns that exceed agent timeouts.
	EffortLevel string `yaml:"effort_level" doc:"CLI reasoning effort: low|medium|high|xhigh|max (default low); honored by claude-cli, ignored by codex-cli."`
}

// ChatHTTPSubConfig describes the HTTP sub-provider used by the router.
type ChatHTTPSubConfig struct {
	Enabled  bool   `yaml:"enabled" doc:"Enable the OpenAI-compatible sub-provider."`
	Endpoint string `yaml:"endpoint" doc:"Endpoint URL for the HTTP sub-provider."`
	APIKey   string `yaml:"api_key" doc:"API key for the HTTP sub-provider."`
	Model    string `yaml:"model" doc:"Default model for the HTTP sub-provider."`
	// MaxTokens caps the per-call output tokens for this
	// subprovider. 0 inherits chat.max_tokens; if both are unset
	// the upstream gateway picks a default (Bedrock applies the
	// model's hard max output, often 128k+, which collapses the
	// usable input budget on long-context models like glm-5).
	MaxTokens int `yaml:"max_tokens"`
}

// ChatVertexSubConfig describes the Google Vertex AI (Gemini) sub-provider.
// Auth is API-key only — service-account OAuth is intentionally out of scope,
// since the deployment model for vornik is long-lived daemons where a
// rotated API key is simpler to operate than short-lived access tokens.
// Requests land on Vertex's OpenAI-compatibility endpoint, so the wire shape
// matches the HTTP sub-provider exactly; only the auth header name and the
// endpoint URL differ.
type ChatVertexSubConfig struct {
	Enabled bool `yaml:"enabled" doc:"Enable Google Vertex AI (Gemini)."`
	// APIKey is the raw Vertex API key; carried in `X-Goog-Api-Key`.
	// Required.
	APIKey string `yaml:"api_key" doc:"Vertex API key."`
	// ProjectID is the GCP project that owns the Vertex endpoint. Required.
	ProjectID string `yaml:"project_id" doc:"GCP project that owns the Vertex endpoint."`
	// Location is the Vertex region, e.g. "us-central1" or "global".
	// Empty defaults to "global", whose hostname is
	// `aiplatform.googleapis.com`; non-global locations use
	// `<location>-aiplatform.googleapis.com`.
	Location string `yaml:"location" doc:"Vertex region; defaults to global."`
	// Model is the default Gemini model identifier in Vertex's
	// OpenAI-compat form (e.g. "google/gemini-2.5-pro",
	// "google/gemini-2.5-flash"). Per-role `model:` values routed here
	// should use the same naming.
	Model string `yaml:"model" doc:"Default Vertex model."`
	// Endpoint overrides the constructed URL. Leave empty in normal
	// deployments — set only when talking to a proxy or a preview
	// endpoint that doesn't follow the standard hostname pattern.
	Endpoint string `yaml:"endpoint"`
	// MaxTokens caps the response output length. 0 = use the provider
	// default.
	MaxTokens int `yaml:"max_tokens"`
}

// ChatOpenRouterSubConfig describes the OpenRouter sub-provider. Requests
// land on OpenRouter's OpenAI-compatibility endpoint
// (https://openrouter.ai/api/v1), so the wire shape matches the HTTP
// sub-provider exactly; what differs is the baked-in endpoint, the
// app-attribution headers, the optional free-only guard, and free-tier
// model discovery. See https://docs.vornik.io
type ChatOpenRouterSubConfig struct {
	Enabled bool `yaml:"enabled" doc:"Enable the OpenRouter sub-provider."`
	// APIKey is the OpenRouter API key; carried as `Authorization:
	// Bearer <key>`. Required when Enabled.
	APIKey string `yaml:"api_key" doc:"OpenRouter API key."`
	// Endpoint overrides the OpenAI-compat base URL. Empty defaults to
	// https://openrouter.ai/api/v1. Set only for a proxy/preview.
	Endpoint string `yaml:"endpoint"`
	// Model is the default model ID used when no per-request override is
	// supplied — typically a ":free" variant
	// (e.g. "deepseek/deepseek-r1:free"). Per-role `model:` values routed
	// here should use OpenRouter's "<vendor>/<model>[:free]" naming.
	Model string `yaml:"model" doc:"OpenRouter default model."`
	// FreeOnly, when true, makes the sub-provider reject any request for
	// a non-":free" model with a typed error before any network call —
	// a hard guard against accidental spend. Default false (free tier is
	// the common case, but paid models stay reachable).
	FreeOnly bool `yaml:"free_only" doc:"Reject any non-:free model — a hard guard against accidental spend."`
	// Referer sets the HTTP-Referer header OpenRouter uses for app
	// attribution. Empty defaults to "vornik/<version>" (no website URL).
	Referer string `yaml:"referer"`
	// Title sets the X-Title header (app display name). Empty defaults to
	// "vornik".
	Title string `yaml:"title"`
	// MaxTokens caps per-call output tokens. 0 inherits chat.max_tokens.
	MaxTokens int `yaml:"max_tokens"`
}

// ChatBedrockSubConfig describes the AWS Bedrock native sub-provider.
// Auth is delegated to the AWS SDK v2 default credential chain (env
// vars / shared credentials / IAM role / instance profile) — this
// struct deliberately doesn't carry an APIKey so operators don't
// accidentally embed credentials in the daemon config. Use
// ~/.config/vornik/secrets/aws.env (loaded by the systemd unit's
// EnvironmentFile) or ~/.aws/credentials per the SDK's standard
// search order.
//
// Different from the http sub-provider in three ways:
//
//   - The Bedrock Converse API is NOT OpenAI-compat. The wire shape
//     differs (system / messages split, content-block schema, tool
//     format, streaming event types). The provider handles the
//     translation; callers pass OpenAI-shaped messages and get
//     OpenAI-shaped responses back.
//   - Auth is SigV4-signed by the SDK; no bearer token slot.
//   - Region is the routing primitive, not endpoint URL. Marketplace
//     model availability varies per region; pin one region per
//     deployment to keep behaviour predictable.
type ChatBedrockSubConfig struct {
	Enabled bool `yaml:"enabled" doc:"Enable AWS Bedrock (credentials via the AWS SDK chain)."`
	// Region is the AWS region the Bedrock Converse API is called
	// against, e.g. "us-east-1", "us-west-2", "ap-southeast-2".
	// Required when Enabled. Marketplace models (zai.*, qwen.*,
	// nvidia.*, etc.) have region-specific availability — pick the
	// region that publishes the IDs your role configs use.
	Region string `yaml:"region" doc:"Bedrock region."`
	// Model is the default model identifier passed to Converse when
	// no per-request WithModel override is set. Bedrock model IDs
	// follow `<provider>.<model>` for marketplace models or longer
	// `<region>.<provider>.<model>-<date>-<version>` for
	// inference profiles. Empty allowed if every caller routes via
	// per-request model selection.
	Model string `yaml:"model" doc:"Bedrock default model."`
	// MaxTokens caps inferenceConfig.maxTokens for every Converse
	// call. 0 inherits chat.max_tokens; if both are unset Bedrock
	// applies the model's hard max output (often 128k+, which
	// collapses the input budget on long-context models).
	MaxTokens int `yaml:"max_tokens"`
}

// ChatCodexSubscriptionSubConfig configures the direct ChatGPT-
// subscription Responses API provider. No subprocess is launched;
// we read the OAuth tokens the Codex CLI already wrote to disk
// and talk to https://chatgpt.com/backend-api/codex/responses
// ourselves, which (a) bills against the subscription like the
// CLI does, and (b) lets us keep control of the tool catalog —
// only tools we declare are visible to the model.
type ChatCodexSubscriptionSubConfig struct {
	Enabled bool `yaml:"enabled"`
	// AuthPath overrides ~/.codex/auth.json. Empty = default.
	AuthPath string `yaml:"auth_path"`
	// Model is the default model id passed in the request body.
	// Empty = "gpt-5.4-mini".
	Model string `yaml:"model"`
	// EffortLevel is the `reasoning.effort` field. Accepts
	// "low" | "medium" | "high". Empty = don't send the field.
	EffortLevel string `yaml:"effort_level" doc:"Codex reasoning effort: low|medium|high (empty = omit)."`
}

// ChatClaudeSubscriptionSubConfig configures the direct Messages API
// provider for Claude Code subscription users. Reads the OAuth tokens
// the `claude` CLI wrote to ~/.claude/.credentials.json (or to
// CLAUDE_CONFIG_DIR/.credentials.json), then POSTs to
// https://api.anthropic.com/v1/messages with the OAuth beta unlock
// header that lets bearer tokens hit the Messages surface. macOS
// operators whose CLI stores tokens in Keychain can instead export
// CLAUDE_CODE_OAUTH_TOKEN and leave AuthPath empty.
type ChatClaudeSubscriptionSubConfig struct {
	Enabled bool `yaml:"enabled"`
	// AuthPath overrides ~/.claude/.credentials.json. Empty = default.
	AuthPath string `yaml:"auth_path"`
	// Model is the default model id (e.g. claude-sonnet-4-6,
	// claude-opus-4-6). Typically overridden per-request via the
	// router when agent roles pin their own model.
	Model string `yaml:"model"`
	// MaxTokens caps the response output length. 0 = use the
	// package default (8192). Required field on the wire; the
	// provider synthesizes one so callers don't have to.
	MaxTokens int `yaml:"max_tokens"`
	// ThinkingBudget enables Anthropic extended thinking with the
	// given budget_tokens. 0 disables thinking. Opus 4.7 always
	// thinks regardless; this knob only matters for Opus 4.5/4.6
	// and Sonnet 4.x.
	ThinkingBudget int `yaml:"thinking_budget" doc:"Anthropic extended-thinking budget_tokens; 0 disables (router subprovider)."`
	// UserAgent overrides the User-Agent header. Empty = default
	// "claude-cli/1.0.0 (external, <os>)" — posing as the CLI keeps
	// us in the same rate-limit bucket the shipped binary uses.
	UserAgent string `yaml:"user_agent"`
}

// ChatRouteConfig is one model-prefix → sub-provider mapping.
type ChatRouteConfig struct {
	// Prefix is the case-sensitive model-name prefix that selects
	// this route. Empty prefix matches everything; only use it as the
	// last entry.
	Prefix string `yaml:"prefix" doc:"Model-name prefix that selects this route."`
	// Suffix is the case-sensitive model-name suffix that selects this
	// route. Suffix matches take PRECEDENCE over prefix matches in the
	// router, regardless of list order — the canonical use is routing
	// OpenRouter's ":free" variants to the openrouter sub-provider even
	// when a vendor-prefix route (e.g. "google/") would also match.
	// Empty (the default) means "no suffix match"; a route may set
	// prefix, suffix, or both. See ChatVertexSubConfig vs
	// ChatOpenRouterSubConfig and chat-openrouter-design.md.
	Suffix string `yaml:"suffix" doc:"Model-name suffix that selects this route (takes precedence over prefix)."`
	// Kind names the sub-provider: "claude-cli", "claude-subscription",
	// "codex-cli", "codex-subscription", "http", "vertex", "openrouter",
	// or "bedrock". Must match an enabled block above.
	Kind string `yaml:"kind" doc:"Target sub-provider for matching models."`
	// QueueDepth is the per-route bounded in-process queue depth
	// (hardening sub-item 4). When > 0, the router serialises this
	// route's calls behind a depth-N semaphore so autonomy bursts
	// don't slam the upstream provider. Zero (the legacy default)
	// keeps the previous "fire all calls in parallel" behaviour.
	QueueDepth int `yaml:"queue_depth"`
	// QueueTimeoutMs is the max wall time a queued call may wait
	// before the router gives up and returns a 503-equivalent
	// RouteOverflowError. Zero means "wait forever" — only safe
	// when caller timeouts dominate. Recommended: small multiple
	// of the upstream call's own timeout (e.g. 30000 = 30s).
	QueueTimeoutMs int `yaml:"queue_timeout_ms"`
}

// ResolvedAgentLLM returns the effective LLM config for agent containers.
// It prefers AgentLLM values and falls back to ChatConfig for any empty/zero field.
// This means a single chat: section is sufficient when agents share the same endpoint.
func (c *Config) ResolvedAgentLLM() AgentLLMConfig {
	resolved := c.Runtime.AgentLLM
	if resolved.Endpoint == "" {
		resolved.Endpoint = c.Chat.Endpoint
	}
	if resolved.APIKey == "" {
		resolved.APIKey = c.Chat.APIKey
	}
	if resolved.Model == "" {
		resolved.Model = c.Chat.Model
	}
	if resolved.ContextSize == 0 {
		resolved.ContextSize = c.Chat.ContextSize
	}
	if resolved.MaxTokens == 0 {
		resolved.MaxTokens = c.Chat.MaxTokens
	}
	if resolved.Timeout == "" {
		resolved.Timeout = c.Chat.Timeout
	}
	return resolved
}

// TelegramConfig holds Telegram bot configuration.
type TelegramConfig struct {
	Enabled      bool                  `yaml:"enabled" doc:"Turn the Telegram bot on."`
	BotToken     string                `yaml:"bot_token" doc:"Bot token. Prefer an environment variable."`
	AllowedUsers map[string]UserAccess `yaml:"allowed_users" doc:"Map of Telegram user ID to access. true = full access; a list of project IDs scopes the user. Absent users are denied."` // user IDs as strings for YAML parsing
	RateLimit    int                   `yaml:"rate_limit" doc:"Requests per minute per user."`                                                                                             // requests per minute per user
	SessionPath  string                `yaml:"session_path" doc:"Path for conversation persistence (empty = none)."`                                                                       // path for conversation persistence (empty = no persistence)
	SessionTTL   string                `yaml:"session_ttl" doc:"Auto-expire idle sessions (e.g. 24h)."`                                                                                    // auto-expire idle sessions (e.g. "24h"); empty = disabled
	// DispatcherProjectID pins the project_id written on every
	// dispatcher LLM usage row to a single project (typically a
	// dedicated "assistant"). Without this, every chat round-
	// trip's cost lands on whichever project the conversation is
	// pinned to as its active project, mixing chat overhead with
	// per-project task cost on the dashboards. Empty = legacy
	// behaviour (active project wins).
	DispatcherProjectID string `yaml:"dispatcher_project_id"`
	// DispatcherMaxIterations caps the dispatcher's tool-calling
	// loop per chat turn. Default 10 (down from the historical 30
	// — runaway tool loops were a recurring cost driver). Operators
	// running heavy tool-use chats can bump this; lighter chats
	// can drop it further.
	DispatcherMaxIterations int `yaml:"dispatcher_max_iterations" doc:"Tool-call loop cap per chat turn."`
	// ForumChatID is the Telegram supergroup with is_forum=true
	// that receives one Forum Topic per task. When 0 (default),
	// forum routing is disabled and the bot uses the existing
	// flat-chat reply-id routing only. The chat must have Topics
	// enabled in Telegram (Group → Edit → Topics).
	ForumChatID int64 `yaml:"forum_chat_id" doc:"Supergroup ID for one forum topic per task (0 = off)."`
	// ForumTopicIconColor is the integer colour for new forum
	// topic icons. Telegram only accepts six palette values:
	// 7322096 (blue), 16766590 (yellow), 13338331 (purple),
	// 9367192 (green), 16749490 (red), 16478047 (orange). Any
	// other value (including 0) causes the bot to omit the
	// field and let Telegram pick a default colour.
	ForumTopicIconColor int `yaml:"forum_topic_icon_color"`
	// WebUIBaseURL (optional) is the externally-reachable base URL
	// of the daemon's web UI — e.g. "https://vornik.example.com".
	// The 2026.6.0 /start onboarding wizard renders a clickable
	// link to /ui/projects/new for new users without projects yet.
	// Empty falls back to a relative-path hint that operators can
	// follow on a self-hosted deployment.
	WebUIBaseURL string `yaml:"web_ui_base_url" doc:"Public base URL of the web UI, used in onboarding links."`
}

// UserAccess describes what a Telegram user may do. Parsed from a
// polymorphic YAML shape to keep the config terse for the common case
// (one user, all access) while supporting per-project scoping.
//
// Accepted YAML values:
//
//	"<id>": true             → Allowed, wildcard (equivalent to ["*"])
//	"<id>": false            → explicit deny
//	"<id>": []               → Allowed to chat, zero project access
//	"<id>": ["*"]            → Allowed, wildcard over all projects
//	"<id>": ["a","b"]        → Allowed, scoped to listed projects
//
// A user absent from the map is unauthorized (same as the legacy bool
// semantics with false).
type UserAccess struct {
	// Allowed is true when the user may interact with the bot at all.
	// When false, every message from this user is rejected before any
	// dispatcher or project logic runs.
	Allowed bool
	// Projects lists the project IDs the user may /project into and see
	// in list_projects output. Empty slice means "dispatcher only" —
	// the user can chat, but any /project command is denied and the
	// registry-backed project tools see no projects.
	Projects []string
}

// Wildcard reports whether the user has unrestricted project access.
// Implemented as a method (rather than exposing the sentinel) so
// callers don't encode the "*" magic string in their own logic.
func (u UserAccess) Wildcard() bool {
	for _, p := range u.Projects {
		if p == "*" {
			return true
		}
	}
	return false
}

// CanAccessProject returns true when the user may interact with the
// named project. Wildcard users return true for any projectID; scoped
// users return true only for exact matches in their Projects list.
func (u UserAccess) CanAccessProject(projectID string) bool {
	if !u.Allowed {
		return false
	}
	if u.Wildcard() {
		return true
	}
	for _, p := range u.Projects {
		if p == projectID {
			return true
		}
	}
	return false
}

// UnmarshalYAML decodes the three accepted shapes — bool, list, or the
// default struct-ish mapping — into a UserAccess. Defaults to the
// legacy semantics (true → wildcard, false → deny) so existing configs
// load unchanged.
func (u *UserAccess) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Try bool first (legacy shape).
	var b bool
	if err := unmarshal(&b); err == nil {
		u.Allowed = b
		if b {
			u.Projects = []string{"*"}
		}
		return nil
	}

	// Then list of strings.
	var list []string
	if err := unmarshal(&list); err == nil {
		u.Allowed = true
		u.Projects = list
		return nil
	}

	// Not a shape we understand — report a useful error.
	return fmt.Errorf("allowed_users entry must be bool (true/false) " +
		"or a list of project IDs (use [\"*\"] for wildcard, [] for " +
		"dispatcher-only access)")
}

// MemoryConfig holds configuration for the project-scoped hybrid RAG memory system.
// MemorySufficiencyConfig governs scored-sufficiency iterative retrieval: when
// a single recall returns too few high-relevance hits, widen the candidate
// pool and retry (bounded), returning the first sufficient round. Doubly inert
// by default — Enabled defaults false AND it only activates when the LLM
// reranker is wired (so the floor is an absolute calibrated score). Absent
// block / disabled / no reranker → single-shot recall, unchanged.
type MemorySufficiencyConfig struct {
	Enabled    bool    `yaml:"enabled" doc:"Widen-and-retry recall when too few high-relevance hits (requires the reranker)."`
	MinHighRel int     `yaml:"min_high_rel" doc:"Hits at/above score_floor that count as 'enough' (0 = default 3)."`
	ScoreFloor float64 `yaml:"score_floor" doc:"Absolute reranker relevance floor in [0,1] (0 = default 0.6)."`
	MaxRounds  int     `yaml:"max_rounds" doc:"Hard cap on retrieval rounds (<=1 = single shot; 0 = default 3)."`
}

// MemoryRerankerConfig configures the LLM reranker that re-orders recall
// results by relevance. Off by default. When enabled it adds one LLM call per
// recall (bounded by timeout, degrading to the underlying RRF order on
// failure) AND activates scored-sufficiency retrieval (whose absolute score
// floor is only meaningful against calibrated reranker scores). Use a small,
// fast open-weight model — scoring a handful of snippets is a light task.
type MemoryRerankerConfig struct {
	Enabled         bool   `yaml:"enabled" doc:"LLM-rerank context-assembly recall (the pre-delegation hint) and activate scored-sufficiency. Adds one LLM call (seconds) to that path only; the interactive memory_search tool and companion recall stay fast RRF."`
	Model           string `yaml:"model" doc:"Reranker model id; small OSS recommended. Empty = chat router default (often overkill)."`
	MaxCandidates   int    `yaml:"max_candidates" doc:"Top-K results scored per recall (0 = default 20)."`
	TimeoutSeconds  int    `yaml:"timeout_seconds" doc:"Per-recall rerank timeout in seconds (0 = default 15)."`
	MaxSnippetBytes int    `yaml:"max_snippet_bytes" doc:"Per-candidate snippet sent to the reranker (0 = default 600)."`
}

type MemoryConfig struct {
	// Enabled activates the memory subsystem. Default: false.
	Enabled bool `yaml:"enabled" doc:"Activate the memory subsystem."`

	// Sufficiency governs scored-sufficiency iterative retrieval.
	Sufficiency MemorySufficiencyConfig `yaml:"sufficiency"`

	// Reranker configures the LLM relevance reranker (and, by activating
	// it, scored-sufficiency).
	Reranker MemoryRerankerConfig `yaml:"reranker"`

	// EmbeddingModel is the model name to request from the embedding endpoint,
	// e.g. "text-embedding-3-small". Required when Enabled=true.
	EmbeddingModel string `yaml:"embedding_model" doc:"Embedding model name. Required when enabled."`

	// EmbeddingDimension is the vector dimension produced by EmbeddingModel.
	// Default: 1536 (matches text-embedding-3-small).
	EmbeddingDimension int `yaml:"embedding_dimension" doc:"Vector dimension produced by the model."`

	// EmbeddingEndpoint overrides the chat endpoint for embedding requests.
	// Falls back to the resolved agent LLM endpoint when empty.
	EmbeddingEndpoint string `yaml:"embedding_endpoint" doc:"Override endpoint for embedding requests."`

	// EmbeddingAPIKey overrides the chat api_key for embedding requests.
	// Falls back to the resolved agent LLM API key when empty.
	EmbeddingAPIKey string `yaml:"embedding_api_key" doc:"Override API key for embedding requests."`

	// EmbeddingCacheEnabled turns on the postgres-backed embedding
	// cache (LLM caching design Phase D). Needs migration 41
	// (`create_embedding_cache`) applied. Default false; operators
	// who run the migration opt in here once they've confirmed the
	// table exists. Identical (content, model) pairs then serve
	// from cache instead of round-tripping to the embedding API.
	EmbeddingCacheEnabled bool `yaml:"embedding_cache_enabled" doc:"Cache embeddings to skip repeat API calls."`

	// ResponseCacheEnabled turns on the postgres-backed response
	// cache (LLM caching design Phase E). Needs migration 47
	// (`create_llm_response_cache`) applied. Default false; operators
	// opt in once the schema is current. When true, the memory
	// background trio (Titler, Classifier, KG Extractor) memoise
	// raw responses keyed on (model, purpose, prompt) so re-runs
	// (e.g. `vornikctl memory reclassify` after restart) skip the
	// upstream LLM call entirely.
	ResponseCacheEnabled bool `yaml:"response_cache_enabled" doc:"Cache memory-pipeline LLM responses."`

	// ChunkTokens is the approximate token count per chunk (1 token ≈ 4 chars).
	// Default: 512.
	ChunkTokens int `yaml:"chunk_tokens" doc:"Approximate tokens per chunk."`

	// ChunkOverlap is the overlap in approximate tokens between adjacent chunks.
	// Default: 64.
	ChunkOverlap int `yaml:"chunk_overlap" doc:"Token overlap between adjacent chunks."`

	// WorkerConcurrency is the number of embed queue worker goroutines.
	// Default: 2.
	WorkerConcurrency int `yaml:"worker_concurrency" doc:"Embed-queue worker goroutines."`

	// Graph configures the knowledge-graph extraction pipeline. When
	// disabled (the default), chunks are still flagged with
	// needs_graph_extraction = TRUE on ingest but no worker drains
	// them. When enabled, the worker pulls flagged chunks and runs
	// the four-stage pipeline (extractor → resolver → relationship →
	// validator) using the chat router with per-stage model pins.
	Graph MemoryGraphConfig `yaml:"graph"`

	// Titler configures LLM-based generation of a short topic label
	// (content_title) for each chunk. The label drives the node
	// names in the operator vector-cloud UI; when disabled the UI
	// falls back to markdown headings then filenames. Costs one LLM
	// call per chunk at ingest time.
	Titler MemoryTitlerConfig `yaml:"titler"`

	// Classifier configures the LLM-driven content-class backfill
	// invoked by `vornikctl memory reclassify --use-llm`. Disabled
	// by default; when enabled, the daemon exposes
	// POST /api/v1/memory/reclassify-llm. Costs one LLM call per
	// unclassified chunk per backfill run.
	Classifier MemoryClassifierConfig `yaml:"classifier"`

	// ConsolidateIntervalSeconds drives the periodic per-project
	// gist-extraction loop (LLM-free, internal/memory/
	// consolidate_worker.go). 0 disables the loop entirely —
	// operators trigger gist generation on demand only. Default
	// 600 (10 min); the library is sub-ms per kilobyte so leaving
	// it on is cheap.
	ConsolidateIntervalSeconds int `yaml:"consolidate_interval_seconds"`

	// ConsolidateMinTokenLength filters out single-character
	// tokens before frequency ranking. Default 3 — keeps "NVDA" /
	// "SPY" but drops "a" / "is" / "of".
	ConsolidateMinTokenLength int `yaml:"consolidate_min_token_length"`

	// ConsolidateTopN bounds the ranked-list size persisted into
	// project_gists.terms_json. Default 25 — matches the UI panel's
	// "top terms" display budget.
	ConsolidateTopN int `yaml:"consolidate_top_n"`

	// ConsolidateScanLimit caps the chunks per project the
	// consolidator inspects each tick. Default 1000 — well above
	// the 99th-percentile project size; raise for projects with
	// long-tail corpora.
	ConsolidateScanLimit int `yaml:"consolidate_scan_limit"`

	// LLMConsolidateEnabled turns on the LLM-tier narrative pass
	// — a short natural-language summary written into
	// project_gists.narrative on top of the term-frequency cloud.
	// Off by default since it incurs per-tick LLM cost; opt in
	// alongside `consolidate_llm_model` when you want the
	// operator UI to surface a sentence-form project description.
	LLMConsolidateEnabled bool `yaml:"llm_consolidate_enabled"`

	// LLMConsolidateIntervalSeconds drives the LLM-tier
	// narrative-generation loop. 0 → 3600 (1h). Slower than the
	// LLM-free term loop because (a) narratives shift much more
	// slowly than the term cloud and (b) per-tick cost matters.
	LLMConsolidateIntervalSeconds int `yaml:"llm_consolidate_interval_seconds"`

	// LLMConsolidateModel identifies the model used to generate
	// project narratives. Empty leaves the chat router's default
	// in place. Recommended a small OSS / managed model — the
	// summary fits in one short paragraph so big-context models
	// don't help.
	LLMConsolidateModel string `yaml:"llm_consolidate_model"`

	// LLMConsolidateSampleSize is how many chunk excerpts feed
	// the narrative prompt. 0 → 8. Higher drives token cost up
	// linearly; the summary doesn't improve materially past
	// ~10 chunks.
	LLMConsolidateSampleSize int `yaml:"llm_consolidate_sample_size"`

	// PromptInjectionScan controls the ingest prompt-injection gate,
	// which scans chunk content for context-manipulation payloads (a
	// stored-injection vector — a poisoned chunk is replayed into every
	// agent that recalls it). One of "off" (default), "detect"
	// (allow + record the signal), or "quarantine" (route flagged
	// content to the quarantine queue for operator review).
	PromptInjectionScan string `yaml:"prompt_injection_scan" doc:"Ingest prompt-injection gate: off (default), detect, or quarantine."`

	// ClaimAuditDisabledProjects lists project IDs for which the
	// claim-audit (hallucination Phase-1) overlap gate is skipped on
	// ingest — a per-project escape hatch for projects where the gate
	// produces too many false-positive shadow signals.
	ClaimAuditDisabledProjects []string `yaml:"claim_audit_disabled_projects" doc:"Project IDs that skip the claim-audit/hallucination ingest gate."`

	// DenyPatterns is the operator-declared substring deny-list the ingest
	// policy_match gate enforces: any chunk whose content contains one of
	// these substrings is routed to quarantine for operator review. Matching
	// is plain substring (NOT regex), so the deny-list is ReDoS-immune by
	// construction. Empty (default) disables the gate. Hot-reloadable: a
	// config.yaml reload swaps the live list without a daemon restart (same
	// path as prompt_injection_scan / claim_audit_disabled_projects).
	DenyPatterns []string `yaml:"deny_patterns" doc:"Substring deny-list (NOT regex) that quarantines matching content at memory ingest."`
}

// InstinctConfig configures the continuous-learning instinct layer.
// Opt-in and advisory-only even when enabled: the extraction worker
// only writes confidence-scored learned patterns; the consumer flags
// below gate whether (in later slices) those patterns are surfaced to
// behaviour-affecting surfaces, all of which keep their existing
// operator/architect approval gates.
//
// Slice 1 wires only the schema, repo, and deterministic-only worker.
// The Model / DeadDays / Consumers fields are accepted now for
// forward-compat (so an operator's config doesn't need editing when
// later slices land) but are inert in slice 1.
//
// Durations are expressed in operator-friendly units (seconds, days)
// rather than Go duration strings, matching the rest of this file
// (e.g. MemoryConfig.ConsolidateIntervalSeconds). The wiring layer
// converts them to an instinct.Thresholds value; 0 means "use the
// design default" at that layer.
type InstinctConfig struct {
	// Enabled is the master switch. Default false → the worker is
	// never constructed and the subsystem is fully dark.
	Enabled bool `yaml:"enabled" doc:"Master switch for the instinct layer."`

	// CadenceSeconds is the extraction worker tick interval.
	// 0 → 1800 (30m).
	CadenceSeconds int `yaml:"cadence_seconds" doc:"Extraction-worker tick interval."`

	// LookbackSeconds is how far back each tick scans. 0 → 2×cadence.
	// Should exceed the cadence so consecutive ticks overlap; the
	// idempotent upsert makes the overlap free.
	LookbackSeconds int `yaml:"lookback_seconds" doc:"How far back each tick scans."`

	// MaxOutcomesPerTick caps the step-outcome rows pulled per tick.
	// 0 → 2000.
	MaxOutcomesPerTick int `yaml:"max_outcomes_per_tick"`

	// MinSupport is the corroborating-outcome floor for the
	// candidate→active transition. 0 → 3.
	MinSupport int `yaml:"min_support" doc:"Corroborating outcomes before a pattern goes active."`

	// ActiveConfidence / PromoteConfidence / PromoteProjects /
	// RetireFloor are the confidence-model thresholds. 0 → design
	// defaults (0.6 / 0.8 / 2 / 0.2).
	ActiveConfidence  float64 `yaml:"active_confidence" doc:"Confidence floor for an active instinct."`
	PromoteConfidence float64 `yaml:"promote_confidence"`
	PromoteProjects   int     `yaml:"promote_projects"`
	RetireFloor       float64 `yaml:"retire_floor"`

	// DecayHalflifeDays is the recency half-life for confidence decay.
	// 0 → 30 (the design's 720h).
	DecayHalflifeDays int `yaml:"decay_halflife_days" doc:"Recency half-life for confidence decay."`

	// Model is the cheap distill model for the (later-slice) LLM
	// generalisation pass. Empty → fall back to chat.model. Inert in
	// slice 1.
	Model string `yaml:"model"`

	// DeadDays is the retrieval prune-candidate threshold for the
	// memory-hygiene consumer. 0 → 60. Inert in slice 1.
	DeadDays int `yaml:"dead_days"`

	// MaxEvidenceAgeDays is the W1 evidence-freshness gate: when > 0, an
	// instinct may not hold active/promoted while its newest corroboration
	// is older than this many days (a freshly history-mined instinct gets a
	// creation grace). 0 → gate OFF (no freshness gating).
	MaxEvidenceAgeDays int `yaml:"max_evidence_age_days"`

	// FloorMinCleanSupport / FloorConfidence are the W5 confidence floor:
	// an instinct with >= FloorMinCleanSupport corroborations and ZERO
	// contradictions has its raw confidence raised to at least
	// FloorConfidence (applied before decay; mixed evidence never floored).
	// Both must be > 0 to activate; either at 0 → floor OFF. Choose
	// FloorConfidence < active_confidence so the floor never auto-activates.
	FloorMinCleanSupport int     `yaml:"floor_min_clean_support"`
	FloorConfidence      float64 `yaml:"floor_confidence"`

	// Consumers gates each behaviour-affecting consumer independently.
	// All default false; all inert in slice 1.
	Consumers InstinctConsumersConfig `yaml:"consumers"`
}

// InstinctConsumersConfig gates the instinct layer's behaviour-
// affecting consumers (slices 3–5). All default false; surfacing a
// learned pattern still goes through the existing operator/architect
// approval gates even when a consumer is enabled.
type InstinctConsumersConfig struct {
	// FailurePlaybooks surfaces recovery instincts on the failed-task
	// UI and in the lead's recovery context (slice 3).
	FailurePlaybooks bool `yaml:"failure_playbooks" doc:"Surface recovery instincts on failed-task views."`
	// ArchitectPriors feeds workflow instincts to the architect as
	// priors and writes rejected proposals back as contradictions
	// (slice 4).
	ArchitectPriors bool `yaml:"architect_priors" doc:"Feed workflow instincts to the architect as priors."`
	// MemoryHygiene feeds retrieval instincts to the consolidation /
	// firewall sweepers as boost/prune candidates (slice 5).
	MemoryHygiene bool `yaml:"memory_hygiene" doc:"Feed retrieval instincts to memory sweepers."`
	// ApplicationFeedback folds instinct_applications results back into
	// the confidence model (slice 7): a "lift" multiplier that erodes
	// confidence when surfacing didn't help, and the application-success
	// gate that lets a distilled instinct graduate out of the candidate
	// cap. Default false → application rows are recorded but never affect
	// scoring (byte-for-byte pre-slice-7 confidence).
	ApplicationFeedback bool `yaml:"application_feedback"`

	// ToolBudget supplies a learned complexity tier to toolbudget.Resolve on
	// the ABSENT-VERDICT path only (the planner emitted no tier). Never
	// overrides an explicit verdict; Resolve's max_factor/autonomy_max_factor
	// caps still bound the result. Default false → budget instincts are mined
	// and surfaced advisory but never change a budget.
	ToolBudget bool `yaml:"tool_budget" doc:"Let learned budget instincts fill an absent complexity tier (default off)."`

	// AutoApply (v2) promotes high-confidence recovery instincts from an
	// ADVISORY overlay to a prompt-level DIRECTIVE in the lead's recovery
	// context, and records the application as "auto_applied" (vs "ignored")
	// so the feedback loop grades whether the auto-application helped. The
	// executor still approval-gates the recovery plan — auto-apply changes
	// how strongly the remediation is surfaced, it does NOT bypass the gate.
	// Default disabled: no behaviour change unless an operator opts in.
	AutoApply InstinctAutoApplyConfig `yaml:"auto_apply"`
}

// InstinctAutoApplyConfig gates the v2 prompt-level auto-apply consumer.
// All-zero (the default) keeps auto-apply OFF — learned remediations stay
// advisory exactly as before.
type InstinctAutoApplyConfig struct {
	// Enabled turns prompt-level auto-apply on. Default false.
	Enabled bool `yaml:"enabled" doc:"Promote high-confidence recovery instincts to a prompt-level directive (default off)."`
	// MinConfidence is the floor a remediation's materialised confidence
	// must meet to be auto-applied. Remediations below it stay advisory.
	// 0 with Enabled is treated as the 0.85 default by the consumer.
	MinConfidence float64 `yaml:"min_confidence" doc:"Confidence floor for auto-apply (default 0.85)."`
	// MinCleanSupport, when > 0, requires a remediation to have at least this
	// many corroborations AND zero contradictions to be auto-applied — the
	// clean-evidence gate that keeps a lowered MinConfidence safe. 0 (default)
	// leaves the gate off: confidence + allowlist only, as before.
	MinCleanSupport int `yaml:"min_clean_support" doc:"Min clean (zero-contradiction) supports for auto-apply; 0 = off."`
	// AllowedErrorClasses, when non-empty, restricts auto-apply to these
	// failure classes (a safety allowlist — e.g. only transient classes).
	// Empty means every class meeting MinConfidence is eligible.
	AllowedErrorClasses []string `yaml:"allowed_error_classes" doc:"Failure classes eligible for auto-apply; empty = all."`
}

// MemoryClassifierConfig configures the LLM classifier — see
// internal/memory/classifier.go. Routed through the same chat
// router as the titler.
type MemoryClassifierConfig struct {
	// Enabled turns the classifier on. When false (default), the
	// reclassify-llm endpoint returns 503 and the CLI's --use-llm
	// flag fails fast with a clear "not enabled" message.
	Enabled bool `yaml:"enabled" doc:"LLM content-class backfill."`

	// Model identifier passed via ModelOverridable. Empty leaves
	// the chat router's default in place. Recommended a small OSS
	// model (gpt-oss-20b range) because classification is a
	// closed-vocabulary task that doesn't need the bigger models.
	Model string `yaml:"model"`

	// TimeoutSeconds caps a single LLM call. Default 30.
	TimeoutSeconds int `yaml:"timeout_seconds"`

	// MaxPreviewBytes caps the chunk body sent to the LLM. Default
	// 2048 — same rationale as the titler.
	MaxPreviewBytes int `yaml:"max_preview_bytes"`

	// AutoBackfillIntervalSeconds drives the background loop that
	// classifies chunks the deterministic role-map left unclassified.
	// Mirrors the titler's auto-backfill knob; without it the daemon
	// only classifies via the operator-triggered CLI / API. 0
	// disables the loop (previous behaviour, kept as the default so
	// existing deployments don't suddenly start billing LLM calls).
	// Recommended ≥ 300 (5 min) to keep cache-warm semantics for the
	// chat router.
	AutoBackfillIntervalSeconds int `yaml:"auto_backfill_interval_seconds"`

	// AutoBackfillBatchSize caps how many chunks the loop processes
	// per tick. Each chunk costs one LLM round-trip, so this is the
	// per-tick spend ceiling. Default 25 (matches the titler).
	AutoBackfillBatchSize int `yaml:"auto_backfill_batch_size"`

	// InlineFallbackEnabled adds an inline LLM classification pass
	// to the ingest pipeline for chunks the deterministic role-map
	// can't resolve (producer_role empty or not in roleClassMap).
	// Default false — the auto-backfill loop above is the
	// recommended catch path; enabling inline fallback trades
	// ingest latency for fresher class labels. See Pipeline.IngestArtifact
	// for the call site.
	InlineFallbackEnabled bool `yaml:"inline_fallback_enabled"`
}

// MemoryTitlerConfig configures the per-chunk topic labeller. The
// labeller routes through the same chat router as the KG extractor
// (ModelOverridable), so the Model field accepts any router-known ID
// — including local Ollama models like "gpt-oss:120b".
type MemoryTitlerConfig struct {
	// Enabled turns the titler on. When false (default), no titles
	// are generated at ingest; the backfill CLI is the only writer.
	Enabled bool `yaml:"enabled" doc:"Generate a short topic label per chunk (one LLM call each)."`

	// Model is the chat model identifier. Empty leaves the
	// chat router's default in place. Recommended: "gpt-oss:120b"
	// for quality, smaller OSS models for cost.
	Model string `yaml:"model"`

	// TimeoutSeconds caps a single LLM call. Default 30.
	TimeoutSeconds int `yaml:"timeout_seconds"`

	// MaxPreviewBytes caps the chunk body sent to the LLM. Default
	// 2048 — chunks are typically 2k tokens (~8 KiB), so this
	// targets the head of each chunk where the topic is densest.
	MaxPreviewBytes int `yaml:"max_preview_bytes"`

	// AutoBackfillIntervalSeconds is the cadence of the background
	// retry loop that fills in titles missed by the inline titler
	// (LLM timeout, empty response, model momentarily unavailable).
	// Without this loop a chunk that fails to title once stays NULL
	// forever — display falls back to H1/source name, which is
	// strictly worse than the LLM label. Default 300 (5 minutes);
	// 0 disables the loop entirely (operator runs the backfill CLI
	// on demand).
	AutoBackfillIntervalSeconds int `yaml:"auto_backfill_interval_seconds"`

	// AutoBackfillBatchSize caps how many NULL-title chunks one
	// background tick processes. Each chunk costs one LLM round-
	// trip, so the cap throttles spend when a large ingest leaves
	// many chunks to backfill. Default 25.
	AutoBackfillBatchSize int `yaml:"auto_backfill_batch_size"`
}

// MemoryGraphConfig configures the knowledge-graph extraction
// worker + per-stage model selection. Per LLD §4.4a, the cheap
// stages (extractor / resolver / validator) run on a small OSS
// model while the relationship stage uses a larger reasoning
// model. Defaults route everything to cost-efficient Bedrock IDs:
//
//	openai.gpt-oss-20b-1:0   for extractor / resolver / validator
//	openai.gpt-oss-120b-1:0  for relationship
//
// All four model fields go through the chat router by string
// prefix; operators on different deployments can pin to whatever
// the router knows about (anthropic.*, qwen.*, nvidia.*, etc.).
//
// Tuning note (2026-05-25 audit): the live KG showed 67% of
// chunks producing zero entity mentions, with sampled chunks
// containing clearly extractable in-vocab content. The
// post-2026-05-25 prompt update (few-shot research/CV/review
// examples) is the cheap fix; if the empty-extraction rate
// (`vornik_memory_graph_extractor_outcomes_total{outcome=
// "empty_response"}`) stays elevated, bumping ExtractorModel to
// a 30b+ open-weight (qwen.qwen3-6-35b, gpt-oss-120b) is the
// recommended next step. The trade-off is per-chunk LLM cost
// for materially better recall. See BACKLOG §"KG extractor
// under-extracts on research-style chunks".
type MemoryGraphConfig struct {
	Enabled bool `yaml:"enabled" doc:"Knowledge-graph extraction pipeline."`

	// Per-stage model IDs. Empty means "fall back to default" —
	// extractor/resolver/validator default to gpt-oss-20b,
	// relationship defaults to gpt-oss-120b.
	//
	// ExtractorModel: candidate bump for low-recall deployments —
	// see the audit note on this struct's doc above. The
	// extractor_outcomes_total{outcome="empty_response"} metric
	// is the decision-driver. No code default change because
	// upgrading the model raises per-chunk cost; operators with
	// the audit symptom should set this explicitly.
	ExtractorModel    string `yaml:"extractor_model"`
	ResolverModel     string `yaml:"resolver_model"`
	RelationshipModel string `yaml:"relationship_model"`
	ValidatorModel    string `yaml:"validator_model"`

	// PollIntervalSeconds between drain ticks. Default 30.
	PollIntervalSeconds int `yaml:"poll_interval_seconds"`

	// BatchSize caps how many chunks one tick processes. Default 10.
	BatchSize int `yaml:"batch_size"`

	// MaxParallel — concurrent pipeline runs per tick. Default 1.
	MaxParallel int `yaml:"max_parallel"`

	// GaugeRefreshSeconds is the cadence for refreshing the
	// catalog Prometheus gauges (chunks_pending / chunks_done /
	// entities / edges / mentions / entities_by_type). Decoupled
	// from PollIntervalSeconds because a single tick can take
	// many minutes (the relationship stage on gpt-oss-120b runs
	// serially) — operators want dashboards updating much faster
	// than that. Default 30.
	GaugeRefreshSeconds int `yaml:"gauge_refresh_seconds"`
}

// VoiceConfig wires the daemon-level speech-to-text + text-to-
// speech providers. The channel adapters (Telegram, Slack) pick
// them up at boot; with both sub-blocks empty voice is disabled
// across the daemon. Either sub-block may be filled
// independently — STT-only deployments accept voice inbound and
// reply in text, TTS-only deployments speak back replies without
// transcribing inbound. See
// https://docs.vornik.io
type VoiceConfig struct {
	STT VoiceSTTConfig `yaml:"stt"`
	TTS VoiceTTSConfig `yaml:"tts"`
}

// VoiceSTTConfig configures the speech-to-text provider. The MVP
// ships one implementation: provider=="whisper-local" wraps the
// whisper.cpp `main` CLI binary. Other provider names parse but
// produce a startup warning and a nil provider (voice inbound
// falls back to the attachment path).
type VoiceSTTConfig struct {
	// Provider selects the implementation. "whisper-local" is the
	// only supported value today; empty disables STT.
	Provider string `yaml:"provider" doc:"Speech-to-text provider (whisper-local)."`

	// Model is the absolute path to the ggml model file
	// (whisper-local).
	Model string `yaml:"model" doc:"Absolute path to the STT model file."`

	// BinaryPath is the absolute path to the whisper.cpp CLI.
	// Empty asks exec.LookPath("whisper-cpp") then "main".
	BinaryPath string `yaml:"binary_path"`

	// FFmpegPath is the absolute path to ffmpeg. Empty asks
	// exec.LookPath("ffmpeg"). Required at runtime — whisper.cpp
	// can't read OGG/Opus or MP4/M4A natively.
	FFmpegPath string `yaml:"ffmpeg_path"`

	// LanguageHint is an optional BCP-47 nudge for the recogniser
	// (e.g. "en"). Empty leaves auto-detect.
	LanguageHint string `yaml:"language_hint"`

	// Threads pins the OMP thread count. Zero defers to
	// whisper.cpp's default.
	Threads int `yaml:"threads"`
}

// VoiceTTSConfig configures the text-to-speech provider. The MVP
// ships one implementation: provider=="piper" wraps the Piper CLI
// binary. Other provider names parse but produce a startup
// warning and a nil provider (outbound replies stay text).
type VoiceTTSConfig struct {
	// Provider selects the implementation. "piper" is the only
	// supported value today; empty disables TTS.
	Provider string `yaml:"provider" doc:"Text-to-speech provider (piper)."`

	// Voice is the absolute path to the Piper voice model (.onnx).
	Voice string `yaml:"voice" doc:"Absolute path to the TTS voice model."`

	// BinaryPath is the absolute path to the piper CLI. Empty asks
	// exec.LookPath("piper").
	BinaryPath string `yaml:"binary_path"`

	// FFmpegPath is the absolute path to ffmpeg used for the
	// WAV→ogg-opus / WAV→mp4-aac transcode. Empty asks
	// exec.LookPath("ffmpeg").
	FFmpegPath string `yaml:"ffmpeg_path"`

	// Speed is the synthesis playback speed (1.0 = natural). Zero
	// falls back to 1.0.
	Speed float64 `yaml:"speed"`

	// MaxTextRunes caps one synthesis call so a wide LLM reply
	// can't overflow the platform's voice envelope. Zero falls
	// back to the provider default (1500 runes ~ 90s).
	MaxTextRunes int `yaml:"max_text_runes"`
}

// NodeConfig declares which responsibilities this node carries. A named
// `profile` selects a preset (see ResolveNodeProfile); any explicitly-set
// flag overrides the preset. Absent block → profile "all" → everything on.
type NodeConfig struct {
	Profile string `yaml:"profile" doc:"Role preset: all|ui|worker|webhook. Default all."`

	// Capability overrides. Pointers so "unset" (nil) is distinguishable
	// from "explicitly false" — nil takes the preset value, non-nil overrides.
	ServeUI       *bool `yaml:"serve_ui" doc:"Mount /ui routes."`
	ServeAPI      *bool `yaml:"serve_api" doc:"Mount data-plane / control endpoints."`
	ServeWebhooks *bool `yaml:"serve_webhooks" doc:"Mount public webhook ingress."`
	RunWorkers    *bool `yaml:"run_workers" doc:"Run scheduler+executor and leader-elected singletons."`

	// Relay configures the DMZ webhook → job-tier hand-off. Required when
	// ServeWebhooks && !RunWorkers; forbidden otherwise. Wired in Slice B.
	Relay RelayConfig `yaml:"relay"`

	// RelayIngress configures the job-tier mTLS listener that receives
	// relayed webhooks from DMZ nodes. Only meaningful on RunWorkers nodes.
	RelayIngress RelayIngressConfig `yaml:"relay_ingress"`
}

// RelayConfig is the DMZ webhook node's mTLS hand-off to the job tier.
// Fields are consumed in Slice B; declared here so Slice A validation can
// enforce presence/absence rules.
type RelayConfig struct {
	Upstream   string `yaml:"upstream" doc:"Job-tier internal ingress base URL (https://host:8443)."`
	ClientCert string `yaml:"client_cert" doc:"PEM client cert for mTLS to the job tier."`
	ClientKey  string `yaml:"client_key" doc:"PEM client key for mTLS to the job tier."`
	CA         string `yaml:"ca" doc:"PEM CA bundle that signed the job-tier server cert."`
	MaxRetries int    `yaml:"max_retries" doc:"Bounded relay retries before 5xx to provider. 0 → 3."`
	Timeout    string `yaml:"timeout" doc:"Per-relay-attempt timeout. Empty → 5s."`
}

// RelayIngressConfig is the job-tier server side of the webhook relay: a
// dedicated mTLS listener that only accepts client certs signed by ClientCA.
type RelayIngressConfig struct {
	Addr       string `yaml:"addr" doc:"Listen address for the mTLS relay ingress, e.g. :8443."`
	ServerCert string `yaml:"server_cert" doc:"PEM server cert presented to relaying webhook nodes."`
	ServerKey  string `yaml:"server_key" doc:"PEM server key for the relay ingress."`
	ClientCA   string `yaml:"client_ca" doc:"PEM CA bundle; only client certs it signed are accepted."`
}

// NodeCapabilities is the resolved, override-applied capability set the
// container gates on. RelayMode is derived (ServeWebhooks && !RunWorkers).
type NodeCapabilities struct {
	ServeUI       bool
	ServeAPI      bool
	ServeWebhooks bool
	RunWorkers    bool
	RelayMode     bool
}
