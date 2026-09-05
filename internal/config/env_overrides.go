package config

import (
	"fmt"
	"os"
)

// envOverride is one VORNIK_* environment override of the loaded config.
//
// Until 2026-09-03 applyEnvOverrides was 33 hand-written `if v := os.Getenv`
// blocks. As a table the set is enumerable for the first time, which buys
// three things at once (resolved-config provenance design §3.1): the loader
// can record `env` as a value's origin as it applies the override; a test can
// pin the variable set and check every Key is a leaf the config walker knows;
// and the generated configuration reference can show the override per key —
// closing the class in which VORNIK_API_URL was documented nowhere.
type envOverride struct {
	// Env is the variable name.
	Env string
	// Keys are the dotted YAML paths the override sets — two when a value is
	// mirrored into the artifacts.* alias block.
	Keys []string
	// Apply sets the value. An error means the value could not be parsed; the
	// prior value is kept and the origin is recorded as env_invalid rather than
	// the override silently not happening.
	Apply func(cfg *Config, v string) error
	// When, if set, gates the override on the config state at that point in
	// the sequence (VORNIK_DATA_DIR applies only when nothing set the
	// artifacts path).
	When func(cfg *Config) bool
}

// envRecorder receives one call per override that fired: the keys it set (or
// would have), the variable, and the parse error if any. nil-safe.
type envRecorder func(keys []string, env string, err error)

func setString(get func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error { *get(c) = v; return nil }
}

func setStrings(gets ...func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error {
		for _, g := range gets {
			*g(c) = v
		}
		return nil
	}
}

func setBools(gets ...func(*Config) *bool) func(*Config, string) error {
	return func(c *Config, v string) error {
		on, ok := parseEnvBool(v)
		if !ok {
			return fmt.Errorf("%q is not a boolean", v)
		}
		for _, g := range gets {
			*g(c) = on
		}
		return nil
	}
}

