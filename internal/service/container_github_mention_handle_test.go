package service

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestResolveGitHubAppConfig_TakesTheMentionHandleFromTheForgeBlock —
// regression for the 2026-09-03 four-week audit's secondary gap.
//
// The handle is ONE project-level setting shared by both forge ingresses. It is
// read off `forge.mention_handle` rather than a github_app key on purpose: a
// second key would let a deployment answer commands under two different names
// depending on which ingress a delivery arrived through, which is a worse
// version of the bug being fixed.
func TestResolveGitHubAppConfig_TakesTheMentionHandleFromTheForgeBlock(t *testing.T) {
	t.Setenv("GH_MENTION_TEST_SECRET", "shhh")
	p := &registry.Project{
		GitHubApp: registry.ProjectGitHubApp{
			WebhookSecretEnv: "GH_MENTION_TEST_SECRET",
			RepoAllowlist:    []string{"acme/api"},
		},
	}
	p.Forge.MentionHandle = "@acme-reviewer"

	cfg, err := resolveGitHubAppConfig(p)
	if err != nil {
		t.Fatalf("resolveGitHubAppConfig: %v", err)
	}
	if cfg.MentionHandle != "acme-reviewer" {
		t.Errorf("MentionHandle = %q, want acme-reviewer — a renamed bot never matches commands on this ingress",
			cfg.MentionHandle)
	}
}

// And it survives the translation into a multi-installation entry, which is the
// shape a daemon serving several projects actually runs.
func TestInstallationConfigFromConfig_CarriesTheMentionHandle(t *testing.T) {
	t.Setenv("GH_MENTION_TEST_SECRET_2", "shhh")
	p := &registry.Project{
		ID: "proj-1",
		GitHubApp: registry.ProjectGitHubApp{
			WebhookSecretEnv: "GH_MENTION_TEST_SECRET_2",
			RepoAllowlist:    []string{"acme/api"},
		},
	}
	p.Forge.MentionHandle = "acme-reviewer"

	cfg, err := resolveGitHubAppConfig(p)
	if err != nil {
		t.Fatalf("resolveGitHubAppConfig: %v", err)
	}
	if got := installationConfigFromConfig(p.ID, cfg).MentionHandle; got != "acme-reviewer" {
		t.Errorf("InstallationConfig.MentionHandle = %q, want acme-reviewer", got)
	}
}
