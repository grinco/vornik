package config

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Server.Address != ":8080" {
		t.Fatalf("expected default server address, got %q", cfg.Server.Address)
	}
	if cfg.Database.Host != "localhost" {
		t.Fatalf("expected default database host localhost, got %q", cfg.Database.Host)
	}
	if cfg.Database.Port != 5432 {
		t.Fatalf("expected default database port 5432, got %d", cfg.Database.Port)
	}
	if cfg.Database.Name != "vornik" {
		t.Fatalf("expected default database name vornik, got %q", cfg.Database.Name)
	}
	if cfg.Database.User != "vornik" {
		t.Fatalf("expected default database user vornik, got %q", cfg.Database.User)
	}
	if cfg.Database.SSLMode != "disable" {
		t.Fatalf("expected default sslmode disable, got %q", cfg.Database.SSLMode)
	}
	if cfg.Metrics.Enabled {
		t.Fatalf("expected metrics disabled by default")
	}
	if cfg.Metrics.Addr != ":9090" {
		t.Fatalf("expected default metrics addr :9090, got %q", cfg.Metrics.Addr)
	}
	if len(cfg.API.APIKeys) != 0 {
		t.Fatalf("expected no default API keys, got %v", cfg.API.APIKeys)
	}

	// Watchdog defaults pin the post-2026-05-13 contract: enabled on,
	// action=fail. The action flipped from warn → fail after the
	// ghost-RUNNING incident where stale execution rows confused
	// operators into cancelling live tasks. If anyone reverts the
	// default to warn without operator buy-in, this test surfaces it.
	if cfg.Watchdog.Enabled == nil || !*cfg.Watchdog.Enabled {
		t.Fatalf("expected watchdog enabled by default")
	}
	if cfg.Watchdog.Action != "fail" {
		t.Fatalf("expected default watchdog action 'fail' (post-2026-05-13 default), got %q", cfg.Watchdog.Action)
	}
	if cfg.Watchdog.StuckThreshold != "30m" {
		t.Fatalf("expected default stuck threshold 30m, got %q", cfg.Watchdog.StuckThreshold)
	}
	if cfg.Watchdog.Interval != "60s" {
		t.Fatalf("expected default scan interval 60s, got %q", cfg.Watchdog.Interval)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *Config
		wantError bool
	}{
		{name: "default config requires explicit api keys", cfg: DefaultConfig(), wantError: true},
		{
			name: "auth enabled with api keys",
			cfg: func() *Config {
				cfg := DefaultConfig()
				cfg.API.APIKeys = []string{"secret-key"}
				return cfg
			}(),
			wantError: false,
		},
		{
			name: "missing host",
			cfg: &Config{
				Server:   ServerConfig{Address: ":8080"},
				Database: DatabaseConfig{Port: 5432, Name: "vornik", User: "vornik"},
			},
			wantError: true,
		},
		{
			name: "missing port",
			cfg: &Config{
				Server:   ServerConfig{Address: ":8080"},
				Database: DatabaseConfig{Host: "localhost", Name: "vornik", User: "vornik"},
			},
			wantError: true,
		},
		{
			name: "missing name",
			cfg: &Config{
				Server:   ServerConfig{Address: ":8080"},
				Database: DatabaseConfig{Host: "localhost", Port: 5432, User: "vornik"},
			},
			wantError: true,
		},
		{
			name: "missing user",
			cfg: &Config{
				Server:   ServerConfig{Address: ":8080"},
				Database: DatabaseConfig{Host: "localhost", Port: 5432, Name: "vornik"},
			},
			wantError: true,
		},
		{
			name: "invalid log level",
			cfg: &Config{
				Server:   ServerConfig{Address: ":8080"},
				Database: DatabaseConfig{Host: "localhost", Port: 5432, Name: "vornik", User: "vornik"},
				Logging:  LoggingConfig{Level: "verbose"},
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantError && err == nil {
				t.Fatal("expected validation error")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestConfigStructDefaults(t *testing.T) {
	cfg := &Config{}

	if cfg.Server.Address != "" {
		t.Fatalf("expected zero-value server address, got %q", cfg.Server.Address)
	}
	if cfg.Database.Host != "" {
		t.Fatalf("expected zero-value database host, got %q", cfg.Database.Host)
	}
	if cfg.Database.Port != 0 {
		t.Fatalf("expected zero-value database port, got %d", cfg.Database.Port)
	}
}

func TestErrVersionRequested(t *testing.T) {
	if ErrVersionRequested.Error() != "version requested" {
		t.Fatalf("unexpected error string: %q", ErrVersionRequested.Error())
	}
}

func TestResolveConfigPath(t *testing.T) {
	tmpDir := t.TempDir()
	origEnv := os.Getenv("VORNIK_CONFIG")
	_ = os.Unsetenv("VORNIK_CONFIG")
	defer func() { _ = os.Setenv("VORNIK_CONFIG", origEnv) }()

	t.Run("flag path takes precedence", func(t *testing.T) {
		if got := resolveConfigPath("/custom/config.yaml"); got != "/custom/config.yaml" {
			t.Fatalf("unexpected path: %q", got)
		}
	})

	t.Run("environment variable used when flag empty", func(t *testing.T) {
		_ = os.Setenv("VORNIK_CONFIG", "/env/config.yaml")
		defer func() { _ = os.Unsetenv("VORNIK_CONFIG") }()

		if got := resolveConfigPath(""); got != "/env/config.yaml" {
			t.Fatalf("unexpected path: %q", got)
		}
	})

	t.Run("current directory config used when present", func(t *testing.T) {
		configPath := filepath.Join(tmpDir, "vornik.yaml")
		if err := os.WriteFile(configPath, []byte{}, 0o644); err != nil {
			t.Fatalf("failed to write config file: %v", err)
		}

		origDir, _ := os.Getwd()
		if err := os.Chdir(tmpDir); err != nil {
			t.Fatalf("failed to chdir: %v", err)
		}
		defer func() { _ = os.Chdir(origDir) }()

		_ = os.Unsetenv("VORNIK_CONFIG")
		if got := resolveConfigPath(""); got != "vornik.yaml" {
			t.Fatalf("unexpected path: %q", got)
		}
	})

	t.Run("config.yaml in current directory also matches", func(t *testing.T) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(configPath, []byte{}, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		origDir, _ := os.Getwd()
		if err := os.Chdir(dir); err != nil {
			t.Fatalf("chdir: %v", err)
		}
		defer func() { _ = os.Chdir(origDir) }()

		_ = os.Unsetenv("VORNIK_CONFIG")
		if got := resolveConfigPath(""); got != "config.yaml" {
			t.Fatalf("unexpected path: %q", got)
		}
	})

	t.Run("XDG user config discovered when cwd has nothing", func(t *testing.T) {
		// Stage a fake $HOME/.config/vornik/config.yaml. Point $HOME at a
		// temp dir so the test doesn't depend on the developer's real
		// config. chdir somewhere empty so step 3 (cwd) doesn't hit.
		fakeHome := t.TempDir()
		vornikDir := filepath.Join(fakeHome, ".config", "vornik")
		if err := os.MkdirAll(vornikDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		userPath := filepath.Join(vornikDir, "config.yaml")
		if err := os.WriteFile(userPath, []byte{}, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		cwdEmpty := t.TempDir()
		origDir, _ := os.Getwd()
		if err := os.Chdir(cwdEmpty); err != nil {
			t.Fatalf("chdir: %v", err)
		}
		defer func() { _ = os.Chdir(origDir) }()

		origHome := os.Getenv("HOME")
		origXDG := os.Getenv("XDG_CONFIG_HOME")
		_ = os.Setenv("HOME", fakeHome)
		_ = os.Unsetenv("XDG_CONFIG_HOME")
		_ = os.Unsetenv("VORNIK_CONFIG")
		defer func() {
			_ = os.Setenv("HOME", origHome)
			_ = os.Setenv("XDG_CONFIG_HOME", origXDG)
		}()

		if got := resolveConfigPath(""); got != userPath {
			t.Fatalf("expected user config discovery; got %q, want %q", got, userPath)
		}
	})

	t.Run("XDG_CONFIG_HOME wins over HOME/.config", func(t *testing.T) {
		xdgRoot := t.TempDir()
		homeRoot := t.TempDir()

		xdgDir := filepath.Join(xdgRoot, "vornik")
		if err := os.MkdirAll(xdgDir, 0o755); err != nil {
			t.Fatalf("mkdir xdg: %v", err)
		}
		xdgPath := filepath.Join(xdgDir, "config.yaml")
		if err := os.WriteFile(xdgPath, []byte{}, 0o644); err != nil {
			t.Fatalf("write xdg: %v", err)
		}

		homeDir := filepath.Join(homeRoot, ".config", "vornik")
		if err := os.MkdirAll(homeDir, 0o755); err != nil {
			t.Fatalf("mkdir home: %v", err)
		}
		homePath := filepath.Join(homeDir, "config.yaml")
		if err := os.WriteFile(homePath, []byte{}, 0o644); err != nil {
			t.Fatalf("write home: %v", err)
		}

		cwdEmpty := t.TempDir()
		origDir, _ := os.Getwd()
		if err := os.Chdir(cwdEmpty); err != nil {
			t.Fatalf("chdir: %v", err)
		}
		defer func() { _ = os.Chdir(origDir) }()

		origHome := os.Getenv("HOME")
		origXDG := os.Getenv("XDG_CONFIG_HOME")
		_ = os.Setenv("HOME", homeRoot)
		_ = os.Setenv("XDG_CONFIG_HOME", xdgRoot)
		_ = os.Unsetenv("VORNIK_CONFIG")
		defer func() {
			_ = os.Setenv("HOME", origHome)
			_ = os.Setenv("XDG_CONFIG_HOME", origXDG)
		}()

		if got := resolveConfigPath(""); got != xdgPath {
			t.Fatalf("expected XDG_CONFIG_HOME to win; got %q, want %q", got, xdgPath)
		}
	})
}

func TestApplyEnvOverrides(t *testing.T) {
	origServer := os.Getenv("VORNIK_SERVER_ADDRESS")
	origHost := os.Getenv("VORNIK_DATABASE_HOST")
	origPort := os.Getenv("VORNIK_DATABASE_PORT")
	origName := os.Getenv("VORNIK_DATABASE_NAME")
	origUser := os.Getenv("VORNIK_DATABASE_USER")
	origPassword := os.Getenv("VORNIK_DATABASE_PASSWORD")
	origSSLMode := os.Getenv("VORNIK_DATABASE_SSLMODE")
	origArtifacts := os.Getenv("VORNIK_ARTIFACTS_PATH")
	origMetricsEnabled := os.Getenv("VORNIK_METRICS_ENABLED")
	origMetricsAddr := os.Getenv("VORNIK_METRICS_ADDR")
	origTracingEnabled := os.Getenv("VORNIK_TRACING_ENABLED")
	origTracingEndpoint := os.Getenv("VORNIK_TRACING_ENDPOINT")
	origLogLevel := os.Getenv("VORNIK_LOG_LEVEL")
	origRuntimeUserNSMode := os.Getenv("VORNIK_RUNTIME_USERNS_MODE")
	defer func() {
		_ = os.Setenv("VORNIK_SERVER_ADDRESS", origServer)
		_ = os.Setenv("VORNIK_DATABASE_HOST", origHost)
		_ = os.Setenv("VORNIK_DATABASE_PORT", origPort)
		_ = os.Setenv("VORNIK_DATABASE_NAME", origName)
		_ = os.Setenv("VORNIK_DATABASE_USER", origUser)
		_ = os.Setenv("VORNIK_DATABASE_PASSWORD", origPassword)
		_ = os.Setenv("VORNIK_DATABASE_SSLMODE", origSSLMode)
		_ = os.Setenv("VORNIK_ARTIFACTS_PATH", origArtifacts)
		_ = os.Setenv("VORNIK_METRICS_ENABLED", origMetricsEnabled)
		_ = os.Setenv("VORNIK_METRICS_ADDR", origMetricsAddr)
		_ = os.Setenv("VORNIK_TRACING_ENABLED", origTracingEnabled)
		_ = os.Setenv("VORNIK_TRACING_ENDPOINT", origTracingEndpoint)
		_ = os.Setenv("VORNIK_LOG_LEVEL", origLogLevel)
		_ = os.Setenv("VORNIK_RUNTIME_USERNS_MODE", origRuntimeUserNSMode)
	}()

	_ = os.Setenv("VORNIK_SERVER_ADDRESS", ":9090")
	_ = os.Setenv("VORNIK_DATABASE_HOST", "db.internal")
	_ = os.Setenv("VORNIK_DATABASE_PORT", "6543")
	_ = os.Setenv("VORNIK_DATABASE_NAME", "customdb")
	_ = os.Setenv("VORNIK_DATABASE_USER", "customuser")
	_ = os.Setenv("VORNIK_DATABASE_PASSWORD", "custompass")
	_ = os.Setenv("VORNIK_DATABASE_SSLMODE", "require")
	_ = os.Setenv("VORNIK_ARTIFACTS_PATH", "/data/artifacts")
	_ = os.Setenv("VORNIK_METRICS_ENABLED", "true")
	_ = os.Setenv("VORNIK_METRICS_ADDR", ":9191")
	_ = os.Setenv("VORNIK_TRACING_ENABLED", "true")
	_ = os.Setenv("VORNIK_TRACING_ENDPOINT", "otel:4317")
	_ = os.Setenv("VORNIK_LOG_LEVEL", "debug")
	_ = os.Setenv("VORNIK_RUNTIME_USERNS_MODE", "host")

	cfg := DefaultConfig()
	applyEnvOverrides(cfg)

	if cfg.Server.Address != ":9090" ||
		cfg.Database.Host != "db.internal" ||
		cfg.Database.Port != 6543 ||
		cfg.Database.Name != "customdb" ||
		cfg.Database.User != "customuser" ||
		cfg.Database.Password != "custompass" ||
		cfg.Database.SSLMode != "require" ||
		cfg.Storage.ArtifactsPath != "/data/artifacts" ||
		!cfg.Metrics.Enabled ||
		cfg.Metrics.Addr != ":9191" ||
		!cfg.Tracing.Enabled ||
		cfg.Tracing.Endpoint != "otel:4317" ||
		cfg.Logging.Level != "debug" ||
		cfg.Runtime.UserNSMode != "host" {
		t.Fatalf("environment overrides were not fully applied: %+v", cfg)
	}
}

func TestExpandEnvPlaceholders(t *testing.T) {
	origChat := os.Getenv("CHAT_API_KEY")
	origBot := os.Getenv("TELEGRAM_BOT_TOKEN")
	defer func() {
		_ = os.Setenv("CHAT_API_KEY", origChat)
		_ = os.Setenv("TELEGRAM_BOT_TOKEN", origBot)
	}()

	_ = os.Setenv("CHAT_API_KEY", "chat-secret")
	_ = os.Setenv("TELEGRAM_BOT_TOKEN", "telegram-secret")

	cfg := &Config{
		Chat: ChatConfig{
			APIKey: "${CHAT_API_KEY}",
		},
		Telegram: TelegramConfig{
			BotToken: "${TELEGRAM_BOT_TOKEN}",
		},
		Runtime: RuntimeConfig{
			AgentLLM: AgentLLMConfig{
				APIKey: "${CHAT_API_KEY}",
			},
		},
	}

	expandEnvPlaceholders(cfg)

	if cfg.Chat.APIKey != "chat-secret" {
		t.Fatalf("expected expanded chat api key, got %q", cfg.Chat.APIKey)
	}
	if cfg.Telegram.BotToken != "telegram-secret" {
		t.Fatalf("expected expanded telegram token, got %q", cfg.Telegram.BotToken)
	}
	if cfg.Runtime.AgentLLM.APIKey != "chat-secret" {
		t.Fatalf("expected expanded runtime api key, got %q", cfg.Runtime.AgentLLM.APIKey)
	}
}

func TestExampleConfigParsesAndValidates(t *testing.T) {
	examplePath := filepath.Join("..", "..", "configs", "vornik.yaml.example")
	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("failed to read example config: %v", err)
	}

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "vornik.yaml")
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	resetFlags()
	origArgs := os.Args
	os.Args = []string{"vornik", "--config", configPath}
	defer func() { os.Args = origArgs }()

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load() failed for example config: %v", err)
	}
	if len(cfg.API.APIKeys) != 1 || cfg.API.APIKeys[0] != "replace-with-a-real-api-key" {
		t.Fatalf("example api keys not loaded correctly: %+v", cfg.API.APIKeys)
	}
}

func TestApplyEnvOverridesDataDir(t *testing.T) {
	origDataDir := os.Getenv("VORNIK_DATA_DIR")
	origArtifacts := os.Getenv("VORNIK_ARTIFACTS_PATH")
	defer func() {
		_ = os.Setenv("VORNIK_DATA_DIR", origDataDir)
		_ = os.Setenv("VORNIK_ARTIFACTS_PATH", origArtifacts)
	}()

	t.Run("VORNIK_DATA_DIR sets artifacts path when config has no explicit path", func(t *testing.T) {
		_ = os.Unsetenv("VORNIK_ARTIFACTS_PATH")
		_ = os.Setenv("VORNIK_DATA_DIR", "/custom/data")

		cfg := DefaultConfig()
		cfg.Storage.ArtifactsPath = "" // no explicit config
		applyEnvOverrides(cfg)

		expected := "/custom/data/artifacts"
		if cfg.Storage.ArtifactsPath != expected {
			t.Fatalf("expected artifacts path %q, got %q", expected, cfg.Storage.ArtifactsPath)
		}
	})

	t.Run("VORNIK_ARTIFACTS_PATH takes precedence over VORNIK_DATA_DIR", func(t *testing.T) {
		_ = os.Setenv("VORNIK_DATA_DIR", "/data/dir")
		_ = os.Setenv("VORNIK_ARTIFACTS_PATH", "/artifacts/path")

		cfg := DefaultConfig()
		applyEnvOverrides(cfg)

		expected := "/artifacts/path"
		if cfg.Storage.ArtifactsPath != expected {
			t.Fatalf("expected artifacts path %q, got %q", expected, cfg.Storage.ArtifactsPath)
		}
	})

	t.Run("explicit config path is not overridden by VORNIK_DATA_DIR", func(t *testing.T) {
		_ = os.Unsetenv("VORNIK_ARTIFACTS_PATH")
		_ = os.Setenv("VORNIK_DATA_DIR", "/data/dir")

		cfg := DefaultConfig()
		cfg.Storage.ArtifactsPath = "/explicit/config/path"
		applyEnvOverrides(cfg)

		if cfg.Storage.ArtifactsPath != "/explicit/config/path" {
			t.Fatalf("expected artifacts path /explicit/config/path, got %q", cfg.Storage.ArtifactsPath)
		}
	})

	t.Run("VORNIK_ARTIFACTS_PATH used when VORNIK_DATA_DIR not set", func(t *testing.T) {
		_ = os.Unsetenv("VORNIK_DATA_DIR")
		_ = os.Setenv("VORNIK_ARTIFACTS_PATH", "/just/artifacts")

		cfg := DefaultConfig()
		applyEnvOverrides(cfg)

		if cfg.Storage.ArtifactsPath != "/just/artifacts" {
			t.Fatalf("expected artifacts path /just/artifacts, got %q", cfg.Storage.ArtifactsPath)
		}
	})
}

func resetFlags() {
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	flag.CommandLine.Usage = func() {}
}

func TestLoadFromYAMLFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "vornik.yaml")

	validYAML := `
server:
  address: ":9090"
database:
  host: db.example
  port: 5433
  name: vornik_test
  user: vornik
  password: secret
  sslmode: disable
logging:
  level: debug
  format: text
metrics:
  enabled: true
  addr: ":9191"
tracing:
  enabled: true
  endpoint: "otel:4317"
api:
  auth_enabled: true
  api_keys:
    - "test-key"
runtime:
  userns_mode: keep-id
  delegation_depth_limit: 7
  delegation_fanout_limit: 12
artifacts:
  storagePath: ./artifacts
`
	if err := os.WriteFile(configPath, []byte(validYAML), 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	resetFlags()
	origArgs := os.Args
	os.Args = []string{"vornik", "--config", configPath}
	defer func() { os.Args = origArgs }()

	cfg, path, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if path != configPath {
		t.Fatalf("unexpected config path: %q", path)
	}
	if cfg.Database.Host != "db.example" || cfg.Database.Port != 5433 || cfg.Database.Name != "vornik_test" {
		t.Fatalf("database config did not load correctly: %+v", cfg.Database)
	}
	if cfg.Storage.ArtifactsPath != "./artifacts" {
		t.Fatalf("artifact path alias did not load correctly: %q", cfg.Storage.ArtifactsPath)
	}
	if !cfg.Metrics.Enabled || cfg.Metrics.Addr != ":9191" {
		t.Fatalf("metrics config did not load correctly: %+v", cfg.Metrics)
	}
	if !cfg.Tracing.Enabled || cfg.Tracing.Endpoint != "otel:4317" {
		t.Fatalf("tracing config did not load correctly: %+v", cfg.Tracing)
	}
	if cfg.Runtime.UserNSMode != "keep-id" {
		t.Fatalf("runtime config did not load correctly: %+v", cfg.Runtime)
	}
	if cfg.Runtime.DelegationDepthLimit != 7 || cfg.Runtime.DelegationFanOutLimit != 12 {
		t.Fatalf("delegation guard limits did not load correctly: %+v", cfg.Runtime)
	}
}

func TestConfigValidateRuntimeUserNSMode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.API.APIKeys = []string{"test-key"}
	cfg.Runtime.UserNSMode = "invalid"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "runtime userns_mode") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestLoadFromJSONFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "vornik.json")

	validJSON := `{
		"server": {"address": ":7070"},
		"database": {
			"host": "localhost",
			"port": 5432,
			"name": "jsondb",
			"user": "jsonuser",
			"password": "jsonpass",
			"sslmode": "disable"
		},
		"logging": {"level": "warn", "format": "console"},
		"api": {"auth_enabled": true, "api_keys": ["json-key"]}
	}`
	if err := os.WriteFile(configPath, []byte(validJSON), 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	resetFlags()
	origArgs := os.Args
	os.Args = []string{"vornik", "--config", configPath}
	defer func() { os.Args = origArgs }()

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.Database.Name != "jsondb" || cfg.Database.User != "jsonuser" {
		t.Fatalf("database JSON config did not load correctly: %+v", cfg.Database)
	}
}

