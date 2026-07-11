package integrations

import (
	"fmt"
	"path/filepath"
)

// SaveTarget describes where one IntegrationKind's fields are written on
// disk and how a Secret field's on-disk value is expressed for that scope
// (design §5.4). It is deliberately separate from IntegrationKind: the
// catalog (§5.1) describes the guided-form shape; this describes the
// write-path target, verified against the real config structs
// (internal/config/config.go, internal/registry/project.go) rather than
// assuming the catalog's Fields already line up 1:1 with them — task 5.2
// wired telegram; task 5.2b reconciled and wired the three
// project-scope kinds (email, github_app, slack), whose 5.1 catalog Fields
// didn't originally match the real registry.Project* schema (see
// catalog.go's per-kind doc comments for the specifics).
type SaveTarget struct {
	// Scope must equal the owning IntegrationKind.Scope; Save cross-checks
	// this defensively.
	Scope Scope
	// ConfigFile resolves the writer's target path. projectID is "" for
	// daemon scope. Returns an error for project scope when projectID
	// fails validateProjectIDForPath (security-audit finding F-1, task
	// 5.2b) — a malformed ProjectID must never be used to build a path.
	ConfigFile func(configDir, projectID string) (string, error)
	// ScalarKeys maps CredentialField.Key -> the full dotted config key
	// SetYAMLKey patches (e.g. "bot_token" -> "telegram.bot_token").
	ScalarKeys map[string]string
	// SecretValue formats a Secret (non-SecretFile) field's EnvName into
	// the literal value written to its config key. Daemon scope embeds an
	// expandable "${NAME}" placeholder (internal/config/loader.go's
	// os.ExpandEnv resolves it generically on every string field at
	// load); project scope returns the bare env-var name itself, matching
	// registry.Project's "*_env" field convention. Required whenever any
	// mapped field is Secret and not SecretFile.
	SecretValue func(envName string) string
	// SecretFilePaths maps a SecretFile field's Key to a function
	// resolving the 0600 file path its literal value is written to, given
	// the deps' secrets dir and the candidate's project ID. Returns an
	// error when projectID fails validateProjectIDForPath. Required for
	// any field with CredentialField.SecretFile true (task 5.2b's
	// github_app private-key file-secret placement mode).
	SecretFilePaths map[string]func(secretsDir, projectID string) (string, error)
}

// daemonConfigFile resolves "<configDir>/config.yaml" — every daemon-scope
// kind targets the same file (design §5.1 table). projectID is always ""
// for daemon scope (Save's Caller/scope gate guarantees this), so there is
// no user-influenced path component here — the fixed "config.yaml" slug is
// not a path-confinement concern (security-audit finding F-1's scope note).
func daemonConfigFile(configDir, _ string) (string, error) {
	return filepath.Join(configDir, "config.yaml"), nil
}

// projectConfigFile resolves "<configDir>/projects/<projectID>.yaml",
// mirroring project_config_form.go's ConfigPath construction. projectID is
// caller/candidate-influenced, so this goes through safeProjectPath
// (validateProjectIDForPath + safepath.JoinUnder, security-audit finding
// F-1, task 5.2b) rather than a hand-rolled filepath.Join — a malformed
// projectID (path separators, "..") must never let a project-scope save
// escape configDir.
func projectConfigFile(configDir, projectID string) (string, error) {
	return safeProjectPath(configDir, projectID, "projects", projectID+".yaml")
}

// daemonPlaceholder is the SecretValue for daemon-scope kinds: an
// expandable "${NAME}" placeholder, matching TelegramConfig.BotToken's
// existing convention (any string field is os.ExpandEnv'd at load).
func daemonPlaceholder(envName string) string { return "${" + envName + "}" }

// projectEnvNameValue is the SecretValue for project-scope kinds: the
// config value for a project-scope "*_env" field IS the bare env-var name
// itself, not an expandable "${NAME}" placeholder — matching
// registry.Project's convention (ProjectEmail.IMAPPasswordEnv,
// ProjectSlack.SigningSecretEnv, ProjectGitHubApp.WebhookSecretEnv, …),
// where the daemon reads os.Getenv(cfg.X.NameEnv) at the point of use
// rather than os.ExpandEnv-ing the field at load time (task 5.2b).
func projectEnvNameValue(envName string) string { return envName }

