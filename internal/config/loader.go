// Package config provides configuration loading and validation for vornik.
package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Load reads configuration from the specified path with the following precedence:
// 1. --config flag (highest priority)
// 2. VORNIK_CONFIG environment variable
// 3. ./vornik.yaml or ./config.yaml (current directory)
// 4. $XDG_CONFIG_HOME/vornik/{config,vornik}.yaml (user config, defaults to $HOME/.config)
// 5. /etc/vornik/vornik.yaml (system config)
//
// If no config file is found, returns a default configuration. Both
// `config.yaml` and `vornik.yaml` are accepted as filenames at each
// search location — the systemd unit template uses `config.yaml`,
// while older docs and some CI configs use `vornik.yaml`. Supporting
// both avoids a silent "CLI can't find what the daemon uses" gap.
func Load() (*Config, string, error) {
	// Parse flags
	var (
		configPath  = flag.String("config", "", "Path to configuration file")
		showVersion = flag.Bool("version", false, "Show version information")
	)
	flag.Parse()

	if *showVersion {
		return nil, "", ErrVersionRequested
	}

	// Determine config path with precedence
	path := resolveConfigPath(*configPath)

	cfg, err := LoadFromPath(path)
	if err != nil {
		return nil, path, err
	}
	return cfg, path, nil
}

// LoadFromPath parses and validates the config at path (or returns a validated
// DefaultConfig when path is empty), applying the same alias-mirroring, env
// overrides, auth defaults, and secret resolution that Load does — but WITHOUT
// touching flags or any other process-global state. This makes it safe to call
// at runtime for a scoped config hot-reload (re-read config.yaml and re-apply
// the keys that are safe to change live). Load delegates to it after resolving
// the path from flags/env.
func LoadFromPath(path string) (*Config, error) {
	cfg := DefaultConfig()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}

		// Parse based on file extension
		if strings.HasSuffix(path, ".json") {
			if err := json.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("failed to parse JSON config: %w", err)
			}
		} else {
			// Default to YAML
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("failed to parse YAML config: %w", err)
			}
		}
	}

	// Accept both the current `storage.artifacts_path` shape and the guide's
	// `artifacts.storagePath` shape.
	if cfg.Artifacts.ArtifactsPath != "" {
		cfg.Storage.ArtifactsPath = cfg.Artifacts.ArtifactsPath
	}
	// Mirror the new backend selector + S3 block from the alias section
	// (`artifacts.backend`/`artifacts.s3`) into the canonical Storage
	// block, so callers that read cfg.Storage see both shapes.
	if cfg.Artifacts.Backend != "" && cfg.Storage.Backend == "" {
		cfg.Storage.Backend = cfg.Artifacts.Backend
	}
	if cfg.Storage.S3.Endpoint == "" && cfg.Artifacts.S3.Endpoint != "" {
		cfg.Storage.S3.Endpoint = cfg.Artifacts.S3.Endpoint
	}
	if cfg.Storage.S3.Region == "" && cfg.Artifacts.S3.Region != "" {
		cfg.Storage.S3.Region = cfg.Artifacts.S3.Region
	}
	if cfg.Storage.S3.Bucket == "" && cfg.Artifacts.S3.Bucket != "" {
		cfg.Storage.S3.Bucket = cfg.Artifacts.S3.Bucket
	}
	if cfg.Storage.S3.Prefix == "" && cfg.Artifacts.S3.Prefix != "" {
		cfg.Storage.S3.Prefix = cfg.Artifacts.S3.Prefix
	}
	if cfg.Storage.S3.AccessKeyID == "" && cfg.Artifacts.S3.AccessKeyID != "" {
		cfg.Storage.S3.AccessKeyID = cfg.Artifacts.S3.AccessKeyID
	}
	if cfg.Storage.S3.SecretAccessKey == "" && cfg.Artifacts.S3.SecretAccessKey != "" {
		cfg.Storage.S3.SecretAccessKey = cfg.Artifacts.S3.SecretAccessKey
	}
	if !cfg.Storage.S3.UsePathStyle && cfg.Artifacts.S3.UsePathStyle {
		cfg.Storage.S3.UsePathStyle = true
	}
	if cfg.Storage.S3.ForceSSL == nil && cfg.Artifacts.S3.ForceSSL != nil {
		cfg.Storage.S3.ForceSSL = cfg.Artifacts.S3.ForceSSL
	}

	// Source `<configDir>/secrets/*.env` into the process environment
	// before resolving placeholders + env overrides, so secrets written
	// by onboarding (e.g. chat.env's VORNIK_CHAT_API_KEY) activate on
	// every deployment without relying on systemd EnvironmentFile= or a
	// compose env_file. Idempotent + fill-empties-only (see the function
	// doc), so explicit deployment env still wins and re-running on a hot
	// reload is safe.
	sourceSecretsEnvFiles(path)

	expandEnvPlaceholders(cfg)

	// Override with environment variables
	applyEnvOverrides(cfg)

	// Apply defaults that depend on the parsed (and env-overridden) config.
	applyAuthDefaults(cfg)

	// Resolve provider secret files → inline ClientSecret.
	if err := resolveAuthSecrets(cfg); err != nil {
		return nil, err
	}

	// Resolve the trading-auth HMAC secret file → inline Secret.
	if err := resolveTradingSecret(cfg); err != nil {
		return nil, err
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return cfg, nil
}

