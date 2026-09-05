package registry

import "testing"

// TestResolveForge_CarriesTheMentionHandle — regression for the 2026-09-03
// four-week audit.
//
// docs/public/features/forge.md and the 2026.9.1 release notes both told
// operators to set `mention_handle`, and forge.Config has carried the field
// since bd862ee6 — but nothing resolved a YAML key onto it, so the setting did
// not exist and every deployment silently ran on the default.
func TestResolveForge_CarriesTheMentionHandle(t *testing.T) {
	p := &Project{}
	p.Forge.Provider = "github"
	p.Forge.MentionHandle = "acme-reviewer"
	p.Forge.GitHub.AppID = 1
	p.Forge.GitHub.InstallationID = 2
	p.Forge.GitHub.PrivateKeyPath = "/tmp/key.pem"

	cfg, ok := p.ResolveForge()
	if !ok {
		t.Fatal("ResolveForge reported the forge disabled")
	}
	if cfg.MentionHandle != "acme-reviewer" {
		t.Errorf("MentionHandle = %q, want acme-reviewer", cfg.MentionHandle)
	}
}

// The handle applies to a project whose credentials come from the back-compat
// top-level `github:` block too — otherwise renaming the bot would work only
// for projects that had migrated to the `forge:` block.
func TestResolveForge_MentionHandleOnTheBackCompatPath(t *testing.T) {
	p := &Project{}
	p.Forge.MentionHandle = "acme-reviewer"
	p.GitHub.AppID = 1
	p.GitHub.InstallationID = 2
	p.GitHub.PrivateKeyPath = "/tmp/key.pem"

	cfg, ok := p.ResolveForge()
	if !ok {
		t.Fatal("ResolveForge reported the forge disabled on the back-compat path")
	}
	if cfg.MentionHandle != "acme-reviewer" {
		t.Errorf("MentionHandle = %q, want acme-reviewer", cfg.MentionHandle)
	}
}

// The leading @ is optional in YAML and normalised away: the matchers strip it
// too, but a handle that reaches two comparisons in two spellings is how the
// two ingresses drift apart.
func TestMentionHandle_NormalisesTheAtSign(t *testing.T) {
	for _, in := range []string{"@acme-reviewer", " @acme-reviewer ", "acme-reviewer"} {
		p := &Project{}
		p.Forge.MentionHandle = in
		if got := p.MentionHandle(); got != "acme-reviewer" {
			t.Errorf("MentionHandle(%q) = %q, want acme-reviewer", in, got)
		}
	}
	p := &Project{}
	if got := p.MentionHandle(); got != "" {
		t.Errorf("unset MentionHandle = %q, want empty so each consumer applies its own default", got)
	}
}
