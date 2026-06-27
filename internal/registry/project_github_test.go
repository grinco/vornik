package registry

import "testing"

func TestProjectGitHub_Enabled(t *testing.T) {
	cases := []struct {
		name string
		g    ProjectGitHub
		want bool
	}{
		{"complete", ProjectGitHub{AppID: 1, InstallationID: 2, PrivateKeyPath: "/k"}, true},
		{"zero", ProjectGitHub{}, false},
		{"no app id", ProjectGitHub{InstallationID: 2, PrivateKeyPath: "/k"}, false},
		{"no installation id", ProjectGitHub{AppID: 1, PrivateKeyPath: "/k"}, false},
		{"no key", ProjectGitHub{AppID: 1, InstallationID: 2}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.g.Enabled(); got != tc.want {
				t.Fatalf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProjectGitHub_ResolvedAPIBaseURL(t *testing.T) {
	if got := (ProjectGitHub{}).ResolvedAPIBaseURL(); got != "https://api.github.com" {
		t.Errorf("default = %q, want public api.github.com", got)
	}
	custom := ProjectGitHub{APIBaseURL: "https://ghe.corp/api/v3"}
	if got := custom.ResolvedAPIBaseURL(); got != "https://ghe.corp/api/v3" {
		t.Errorf("override = %q, want the configured base", got)
	}
}

// TestProjectValidate_GitHubAllOrNothing: a partial github block fails
// validation; a complete one (or none) passes.
func TestProjectValidate_GitHubAllOrNothing(t *testing.T) {
	base := func() *Project {
		return &Project{ID: "p", DisplayName: "P", SwarmID: "s", DefaultWorkflowID: "w"}
	}
	t.Run("partial fails", func(t *testing.T) {
		p := base()
		p.GitHub = ProjectGitHub{AppID: 1} // missing installation_id + key
		if err := p.Validate("p.yaml"); err == nil {
			t.Fatal("partial github block must fail validation")
		}
	})
	t.Run("complete passes", func(t *testing.T) {
		p := base()
		p.GitHub = ProjectGitHub{AppID: 1, InstallationID: 2, PrivateKeyPath: "/k"}
		if err := p.Validate("p.yaml"); err != nil {
			t.Fatalf("complete github block should pass, got %v", err)
		}
	})
	t.Run("absent passes", func(t *testing.T) {
		if err := base().Validate("p.yaml"); err != nil {
			t.Fatalf("no github block should pass, got %v", err)
		}
	})
}

// TestResolveForge covers the discriminated forge config + the github back-compat
// alias resolution paths.
func TestResolveForge(t *testing.T) {
	fullGH := ProjectGitHub{AppID: 1, InstallationID: 2, PrivateKeyPath: "/k", APIBaseURL: "https://ghe/api/v3"}

	t.Run("disabled when nothing set", func(t *testing.T) {
		if _, ok := (&Project{}).ResolveForge(); ok {
			t.Fatal("no forge + no github should be disabled")
		}
	})

	t.Run("back-compat: top-level github maps to provider github", func(t *testing.T) {
		cfg, ok := (&Project{GitHub: fullGH}).ResolveForge()
		if !ok || cfg.Provider != "github" {
			t.Fatalf("cfg=%+v ok=%v", cfg, ok)
		}
		if cfg.GitHub.AppID != 1 || cfg.GitHub.PrivateKeyPath != "/k" || cfg.GitHub.APIBaseURL != "https://ghe/api/v3" {
			t.Errorf("github creds not mapped: %+v", cfg.GitHub)
		}
	})

	t.Run("explicit forge block wins", func(t *testing.T) {
		p := &Project{Forge: ProjectForge{Provider: "github", GitHub: fullGH}}
		cfg, ok := p.ResolveForge()
		if !ok || cfg.Provider != "github" || cfg.GitHub.InstallationID != 2 {
			t.Fatalf("cfg=%+v ok=%v", cfg, ok)
		}
	})

	t.Run("explicit github provider falls back to top-level creds", func(t *testing.T) {
		p := &Project{Forge: ProjectForge{Provider: "github"}, GitHub: fullGH}
		cfg, ok := p.ResolveForge()
		if !ok || cfg.GitHub.AppID != 1 {
			t.Fatalf("should reuse top-level github creds: cfg=%+v ok=%v", cfg, ok)
		}
	})

	t.Run("non-github provider carries provider only", func(t *testing.T) {
		cfg, ok := (&Project{Forge: ProjectForge{Provider: "gitlab"}}).ResolveForge()
		if !ok || cfg.Provider != "gitlab" {
			t.Fatalf("cfg=%+v ok=%v", cfg, ok)
		}
	})
}

// TestProjectValidate_ForgeBlock: unknown provider and a partial explicit github
// forge block both fail; a complete or absent block passes.
func TestProjectValidate_ForgeBlock(t *testing.T) {
	base := func() *Project {
		return &Project{ID: "p", DisplayName: "P", SwarmID: "s", DefaultWorkflowID: "w"}
	}
	t.Run("unknown provider fails", func(t *testing.T) {
		p := base()
		p.Forge = ProjectForge{Provider: "bitbucket"}
		if err := p.Validate("p.yaml"); err == nil {
			t.Fatal("unknown forge provider must fail")
		}
	})
	t.Run("partial nested github fails", func(t *testing.T) {
		p := base()
		p.Forge = ProjectForge{Provider: "github", GitHub: ProjectGitHub{AppID: 1}}
		if err := p.Validate("p.yaml"); err == nil {
			t.Fatal("partial nested github forge block must fail")
		}
	})
	t.Run("github provider with top-level creds passes", func(t *testing.T) {
		p := base()
		p.Forge = ProjectForge{Provider: "github"}
		p.GitHub = ProjectGitHub{AppID: 1, InstallationID: 2, PrivateKeyPath: "/k"}
		if err := p.Validate("p.yaml"); err != nil {
			t.Fatalf("should pass with top-level creds, got %v", err)
		}
	})
}

func TestEffectivePRReviewWorkflowID(t *testing.T) {
	// explicit pr_review_workflow_id wins
	g := ProjectGitHubApp{PRReviewWorkflowID: "github-review", ReplyWorkflowID: "github-router"}
	if got := g.EffectivePRReviewWorkflowID("def"); got != "github-review" {
		t.Errorf("explicit = %q", got)
	}
	// unset → falls back to reply workflow
	g2 := ProjectGitHubApp{ReplyWorkflowID: "github-router"}
	if got := g2.EffectivePRReviewWorkflowID("def"); got != "github-router" {
		t.Errorf("fallback-to-reply = %q", got)
	}
	// neither set → project default
	if got := (ProjectGitHubApp{}).EffectivePRReviewWorkflowID("def"); got != "def" {
		t.Errorf("fallback-to-default = %q", got)
	}
}