// ValidateFile reads the YAML at path into a DefaultConfig and calls Validate.
// It does NOT call flag.Parse or touch any global state, making it safe to
// invoke at runtime (e.g. after writing a new config.yaml to verify the edit
// is parseable before restarting the daemon).
func ValidateFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := ValidateBytes(data); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// ValidateBytes parses YAML config bytes into a DefaultConfig and runs the same
// validation the daemon enforces at startup — WITHOUT reading any file, touching
// flags, or other process-global state. It is the in-memory analogue of
// ValidateFile, used to confirm freshly-generated config bytes would start the
// daemon BEFORE they are written to disk (the gen-config bootstrap path). It
// deliberately does NOT apply env overrides or resolve secret files: it answers
// "is this YAML, on its own, a valid daemon config?" rather than "what would the
// running process see after env/secret resolution?".
func ValidateBytes(data []byte) error {
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	return nil
}

func expandEnvPlaceholders(v interface{}) {
	expandEnvValue(reflect.ValueOf(v))
}

func expandEnvValue(v reflect.Value) {
	if !v.IsValid() {
		return
	}

	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		expandEnvValue(v.Elem())
		return
	}

	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			if field.CanSet() || field.Kind() == reflect.Struct || field.Kind() == reflect.Map || field.Kind() == reflect.Slice || field.Kind() == reflect.Pointer {
				expandEnvValue(field)
			}
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(os.ExpandEnv(v.String()))
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			expandEnvValue(v.Index(i))
		}
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return
		}
		iter := v.MapRange()
		for iter.Next() {
			key := iter.Key()
			value := iter.Value()
			switch value.Kind() {
			case reflect.String:
				v.SetMapIndex(key, reflect.ValueOf(os.ExpandEnv(value.String())).Convert(v.Type().Elem()))
			case reflect.Interface, reflect.Pointer, reflect.Struct, reflect.Map, reflect.Slice:
				copied := reflect.New(value.Type()).Elem()
				copied.Set(value)
				expandEnvValue(copied)
				v.SetMapIndex(key, copied)
			}
		}
	}
}