func TestLoadMissingConfigUsesDefaults(t *testing.T) {
	tmpDir := t.TempDir()

	resetFlags()
	origArgs := os.Args
	origDir, _ := os.Getwd()
	os.Args = []string{"vornik"}
	_ = os.Chdir(tmpDir)
	// Isolate from the developer/CI operator's $HOME and
	// $XDG_CONFIG_HOME so the user-config discovery added for vornikctl
	// doesn't pick up a real ~/.config/vornik/config.yaml and make the
	// "missing config" scenario unreachable.
	origHome := os.Getenv("HOME")
	origXDG := os.Getenv("XDG_CONFIG_HOME")
	isolated := t.TempDir()
	_ = os.Setenv("HOME", isolated)
	_ = os.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".xdg"))
	defer func() {
		os.Args = origArgs
		_ = os.Chdir(origDir)
		_ = os.Setenv("HOME", origHome)
		_ = os.Setenv("XDG_CONFIG_HOME", origXDG)
	}()

	_, path, err := Load()
	if err == nil {
		t.Fatal("expected Load() to fail without explicit api.api_keys")
	}
	if path != "" {
		t.Fatalf("expected empty path, got %q", path)
	}
	if !strings.Contains(err.Error(), "api.api_keys") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadInvalidConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.yaml")

	invalidYAML := `
server:
  address: ":8080"
database:
  host: localhost
  port: 0
  name: vornik
  user: vornik
`
	if err := os.WriteFile(configPath, []byte(invalidYAML), 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	resetFlags()
	origArgs := os.Args
	os.Args = []string{"vornik", "--config", configPath}
	defer func() { os.Args = origArgs }()

	cfg, _, err := Load()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if cfg != nil {
		t.Fatal("expected nil config on validation error")
	}
}

