package service

// Watchdog wiring regression tests. The 2026-05-18 stuck-execution
// incident traced back to initWatchdog blindly copying
// WatchdogConfig.Enabled (a plain bool) over watchdog.DefaultConfig()'s
// Enabled: true — so a config.yaml with no `watchdog:` block sailed
// past the loader with Enabled = Go's zero value (false) and shipped
// the safety net DARK. Field is now *bool; initWatchdog only
// overrides when the operator explicitly set it. Tests below pin
// both halves of the contract.

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/storage"
)

// loadConfigForTest mirrors the production Load() path — start
// from DefaultConfig(), then yaml.Unmarshal the operator file on
// top. Tests use this to exercise the same defaults-then-override
// merge the daemon does, without depending on the filesystem.
func loadConfigForTest(t *testing.T, yamlBody string) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	require.NoError(t, yaml.Unmarshal([]byte(yamlBody), cfg))
	return cfg
}

// containerForWatchdogTest builds the minimal Container shape
// initWatchdog reads: Config (operator YAML merged onto defaults),
// Logger, and a repos with non-nil Executions so watchdog.New()
// returns a real *Watchdog whose Enabled() reflects the wired
// config rather than the nil-shortcut.
func containerForWatchdogTest(cfg *config.Config) *Container {
	return &Container{
		Logger: zerolog.Nop(),
		Config: cfg,
		repos: &storage.Repositories{
			Executions: &mocks.MockExecutionRepository{},
			Tasks:      &mocks.MockTaskRepository{},
		},
	}
}

// TestWatchdogConfig_EnabledDefaultsWhenBlockAbsent — pins the
// 2026-05-18 regression. An operator YAML that doesn't mention
// `watchdog:` at all must still ship the safety net armed: the
// merge of DefaultConfig() (Enabled: *true) + yaml.Unmarshal with
// no `watchdog:` key leaves the pointer alone, so initWatchdog
// sees nil and keeps the package default (true).
func TestWatchdogConfig_EnabledDefaultsWhenBlockAbsent(t *testing.T) {
	cfg := loadConfigForTest(t, `
server:
  address: ":8080"
database:
  host: localhost
  port: 5432
  name: vornik
  user: vornik
`)
	// Loader-level invariant: pointer survived unmarshal.
	require.NotNil(t, cfg.Watchdog.Enabled, "DefaultConfig() pointer must survive a yaml.Unmarshal that omits the watchdog block")
	assert.True(t, *cfg.Watchdog.Enabled, "default Enabled should be true when YAML omits the watchdog block")

	c := containerForWatchdogTest(cfg)
	require.NoError(t, c.initWatchdog())
	require.NotNil(t, c.Watchdog)
	assert.True(t, c.Watchdog.Enabled(), "initWatchdog must keep the package default (Enabled: true) when YAML omits the watchdog block")
}

// TestWatchdogConfig_EnabledFalseHonored — operator who *explicitly*
// disables the watchdog gets what they asked for. The pointer-bool
// migration only rescues the absent case; an explicit `enabled: false`
// still flips Enabled to false on the wired watchdog.
func TestWatchdogConfig_EnabledFalseHonored(t *testing.T) {
	cfg := loadConfigForTest(t, `
server:
  address: ":8080"
database:
  host: localhost
  port: 5432
  name: vornik
  user: vornik
watchdog:
  enabled: false
`)
	require.NotNil(t, cfg.Watchdog.Enabled)
	assert.False(t, *cfg.Watchdog.Enabled, "explicit enabled: false must materialise as a *bool pointing at false")

	c := containerForWatchdogTest(cfg)
	require.NoError(t, c.initWatchdog())
	require.NotNil(t, c.Watchdog)
	assert.False(t, c.Watchdog.Enabled(), "initWatchdog must honour an explicit enabled: false from the operator YAML")
}

// TestWatchdogConfig_PartialBlockKeepsDefault — operator who wrote
// a `watchdog:` block but only set `interval:` (no `enabled:`) still
// gets the safety net on. Belt-and-suspenders for the pointer-bool
// shape: confirms yaml.Unmarshal doesn't overwrite an unrelated
// pointer field while patching siblings.
func TestWatchdogConfig_PartialBlockKeepsDefault(t *testing.T) {
	cfg := loadConfigForTest(t, `
server:
  address: ":8080"
database:
  host: localhost
  port: 5432
  name: vornik
  user: vornik
watchdog:
  interval: "30s"
`)
	require.NotNil(t, cfg.Watchdog.Enabled, "yaml.Unmarshal should not zero the Enabled pointer when only interval is set")
	assert.True(t, *cfg.Watchdog.Enabled)
	assert.Equal(t, "30s", cfg.Watchdog.Interval)

	c := containerForWatchdogTest(cfg)
	require.NoError(t, c.initWatchdog())
	require.NotNil(t, c.Watchdog)
	assert.True(t, c.Watchdog.Enabled(), "partial watchdog block (no enabled key) must keep the package default true")
}