// resolveConfigPath determines the configuration file path. Walks a
// fixed priority list and returns the first existing file. Both
// `vornik.yaml` and `config.yaml` are accepted at each filesystem
// location to match both the systemd unit template (`config.yaml`)
// and older conventions (`vornik.yaml`) without forcing operators
// to rename their file.
func resolveConfigPath(flagPath string) string {
	// 1. Flag has highest priority
	if flagPath != "" {
		return flagPath
	}

	// 2. Environment variable
	if envPath := os.Getenv("VORNIK_CONFIG"); envPath != "" {
		return envPath
	}

	// 3. Current directory — either filename
	for _, name := range []string{"vornik.yaml", "config.yaml"} {
		if _, err := os.Stat(name); err == nil {
			return name
		}
	}

	// 4. User config ($XDG_CONFIG_HOME/vornik/ or $HOME/.config/vornik/).
	//    Matches the systemd-unit default so `vornikctl` sees the same
	//    config the daemon is reading.
	for _, dir := range userConfigDirs() {
		for _, name := range []string{"config.yaml", "vornik.yaml"} {
			candidate := dir + "/" + name
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}

	// 5. System config
	for _, name := range []string{"/etc/vornik/vornik.yaml", "/etc/vornik/config.yaml"} {
		if _, err := os.Stat(name); err == nil {
			return name
		}
	}

	// No config file found, use defaults
	return ""
}

// userConfigDirs returns the XDG-style user config directories to search,
// in priority order. $XDG_CONFIG_HOME wins when set (spec-compliant);
// otherwise falls back to $HOME/.config. Empty if neither is available.
func userConfigDirs() []string {
	var dirs []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		dirs = append(dirs, xdg+"/vornik")
	}
	if home := os.Getenv("HOME"); home != "" {
		dirs = append(dirs, home+"/.config/vornik")
	}
	return dirs
}

// applyEnvOverrides applies environment variable overrides to the configuration.
// parseEnvBool interprets a boolean environment-variable value. It
// accepts the strconv.ParseBool set plus the common operator spellings
// yes/no/on/off/y/n (case-insensitive, surrounding whitespace
// trimmed). ok=false means the value was not recognised — callers
// leave the existing config value unchanged rather than silently
// coercing an unrecognised value to false.
//
// bug sweep 2026-06-04: the old `EqualFold(v,"true") || v=="1"` idiom
// turned VORNIK_STORAGE_S3_FORCE_SSL=yes into false, silently
// disabling TLS enforcement on S3 — an operator who typed "yes"
// believing they had enabled it got the opposite.
func parseEnvBool(v string) (val bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, true
	case "0", "f", "false", "n", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("VORNIK_SERVER_ADDRESS"); v != "" {
		cfg.Server.Address = v
	}
	if v := os.Getenv("VORNIK_SERVER_UNIX_SOCKET"); v != "" {
		cfg.Server.UnixSocket = v
	}
	if v := os.Getenv("VORNIK_DATABASE_HOST"); v != "" {
		cfg.Database.Host = v
	}
	if v := os.Getenv("VORNIK_DATABASE_PORT"); v != "" {
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil {
			cfg.Database.Port = port
		}
	}
	if v := os.Getenv("VORNIK_DATABASE_NAME"); v != "" {
		cfg.Database.Name = v
	}
	if v := os.Getenv("VORNIK_DATABASE_USER"); v != "" {
		cfg.Database.User = v
	}
	if v := os.Getenv("VORNIK_DATABASE_PASSWORD"); v != "" {
		cfg.Database.Password = v
	}
	if v := os.Getenv("VORNIK_DATABASE_SSLMODE"); v != "" {
		cfg.Database.SSLMode = v
	}

	// VORNIK_DATA_DIR is a base data directory used as a fallback for artifact
	// storage when no explicit path is configured. An explicit artifacts path
	// (from the config file or VORNIK_ARTIFACTS_PATH) always wins.
	if v := os.Getenv("VORNIK_ARTIFACTS_PATH"); v != "" {
		cfg.Storage.ArtifactsPath = v
		cfg.Artifacts.ArtifactsPath = v
	} else if cfg.Storage.ArtifactsPath == "" {
		if v := os.Getenv("VORNIK_DATA_DIR"); v != "" {
			artifactsPath := v + "/artifacts"
			cfg.Storage.ArtifactsPath = artifactsPath
			cfg.Artifacts.ArtifactsPath = artifactsPath
		}
	}

	// Storage backend selection + S3 credentials. The pattern mirrors
	// VORNIK_DATABASE_PASSWORD: secrets prefer env over file. A
	// non-empty VORNIK_STORAGE_BACKEND flips the backend; empty
	// preserves whatever the config file said.
	if v := os.Getenv("VORNIK_STORAGE_BACKEND"); v != "" {
		cfg.Storage.Backend = v
		cfg.Artifacts.Backend = v
	}
	if v := os.Getenv("VORNIK_STORAGE_S3_ENDPOINT"); v != "" {
		cfg.Storage.S3.Endpoint = v
		cfg.Artifacts.S3.Endpoint = v
	}
	if v := os.Getenv("VORNIK_STORAGE_S3_REGION"); v != "" {
		cfg.Storage.S3.Region = v
		cfg.Artifacts.S3.Region = v
	}
	if v := os.Getenv("VORNIK_STORAGE_S3_BUCKET"); v != "" {
		cfg.Storage.S3.Bucket = v
		cfg.Artifacts.S3.Bucket = v
	}
	if v := os.Getenv("VORNIK_STORAGE_S3_PREFIX"); v != "" {
		cfg.Storage.S3.Prefix = v
		cfg.Artifacts.S3.Prefix = v
	}
	if v := os.Getenv("VORNIK_STORAGE_S3_ACCESS_KEY_ID"); v != "" {
		cfg.Storage.S3.AccessKeyID = v
		cfg.Artifacts.S3.AccessKeyID = v
	}
	if v := os.Getenv("VORNIK_STORAGE_S3_SECRET_ACCESS_KEY"); v != "" {
		cfg.Storage.S3.SecretAccessKey = v
		cfg.Artifacts.S3.SecretAccessKey = v
	}
	if v := os.Getenv("VORNIK_STORAGE_S3_USE_PATH_STYLE"); v != "" {
		if on, ok := parseEnvBool(v); ok {
			cfg.Storage.S3.UsePathStyle = on
			cfg.Artifacts.S3.UsePathStyle = on
		}
	}
	if v := os.Getenv("VORNIK_STORAGE_S3_FORCE_SSL"); v != "" {
		if on, ok := parseEnvBool(v); ok {
			cfg.Storage.S3.ForceSSL = &on
			cfg.Artifacts.S3.ForceSSL = &on
		}
	}

	if v := os.Getenv("VORNIK_METRICS_ENABLED"); v != "" {
		if on, ok := parseEnvBool(v); ok {
			cfg.Metrics.Enabled = on
		}
	}
	if v := os.Getenv("VORNIK_METRICS_ADDR"); v != "" {
		cfg.Metrics.Addr = v
	}
	if v := os.Getenv("VORNIK_TRACING_ENABLED"); v != "" {
		if on, ok := parseEnvBool(v); ok {
			cfg.Tracing.Enabled = on
		}
	}
	if v := os.Getenv("VORNIK_TRACING_ENDPOINT"); v != "" {
		cfg.Tracing.Endpoint = v
	}
	if v := os.Getenv("VORNIK_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("VORNIK_RUNTIME_USERNS_MODE"); v != "" {
		cfg.Runtime.UserNSMode = v
	}
	if v := os.Getenv("VORNIK_RUNTIME_RUN_AS_USER"); v != "" {
		cfg.Runtime.RunAsUser = v
	}
}

