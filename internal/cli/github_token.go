package cli

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/github"
)

// `vornikctl github-token` mints a short-lived GitHub App installation access
// token and prints it to stdout. The headmatch GitHub automation uses it to
// hand the agent a GH_TOKEN for `git push` / `gh pr` without a long-lived PAT:
//
//	GH_TOKEN=$(vornikctl github-token \
//	  --app-id 4040507 --installation-id 139940331 \
//	  --key /app/input/secrets/github-app.pem)
//
// Tokens are valid ~1h; mint one per publish step.
var (
	ghTokenAppID   int64
	ghTokenInstall int64
	ghTokenKeyPath string
	ghTokenAPIBase string
	ghTokenWithExp bool
)

var githubTokenCmd = &cobra.Command{
	Use:   "github-token",
	Short: "Mint a GitHub App installation access token (for agent git/gh outbound)",
	Long: `Exchange a GitHub App JWT for an installation access token via
POST /app/installations/{id}/access_tokens and print the token to stdout.

The private key never leaves this process; only the short-lived (~1h) token is
emitted. Intended to be called from an agent publish step to populate GH_TOKEN.`,
	RunE: runGitHubToken,
}

func init() {
	f := githubTokenCmd.Flags()
	f.Int64Var(&ghTokenAppID, "app-id", 0, "GitHub App ID (required)")
	f.Int64Var(&ghTokenInstall, "installation-id", 0, "GitHub App installation ID (required)")
	f.StringVar(&ghTokenKeyPath, "key", "", "path to the App private key PEM (required)")
	f.StringVar(&ghTokenAPIBase, "api-base", "https://api.github.com", "GitHub REST API base URL")
	f.BoolVar(&ghTokenWithExp, "with-expiry", false, "also print the RFC3339 expiry on a second line")
	rootCmd.AddCommand(githubTokenCmd)
}

func runGitHubToken(cmd *cobra.Command, _ []string) error {
	if ghTokenAppID == 0 || ghTokenInstall == 0 || ghTokenKeyPath == "" {
		return fmt.Errorf("--app-id, --installation-id, and --key are all required")
	}
	keyBytes, err := os.ReadFile(ghTokenKeyPath)
	if err != nil {
		return fmt.Errorf("read private key %q: %w", ghTokenKeyPath, err)
	}
	key, err := github.LoadPrivateKeyPEM(keyBytes)
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	token, expires, err := github.MintInstallationToken(
		cmd.Context(), client, ghTokenAPIBase, ghTokenAppID, ghTokenInstall, key)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), token); err != nil {
		return err
	}
	if ghTokenWithExp {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), expires.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return nil
}
