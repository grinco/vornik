// Package telemetryclient emits Vornik's closed, anonymous lifecycle schema.
package telemetryclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"time"
)

const (
	SchemaVersion         = 2
	DefaultEndpoint       = "https://hpbps3m32bqwt6h6ht2flxkfv40msrtt.lambda-url.eu-central-1.on.aws/"
	SourceQuickstart      = "quickstart"
	SourceMacOSQuickstart = "macos_quickstart"
	SourceCLIBasic        = "cli_basic"
	SourceCLITemplate     = "cli_template"
	SourceAPITemplate     = "api_template"
)

const ProductionEmissionEnabled = true

var (
	safeVersion    = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)
	releaseVersion = regexp.MustCompile(`^[0-9]{4}\.[0-9]{1,2}\.[0-9]{1,3}(-(?:alpha|beta|rc)\.?[0-9]{1,3})?$`)
)

type platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}
type properties struct {
	Template        string `json:"template"`
	AutonomyEnabled bool   `json:"autonomy_enabled"`
}

type Event struct {
	schemaVersion int
	event         string
	vornikVersion string
	platform      platform
	source        string
	properties    *properties
}

func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		SchemaVersion int         `json:"schema_version"`
		Event         string      `json:"event"`
		VornikVersion string      `json:"vornik_version"`
		Platform      platform    `json:"platform"`
		Source        string      `json:"source"`
		Properties    *properties `json:"properties,omitempty"`
	}{e.schemaVersion, e.event, e.vornikVersion, e.platform, e.source, e.properties})
}

func InstallEvent(version, source string) Event {
	return Event{schemaVersion: SchemaVersion, event: "install_succeeded", vornikVersion: sanitizeVersion(version), platform: currentPlatform(), source: normalizeSource(source, true)}
}

func ProjectEvent(version, source, template string, builtIn, autonomyEnabled bool) Event {
	if !builtIn || !safeTemplate(template) {
		template = "custom"
	}
	return Event{schemaVersion: SchemaVersion, event: "project_created", vornikVersion: sanitizeVersion(version), platform: currentPlatform(), source: normalizeSource(source, false), properties: &properties{Template: template, AutonomyEnabled: autonomyEnabled}}
}

type Client struct {
	Endpoint string
	HTTP     *http.Client
	Enabled  bool
}

func ProductionClient(effectiveEnabled bool) Client {
	return Client{Endpoint: DefaultEndpoint, Enabled: effectiveEnabled && ProductionEmissionEnabled}
}

func (c Client) Emit(ctx context.Context, event Event) error {
	if !c.Enabled {
		return nil
	}
	req, err := BuildRequest(c.Endpoint, event)
	if err != nil {
		return err
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telemetry endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func BuildRequest(endpoint string, event Event) (*http.Request, error) {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
		return nil, fmt.Errorf("telemetry endpoint must use https")
	}
	if event.schemaVersion != SchemaVersion || (event.event != "install_succeeded" && event.event != "project_created") {
		return nil, fmt.Errorf("invalid telemetry event")
	}
	data, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	if len(data) > 4096 {
		return nil, fmt.Errorf("telemetry body exceeds 4 KiB")
	}
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vornik-telemetry/2")
	return req, nil
}

func currentPlatform() platform {
	return platform{OS: normalizeOS(runtime.GOOS), Arch: normalizeArch(runtime.GOARCH)}
}
func normalizeOS(v string) string {
	switch v {
	case "linux", "darwin", "windows", "freebsd":
		return v
	default:
		return "other"
	}
}
func normalizeArch(v string) string {
	switch v {
	case "amd64", "arm64", "386", "arm":
		return v
	default:
		return "other"
	}
}
func sanitizeVersion(v string) string {
	if !safeVersion.MatchString(v) {
		return "unknown"
	}
	if releaseVersion.MatchString(v) {
		return v
	}
	return "dev"
}
func normalizeSource(v string, install bool) string {
	if install {
		if v == SourceMacOSQuickstart {
			return v
		}
		return SourceQuickstart
	}
	switch v {
	case SourceCLIBasic, SourceCLITemplate, SourceAPITemplate:
		return v
	default:
		return SourceCLIBasic
	}
}
func safeTemplate(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}