// DefaultConfig returns a configuration with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		// Steering notifications default ON — a task that needs the operator
		// should reach them on the channel they used (opt-out via YAML).
		SteeringNotificationsEnabled: true,
		Server: ServerConfig{
			Address:     ":8080",
			ReadTimeout: "30s",
			// WriteTimeout sized for LLM proxy calls: chat.DefaultTimeout
			// (300s) plus 60s headroom so a provider that pushes right up
			// to the per-call limit still has time to flush its response
			// before the HTTP server kills the connection. Plain REST
			// endpoints finish in milliseconds, so the generous default
			// only matters for /api/v1/chat/completions.
			WriteTimeout: "360s",
		},
		Database: DatabaseConfig{
			Driver:   "postgres",
			Host:     "localhost",
			Port:     5432,
			Name:     "vornik",
			User:     "vornik",
			Password: "vornik",
			SSLMode:  "disable",
		},
		Storage: StorageConfig{
			ArtifactsPath: "/var/lib/vornik/artifacts",
		},
		Runtime: RuntimeConfig{},
		Scheduler: SchedulerConfig{
			MaxConcurrentTasks: 4,
			LeaseTimeout:       "5m",
		},
		// ApprovalTimeoutHours pre-set so an omitted key keeps the 96h
		// default (the watchdog cancels stale AWAITING_APPROVAL tasks);
		// an explicit `approval_timeout_hours: 0` disables expiry.
		Autonomy: AutonomyConfig{
			ApprovalTimeoutHours: 96,
		},
		Watchdog: WatchdogConfig{
			// Default on so a fresh deployment gets stuck-execution
			// surfacing without operator action. Default action
			// flipped warn → fail on 2026-05-13 after a ghost-RUNNING
			// incident: warn-only let stale execution rows sit at
			// RUNNING in the dashboard while the scheduler had moved
			// on to the next retry attempt, leading the operator to
			// cancel live tasks thinking multiple were running in
			// parallel. The 30m threshold is well above any
			// legitimate single-step duration we ship, so the action
			// is safe to take automatically. Operators with longer
			// legitimate steps revert to "warn" in the project YAML.
			//
			// Enabled is a pointer so a YAML that omits the field
			// keeps this default; only an explicit `enabled: false`
			// disables the safety net.
			Enabled:        boolPtr(true),
			Interval:       "60s",
			StuckThreshold: "30m",
			Action:         "fail",
		},
		Secrets: SecretsConfig{
			// Default on. Curated patterns + default allowlist
			// + entropy at compiled defaults. Operators tune
			// per-checkpoint actions in YAML if defaults are
			// too aggressive.
			Enabled: true,
		},
		EffectiveCost: EffectiveCostConfig{
			// Default on. Operator gets one Telegram message per
			// (role, model) every 12h when 24h $/success is 2x+
			// over the 7d baseline. Tuned to fire on real
			// regressions; tighten via project YAML if too noisy.
			Enabled:            true,
			Interval:           "1h",
			CurrentWindow:      "24h",
			BaselineWindow:     "168h",
			RatioThreshold:     2.0,
			MinCurrentSpendUSD: 0.10,
			MinBaselineOks:     5,
			Cooldown:           "12h",
		},
		Metrics: MetricsConfig{
			Enabled: false,
			Addr:    ":9090",
		},
		Tracing: TracingConfig{
			Enabled:  false,
			Endpoint: "localhost:4317",
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
		},
		API: APIConfig{
			AuthEnabled: true,
		},
		Chat: ChatConfig{
			MaxConcurrentRequests: 1,
		},
	}
}