// githubAppPrivateKeyPath resolves the 0600 file a saved GitHub App's
// pasted private key is written to: "<secretsDir>/github-app-<projectID>.pem"
// (task 5.2b's file-secret placement mode). projectID is candidate-influenced,
// so it is validated FIRST — before it is ever interpolated into the
// filename — and only the validated value is used to build the slug
// (companion review review-20260709-9160 finding 1: an earlier version
// built the "github-app-<projectID>.pem" string via fmt.Sprintf and passed
// it to safeProjectPath, meaning the raw, not-yet-validated projectID was
// already baked into the filename argument before validation ran, even
// though the join itself was still gated — validate-then-build closes that
// ordering gap so a malformed projectID is never used to construct any
// string, not just never used to reach the filesystem). safeProjectPath
// still performs its own (now redundant, harmless) validation plus the
// safepath.JoinUnder confinement, matching projectConfigFile's belt-and-
// suspenders posture.
func githubAppPrivateKeyPath(secretsDir, projectID string) (string, error) {
	if err := validateProjectIDForPath(projectID); err != nil {
		return "", err
	}
	slug := fmt.Sprintf("github-app-%s.pem", projectID)
	return safeProjectPath(secretsDir, projectID, slug)
}

// saveTargets is the verified kind-ID -> SaveTarget table (design §5.4).
// Every catalog kind's real config target has been confirmed against
// internal/config/config.go (daemon scope) / internal/registry/project.go
// (project scope) before it lands here.
var saveTargets = map[string]SaveTarget{
	// Telegram (daemon scope): TelegramConfig.BotToken, yaml:"bot_token"
	// (internal/config/config.go) — a plain string field, ${ENV}-expandable
	// at load, matching the catalog's single Secret "bot_token" field
	// exactly.
	"telegram": {
		Scope:      ScopeDaemon,
		ConfigFile: daemonConfigFile,
		ScalarKeys: map[string]string{
			"bot_token": "telegram.bot_token",
		},
		SecretValue: daemonPlaceholder,
	},
	// MCP tool servers are deliberately absent: the control-plane hub's
	// MCP-servers tab is the canonical (ledger-gated) write surface — see
	// catalog.go's Registry doc for the 2026-07-10 removal rationale.
	// Email (project scope): registry.ProjectEmail (internal/registry/
	// project.go). Every field maps to a real yaml key (task 5.2b's
	// catalog reconciliation) — see catalog.go's emailKind doc for the
	// IMAP/SMTP split rationale.
	"email": {
		Scope:      ScopeProject,
		ConfigFile: projectConfigFile,
		ScalarKeys: map[string]string{
			"imap_host":         "email.imap_host",
			"imap_port":         "email.imap_port",
			"imap_username":     "email.imap_username",
			"imap_password_env": "email.imap_password_env",
			"smtp_host":         "email.smtp_host",
			"smtp_port":         "email.smtp_port",
			"smtp_username":     "email.smtp_username",
			"smtp_password_env": "email.smtp_password_env",
			"from_address":      "email.from_address",
		},
		SecretValue: projectEnvNameValue,
	},
	// Slack (project scope): registry.ProjectSlack. team_id +
	// signing_secret_env are new relative to 5.1's catalog — required for
	// ProjectSlack.Enabled() to ever be true (task 5.2b).
	"slack": {
		Scope:      ScopeProject,
		ConfigFile: projectConfigFile,
		ScalarKeys: map[string]string{
			"team_id":            "slack.team_id",
			"bot_token_env":      "slack.bot_token_env",
			"signing_secret_env": "slack.signing_secret_env",
		},
		SecretValue: projectEnvNameValue,
	},
	// GitHub App (project scope): registry.ProjectGitHubApp. private_key
	// uses the file-secret placement mode (task 5.2b step 3) since the real
	// schema's PrivateKeyPath is a filesystem path, not an env-expandable
	// string; webhook_secret_env + repo_allowlist are new relative to 5.1's
	// catalog — required for ProjectGitHubApp.Enabled()/Project.Validate's
	// cross-field rule once any github_app field is set.
	"github_app": {
		Scope:      ScopeProject,
		ConfigFile: projectConfigFile,
		ScalarKeys: map[string]string{
			"app_id":             "github_app.app_id",
			"installation_id":    "github_app.installation_id",
			"private_key_path":   "github_app.private_key_path",
			"webhook_secret_env": "github_app.webhook_secret_env",
			"repo_allowlist":     "github_app.repo_allowlist",
			"api_base_url":       "github_app.api_base_url",
		},
		SecretValue: projectEnvNameValue,
		SecretFilePaths: map[string]func(string, string) (string, error){
			"private_key_path": githubAppPrivateKeyPath,
		},
	},
}

// SaveTargetForKind returns the verified write-path target for kindID, and
// false if kindID has no write path. All four catalog kinds
// (telegram/email/github_app/slack) resolve as of task 5.2b — every
// entry's Fields were reconciled against the real config target
// (internal/config/config.go for daemon scope, internal/registry/project.go
// for project scope) before being wired here; see save.go and catalog.go
// for the field-split / secret-placement / validation machinery each
// target rides on.
func SaveTargetForKind(kindID string) (SaveTarget, bool) {
	t, ok := saveTargets[kindID]
	return t, ok
}
