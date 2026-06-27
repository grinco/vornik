package executor

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/registry"
)

// ghCachedToken is a minted installation token and its expiry.
type ghCachedToken struct {
	token   string
	expires time.Time
}

// ghTokenRenewBuffer re-mints a cached token this long before it expires so a
// long-running task's later steps never present a token about to lapse.
const ghTokenRenewBuffer = 5 * time.Minute

// installationToken returns a short-lived GitHub App installation token for the
// project's agent outbound, or "" (no error) when the project has no `github`
// credentials. Tokens are cached per installation id and reused across a task's
// steps until ghTokenRenewBuffer before expiry. Concurrency-safe.
func (e *Executor) installationToken(ctx context.Context, project *registry.Project) (string, error) {
	if project == nil || !project.GitHub.Enabled() {
		return "", nil
	}
	g := project.GitHub

	e.ghTokenMu.Lock()
	defer e.ghTokenMu.Unlock()
	if e.ghTokens == nil {
		e.ghTokens = make(map[int64]ghCachedToken)
	}
	if c, ok := e.ghTokens[g.InstallationID]; ok && time.Until(c.expires) > ghTokenRenewBuffer {
		return c.token, nil
	}

	mint := e.ghMintFn
	if mint == nil {
		mint = defaultGitHubMint
	}
	token, expires, err := mint(ctx, g.ResolvedAPIBaseURL(), g.AppID, g.InstallationID, g.PrivateKeyPath)
	if err != nil {
		return "", err
	}
	e.ghTokens[g.InstallationID] = ghCachedToken{token: token, expires: expires}
	return token, nil
}

// injectGitHubToken mints a short-lived installation token for the project (if
// it wires `github` credentials) and sets GH_TOKEN + GITHUB_TOKEN into env so
// the agent can authenticate gh / git push. Shared by BOTH the warm and
// ephemeral step paths — keeping the injection in one place prevents the
// warm/ephemeral drift that left ephemeral agents (the dev-swarm default)
// without a token (incident 2026-06-13: github-classifier hit "gh: not logged
// into any GitHub hosts"). No-op when the project has no github credentials;
// a mint failure is logged but never fails the step (outbound just degrades).
func (e *Executor) injectGitHubToken(ctx context.Context, env map[string]string, project *registry.Project) {
	tok, err := e.installationToken(ctx, project)
	if err != nil {
		projectID := ""
		if project != nil {
			projectID = project.ID
		}
		e.logger.Warn().Err(err).Str("project_id", projectID).
			Msg("github: installation-token mint failed; agent outbound will be unauthenticated")
		return
	}
	if tok != "" {
		env["GH_TOKEN"] = tok
		env["GITHUB_TOKEN"] = tok
	}
}

// defaultGitHubMint loads the App private key from disk and exchanges a JWT for
// an installation access token via the GitHub REST API. The key never leaves
// this process — only the short-lived token is returned to the caller, which
// injects it as GH_TOKEN into the agent container.
func defaultGitHubMint(ctx context.Context, apiBase string, appID, installID int64, keyPath string) (string, time.Time, error) {
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read github private key: %w", err)
	}
	key, err := github.LoadPrivateKeyPEM(keyBytes)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parse github private key: %w", err)
	}
	return github.MintInstallationToken(ctx, &http.Client{Timeout: 15 * time.Second}, apiBase, appID, installID, key)
}