func TestLoadVersionFlag(t *testing.T) {
	resetFlags()
	origArgs := os.Args
	os.Args = []string{"vornik", "--version"}
	defer func() { os.Args = origArgs }()

	cfg, _, err := Load()
	if err != ErrVersionRequested {
		t.Fatalf("expected version error, got %v", err)
	}
	if cfg != nil {
		t.Fatal("expected nil config when version flag is set")
	}
}

// TestUserAccess_YAMLPolymorphic locks in the accepted YAML shapes for
// an allowed_users entry. Operators can write bool (legacy) or list
// (new); both decode into the same Go type with clear semantics.
// Regressions here break operator-facing config, so keep it tight.
func TestUserAccess_YAMLPolymorphic(t *testing.T) {
	cases := []struct {
		name         string
		yaml         string
		wantAllowed  bool
		wantProjects []string
		wantErr      bool
	}{
		{
			name:         "true is legacy wildcard",
			yaml:         "true",
			wantAllowed:  true,
			wantProjects: []string{"*"},
		},
		{
			name:        "false is explicit deny",
			yaml:        "false",
			wantAllowed: false,
		},
		{
			name:         "empty list is dispatcher-only",
			yaml:         "[]",
			wantAllowed:  true,
			wantProjects: []string{},
		},
		{
			name:         "wildcard list",
			yaml:         `["*"]`,
			wantAllowed:  true,
			wantProjects: []string{"*"},
		},
		{
			name:         "scoped list",
			yaml:         `["snake", "headmatch"]`,
			wantAllowed:  true,
			wantProjects: []string{"snake", "headmatch"},
		},
		{
			name:    "unsupported shape rejected",
			yaml:    `{projects: ["snake"]}`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Decode through a wrapping map so we exercise the same
			// UnmarshalYAML path the real config loader uses.
			doc := "u: " + tc.yaml + "\n"
			var out struct {
				U UserAccess `yaml:"u"`
			}
			err := yaml.Unmarshal([]byte(doc), &out)
			if err != nil {
				if !tc.wantErr {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if tc.wantErr {
				t.Fatalf("expected error, got success with %+v", out.U)
			}
			if out.U.Allowed != tc.wantAllowed {
				t.Errorf("Allowed = %v, want %v", out.U.Allowed, tc.wantAllowed)
			}
			if !stringSlicesEqual(out.U.Projects, tc.wantProjects) {
				t.Errorf("Projects = %v, want %v", out.U.Projects, tc.wantProjects)
			}
		})
	}
}

// TestConfig_MCP_ParsesDaemonLevelBlock locks in the YAML contract
// for the daemon-level mcp.servers block. The block is purely
// declarative — the loader doesn't validate transport / URL beyond
// what UnmarshalYAML does, so this test guards the field-name
// mapping (which is the contract operators see).
func TestConfig_MCP_ParsesDaemonLevelBlock(t *testing.T) {
	doc := `
mcp:
  servers:
    - name: scraper
      transport: sse
      url: http://127.0.0.1:8787
      allowed_tools:
        - web_fetch
        - ical_events
    - name: linter
      transport: stdio
      command: /usr/bin/python3
      args:
        - -m
        - mcp_server_linter
      env:
        LINTER_CONFIG: /etc/linter.yaml
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := len(cfg.MCP.Servers); got != 2 {
		t.Fatalf("got %d servers, want 2", got)
	}

	s := cfg.MCP.Servers[0]
	if s.Name != "scraper" {
		t.Errorf("server[0].Name = %q, want scraper", s.Name)
	}
	if s.Transport != "sse" {
		t.Errorf("server[0].Transport = %q, want sse", s.Transport)
	}
	if s.URL != "http://127.0.0.1:8787" {
		t.Errorf("server[0].URL = %q", s.URL)
	}
	if !stringSlicesEqual(s.AllowedTools, []string{"web_fetch", "ical_events"}) {
		t.Errorf("server[0].AllowedTools = %v", s.AllowedTools)
	}

	s = cfg.MCP.Servers[1]
	if s.Name != "linter" {
		t.Errorf("server[1].Name = %q", s.Name)
	}
	if s.Transport != "stdio" {
		t.Errorf("server[1].Transport = %q", s.Transport)
	}
	if s.Command != "/usr/bin/python3" {
		t.Errorf("server[1].Command = %q", s.Command)
	}
	if !stringSlicesEqual(s.Args, []string{"-m", "mcp_server_linter"}) {
		t.Errorf("server[1].Args = %v", s.Args)
	}
	if s.Env["LINTER_CONFIG"] != "/etc/linter.yaml" {
		t.Errorf("server[1].Env = %v", s.Env)
	}
}

// TestConfig_MCP_DefaultEmpty asserts that a config without an mcp
// block is zero-valued — no servers, no defaults invented. The
// daemon must treat an unset block as "operator hasn't opted in"
// and skip the daemon-level discovery surface entirely.
func TestConfig_MCP_DefaultEmpty(t *testing.T) {
	cfg := DefaultConfig()
	if len(cfg.MCP.Servers) != 0 {
		t.Fatalf("DefaultConfig.MCP.Servers should be empty, got %d", len(cfg.MCP.Servers))
	}
}

func TestAuthSettingsBootstrapAdminsParsed(t *testing.T) {
	yamlSrc := []byte("auth:\n  bootstrap_admins:\n    - \"google:vadim@vornik.io\"\n    - \"github:12345678\"\n")
	var cfg Config
	if err := yaml.Unmarshal(yamlSrc, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cfg.Auth.BootstrapAdmins) != 2 || cfg.Auth.BootstrapAdmins[0] != "google:vadim@vornik.io" {
		t.Fatalf("BootstrapAdmins = %v", cfg.Auth.BootstrapAdmins)
	}
}

// minValidConfig returns the smallest Config that passes Validate() —
// used by validation table tests that only want to exercise one field.
func minValidConfig() *Config {
	return &Config{
		Server:   ServerConfig{Address: ":8080"},
		Database: DatabaseConfig{Host: "localhost", Port: 5432, Name: "vornik", User: "vornik"},
		API:      APIConfig{AuthEnabled: false},
	}
}

func TestAuthSettingsBootstrapAdminsValidate(t *testing.T) {
	tests := []struct {
		name      string
		admins    []string
		wantError bool
	}{
		{name: "valid google email", admins: []string{"google:vadim@vornik.io"}, wantError: false},
		{name: "valid telegram numeric", admins: []string{"telegram:12345"}, wantError: false},
		{name: "valid github immutable id", admins: []string{"github:12345678"}, wantError: false},
		{name: "multi-colon external ID", admins: []string{"google:weird:subject"}, wantError: false},
		{name: "multiple valid entries", admins: []string{"google:vadim@vornik.io", "telegram:12345"}, wantError: false},
		{name: "empty list", admins: []string{}, wantError: false},
		{name: "no colon", admins: []string{"no-colon"}, wantError: true},
		{name: "missing channel", admins: []string{":missing-channel"}, wantError: true},
		{name: "missing id", admins: []string{"missing-id:"}, wantError: true},
		{name: "whitespace only", admins: []string{"  "}, wantError: true},
		{name: "space after colon", admins: []string{"google: vadim@x"}, wantError: true},
		{name: "leading space", admins: []string{" google:vadim@x"}, wantError: true},
		{name: "space before colon", admins: []string{"google :x"}, wantError: true},
		{name: "trailing space", admins: []string{"google:vadim@x "}, wantError: true},
		{name: "github mutable login rejected", admins: []string{"github:vgrinco"}, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := minValidConfig()
			cfg.Auth.BootstrapAdmins = tt.admins
			err := cfg.Validate()
			if tt.wantError {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if !strings.Contains(err.Error(), "auth.bootstrap_admins") {
					t.Fatalf("expected error to name auth.bootstrap_admins, got: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Phase-3 login config tests
// ---------------------------------------------------------------------------

// TestAuthSettings_Phase3_ParseRoundTrip verifies that the full auth block
// (external_base_url + session + providers.github) round-trips through
// yaml.Unmarshal without loss.
func TestAuthSettings_Phase3_ParseRoundTrip(t *testing.T) {
	src := `
auth:
  external_base_url: "http://192.0.2.10:8080"
  session:
    lifetime: "168h"
    idle_timeout: "24h"
  providers:
    github:
      client_id: "Ov23xxxx"
      client_secret_file: "/tmp/secret"
      org: "grinco"
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	a := cfg.Auth
	if a.ExternalBaseURL != "http://192.0.2.10:8080" {
		t.Errorf("ExternalBaseURL = %q", a.ExternalBaseURL)
	}
	if a.Session.Lifetime != "168h" {
		t.Errorf("Session.Lifetime = %q", a.Session.Lifetime)
	}
	if a.Session.IdleTimeout != "24h" {
		t.Errorf("Session.IdleTimeout = %q", a.Session.IdleTimeout)
	}
	if a.Providers.GitHub == nil {
		t.Fatal("Providers.GitHub is nil")
	}
	if a.Providers.GitHub.ClientID != "Ov23xxxx" {
		t.Errorf("GitHub.ClientID = %q", a.Providers.GitHub.ClientID)
	}
	if a.Providers.GitHub.ClientSecretFile != "/tmp/secret" {
		t.Errorf("GitHub.ClientSecretFile = %q", a.Providers.GitHub.ClientSecretFile)
	}
	if a.Providers.GitHub.Org != "grinco" {
		t.Errorf("GitHub.Org = %q", a.Providers.GitHub.Org)
	}
}

// TestAuthSettings_Phase3_Validate covers the validation table for the
// new Phase-3 fields.
func TestAuthSettings_Phase3_Validate(t *testing.T) {
	// helper: minimal config that passes all non-auth validation.
	base := func() *Config {
		return minValidConfig()
	}

	// github provider block with all required fields filled.
	validGitHub := func() *GitHubProviderSettings {
		return &GitHubProviderSettings{
			ClientID:     "cid",
			ClientSecret: "s3cret",
		}
	}

	tests := []struct {
		name      string
		setup     func(*Config)
		wantError bool
		errFrag   string // substring that must appear in the error
	}{
		{
			name: "github set, external_base_url empty → error",
			setup: func(c *Config) {
				c.Auth.Providers.GitHub = validGitHub()
				c.Auth.ExternalBaseURL = ""
			},
			wantError: true,
			errFrag:   "auth.external_base_url",
		},
		{
			name: "external_base_url no scheme → error",
			setup: func(c *Config) {
				c.Auth.Providers.GitHub = validGitHub()
				c.Auth.ExternalBaseURL = "192.0.2.10:8080"
			},
			wantError: true,
			errFrag:   "auth.external_base_url",
		},
		{
			name: "external_base_url with path → error",
			setup: func(c *Config) {
				c.Auth.Providers.GitHub = validGitHub()
				c.Auth.ExternalBaseURL = "http://host:8080/path"
			},
			wantError: true,
			errFrag:   "auth.external_base_url",
		},
		{
			name: "external_base_url bare slash is OK (normalizable)",
			setup: func(c *Config) {
				c.Auth.Providers.GitHub = validGitHub()
				c.Auth.ExternalBaseURL = "http://host:8080/"
			},
			wantError: false,
		},
		{
			name: "github.client_id empty → error",
			setup: func(c *Config) {
				c.Auth.ExternalBaseURL = "http://host:8080"
				c.Auth.Providers.GitHub = &GitHubProviderSettings{
					ClientSecret: "s3cret",
				}
			},
			wantError: true,
			errFrag:   "client_id",
		},
		{
			name: "both client_secret and client_secret_file set → error",
			setup: func(c *Config) {
				c.Auth.ExternalBaseURL = "http://host:8080"
				c.Auth.Providers.GitHub = &GitHubProviderSettings{
					ClientID:         "cid",
					ClientSecret:     "s3cret",
					ClientSecretFile: "/tmp/secret",
				}
			},
			wantError: true,
			errFrag:   "client_secret",
		},
		{
			name: "neither client_secret nor client_secret_file → error",
			setup: func(c *Config) {
				c.Auth.ExternalBaseURL = "http://host:8080"
				c.Auth.Providers.GitHub = &GitHubProviderSettings{
					ClientID: "cid",
				}
			},
			wantError: true,
			errFrag:   "client_secret",
		},
		{
			name: "session.lifetime garbage → error",
			setup: func(c *Config) {
				c.Auth.ExternalBaseURL = "http://host:8080"
				c.Auth.Providers.GitHub = validGitHub()
				c.Auth.Session.Lifetime = "garbage"
			},
			wantError: true,
			errFrag:   "session.lifetime",
		},
		{
			name: "valid full config passes",
			setup: func(c *Config) {
				c.Auth.ExternalBaseURL = "http://host:8080"
				c.Auth.Providers.GitHub = validGitHub()
			},
			wantError: false,
		},
		{
			name: "org_member_role invalid value → error",
			setup: func(c *Config) {
				c.Auth.ExternalBaseURL = "http://host:8080"
				gh := validGitHub()
				gh.Org = "grinco"
				gh.OrgMemberRole = "superuser"
				c.Auth.Providers.GitHub = gh
			},
			wantError: true,
			errFrag:   "org_member_role",
		},
		{
			name: "org_member_role set without org → error",
			setup: func(c *Config) {
				c.Auth.ExternalBaseURL = "http://host:8080"
				gh := validGitHub()
				gh.OrgMemberRole = "user"
				gh.OrgMemberProjects = []string{"janka"}
				c.Auth.Providers.GitHub = gh
			},
			wantError: true,
			errFrag:   "requires auth.providers.github.org",
		},
		{
			name: "org_member_role user without projects → error",
			setup: func(c *Config) {
				c.Auth.ExternalBaseURL = "http://host:8080"
				gh := validGitHub()
				gh.Org = "grinco"
				gh.OrgMemberRole = "user"
				c.Auth.Providers.GitHub = gh
			},
			wantError: true,
			errFrag:   "org_member_projects",
		},
		{
			name: "org_member_role user with projects passes",
			setup: func(c *Config) {
				c.Auth.ExternalBaseURL = "http://host:8080"
				gh := validGitHub()
				gh.Org = "grinco"
				gh.OrgMemberRole = "user"
				gh.OrgMemberProjects = []string{"janka"}
				c.Auth.Providers.GitHub = gh
			},
			wantError: false,
		},
		{
			// Hardening 2026-06-15: a login provider on sqlite would
			// silently no-op (identity core is postgres-only). Boot
			// must refuse rather than run a config that can't log in.
			name: "github provider + sqlite driver → error",
			setup: func(c *Config) {
				c.Database.Driver = "sqlite"
				c.Database.Path = "/tmp/vornik-test.db"
				c.Auth.ExternalBaseURL = "http://host:8080"
				c.Auth.Providers.GitHub = validGitHub()
			},
			wantError: true,
			errFrag:   "sqlite",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.setup(c)
			err := c.Validate()
			if tt.wantError {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if tt.errFrag != "" && !strings.Contains(err.Error(), tt.errFrag) {
					t.Fatalf("expected error containing %q, got: %v", tt.errFrag, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
			}
		})
	}
}

// TestAuthSettings_Phase3_SessionLifetimeDefault verifies that after Load()
// an absent session.lifetime is defaulted to "168h".
func TestAuthSettings_Phase3_SessionLifetimeDefault(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "vornik.yaml")

	// Config with github provider but no session.lifetime — loader must apply default.
	src := `
server:
  address: ":8080"
database:
  host: localhost
  port: 5432
  name: vornik
  user: vornik
api:
  auth_enabled: false
auth:
  external_base_url: "http://host:8080"
  providers:
    github:
      client_id: "cid"
      client_secret: "s3cret"
`
	if err := os.WriteFile(configPath, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	resetFlags()
	origArgs := os.Args
	os.Args = []string{"vornik", "--config", configPath}
	defer func() { os.Args = origArgs }()

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.Session.Lifetime != "168h" {
		t.Fatalf("expected default session.lifetime 168h, got %q", cfg.Auth.Session.Lifetime)
	}
}

// TestAuthSettings_Phase3_SecretFileResolution verifies that the loader reads
// client_secret_file, trims whitespace, and populates ClientSecret; and that
// an unreadable path returns an error.
func TestAuthSettings_Phase3_SecretFileResolution(t *testing.T) {
	t.Run("reads and trims secret file", func(t *testing.T) {
		tmpDir := t.TempDir()
		secretFile := filepath.Join(tmpDir, "gh-secret")
		if err := os.WriteFile(secretFile, []byte("  s3cret\n"), 0o600); err != nil {
			t.Fatalf("write secret: %v", err)
		}

		configPath := filepath.Join(tmpDir, "vornik.yaml")
		src := `
server:
  address: ":8080"
database:
  host: localhost
  port: 5432
  name: vornik
  user: vornik
api:
  auth_enabled: false
auth:
  external_base_url: "http://host:8080"
  providers:
    github:
      client_id: "cid"
      client_secret_file: "` + secretFile + `"
`
		if err := os.WriteFile(configPath, []byte(src), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}

		resetFlags()
		origArgs := os.Args
		os.Args = []string{"vornik", "--config", configPath}
		defer func() { os.Args = origArgs }()

		cfg, _, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Auth.Providers.GitHub == nil {
			t.Fatal("GitHub provider is nil after load")
		}
		if cfg.Auth.Providers.GitHub.ClientSecret != "s3cret" {
			t.Fatalf("expected ClientSecret=s3cret, got %q", cfg.Auth.Providers.GitHub.ClientSecret)
		}
		// ClientSecretFile should be cleared after resolution.
		if cfg.Auth.Providers.GitHub.ClientSecretFile != "" {
			t.Fatalf("expected ClientSecretFile to be cleared after resolution, got %q", cfg.Auth.Providers.GitHub.ClientSecretFile)
		}
	})

	t.Run("unreadable secret file returns error", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "vornik.yaml")
		src := `
server:
  address: ":8080"
database:
  host: localhost
  port: 5432
  name: vornik
  user: vornik
api:
  auth_enabled: false
auth:
  external_base_url: "http://host:8080"
  providers:
    github:
      client_id: "cid"
      client_secret_file: "/nonexistent/path/secret"
`
		if err := os.WriteFile(configPath, []byte(src), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}

		resetFlags()
		origArgs := os.Args
		os.Args = []string{"vornik", "--config", configPath}
		defer func() { os.Args = origArgs }()

		_, _, err := Load()
		if err == nil {
			t.Fatal("expected error for unreadable secret file, got nil")
		}
	})
}

