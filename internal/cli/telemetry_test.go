package cli

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/telemetryclient"
)

func TestRunTelemetrySampleDoesNotSendAndShowsBothEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("telemetry:\n  enabled: false\n"), 0o600))
	t.Setenv("VORNIK_CONFIG", path)
	t.Setenv("VORNIK_TELEMETRY", "on")

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	require.NoError(t, runTelemetrySample(cmd, nil))
	require.Contains(t, out.String(), "Anonymous lifecycle telemetry: false (source: config)")
	require.Contains(t, out.String(), "install_succeeded")
	require.Contains(t, out.String(), "project_created")
	require.Contains(t, out.String(), telemetryclient.DefaultEndpoint)
}

func TestRunTelemetryEmitInstallRespectsExplicitOptOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("telemetry:\n  enabled: false\n"), 0o600))
	t.Setenv("VORNIK_CONFIG", path)
	t.Setenv("VORNIK_TELEMETRY", "on")

	oldFactory := lifecycleTelemetryClient
	t.Cleanup(func() { lifecycleTelemetryClient = oldFactory })
	var calls int
	lifecycleTelemetryClient = func(enabled bool) telemetryclient.Client {
		return telemetryclient.Client{
			Endpoint: "https://telemetry.example.test/v1/collect.json",
			Enabled:  enabled,
			HTTP: &http.Client{Transport: cliTelemetryRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: http.StatusAccepted,
					Body:       io.NopCloser(bytes.NewBufferString(`{"accepted":true}`)),
					Header:     make(http.Header),
				}, nil
			})},
		}
	}
	telemetryEmitInstallSource = telemetryclient.SourceQuickstart
	require.NoError(t, runTelemetryEmitInstall(&cobra.Command{}, nil))
	require.Zero(t, calls)

	require.NoError(t, os.WriteFile(path, []byte("server:\n  address: ':8080'\n"), 0o600))
	require.NoError(t, runTelemetryEmitInstall(&cobra.Command{}, nil))
	require.Equal(t, 1, calls)
}

type cliTelemetryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f cliTelemetryRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