// Validate validates the configuration.
func (c *Config) Validate() error {
	if c.Server.Address == "" {
		return fmt.Errorf("server address is required")
	}
	driver := c.Database.Driver
	if driver == "" {
		driver = "postgres"
	}
	switch driver {
	case "postgres":
		if c.Database.Host == "" {
			return fmt.Errorf("database host is required")
		}
		if c.Database.Port <= 0 {
			return fmt.Errorf("database port must be greater than zero")
		}
		if c.Database.Name == "" {
			return fmt.Errorf("database name is required")
		}
		if c.Database.User == "" {
			return fmt.Errorf("database user is required")
		}
	case "sqlite":
		if c.Database.Path == "" {
			return fmt.Errorf("database path is required for sqlite driver")
		}
	default:
		return fmt.Errorf("unsupported database driver: %s", driver)
	}
	if c.Logging.Level != "" {
		validLevels := map[string]bool{
			"debug": true,
			"info":  true,
			"warn":  true,
			"error": true,
		}
		if !validLevels[strings.ToLower(c.Logging.Level)] {
			return fmt.Errorf("invalid log level: %s", c.Logging.Level)
		}
	}
	if c.API.AuthEnabled && c.API.AuthDryRun {
		return fmt.Errorf("api.auth_dry_run is an evaluation mode; remove it when setting api.auth_enabled: true")
	}
	if c.API.AuthEnabled {
		if len(c.API.APIKeys) == 0 {
			return fmt.Errorf("api.api_keys is required when api.auth_enabled is true")
		}
		for _, key := range c.API.APIKeys {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("api.api_keys must not contain empty values")
			}
		}
	}
	if c.Trading.Auth.Enabled && strings.TrimSpace(c.Trading.Auth.Secret) == "" && c.Trading.Auth.SecretFile == "" {
		return fmt.Errorf("trading.auth.secret (or secret_file) is required when trading.auth.enabled is true")
	}
	if c.Trading.Auth.ClockSkew != "" {
		if _, err := time.ParseDuration(c.Trading.Auth.ClockSkew); err != nil {
			return fmt.Errorf("invalid trading.auth.clock_skew %q: %w", c.Trading.Auth.ClockSkew, err)
		}
	}
	for _, b := range c.Auth.BootstrapAdmins {
		entry := strings.TrimSpace(b)
		channel, externalID, ok := strings.Cut(entry, ":")
		if !ok || channel == "" || externalID == "" || entry != b ||
			channel != strings.TrimSpace(channel) || externalID != strings.TrimSpace(externalID) {
			return fmt.Errorf("auth.bootstrap_admins entry %q must be \"channel:external_id\" with no surrounding whitespace (e.g. \"google:vadim@vornik.io\")", b)
		}
		if channel == "github" {
			id, err := strconv.ParseInt(externalID, 10, 64)
			if err != nil || id <= 0 {
				return fmt.Errorf("auth.bootstrap_admins entry %q must use the immutable numeric GitHub account ID, not a login name", b)
			}
		}
	}
	// Phase-3 login config validation.
	if c.Auth.Providers.GitHub != nil {
		// Postgres prerequisite: the identity core (users / ui_sessions
		// tables + the Identity repository) ships only in the Postgres
		// migrations. On sqlite, buildSessionLogin finds a nil
		// Identity/UISessions repo and silently disables login with only
		// a WARN — an operator who configured a login provider then can't
		// log in and gets no hard signal. Refuse to boot instead.
		// (Hardening 2026-06-15, auth LLD review batch 2.)
		if driver == "sqlite" {
			return fmt.Errorf("auth.providers.github requires the postgres driver: the identity core (users/ui_sessions) is not available on sqlite, so login would be silently disabled")
		}
		// external_base_url is required when any provider is configured.
		if c.Auth.ExternalBaseURL == "" {
			return fmt.Errorf("auth.external_base_url is required when a login provider is configured")
		}
		// Validate URL: must have http/https scheme, non-empty host, no path
		// (or bare "/"), no query, no fragment.
		if err := validateExternalBaseURL(c.Auth.ExternalBaseURL); err != nil {
			return err
		}
		// Validate github provider fields.
		gh := c.Auth.Providers.GitHub
		if gh.ClientID == "" {
			return fmt.Errorf("auth.providers.github.client_id is required")
		}
		if gh.ClientSecret != "" && gh.ClientSecretFile != "" {
			return fmt.Errorf("auth.providers.github: set exactly one of client_secret or client_secret_file, not both")
		}
		if gh.ClientSecret == "" && gh.ClientSecretFile == "" {
			return fmt.Errorf("auth.providers.github: one of client_secret or client_secret_file is required")
		}
		// Org-member auto-grant (github-org-member-default-access-design.md).
		switch gh.OrgMemberRole {
		case "", "user", "admin":
		default:
			return fmt.Errorf("auth.providers.github.org_member_role %q must be empty, \"user\", or \"admin\"", gh.OrgMemberRole)
		}
		if gh.OrgMemberRole != "" && gh.Org == "" {
			return fmt.Errorf("auth.providers.github.org_member_role requires auth.providers.github.org to be set (no membership signal to gate on)")
		}
		if gh.OrgMemberRole == "user" && len(gh.OrgMemberProjects) == 0 {
			return fmt.Errorf("auth.providers.github.org_member_role \"user\" requires auth.providers.github.org_member_projects (use [\"*\"] for all projects)")
		}
		// Validate session.lifetime if non-empty (empty is OK — default applied at load).
		if c.Auth.Session.Lifetime != "" {
			if _, err := time.ParseDuration(c.Auth.Session.Lifetime); err != nil {
				return fmt.Errorf("auth.session.lifetime %q is not a valid duration: %w", c.Auth.Session.Lifetime, err)
			}
		}
	} else if c.Auth.ExternalBaseURL != "" {
		// external_base_url set but no provider — still validate the URL shape
		// so operators don't commit a malformed URL that will break at provider-add time.
		if err := validateExternalBaseURL(c.Auth.ExternalBaseURL); err != nil {
			return err
		}
	}
	if c.Metrics.Enabled && c.Metrics.Addr == "" {
		return fmt.Errorf("metrics addr is required when metrics are enabled")
	}
	if c.Tracing.Enabled && c.Tracing.Endpoint == "" {
		return fmt.Errorf("tracing endpoint is required when tracing is enabled")
	}
	if err := c.Storage.Validate(); err != nil {
		return err
	}
	switch c.Memory.PromptInjectionScan {
	case "", "off", "detect", "quarantine":
		// ok — empty/off disable the gate
	default:
		return fmt.Errorf("memory.prompt_injection_scan must be one of off|detect|quarantine, got %q", c.Memory.PromptInjectionScan)
	}
	if c.Runtime.UserNSMode != "" {
		validUserNSModes := map[string]bool{
			"host":    true,
			"keep-id": true,
			"private": true,
		}
		if !validUserNSModes[strings.ToLower(c.Runtime.UserNSMode)] {
			return fmt.Errorf("invalid runtime userns_mode: %s", c.Runtime.UserNSMode)
		}
	}
	if err := c.Node.Validate(); err != nil {
		return err
	}
	return nil
}

