package integrations

import "time"

// Scope decides where an integration's config is written, which secret
// store is used, and who is allowed to touch it (design §5.1, §6).
type Scope string

const (
	// ScopeDaemon integrations live in config.yaml root (e.g. telegram.*)
	// and are admin-only.
	ScopeDaemon Scope = "daemon"
	// ScopeProject integrations live in projects/<id>.yaml (e.g. email,
	// github_app, slack) and are gated by project scope.
	ScopeProject Scope = "project"
)

// CredentialField describes one guided-form field for an IntegrationKind.
type CredentialField struct {
	// Key is the dotted config key relative to the scope root (e.g.
	// "bot_token"). It must resolve to a real yaml key on the config
	// struct the kind's SaveTarget writes into (daemon: internal/config;
	// project: internal/registry.Project*) — see save_targets.go's
	// SaveTarget.ScalarKeys, which maps this Key to the full dotted path.
	Key string
	// Label is the human-readable field name (e.g. "Bot token").
	Label string
	// Secret, when true, routes the value to a secret store instead of
	// being written literally into config: SecretFile picks which of the
	// two secret-placement modes applies (see its doc). Required when
	// Secret is true: either EnvName (env-line mode) or SecretFile
	// (file mode), never neither.
	Secret bool
	// EnvName is the env-var name a Secret field's placement references:
	// for daemon scope, the config value becomes the expandable
	// "${EnvName}" placeholder; for project scope, the config value IS
	// the (per-project-suffixed) env-var name itself, matching
	// registry.Project's "*_env" field convention (e.g.
	// ProjectEmail.IMAPPasswordEnv). Required when Secret is true and
	// SecretFile is false. Ignored when SecretFile is true.
	EnvName string
	// SecretFile, when true (only meaningful alongside Secret), routes
	// the field's literal value to a 0600 file in the scope's secrets
	// dir instead of an env-file entry — the config value written is the
	// FILE'S PATH, not an env-var name or placeholder. Added in task
	// 5.2b for GitHub App's private_key_path, which the real schema
	// models as a filesystem path (registry.ProjectGitHubApp), not an
	// env-expandable string. See SaveTarget.SecretFilePaths for how a
	// kind supplies the path-resolution function.
	SecretFile bool
	// List, when true, means the candidate's literal value is a
	// comma-separated list (e.g. "org/repo1, org/repo2") that must be
	// written as a YAML string sequence, not a scalar — for fields whose
	// real config type is []string (e.g.
	// registry.ProjectGitHubApp.RepoAllowlist). Never combined with
	// Secret in this catalog.
	List bool
	// Int, when true, means the candidate's literal string value must be
	// parsed and written as a YAML integer, not a string scalar — for
	// fields whose real config type is int/int64 (e.g. app_id,
	// installation_id, imap_port). Never combined with Secret in this
	// catalog.
	Int bool
	// DocHint is the always-shown, plain-language "where do I find this?"
	// text (design §5.6).
	DocHint  string
	Required bool
}

// IntegrationKind is one catalog entry — a code change to add one, mirroring
// featuredoctor.Registry() (design §5.1).
type IntegrationKind struct {
	ID          string
	DisplayName string
	Category    string
	Scope       Scope
	DocURL      string
	Fields      []CredentialField
	Prober      Prober
	// ProbeTimeout overrides integrationProbeTimeout for this kind (0 =>
	// use the default).
	ProbeTimeout time.Duration
	// MinProbeInterval throttles re-checks per kind (0 => no per-kind
	// limit). Ships 0 for every kind in v1 (design §5.1) — a config value
	// to raise later, not code.
	MinProbeInterval time.Duration
}

