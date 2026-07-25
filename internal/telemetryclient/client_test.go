package telemetryclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBuildInstallRequestContainsOnlyAllowlistedFields(t *testing.T) {
	req, err := BuildRequest("https://telemetry.vornik.io/v1/collect.json",
		InstallEvent("2026.7.4", SourceQuickstart))
	require.NoError(t, err)

	q := req.URL.Query()
	require.Equal(t, "install_succeeded", q.Get("e"))
	require.Equal(t, "1", q.Get("sv"))
	require.Equal(t, "2026.7.4", q.Get("v"))
	require.NotEmpty(t, q.Get("os"))
	require.NotEmpty(t, q.Get("arch"))
	require.Equal(t, "quickstart", q.Get("source"))
	require.Len(t, q, 6)

	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	require.ElementsMatch(t,
		[]string{"schema_version", "event", "vornik_version", "platform", "source"},
		mapKeys(got))
}

func TestProjectEventNeverLeaksCustomValues(t *testing.T) {
	event := ProjectEvent("bad version /home/alice", SourceCLITemplate,
		"secret-project-name", false, true)
	req, err := BuildRequest("https://telemetry.vornik.io/v1/collect.json", event)
	require.NoError(t, err)
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	combined := req.URL.String() + string(body)
	require.NotContains(t, combined, "alice")
	require.NotContains(t, combined, "secret-project-name")
	require.Contains(t, combined, "custom")
	require.Contains(t, combined, "unknown")
}

func TestClientBestEffortHTTPBehavior(t *testing.T) {
	var calls int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Body:       io.NopCloser(strings.NewReader(`{"accepted":true}`)),
			Header:     make(http.Header),
		}, nil
	})

	client := Client{
		Endpoint: "https://telemetry.example.test/v1/collect.json",
		HTTP:     &http.Client{Timeout: time.Second, Transport: transport},
		Enabled:  true,
	}
	require.NoError(t, client.Emit(context.Background(), InstallEvent("2026.7.4", SourceQuickstart)))
	require.Equal(t, 1, calls)

	client.Enabled = false
	require.NoError(t, client.Emit(context.Background(), InstallEvent("2026.7.4", SourceQuickstart)))
	require.Equal(t, 1, calls)
}

func TestClientRejectsRedirect(t *testing.T) {
	client := Client{
		Endpoint: "https://telemetry.example.test/v1/collect.json",
		HTTP: &http.Client{
			Timeout: time.Second,
			Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusFound,
					Body:       io.NopCloser(strings.NewReader("redirect refused")),
					Header:     make(http.Header),
				}, nil
			}),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		Enabled: true,
	}
	require.Error(t, client.Emit(context.Background(), InstallEvent("2026.7.4", SourceQuickstart)))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestNormalizePlatform(t *testing.T) {
	require.Equal(t, "other", normalizeOS("plan9"))
	require.Equal(t, "other", normalizeArch("mips64"))
	require.Equal(t, "linux", normalizeOS("linux"))
	require.Equal(t, "arm64", normalizeArch("arm64"))
}

func TestVersionSanitization(t *testing.T) {
	require.Equal(t, "unknown", sanitizeVersion(strings.Repeat("x", 65)))
	// "2026.7.4+dev" is safe-charset but unbounded build metadata, so it
	// collapses to "dev" — see TestVersionIsBoundedToReleaseIdentifiers.
	require.Equal(t, "dev", sanitizeVersion("2026.7.4+dev"))
}

func TestProductionClientEnabledAfterProviderGate(t *testing.T) {
	client := ProductionClient(true)
	require.True(t, client.Enabled)
	require.True(t, ProductionEmissionEnabled)
	require.Equal(t, DefaultEndpoint, client.Endpoint)

	require.False(t, ProductionClient(false).Enabled)
}

func TestBuildRequestRejectsUnconstructedEvent(t *testing.T) {
	_, err := BuildRequest(DefaultEndpoint, Event{})
	require.Error(t, err)
}
