package integrations

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/github"
)

// githubDefaultAPIBaseURL mirrors github.defaultAPIBaseURL (unexported in
// that package); used when the candidate doesn't override api_base_url
// (GitHub Enterprise deployments set their own).
const githubDefaultAPIBaseURL = "https://api.github.com"

// githubAppProber is the Prober adapter around github.MintInstallationToken
// — reused verbatim (design §5.3), not reimplemented. It only adds the
// PEM-parse + field-split + Outcome classification a Prober needs.
type githubAppProber struct {
	client  *http.Client
	timeout time.Duration
}

// newGitHubAppProber constructs the GitHub App Prober. client MUST already
// be guarded (DialGuard.HTTPClient) — a self-hosted GitHub Enterprise
// api_base_url is user-supplied, so this is a live SSRF surface, unlike
// telegram/slack's fixed hosts.
func newGitHubAppProber(client *http.Client, timeout time.Duration) githubAppProber {
	return githubAppProber{client: client, timeout: probeTimeout(timeout)}
}

func (p githubAppProber) Kind() string { return "github_app" }

// installationTokenHTTPStatusPattern extracts the status code out of
// MintInstallationToken's error text ("github-app: installation-token
// HTTP %d: %s") — the function returns a plain error, not a typed one, so
// this is the only way to recover the classification signal without
// changing that function's signature (out of scope: MintInstallationToken
// is reused verbatim per the brief).
var installationTokenHTTPStatusPattern = regexp.MustCompile(`installation-token HTTP (\d+)`)

func (p githubAppProber) Probe(ctx context.Context, cand CandidateConfig) ProbeResult {
	start := time.Now()

	var missing []CheckFailure
	appIDStr := strings.TrimSpace(cand.Values["app_id"])
	installationIDStr := strings.TrimSpace(cand.Values["installation_id"])
	// private_key_path is the catalog Key (matching
	// ProjectGitHubApp.PrivateKeyPath's real yaml tag, task 5.2b); the
	// candidate value under it is the literal pasted PEM, not a filesystem
	// path — see CandidateConfig's doc (Values hold literal input, not the
	// persisted representation).
	privateKeyPEM := cand.Values["private_key_path"]
	if appIDStr == "" {
		missing = append(missing, CheckFailure{Field: "app_id", Reason: "required"})
	}
	if installationIDStr == "" {
		missing = append(missing, CheckFailure{Field: "installation_id", Reason: "required"})
	}
	if strings.TrimSpace(privateKeyPEM) == "" {
		missing = append(missing, CheckFailure{Field: "private_key_path", Reason: "required"})
	}
	if len(missing) > 0 {
		return ProbeResult{
			Kind: "github_app", OK: false, Outcome: OutcomeFail,
			Summary:  "App ID, installation ID, and private key are required",
			Failures: missing, Latency: time.Since(start),
		}
	}

	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		return p.fieldFail(start, "app_id", "must be a number")
	}
	installationID, err := strconv.ParseInt(installationIDStr, 10, 64)
	if err != nil {
		return p.fieldFail(start, "installation_id", "must be a number")
	}
	key, err := github.LoadPrivateKeyPEM([]byte(privateKeyPEM))
	if err != nil {
		return p.fieldFail(start, "private_key_path", "could not parse as a PEM-encoded RSA private key")
	}

	apiBaseURL := strings.TrimSpace(cand.Values["api_base_url"])
	if apiBaseURL == "" {
		apiBaseURL = githubDefaultAPIBaseURL
	}

	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	_, expires, mintErr := github.MintInstallationToken(probeCtx, p.client, apiBaseURL, appID, installationID, key)
	latency := time.Since(start)
	if mintErr != nil {
		outcome, failures := classifyGitHubMintErr(mintErr)
		return ProbeResult{
			Kind: "github_app", OK: false, Outcome: outcome,
			Summary:  githubFailureSummary(outcome),
			Detail:   redactSecrets(mintErr.Error(), cand),
			Failures: failures,
			Latency:  latency,
		}
	}
	// MintInstallationToken returns only the token + expiry, not the
	// installation's login — fetching that would need a second GitHub API
	// call this task doesn't add (see task report). The expiry alone is a
	// secret-free, useful confirmation that minting actually worked.
	return ProbeResult{
		Kind:    "github_app",
		OK:      true,
		Outcome: OutcomeOK,
		Summary: "Installation token minted successfully (expires " + expires.Format(time.RFC3339) + ")",
		Latency: latency,
	}
}

func (p githubAppProber) fieldFail(start time.Time, field, reason string) ProbeResult {
	return ProbeResult{
		Kind: "github_app", OK: false, Outcome: OutcomeFail,
		Summary:  "GitHub App credentials are invalid",
		Failures: []CheckFailure{{Field: field, Reason: reason}},
		Latency:  time.Since(start),
	}
}

func githubFailureSummary(outcome Outcome) string {
	if outcome == OutcomeFail {
		return "GitHub rejected this App/installation configuration"
	}
	return "Couldn't reach GitHub — try again"
}

// classifyGitHubMintErr maps a MintInstallationToken error to an Outcome:
// HTTP 401/403/404 (bad app id / installation id / revoked key) is
// OutcomeFail; 429/5xx/network/JWT-signing failures are OutcomeError.
func classifyGitHubMintErr(err error) (Outcome, []CheckFailure) {
	if m := installationTokenHTTPStatusPattern.FindStringSubmatch(err.Error()); m != nil {
		status, _ := strconv.Atoi(m[1])
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return OutcomeFail, []CheckFailure{{Field: "installation_id", Reason: "GitHub rejected the App/installation credentials"}}
		}
	}
	return OutcomeError, nil
}
