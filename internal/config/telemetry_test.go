package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestTelemetryConfig_DefaultAndExplicitPresence(t *testing.T) {
	t.Run("compiled default is enabled but not explicit", func(t *testing.T) {
		cfg := DefaultConfig()
		require.True(t, cfg.Telemetry.Enabled)
		require.False(t, cfg.Telemetry.Explicit())
	})

	t.Run("absent block preserves default", func(t *testing.T) {
		cfg := DefaultConfig()
		require.NoError(t, yaml.Unmarshal([]byte("server:\n  address: ':8080'\n"), cfg))
		require.True(t, cfg.Telemetry.Enabled)
		require.False(t, cfg.Telemetry.Explicit())
	})

	t.Run("explicit false is distinguishable", func(t *testing.T) {
		cfg := DefaultConfig()
		require.NoError(t, yaml.Unmarshal([]byte("telemetry:\n  enabled: false\n"), cfg))
		require.False(t, cfg.Telemetry.Enabled)
		require.True(t, cfg.Telemetry.Explicit())
	})

	t.Run("explicit true is distinguishable", func(t *testing.T) {
		cfg := DefaultConfig()
		require.NoError(t, yaml.Unmarshal([]byte("telemetry:\n  enabled: true\n"), cfg))
		require.True(t, cfg.Telemetry.Enabled)
		require.True(t, cfg.Telemetry.Explicit())
	})
}

func TestTelemetryConfigResolve(t *testing.T) {
	tests := []struct {
		name    string
		cfg     TelemetryConfig
		env     string
		enabled bool
		source  string
		wantErr bool
	}{
		{"default", TelemetryConfig{Enabled: true}, "", true, "default", false},
		{"env off", TelemetryConfig{Enabled: true}, "off", false, "environment", false},
		{"env on", TelemetryConfig{Enabled: true}, "YES", true, "environment", false},
		{"invalid fails closed", TelemetryConfig{Enabled: true}, "perhaps", false, "environment", true},
		{"explicit false wins", explicitTelemetry(false), "on", false, "config", false},
		{"explicit true wins", explicitTelemetry(true), "off", true, "config", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enabled, source, err := tt.cfg.Resolve(tt.env)
			require.Equal(t, tt.enabled, enabled)
			require.Equal(t, tt.source, source)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestTelemetryConfigImplicitDefaultIsNotMarshaled(t *testing.T) {
	data, err := yaml.Marshal(DefaultConfig())
	require.NoError(t, err)
	require.NotContains(t, string(data), "telemetry:")

	cfg := DefaultConfig()
	cfg.Telemetry = explicitTelemetry(false)
	data, err = yaml.Marshal(cfg)
	require.NoError(t, err)
	require.Contains(t, string(data), "telemetry:")
	require.Contains(t, string(data), "enabled: false")
}

func explicitTelemetry(enabled bool) TelemetryConfig {
	return TelemetryConfig{Enabled: enabled, explicit: true}
}
