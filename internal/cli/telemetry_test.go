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
	require.Contains(t, out.String(), "Anonymous telemetry: false (source: config)")
	require.Contains(t, out.String(), "install_succeeded")
	require.Contains(t, out.String(), "project_created")
	require.Contains(t, out.String(), "telemetry.vornik.io")
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

// `vornik telemetry sample` must print the body BuildRequest actually built,
// not a re-marshal of the event. The two agree today because both go through
// Event.MarshalJSON, but they are separate paths and this command exists so an
// operator can see exactly what leaves their machine — a reconstruction that
// silently drifts would be worse than no command at all.
func TestRenderTelemetrySampleShowsTheRealRequestBody(t *testing.T) {
	event := telemetryclient.InstallEvent("2026.8.7", telemetryclient.SourceQuickstart)

	gotURL, gotBody, err := renderTelemetrySample(event)
	require.NoError(t, err)

	req, err := telemetryclient.BuildRequest(telemetryclient.DefaultEndpoint, event)
	require.NoError(t, err)
	wantBody, err := io.ReadAll(req.Body)
	require.NoError(t, err)

	require.Equal(t, req.URL.String(), gotURL)
	require.Equal(t, string(wantBody), gotBody,
		"the printed body diverged from the request BuildRequest produces")
}
