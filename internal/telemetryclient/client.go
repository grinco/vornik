// Package telemetryclient builds and emits Vornik's anonymous lifecycle
// telemetry. Its wire types are intentionally closed: callers cannot attach
// arbitrary properties or identifiers.
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
	"strconv"
	"time"
)

const (
	// SchemaVersion is the initial anonymous lifecycle telemetry wire schema.
	SchemaVersion = 1
	// DefaultEndpoint is the fixed production lifecycle telemetry URL.
	//
	// Path-less on purpose. The collector is reached through an API Gateway
	// regional custom domain in eu-central-1 (grinco/vornik-infra
	// `domain.tf`), and it accepts both `POST /` and `POST /v1/collect.json`,
	// so naming the bare host is the simpler of two equally valid forms.
	//
	// A CNAME cannot point straight at a Lambda Function URL, which is why the
	// custom domain exists: DNS-only, the Function URL serves a certificate for
	// *.lambda-url.eu-central-1.on.aws and SNI verification fails; proxied with
	// the original Host, Function URLs route on the Host they are addressed by
	// and answer 403. API Gateway serves the vornik.io certificate itself and
	// does not care about the Host — and no CDN sits in the request path, which
	// is what lets the privacy notice keep promising that no request is recorded
	// together with a CDN/WAF identifier.
	//
	// This constant must name a host the project controls; TestDefaultEndpoint-
	// IsOnTheProjectDomain enforces it. grinco/vornik#8 proposed replacing it
	// with a raw Function URL while telemetry stayed on by default, and nothing
	// in the build objected.
	DefaultEndpoint = "https://telemetry.vornik.io"

	// SourceQuickstart identifies the Linux quickstart installer.
	SourceQuickstart = "quickstart"
	// SourceMacOSQuickstart identifies the macOS quickstart installer.
	SourceMacOSQuickstart = "macos_quickstart"
	// SourceCLIBasic identifies basic CLI project creation.
	SourceCLIBasic = "cli_basic"
	// SourceCLITemplate identifies template-based CLI project creation.
	SourceCLITemplate = "cli_template"
	// SourceAPITemplate identifies template creation through the API or UI.
	SourceAPITemplate = "api_template"
	// SourceConfigFile identifies a project the daemon first observed in the
	// config tree — written by hand, by config management, or copied from an
	// existing project — rather than created through a wizard.
	//
	// It needed its own member rather than reusing one: normaliseSource folds
	// anything unrecognised to SourceCLIBasic, so emitting without extending the
	// enum would have MISLABELLED the event rather than fixed the gap, which is
	// worse than the silence it replaced. The three wizard paths counted projects
	// created through a wizard; this is the one that counts projects that exist.
	SourceConfigFile = "config_file"
)

// ProductionEmissionEnabled is the compile-time gate on emitting from
// default-configuration installs. The criterion it stands for is not that the
// endpoint answers — it is that the receiving end actually RETAINS what the
// privacy notice promises. See the history at the bottom for why that is the
// distinction worth encoding.
//
// The collector behind telemetry.vornik.io moved from a Cloudflare Worker to
// AWS on 2026-08-20 (grinco/vornik-infra). DefaultEndpoint's own comment
// covers the transport; what matters here is what retains what:
//
//   - one bucketed line in CloudWatch Logs, ~7 days;
//   - a DynamoDB COUNTER keyed by (day, dimensions), TTL pinned to the start of
//     its own day, ~90 days.
//
// A counter rather than an event row, carrying no timestamp finer than the day
// key, is what keeps the data anonymous rather than pseudonymous — a privacy
// property, not a storage optimisation. Two identical events stay
// indistinguishable afterwards, which is the whole point. The body is
// size-checked and never parsed, stored, or forwarded.
//
// Both retention figures are published, so they are not tuning knobs: changing
// one changes what users were told.
//
// This is now the ONLY backend. The Cloudflare Worker that implemented the same
// contract was decommissioned 2026-08-20. Its source survives at
// deployments/telemetry-server/ and is worth keeping — the response body, the
// dimension vocabularies and the test cases were all ported from it, so it is
// the provenance of shapes that look arbitrary otherwise — but it serves no
// traffic. Read it as a reference implementation, not a second deployment.
//
// What that costs: while both existed, each was a cross-check on the other. Now
// nothing independently corroborates that the collector does what the notice
// says, and lambda/telemetry/handler.test.mjs is what is left of that check.
//
// Transport gate SATISFIED 2026-08-20, verified rather than assumed:
// telemetry.vornik.io resolves to the API Gateway regional domain
// (d-mwo17pkge4.execute-api.eu-central-1.amazonaws.com), presents a certificate
// for telemetry.vornik.io that passes hostname verification, and routes both
// declared paths to the Lambda — POST / and POST /v1/collect.json each answer
// 415 without a content type, while POST /nope answers 404. The 415 rather than
// a 403 is the part that matters: the gateway reached the function.
//
// Emission was once enabled while the "aggregateable edge logs" criterion was
// not met — the endpoint accepted events and discarded them, so nothing was
// countable anywhere. That is the failure this constant exists to prevent, and
// it is not hypothetical: a delivery fault is invisible from inside the product,
// because Emit treats every transport error as diagnostic and never changes the
// user operation, and nothing in a build will tell you the collector stopped
// retaining. So do not set this true unless the receiving end retains the
// dimensions AND telemetry.vornik.io resolves to the API Gateway target rather
// than to the Lambda's Function URL — a CNAME pointed straight at the Function
// URL fails hostname verification and drops every event in silence, which is
// exactly the state this endpoint was in earlier on 2026-08-20.
const ProductionEmissionEnabled = true

var safeVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)

// releaseVersion matches the bounded release identifiers that may travel on the
// wire: a calendar version, optionally with a simple pre-release tag.
//
// Anything else collapses to "dev". A `git describe` build stamp such as
// 2026.7.4-112-g29df3bdb(-dirty) identifies exactly one commit, so putting it
// in a URL would contradict the design's own rule — "never put a unique value
// in them because URLs are especially likely to be logged" — and, paired with
// the source IP the edge necessarily sees, would weaken the anonymity the
// privacy page promises. Release tags are shared by every install of that
// release, so their cardinality stays bounded.
var releaseVersion = regexp.MustCompile(`^[0-9]{4}\.[0-9]{1,2}\.[0-9]{1,3}(-(?:alpha|beta|rc)\.?[0-9]{1,3})?$`)

type platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type properties struct {
	Template        string `json:"template"`
	AutonomyEnabled bool   `json:"autonomy_enabled"`
}

// Event is closed to this package so arbitrary caller data cannot reach JSON.
type Event struct {
	schemaVersion int
	event         string
	vornikVersion string
	platform      platform
	source        string
	properties    *properties
}

// MarshalJSON exposes only the fixed wire shape; Event fields themselves stay
// private so callers cannot attach arbitrary payload data.
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		SchemaVersion int         `json:"schema_version"`
		Event         string      `json:"event"`
		VornikVersion string      `json:"vornik_version"`
		Platform      platform    `json:"platform"`
		Source        string      `json:"source"`
		Properties    *properties `json:"properties,omitempty"`
	}{
		SchemaVersion: e.schemaVersion,
		Event:         e.event,
		VornikVersion: e.vornikVersion,
		Platform:      e.platform,
		Source:        e.source,
		Properties:    e.properties,
	})
}

// InstallEvent constructs the closed successful-install event shape.
func InstallEvent(version, source string) Event {
	return Event{
		schemaVersion: SchemaVersion,
		event:         "install_succeeded",
		vornikVersion: sanitizeVersion(version),
		platform:      currentPlatform(),
		source:        normalizeSource(source, true),
	}
}

// ProjectEvent accepts only a catalog-membership decision from its caller.
// Unknown/custom template slugs never enter the event.
func ProjectEvent(version, source, template string, builtIn, autonomyEnabled bool) Event {
	if !builtIn {
		template = "custom"
	}
	if !safeTemplate(template) {
		template = "custom"
	}
	return Event{
		schemaVersion: SchemaVersion,
		event:         "project_created",
		vornikVersion: sanitizeVersion(version),
		platform:      currentPlatform(),
		source:        normalizeSource(source, false),
		properties: &properties{
			Template:        template,
			AutonomyEnabled: autonomyEnabled,
		},
	}
}

// Client emits closed lifecycle telemetry events to a fixed endpoint.
type Client struct {
	Endpoint string
	HTTP     *http.Client
	Enabled  bool
}

// ProductionClient returns the fixed-endpoint client. The compile-time rollout
// gate documents that telemetry.vornik.io passed its privacy and delivery
// checks before production emission was enabled.
func ProductionClient(effectiveEnabled bool) Client {
	return Client{
		Endpoint: DefaultEndpoint,
		Enabled:  effectiveEnabled && ProductionEmissionEnabled,
	}
}

// Emit sends one event synchronously. Callers treat its error as diagnostic;
// telemetry must never change the result of the user operation.
func (c Client) Emit(ctx context.Context, event Event) error {
	if !c.Enabled {
		return nil
	}
	req, err := BuildRequest(c.Endpoint, event)
	if err != nil {
		return err
	}
	req = req.WithContext(ctx)
	client := c.HTTP
	if client == nil {
		client = &http.Client{
			Timeout: 2 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telemetry endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// BuildRequest constructs the allowlisted URL and bounded JSON request.
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
	q := u.Query()
	if event.schemaVersion != SchemaVersion ||
		(event.event != "install_succeeded" && event.event != "project_created") {
		return nil, fmt.Errorf("invalid telemetry event")
	}
	q.Set("e", event.event)
	q.Set("sv", strconv.Itoa(event.schemaVersion))
	q.Set("v", event.vornikVersion)
	q.Set("os", event.platform.OS)
	q.Set("arch", event.platform.Arch)
	q.Set("source", event.source)
	// Project properties travel as URL dimensions too, not only in the body.
	// The edge aggregates from the query string precisely so it never has to
	// parse the body; leaving these body-only made "which template, and was
	// autonomy on" unanswerable from anywhere. Both are bounded: a catalog
	// slug or "custom", and a boolean.
	if event.properties != nil {
		q.Set("tpl", event.properties.Template)
		// Written as an explicit 0/1 rather than omitted-when-false, so a
		// missing key means "old client", not "autonomy disabled".
		if event.properties.AutonomyEnabled {
			q.Set("auto", "1")
		} else {
			q.Set("auto", "0")
		}
	}
	u.RawQuery = q.Encode()
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
	req.Header.Set("User-Agent", "vornik-telemetry/1")
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
	// Safe characters but not a bounded release identifier — a source build,
	// a git-describe stamp, or build metadata. Report the fact, not the build.
	return "dev"
}

func normalizeSource(v string, install bool) string {
	if install {
		switch v {
		case SourceQuickstart, SourceMacOSQuickstart:
			return v
		}
		return SourceQuickstart
	}
	switch v {
	case SourceCLIBasic, SourceCLITemplate, SourceAPITemplate, SourceConfigFile:
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