// ErrVersionRequested is returned when the --version flag is set.
var ErrVersionRequested = fmt.Errorf("version requested")

// boolPtr returns a *bool pointing at v. Used by DefaultConfig() for
// fields whose YAML key is a pointer-bool so the loader can tell
// "operator omitted the key" (pointer stays at the default) apart
// from "operator wrote enabled: false" (pointer overwritten to false).
func boolPtr(v bool) *bool { return &v }

// validateExternalBaseURL checks that the supplied URL has a http/https
// scheme, a non-empty host, no path (or only a bare "/"), no query, and
// no fragment. This is the shape required for an OAuth callback origin.
func validateExternalBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("auth.external_base_url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("auth.external_base_url %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("auth.external_base_url %q: host is required", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("auth.external_base_url %q: must not include a path (got %q); use scheme://host[:port] only", raw, u.Path)
	}
	if u.RawQuery != "" {
		return fmt.Errorf("auth.external_base_url %q: must not include a query string", raw)
	}
	if u.Fragment != "" {
		return fmt.Errorf("auth.external_base_url %q: must not include a fragment", raw)
	}
	return nil
}

// applyAuthDefaults applies session-lifetime default ("168h") when the
// field was not set by the operator. Called after parse + env-overrides;
// no env override exists for session.lifetime.
func applyAuthDefaults(cfg *Config) {
	if cfg.Auth.Session.Lifetime == "" {
		cfg.Auth.Session.Lifetime = "168h"
	}
}

