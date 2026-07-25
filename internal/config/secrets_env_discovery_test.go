package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Customer report, fresh quickstart install (2026-07-26): every config-loading
// vornikctl command failed with
//
//	load config: configuration validation failed: database name is required
//
// while the daemon on the same host worked. config.yaml carries
// `name: "${POSTGRES_DB}"`, and POSTGRES_DB lives in the EnvironmentFile the
// quickstart seeds at <configDir>/vornik.env. systemd loads that for the daemon;
// nothing loads it for the CLI, so the placeholder expanded to empty.
//
// sourceSecretsEnvFiles globbed only <configDir>/secrets/*.env, which misses
// both env files the shipped deployments actually use:
//
//   - <configDir>/vornik.env — deployments/podman/systemd/vornik.service
//   - <configDir>/secrets/env — contrib/systemd/vornik.service (no .env suffix,
//     so the glob never matched it)
func TestSourceSecretsEnvFilesFindsQuickstartEnvironmentFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("server:\n  address: \":8080\"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vornik.env"),
		[]byte("POSTGRES_DB=vornik\nPOSTGRES_USER=vornik\n"), 0o600))

	t.Setenv("POSTGRES_DB", "")
	t.Setenv("POSTGRES_USER", "")

	sourceSecretsEnvFiles(configPath)

	require.Equal(t, "vornik", os.Getenv("POSTGRES_DB"),
		"the quickstart's vornik.env must be sourced, or vornikctl cannot resolve ${POSTGRES_DB}")
	require.Equal(t, "vornik", os.Getenv("POSTGRES_USER"))
}

func TestSourceSecretsEnvFilesFindsSuffixlessSecretsEnv(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("server:\n  address: \":8080\"\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "secrets"), 0o700))
	// contrib/systemd/vornik.service loads exactly this filename.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secrets", "env"),
		[]byte("VORNIK_FROM_SUFFIXLESS=yes\n"), 0o600))

	t.Setenv("VORNIK_FROM_SUFFIXLESS", "")
	sourceSecretsEnvFiles(configPath)

	require.Equal(t, "yes", os.Getenv("VORNIK_FROM_SUFFIXLESS"),
		"secrets/env is referenced by the shipped unit but has no .env suffix")
}

func TestSourceSecretsEnvFilesStillReadsSecretsGlob(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("server:\n  address: \":8080\"\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "secrets"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secrets", "chat.env"),
		[]byte("VORNIK_CHAT_API_KEY=sk-test\n"), 0o600))

	t.Setenv("VORNIK_CHAT_API_KEY", "")
	sourceSecretsEnvFiles(configPath)
	require.Equal(t, "sk-test", os.Getenv("VORNIK_CHAT_API_KEY"))
}

// An explicit value in the real environment must still win over every file, and
// the existing fill-empties-only contract must hold for the new locations too.
func TestSourceSecretsEnvFilesNeverOverridesRealEnvironment(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("server:\n  address: \":8080\"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vornik.env"),
		[]byte("POSTGRES_DB=from-file\n"), 0o600))

	t.Setenv("POSTGRES_DB", "from-environment")
	sourceSecretsEnvFiles(configPath)
	require.Equal(t, "from-environment", os.Getenv("POSTGRES_DB"))
}

// End-to-end: the exact shape of the customer's install must load.
func TestLoadResolvesPlaceholdersFromQuickstartEnvFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`server:
  address: "0.0.0.0:8080"
database:
  host: 127.0.0.1
  port: 5432
  name: "${POSTGRES_DB}"
  user: "${POSTGRES_USER}"
  password: "${VORNIK_DATABASE_PASSWORD}"
  sslmode: disable
api:
  auth_enabled: false
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vornik.env"), []byte(
		"POSTGRES_DB=vornik\nPOSTGRES_USER=vornik\nVORNIK_DATABASE_PASSWORD=secret\n"), 0o600))

	t.Setenv("POSTGRES_DB", "")
	t.Setenv("POSTGRES_USER", "")
	t.Setenv("VORNIK_DATABASE_PASSWORD", "")

	// LoadFromPath, not Load: Load registers the -config flag, so only one test
	// per binary can call it. LoadFromPath is the flag-free path Load delegates
	// to, and exercises the same sourcing/expansion/validation sequence.
	cfg, err := LoadFromPath(configPath)
	require.NoError(t, err, "a fresh quickstart install must load without the operator sourcing env by hand")
	require.Equal(t, "vornik", cfg.Database.Name)
	require.Equal(t, "vornik", cfg.Database.User)
}

// When a placeholder genuinely cannot be resolved, the error has to name the
// variable. "database name is required" sent the customer looking at the wrong
// thing entirely — the field was present, its ${VAR} just expanded to empty.
func TestValidateNamesTheUnresolvedPlaceholder(t *testing.T) {
	cfg := &Config{}
	cfg.Server.Address = ":8080"
	cfg.Database.Host = "127.0.0.1"
	cfg.Database.Port = 5432
	cfg.Database.Name = ""
	cfg.Database.User = "vornik"
	cfg.unresolvedPlaceholders = []string{"POSTGRES_DB", "POSTGRES_USER"}

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "database name is required", "keep the original cause")
	require.Contains(t, err.Error(), "POSTGRES_DB",
		"the operator needs to know WHICH variable did not resolve")
	require.Contains(t, err.Error(), "vornik.env",
		"and where the loader looks for it")
}

// With every placeholder resolved, the hint must not appear — a validation
// failure for an unrelated reason should not blame the environment.
func TestValidateOmitsPlaceholderHintWhenNoneUnresolved(t *testing.T) {
	cfg := &Config{}
	cfg.Server.Address = ":8080"
	cfg.Database.Host = "127.0.0.1"
	cfg.Database.Port = 5432
	cfg.Database.User = "vornik"

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "database name is required")
	require.NotContains(t, err.Error(), "vornik.env")
}