// Registry returns the four Integrations Hub kinds (design §5.1), each
// wired to its Prober through guard — the shared SSRF dialer every probe
// must dial through (design §6). Called fresh by the (later-task) HTTP
// handlers rather than cached at startup, so a future per-request
// AllowedHosts override (e.g. from live config) doesn't require a
// singleton rebuild.
//
// MCP tool servers are deliberately NOT a hub kind (removed 2026-07-10):
// the control-plane hub's MCP-servers tab (/ui/admin/control-plane
// ?section=mcp, internal/ui/admin_control_plane_mcp.go) is the canonical
// management surface — ledger-gated, stdio-capable, with its own probe.
// The hub kind was an exact-but-inferior duplicate (direct config write
// bypassing the proposal ledger, no stdio/command support, and a list
// shape that never fit the hub's one-candidate-per-kind form model; its
// recheck had to be special-cased off). Community builds manage
// mcp.servers in config.yaml directly.
func Registry(guard DialGuard) []IntegrationKind {
	return []IntegrationKind{
		telegramKind(guard),
		emailKind(guard),
		githubAppKind(guard),
		slackKind(guard),
	}
}

func telegramKind(guard DialGuard) IntegrationKind {
	return IntegrationKind{
		ID:          "telegram",
		DisplayName: "Telegram",
		Category:    "Chat channel",
		Scope:       ScopeDaemon,
		DocURL:      "https://docs.vornik.io/integrations/telegram",
		Fields: []CredentialField{
			{
				Key:      "bot_token",
				Label:    "Bot token",
				Secret:   true,
				EnvName:  "TELEGRAM_BOT_TOKEN",
				DocHint:  "Message @BotFather → /newbot → copy the token it gives you",
				Required: true,
			},
		},
		Prober: newTelegramProber(guard.HTTPClient(integrationProbeTimeout), 0),
	}
}

// emailKind's Fields are reconciled against registry.ProjectEmail
// (task 5.2b): the real schema splits IMAP and SMTP into distinct
// host/port/username/password-env fields (not one unified
// username/password pair, as 5.1's catalog had it). IMAP is the
// inbound-required trio (host, username, password env); SMTP + from_address
// are the outbound leg — modeled Required here because emailProber (§5.3)
// probes both legs in one round trip, so the guided form always collects a
// working full setup. ProjectEmail.Enabled() only requires the IMAP trio;
// an operator who genuinely wants inbound-only should use the manual
// project-config-form route instead of the hub.
func emailKind(guard DialGuard) IntegrationKind {
	return IntegrationKind{
		ID:          "email",
		DisplayName: "Email",
		Category:    "Chat channel",
		Scope:       ScopeProject,
		DocURL:      "https://docs.vornik.io/integrations/email",
		Fields: []CredentialField{
			{Key: "imap_host", Label: "IMAP host", Required: true, DocHint: "Your mail provider's IMAP server, e.g. imap.gmail.com"},
			{Key: "imap_port", Label: "IMAP port", Int: true, DocHint: "Usually 993 (implicit TLS); leave blank for the default"},
			{Key: "imap_username", Label: "IMAP username", Required: true, DocHint: "Usually your full email address"},
			{
				Key:      "imap_password_env",
				Label:    "IMAP password",
				Secret:   true,
				EnvName:  "EMAIL_IMAP_PASSWORD",
				DocHint:  "An app-specific password if your provider requires one (e.g. Gmail)",
				Required: true,
			},
			{Key: "smtp_host", Label: "SMTP host", Required: true, DocHint: "Your mail provider's SMTP server, e.g. smtp.gmail.com"},
			{Key: "smtp_port", Label: "SMTP port", Int: true, DocHint: "Usually 587 (STARTTLS); leave blank for the default"},
			{Key: "smtp_username", Label: "SMTP username", Required: true, DocHint: "Usually your full email address"},
			{
				Key:      "smtp_password_env",
				Label:    "SMTP password",
				Secret:   true,
				EnvName:  "EMAIL_SMTP_PASSWORD",
				DocHint:  "Often the same app-specific password as IMAP",
				Required: true,
			},
			{Key: "from_address", Label: "From address", Required: true, DocHint: "The address your outbound replies appear to come from"},
		},
		Prober: newEmailProber(guard, 0),
	}
}

