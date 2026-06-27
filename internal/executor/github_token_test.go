package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/registry"
)

func enabledGitHubProject() *registry.Project {
	return &registry.Project{GitHub: registry.ProjectGitHub{
		AppID:          4040507,
		InstallationID: 139940331,
		PrivateKeyPath: "/run/secrets/key.pem",
	}}
}

// TestInjectGitHubToken sets GH_TOKEN + GITHUB_TOKEN when the project has creds
// (the shared helper both the warm and ephemeral step paths call), and leaves
// env untouched when it doesn't.
func TestInjectGitHubToken(t *testing.T) {
	e := &Executor{ghMintFn: func(context.Context, string, int64, int64, string) (string, time.Time, error) {
		return "ghs_inject", time.Now().Add(time.Hour), nil
	}}
	env := map[string]string{}
	e.injectGitHubToken(context.Background(), env, enabledGitHubProject())
	if env["GH_TOKEN"] != "ghs_inject" || env["GITHUB_TOKEN"] != "ghs_inject" {
		t.Fatalf("want GH_TOKEN+GITHUB_TOKEN set to the minted token, got %+v", env)
	}

	env2 := map[string]string{}
	e.injectGitHubToken(context.Background(), env2, &registry.Project{}) // no creds
	if len(env2) != 0 {
		t.Errorf("no github creds must inject nothing, got %+v", env2)
	}
}

// TestInstallationToken_DisabledProject: no `github` creds → no token, no mint.
func TestInstallationToken_DisabledProject(t *testing.T) {
	calls := 0
	e := &Executor{ghMintFn: func(context.Context, string, int64, int64, string) (string, time.Time, error) {
		calls++
		return "ghs_x", time.Now().Add(time.Hour), nil
	}}
	tok, err := e.installationToken(context.Background(), &registry.Project{})
	if err != nil || tok != "" {
		t.Fatalf("disabled project: want \"\", nil; got %q, %v", tok, err)
	}
	if calls != 0 {
		t.Errorf("mint must not be called for a project without github creds (calls=%d)", calls)
	}
	// nil project is also safe.
	if tok, err := e.installationToken(context.Background(), nil); err != nil || tok != "" {
		t.Errorf("nil project: want \"\", nil; got %q, %v", tok, err)
	}
}

// TestInstallationToken_MintsAndCaches: enabled project mints once, then reuses
// the cached token across a task's later steps.
func TestInstallationToken_MintsAndCaches(t *testing.T) {
	calls := 0
	e := &Executor{ghMintFn: func(_ context.Context, apiBase string, appID, installID int64, keyPath string) (string, time.Time, error) {
		calls++
		if apiBase != "https://api.github.com" || appID != 4040507 || installID != 139940331 || keyPath != "/run/secrets/key.pem" {
			t.Errorf("mint got unexpected args: %s %d %d %s", apiBase, appID, installID, keyPath)
		}
		return "ghs_token", time.Now().Add(time.Hour), nil
	}}
	p := enabledGitHubProject()
	for i := 0; i < 3; i++ {
		tok, err := e.installationToken(context.Background(), p)
		if err != nil || tok != "ghs_token" {
			t.Fatalf("call %d: want ghs_token, nil; got %q, %v", i, tok, err)
		}
	}
	if calls != 1 {
		t.Errorf("token should be minted once and cached, minted %d times", calls)
	}
}

// TestInstallationToken_RemintsNearExpiry: a token within the renew buffer is
// re-minted rather than handed out about to lapse.
func TestInstallationToken_RemintsNearExpiry(t *testing.T) {
	calls := 0
	e := &Executor{ghMintFn: func(context.Context, string, int64, int64, string) (string, time.Time, error) {
		calls++
		return "ghs_short", time.Now().Add(time.Minute), nil // < ghTokenRenewBuffer
	}}
	p := enabledGitHubProject()
	_, _ = e.installationToken(context.Background(), p)
	_, _ = e.installationToken(context.Background(), p)
	if calls != 2 {
		t.Errorf("near-expiry token must be re-minted each call, minted %d times", calls)
	}
}

// TestInstallationToken_MintError: a mint failure surfaces, no token cached.
func TestInstallationToken_MintError(t *testing.T) {
	e := &Executor{ghMintFn: func(context.Context, string, int64, int64, string) (string, time.Time, error) {
		return "", time.Time{}, errors.New("boom")
	}}
	tok, err := e.installationToken(context.Background(), enabledGitHubProject())
	if err == nil || tok != "" {
		t.Fatalf("want error and empty token, got %q, %v", tok, err)
	}
}