// resolveAuthSecrets reads provider ClientSecretFile paths, trims
// whitespace from the contents, and stores the result in ClientSecret.
// The file path is cleared after a successful read so downstream code
// only sees ClientSecret. An unreadable path is a fatal startup error.
func resolveAuthSecrets(cfg *Config) error {
	gh := cfg.Auth.Providers.GitHub
	if gh == nil || gh.ClientSecretFile == "" {
		return nil
	}
	data, err := os.ReadFile(gh.ClientSecretFile)
	if err != nil {
		return fmt.Errorf("auth.providers.github.client_secret_file %q: %w", gh.ClientSecretFile, err)
	}
	gh.ClientSecret = strings.TrimSpace(string(data))
	gh.ClientSecretFile = ""
	return nil
}

// resolveTradingSecret reads trading.auth.secret_file into
// trading.auth.secret (whitespace trimmed) and clears the path, so
// downstream code only consults Secret. An unreadable path is a fatal
// startup error — fail closed rather than booting with trading auth
// silently mis-keyed.
func resolveTradingSecret(cfg *Config) error {
	if cfg == nil || cfg.Trading.Auth.SecretFile == "" {
		return nil
	}
	path := cfg.Trading.Auth.SecretFile
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("trading.auth.secret_file %q: %w", path, err)
	}
	cfg.Trading.Auth.Secret = strings.TrimSpace(string(data))
	cfg.Trading.Auth.SecretFile = ""
	return nil
}