// githubAppKind's Fields are reconciled against registry.ProjectGitHubApp
// (task 5.2b): the private key is a filesystem PATH (private_key_path) in
// the real schema, not inline content — so it's modeled as a SecretFile
// field (the pasted PEM is written to a 0600 file; the config carries the
// path). webhook_secret_env and repo_allowlist are added — absent from
// 5.1's catalog — because ProjectGitHubApp.Enabled() (and Project.Validate's
// cross-field rule) requires both whenever any github_app field is set,
// which the guided form always does.
func githubAppKind(guard DialGuard) IntegrationKind {
	return IntegrationKind{
		ID:          "github_app",
		DisplayName: "GitHub App",
		Category:    "Code forge",
		Scope:       ScopeProject,
		DocURL:      "https://docs.vornik.io/integrations/github-app",
		Fields: []CredentialField{
			{Key: "app_id", Label: "App ID", Int: true, Required: true, DocHint: "Shown on your GitHub App's settings page"},
			{Key: "installation_id", Label: "Installation ID", Int: true, Required: true, DocHint: "Shown in the URL after installing the App on your org/repo"},
			{
				Key:        "private_key_path",
				Label:      "Private key",
				Secret:     true,
				SecretFile: true,
				DocHint:    "Generate a private key on your GitHub App's settings page and paste the whole .pem file",
				Required:   true,
			},
			{
				Key:      "webhook_secret_env",
				Label:    "Webhook secret",
				Secret:   true,
				EnvName:  "GITHUB_APP_WEBHOOK_SECRET",
				DocHint:  "The secret you set on your GitHub App's webhook configuration page",
				Required: true,
			},
			{
				Key:      "repo_allowlist",
				Label:    "Allowed repositories",
				List:     true,
				Required: true,
				DocHint:  "Comma-separated owner/repo entries this channel accepts events from, e.g. myorg/myrepo",
			},
			{Key: "api_base_url", Label: "API base URL", DocHint: "Only needed for GitHub Enterprise Server; leave blank for github.com"},
		},
		Prober: newGitHubAppProber(guard.HTTPClient(integrationProbeTimeout), 0),
	}
}

// slackKind's Fields are reconciled against registry.ProjectSlack (task
// 5.2b): team_id and signing_secret_env are added — absent from 5.1's
// catalog — because ProjectSlack.Enabled() requires BOTH alongside the bot
// token before the channel ever activates. bot_token's Key is renamed to
// bot_token_env to match ProjectSlack.BotTokenEnv's real yaml tag (the
// candidate's literal bot-token value still lives under that Key at probe
// time — see CandidateConfig's doc: Values are literal, not persisted
// representations).
func slackKind(guard DialGuard) IntegrationKind {
	return IntegrationKind{
		ID:          "slack",
		DisplayName: "Slack",
		Category:    "Chat channel",
		Scope:       ScopeProject,
		DocURL:      "https://docs.vornik.io/integrations/slack",
		Fields: []CredentialField{
			{Key: "team_id", Label: "Workspace (Team) ID", Required: true, DocHint: "Starts with T — shown in your Slack App's \"Basic Information\" page"},
			{
				Key:      "bot_token_env",
				Label:    "Bot token",
				Secret:   true,
				EnvName:  "SLACK_BOT_TOKEN",
				DocHint:  "From your Slack App's \"OAuth & Permissions\" page — starts with xoxb-",
				Required: true,
			},
			{
				Key:      "signing_secret_env",
				Label:    "Signing secret",
				Secret:   true,
				EnvName:  "SLACK_SIGNING_SECRET",
				DocHint:  "From your Slack App's \"Basic Information\" page, under App Credentials",
				Required: true,
			},
		},
		Prober: newSlackProber(guard.HTTPClient(integrationProbeTimeout), 0),
	}
}