// TestAuthDryRunConfig exercises the api.auth_dry_run evaluation flag:
// (a) round-trip parse with auth_enabled: false,
// (b) validation error when combined with auth_enabled: true,
// (c) default false — absent key via Load() and DefaultConfig.
func TestAuthDryRunConfig(t *testing.T) {
	// (a) parse round-trip: auth_enabled: false + auth_dry_run: true → true.
	t.Run("auth_dry_run true parses correctly when auth_enabled false", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "vornik.yaml")
		src := `
server:
  address: ":8080"
database:
  host: localhost
  port: 5432
  name: vornik
  user: vornik
api:
  auth_enabled: false
  auth_dry_run: true
`
		if err := os.WriteFile(configPath, []byte(src), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}

		resetFlags()
		origArgs := os.Args
		os.Args = []string{"vornik", "--config", configPath}
		defer func() { os.Args = origArgs }()

		cfg, _, err := Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if !cfg.API.AuthDryRun {
			t.Fatal("expected AuthDryRun == true after parse, got false")
		}
	})

	// (b) auth_enabled: true + auth_dry_run: true → config error mentioning "auth_dry_run".
	t.Run("auth_dry_run with auth_enabled errors", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "vornik.yaml")
		src := `
server:
  address: ":8080"
database:
  host: localhost
  port: 5432
  name: vornik
  user: vornik
api:
  auth_enabled: true
  auth_dry_run: true
  api_keys:
    - "sk-some-key"
`
		if err := os.WriteFile(configPath, []byte(src), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}

		resetFlags()
		origArgs := os.Args
		os.Args = []string{"vornik", "--config", configPath}
		defer func() { os.Args = origArgs }()

		_, _, err := Load()
		if err == nil {
			t.Fatal("expected validation error for auth_enabled+auth_dry_run, got nil")
		}
		if !strings.Contains(err.Error(), "auth_dry_run") {
			t.Fatalf("expected error to mention auth_dry_run, got: %v", err)
		}
	})

	// (c) default false — YAML that omits auth_dry_run entirely must not activate the flag.
	t.Run("default false", func(t *testing.T) {
		// Verify via DefaultConfig() — no YAML involved.
		cfg := DefaultConfig()
		if cfg.API.AuthDryRun {
			t.Fatal("DefaultConfig: expected AuthDryRun to be false by default")
		}

		// Also verify via Load() with a YAML that never mentions auth_dry_run,
		// proving an absent key can never silently activate the flag.
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "vornik.yaml")
		src := `
server:
  address: ":8080"
database:
  host: localhost
  port: 5432
  name: vornik
  user: vornik
api:
  auth_enabled: false
`
		if err := os.WriteFile(configPath, []byte(src), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}

		resetFlags()
		origArgs := os.Args
		os.Args = []string{"vornik", "--config", configPath}
		defer func() { os.Args = origArgs }()

		loaded, _, err := Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if loaded.API.AuthDryRun {
			t.Fatal("Load() with absent auth_dry_run key: expected false, got true")
		}
	})
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStorageConfig_NormalizedBackend(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty defaults to filesystem", in: "", want: "filesystem"},
		{name: "local alias", in: "local", want: "filesystem"},
		{name: "filesystem canonical", in: "filesystem", want: "filesystem"},
		{name: "s3 passthrough", in: "s3", want: "s3"},
		{name: "unknown stays as-given", in: "azure", want: "azure"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := StorageConfig{Backend: tc.in}
			got := cfg.NormalizedBackend()
			if got != tc.want {
				t.Fatalf("NormalizedBackend(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStorageConfig_Validate(t *testing.T) {
	t.Run("filesystem ok with no S3 block", func(t *testing.T) {
		cfg := StorageConfig{Backend: "filesystem"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
	t.Run("default (empty) ok", func(t *testing.T) {
		cfg := StorageConfig{}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
	t.Run("s3 requires bucket", func(t *testing.T) {
		cfg := StorageConfig{Backend: "s3", S3: S3StorageConfig{Region: "us-east-1"}}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "bucket") {
			t.Fatalf("expected bucket error, got %v", err)
		}
	})
	t.Run("s3 requires region", func(t *testing.T) {
		cfg := StorageConfig{Backend: "s3", S3: S3StorageConfig{Bucket: "vornik-art"}}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "region") {
			t.Fatalf("expected region error, got %v", err)
		}
	})
	t.Run("s3 with bucket+region ok", func(t *testing.T) {
		cfg := StorageConfig{Backend: "s3", S3: S3StorageConfig{Bucket: "b", Region: "r"}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
	t.Run("unsupported backend rejected", func(t *testing.T) {
		cfg := StorageConfig{Backend: "gcs"}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("expected unsupported-backend error, got %v", err)
		}
	})
}

func TestS3StorageConfig_ResolveForceSSL(t *testing.T) {
	t.Run("nil defaults to true", func(t *testing.T) {
		s := S3StorageConfig{}
		if !s.ResolveForceSSL() {
			t.Fatal("expected default ForceSSL=true")
		}
	})
	t.Run("explicit false", func(t *testing.T) {
		f := false
		s := S3StorageConfig{ForceSSL: &f}
		if s.ResolveForceSSL() {
			t.Fatal("expected ForceSSL=false")
		}
	})
	t.Run("explicit true", func(t *testing.T) {
		tr := true
		s := S3StorageConfig{ForceSSL: &tr}
		if !s.ResolveForceSSL() {
			t.Fatal("expected ForceSSL=true")
		}
	})
}

func TestStorageConfig_UnmarshalYAML_S3Block(t *testing.T) {
	src := `
artifacts_path: /var/lib/vornik/artifacts
backend: s3
s3:
  endpoint: http://localhost:9000
  region: us-east-1
  bucket: vornik-art
  prefix: prod/
  access_key_id: AKIA
  secret_access_key: shhh
  use_path_style: true
  force_ssl: false
`
	var cfg StorageConfig
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.ArtifactsPath != "/var/lib/vornik/artifacts" {
		t.Fatalf("ArtifactsPath = %q", cfg.ArtifactsPath)
	}
	if cfg.Backend != "s3" {
		t.Fatalf("Backend = %q", cfg.Backend)
	}
	if cfg.S3.Endpoint != "http://localhost:9000" {
		t.Fatalf("Endpoint = %q", cfg.S3.Endpoint)
	}
	if cfg.S3.Region != "us-east-1" {
		t.Fatalf("Region = %q", cfg.S3.Region)
	}
	if cfg.S3.Bucket != "vornik-art" {
		t.Fatalf("Bucket = %q", cfg.S3.Bucket)
	}
	if cfg.S3.Prefix != "prod/" {
		t.Fatalf("Prefix = %q", cfg.S3.Prefix)
	}
	if cfg.S3.AccessKeyID != "AKIA" {
		t.Fatalf("AccessKeyID = %q", cfg.S3.AccessKeyID)
	}
	if cfg.S3.SecretAccessKey != "shhh" {
		t.Fatalf("SecretAccessKey = %q", cfg.S3.SecretAccessKey)
	}
	if !cfg.S3.UsePathStyle {
		t.Fatalf("UsePathStyle = false, want true")
	}
	if cfg.S3.ResolveForceSSL() {
		t.Fatalf("ResolveForceSSL = true, want false")
	}
}

func TestApplyEnvOverrides_StorageS3(t *testing.T) {
	t.Setenv("VORNIK_STORAGE_BACKEND", "s3")
	t.Setenv("VORNIK_STORAGE_S3_ENDPOINT", "http://minio:9000")
	t.Setenv("VORNIK_STORAGE_S3_REGION", "us-east-1")
	t.Setenv("VORNIK_STORAGE_S3_BUCKET", "vornik-art")
	t.Setenv("VORNIK_STORAGE_S3_PREFIX", "p1/")
	t.Setenv("VORNIK_STORAGE_S3_ACCESS_KEY_ID", "AKIA")
	t.Setenv("VORNIK_STORAGE_S3_SECRET_ACCESS_KEY", "secret")
	t.Setenv("VORNIK_STORAGE_S3_USE_PATH_STYLE", "true")
	t.Setenv("VORNIK_STORAGE_S3_FORCE_SSL", "false")

	cfg := DefaultConfig()
	applyEnvOverrides(cfg)

	if cfg.Storage.Backend != "s3" {
		t.Fatalf("Storage.Backend = %q", cfg.Storage.Backend)
	}
	if cfg.Storage.S3.Endpoint != "http://minio:9000" {
		t.Fatalf("Storage.S3.Endpoint = %q", cfg.Storage.S3.Endpoint)
	}
	if cfg.Storage.S3.Region != "us-east-1" {
		t.Fatalf("Storage.S3.Region = %q", cfg.Storage.S3.Region)
	}
	if cfg.Storage.S3.Bucket != "vornik-art" {
		t.Fatalf("Storage.S3.Bucket = %q", cfg.Storage.S3.Bucket)
	}
	if cfg.Storage.S3.Prefix != "p1/" {
		t.Fatalf("Storage.S3.Prefix = %q", cfg.Storage.S3.Prefix)
	}
	if cfg.Storage.S3.AccessKeyID != "AKIA" {
		t.Fatalf("Storage.S3.AccessKeyID = %q", cfg.Storage.S3.AccessKeyID)
	}
	if cfg.Storage.S3.SecretAccessKey != "secret" {
		t.Fatalf("Storage.S3.SecretAccessKey hidden, got %q", cfg.Storage.S3.SecretAccessKey)
	}
	if !cfg.Storage.S3.UsePathStyle {
		t.Fatalf("Storage.S3.UsePathStyle = false")
	}
	if cfg.Storage.S3.ForceSSL == nil || *cfg.Storage.S3.ForceSSL {
		t.Fatalf("Storage.S3.ForceSSL not set to false")
	}
	// Validate the merged config now passes.
	if err := cfg.Storage.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestServerPublicBaseURL verifies that server.public_base_url is correctly
// unmarshalled from YAML and defaults to empty when absent.
func TestServerPublicBaseURL(t *testing.T) {
	t.Run("set from YAML", func(t *testing.T) {
		raw := `server:
  public_base_url: https://vornik.example.com
`
		var cfg Config
		if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if cfg.Server.PublicBaseURL != "https://vornik.example.com" {
			t.Errorf("expected PublicBaseURL == %q, got %q", "https://vornik.example.com", cfg.Server.PublicBaseURL)
		}
	})

	t.Run("empty by default when absent", func(t *testing.T) {
		var cfg Config
		if cfg.Server.PublicBaseURL != "" {
			t.Errorf("expected PublicBaseURL == %q (default), got %q", "", cfg.Server.PublicBaseURL)
		}
	})
}