// envOverrides is the table, in the order the overrides have always applied.
var envOverrides = []envOverride{
	{Env: "VORNIK_SERVER_ADDRESS", Keys: []string{"server.address"},
		Apply: setString(func(c *Config) *string { return &c.Server.Address })},
	{Env: "VORNIK_SERVER_UNIX_SOCKET", Keys: []string{"server.unix_socket"},
		Apply: setString(func(c *Config) *string { return &c.Server.UnixSocket })},
	{Env: "VORNIK_DATABASE_HOST", Keys: []string{"database.host"},
		Apply: setString(func(c *Config) *string { return &c.Database.Host })},
	{Env: "VORNIK_DATABASE_PORT", Keys: []string{"database.port"},
		Apply: func(c *Config, v string) error {
			var port int
			if _, err := fmt.Sscanf(v, "%d", &port); err != nil {
				return fmt.Errorf("%q is not an integer", v)
			}
			c.Database.Port = port
			return nil
		}},
	{Env: "VORNIK_DATABASE_NAME", Keys: []string{"database.name"},
		Apply: setString(func(c *Config) *string { return &c.Database.Name })},
	{Env: "VORNIK_DATABASE_USER", Keys: []string{"database.user"},
		Apply: setString(func(c *Config) *string { return &c.Database.User })},
	{Env: "VORNIK_DATABASE_PASSWORD", Keys: []string{"database.password"},
		Apply: setString(func(c *Config) *string { return &c.Database.Password })},
	{Env: "VORNIK_DATABASE_SSLMODE", Keys: []string{"database.sslmode"},
		Apply: setString(func(c *Config) *string { return &c.Database.SSLMode })},

	// VORNIK_DATA_DIR is a base data directory used as a fallback for artifact
	// storage when no explicit path is configured. An explicit artifacts path
	// (from the config file or VORNIK_ARTIFACTS_PATH) always wins.
	{Env: "VORNIK_ARTIFACTS_PATH", Keys: []string{"storage.artifacts_path", "artifacts.artifacts_path"},
		Apply: setStrings(func(c *Config) *string { return &c.Storage.ArtifactsPath },
			func(c *Config) *string { return &c.Artifacts.ArtifactsPath })},
	{Env: "VORNIK_DATA_DIR", Keys: []string{"storage.artifacts_path", "artifacts.artifacts_path"},
		When: func(c *Config) bool { return c.Storage.ArtifactsPath == "" },
		Apply: func(c *Config, v string) error {
			c.Storage.ArtifactsPath = v + "/artifacts"
			c.Artifacts.ArtifactsPath = v + "/artifacts"
			return nil
		}},

	// Storage backend selection + S3 credentials. The pattern mirrors
	// VORNIK_DATABASE_PASSWORD: secrets prefer env over file. A non-empty
	// VORNIK_STORAGE_BACKEND flips the backend; empty preserves whatever the
	// config file said.
	{Env: "VORNIK_STORAGE_BACKEND", Keys: []string{"storage.backend", "artifacts.backend"},
		Apply: setStrings(func(c *Config) *string { return &c.Storage.Backend },
			func(c *Config) *string { return &c.Artifacts.Backend })},
	{Env: "VORNIK_STORAGE_S3_ENDPOINT", Keys: []string{"storage.s3.endpoint", "artifacts.s3.endpoint"},
		Apply: setStrings(func(c *Config) *string { return &c.Storage.S3.Endpoint },
			func(c *Config) *string { return &c.Artifacts.S3.Endpoint })},
	{Env: "VORNIK_STORAGE_S3_REGION", Keys: []string{"storage.s3.region", "artifacts.s3.region"},
		Apply: setStrings(func(c *Config) *string { return &c.Storage.S3.Region },
			func(c *Config) *string { return &c.Artifacts.S3.Region })},
	{Env: "VORNIK_STORAGE_S3_BUCKET", Keys: []string{"storage.s3.bucket", "artifacts.s3.bucket"},
		Apply: setStrings(func(c *Config) *string { return &c.Storage.S3.Bucket },
			func(c *Config) *string { return &c.Artifacts.S3.Bucket })},
	{Env: "VORNIK_STORAGE_S3_PREFIX", Keys: []string{"storage.s3.prefix", "artifacts.s3.prefix"},
		Apply: setStrings(func(c *Config) *string { return &c.Storage.S3.Prefix },
			func(c *Config) *string { return &c.Artifacts.S3.Prefix })},
	{Env: "VORNIK_STORAGE_S3_ACCESS_KEY_ID", Keys: []string{"storage.s3.access_key_id", "artifacts.s3.access_key_id"},
		Apply: setStrings(func(c *Config) *string { return &c.Storage.S3.AccessKeyID },
			func(c *Config) *string { return &c.Artifacts.S3.AccessKeyID })},
	{Env: "VORNIK_STORAGE_S3_SECRET_ACCESS_KEY", Keys: []string{"storage.s3.secret_access_key", "artifacts.s3.secret_access_key"},
		Apply: setStrings(func(c *Config) *string { return &c.Storage.S3.SecretAccessKey },
			func(c *Config) *string { return &c.Artifacts.S3.SecretAccessKey })},
	{Env: "VORNIK_STORAGE_S3_USE_PATH_STYLE", Keys: []string{"storage.s3.use_path_style", "artifacts.s3.use_path_style"},
		Apply: setBools(func(c *Config) *bool { return &c.Storage.S3.UsePathStyle },
			func(c *Config) *bool { return &c.Artifacts.S3.UsePathStyle })},
	{Env: "VORNIK_STORAGE_S3_FORCE_SSL", Keys: []string{"storage.s3.force_ssl", "artifacts.s3.force_ssl"},
		Apply: func(c *Config, v string) error {
			on, ok := parseEnvBool(v)
			if !ok {
				return fmt.Errorf("%q is not a boolean", v)
			}
			c.Storage.S3.ForceSSL = &on
			c.Artifacts.S3.ForceSSL = &on
			return nil
		}},

	{Env: "VORNIK_METRICS_ENABLED", Keys: []string{"metrics.enabled"},
		Apply: setBools(func(c *Config) *bool { return &c.Metrics.Enabled })},
	{Env: "VORNIK_METRICS_ADDR", Keys: []string{"metrics.addr"},
		Apply: setString(func(c *Config) *string { return &c.Metrics.Addr })},
	{Env: "VORNIK_TRACING_ENABLED", Keys: []string{"tracing.enabled"},
		Apply: setBools(func(c *Config) *bool { return &c.Tracing.Enabled })},
	{Env: "VORNIK_TRACING_ENDPOINT", Keys: []string{"tracing.endpoint"},
		Apply: setString(func(c *Config) *string { return &c.Tracing.Endpoint })},
	{Env: "VORNIK_LOG_LEVEL", Keys: []string{"logging.level"},
		Apply: setString(func(c *Config) *string { return &c.Logging.Level })},
	{Env: "VORNIK_RUNTIME_USERNS_MODE", Keys: []string{"runtime.userns_mode"},
		Apply: setString(func(c *Config) *string { return &c.Runtime.UserNSMode })},
	{Env: "VORNIK_RUNTIME_RUN_AS_USER", Keys: []string{"runtime.run_as_user"},
		Apply: setString(func(c *Config) *string { return &c.Runtime.RunAsUser })},

	{Env: "VORNIK_GATEWAY_ENABLED", Keys: []string{"gateway.enabled"},
		Apply: setBools(func(c *Config) *bool { return &c.Gateway.Enabled })},
	{Env: "VORNIK_GATEWAY_ADDRESS", Keys: []string{"gateway.address"},
		Apply: setString(func(c *Config) *string { return &c.Gateway.Address })},
	{Env: "VORNIK_GATEWAY_TOKEN", Keys: []string{"gateway.token"},
		Apply: setString(func(c *Config) *string { return &c.Gateway.Token })},
	// Agent-write policy override. Set the raw value; AgentWritesMode() (called
	// from Validate) does the normalize+validate, so an invalid env value is a
	// startup error via the SAME path as YAML — never a silent off.
	{Env: "VORNIK_GATEWAY_AGENT_WRITES", Keys: []string{"gateway.agent_writes"},
		Apply: setString(func(c *Config) *string { return &c.Gateway.AgentWrites })},
	// Taint-lineage enforcement override. Raw value; TaintLineageMode() (from
	// Validate) normalizes+validates, so an invalid env value is a startup
	// error via the SAME path as YAML — never a silent advisory.
	{Env: "VORNIK_TAINT_LINEAGE_MODE", Keys: []string{"taint_lineage.enforcement_mode"},
		Apply: setString(func(c *Config) *string { return &c.TaintLineage.EnforcementMode })},
	// Daemon↔scraper web_submit capability secret (shared C1 contract). Env
	// override wins over YAML so operators can inject it from the daemon
	// environment (mirroring the same value passed to the scraper's
	// SCRAPER_WEB_SUBMIT_SECRET) without committing it to config.
	{Env: "VORNIK_WEB_SUBMIT_SECRET", Keys: []string{"web.submit_secret"},
		Apply: setString(func(c *Config) *string { return &c.Web.SubmitSecret })},
}

// applyEnvOverrides applies the table in order. An unset or empty variable
// never applies (an empty value preserves whatever the file said, for every
// override — the historical rule).
func applyEnvOverrides(cfg *Config) {
	applyEnvOverridesRecording(cfg, nil)
}

// applyEnvOverridesRecording is applyEnvOverrides with a recorder for the
// provenance capture. A parse failure keeps the prior value — as the
// hand-written version silently did — and is reported to the recorder so the
// dump can say why the value is not what the environment says.
func applyEnvOverridesRecording(cfg *Config, rec envRecorder) {
	for _, o := range envOverrides {
		v := os.Getenv(o.Env)
		if v == "" {
			continue
		}
		if o.When != nil && !o.When(cfg) {
			continue
		}
		err := o.Apply(cfg, v)
		if rec != nil {
			rec(o.Keys, o.Env, err)
		}
	}
}

// EnvOverrideFor returns the environment variable that overrides the dotted
// key, or "" — for the generated configuration reference.
func EnvOverrideFor(key string) string {
	for _, o := range envOverrides {
		for _, k := range o.Keys {
			if k == key {
				return o.Env
			}
		}
	}
	return ""
}
