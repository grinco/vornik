package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveTelemetryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("telemetry:\n  enabled: false\ninvalid_other_section: [\n"), 0o600))
	_, _, err := ResolveTelemetryFile(path, "")
	require.Error(t, err, "malformed YAML must fail closed")

	require.NoError(t, os.WriteFile(path, []byte("telemetry:\n  enabled: false\nunrelated: value\n"), 0o600))
	enabled, source, err := ResolveTelemetryFile(path, "on")
	require.NoError(t, err)
	require.False(t, enabled)
	require.Equal(t, "config", source)
}
