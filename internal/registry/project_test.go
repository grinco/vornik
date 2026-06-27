package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadProjectValidation tests project validation rules.
func TestLoadProjectValidation(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		wantError bool
	}{
		{
			name: "valid minimal project",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`,
			wantError: false,
		},
		{
			name: "valid full project",
			yaml: `projectId: "test-project"
displayName: "Test Project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
defaultPriority: 75
maxConcurrentTasks: 5
autonomy:
  enabled: true
  maxTasksPerHour: 10
permissions:
  secrets:
    - "API_KEY"
  allowedTools:
    - "file_read"
    - "file_write"
`,
			wantError: false,
		},
		{
			name: "missing projectId",
			yaml: `displayName: "Test Project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`,
			wantError: true,
		},
		{
			name: "missing swarmId",
			yaml: `projectId: "test-project"
defaultWorkflowId: "test-workflow"
displayName: "Test Project"
`,
			wantError: true,
		},
		{
			name: "missing defaultWorkflowId",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
`,
			wantError: true,
		},
		{
			name: "invalid priority low",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
defaultPriority: -10
`,
			wantError: true,
		},
		{
			name: "invalid priority high",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
defaultPriority: 150
`,
			wantError: true,
		},
		{
			name: "invalid maxConcurrentTasks negative",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
maxConcurrentTasks: -1
`,
			wantError: true,
		},
		{
			name: "invalid project rate limit negative",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
rate_limit:
  tasks_per_minute: -1
`,
			wantError: true,
		},
		{
			name: "invalid project budget negative",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
budget:
  daily_hard_usd: -1
`,
			wantError: true,
		},
		{
			name: "invalid daily soft budget above hard budget",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
budget:
  daily_soft_usd: 20
  daily_hard_usd: 10
`,
			wantError: true,
		},
		{
			name: "invalid monthly soft budget above hard budget",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
budget:
  monthly_soft_usd: 20
  monthly_hard_usd: 10
`,
			wantError: true,
		},
		{
			name: "valid trading block",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
trading:
  mode: "paper"
  killSwitch: false
  caps:
    max_position_usd: 1000
    max_daily_turnover_usd: 5000
    max_orders_per_hour: 15
    max_orders_per_minute: 4
    drawdown_circuit_breaker_pct: 5
`,
			wantError: false,
		},
		{
			name: "invalid trading mode",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
trading:
  mode: "wild-west"
`,
			wantError: true,
		},
		{
			name: "invalid trading max_position_usd negative",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
trading:
  caps:
    max_position_usd: -1
`,
			wantError: true,
		},
		{
			name: "invalid trading drawdown over 100",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
trading:
  caps:
    drawdown_circuit_breaker_pct: 101
`,
			wantError: true,
		},
		{
			name: "valid webhook source with secret",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
webhooks:
  sources:
    - name: github
      secret: shhh
      task_type_template: "github-event"
`,
			wantError: false,
		},
		{
			name: "valid webhook source with secret_env",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
webhooks:
  sources:
    - name: github
      secret_env: GITHUB_WEBHOOK_SECRET
      task_type_template: "github-event"
`,
			wantError: false,
		},
		{
			name: "invalid webhook source missing secret",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
webhooks:
  sources:
    - name: github
      task_type_template: "github-event"
`,
			wantError: true,
		},
		{
			name: "invalid webhook source missing name",
			yaml: `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
webhooks:
  sources:
    - secret: shhh
      task_type_template: "github-event"
`,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "project-test")
			if err != nil {
				t.Fatalf("failed to create temp dir: %v", err)
			}
			defer func() { _ = os.RemoveAll(tmpDir) }()

			projectsDir := filepath.Join(tmpDir, "projects")
			if err := os.Mkdir(projectsDir, 0755); err != nil {
				t.Fatalf("failed to create projects dir: %v", err)
			}

			if err := os.WriteFile(filepath.Join(projectsDir, "test.yaml"), []byte(tt.yaml), 0644); err != nil {
				t.Fatalf("failed to write project: %v", err)
			}

			_, err = LoadProjects(tmpDir)
			if tt.wantError && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestProjectDefaults tests default value handling for projects.
func TestProjectDefaults(t *testing.T) {
	yaml := `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`
	tmpDir, err := os.MkdirTemp("", "project-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	projectsDir := filepath.Join(tmpDir, "projects")
	if err := os.Mkdir(projectsDir, 0755); err != nil {
		t.Fatalf("failed to create projects dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(projectsDir, "test.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("failed to write project: %v", err)
	}

	projects, err := LoadProjects(tmpDir)
	if err != nil {
		t.Fatalf("LoadProjects failed: %v", err)
	}

	project := projects["test-project"]
	if project == nil {
		t.Fatal("expected test-project")
	}

	// Check defaults
	if project.DefaultPriority != 0 {
		t.Errorf("expected default priority 0, got %d", project.DefaultPriority)
	}
	if project.Autonomy.Enabled {
		t.Error("expected autonomy to be disabled by default")
	}
}

// TestProjectAutonomyConfiguration tests autonomy settings.
func TestProjectAutonomyConfiguration(t *testing.T) {
	yaml := `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
autonomy:
  enabled: true
  maxTasksPerHour: 10
  requireApproval: false
  allowedTaskTypes:
    - "file_read"
    - "exec"
`
	tmpDir, err := os.MkdirTemp("", "project-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	projectsDir := filepath.Join(tmpDir, "projects")
	if err := os.Mkdir(projectsDir, 0755); err != nil {
		t.Fatalf("failed to create projects dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(projectsDir, "test.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("failed to write project: %v", err)
	}

	projects, err := LoadProjects(tmpDir)
	if err != nil {
		t.Fatalf("LoadProjects failed: %v", err)
	}

	project := projects["test-project"]
	if project == nil {
		t.Fatal("expected test-project")
	}

	if !project.Autonomy.Enabled {
		t.Error("expected autonomy to be enabled")
	}
	if project.Autonomy.MaxTasksPerHour != 10 {
		t.Errorf("expected MaxTasksPerHour 10, got %d", project.Autonomy.MaxTasksPerHour)
	}
}

// TestProjectTradingCaps confirms trading.caps fields parse off
// the project YAML into Project.Trading.Caps and the values
// flow through unchanged. Per-project caps are the source of
// truth for what the broker enforces — see container.brokerHeadersFor
// for the daemon → broker MCP plumbing.
func TestProjectTradingCaps(t *testing.T) {
	yaml := `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
trading:
  mode: "paper"
  killSwitch: true
  caps:
    max_position_usd: 1500.50
    max_daily_turnover_usd: 7500
    max_orders_per_hour: 12
    max_orders_per_minute: 3
    drawdown_circuit_breaker_pct: 7.5
`
	tmpDir, err := os.MkdirTemp("", "project-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	projectsDir := filepath.Join(tmpDir, "projects")
	if err := os.Mkdir(projectsDir, 0755); err != nil {
		t.Fatalf("failed to create projects dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectsDir, "test.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("failed to write project: %v", err)
	}
	projects, err := LoadProjects(tmpDir)
	if err != nil {
		t.Fatalf("LoadProjects failed: %v", err)
	}
	p := projects["test-project"]
	if p == nil {
		t.Fatal("expected test-project")
	}
	if p.Trading.Mode != "paper" {
		t.Errorf("Trading.Mode: want %q, got %q", "paper", p.Trading.Mode)
	}
	if !p.Trading.KillSwitch {
		t.Error("Trading.KillSwitch: want true")
	}
	if p.Trading.Caps.MaxPositionUSD != 1500.50 {
		t.Errorf("MaxPositionUSD: want 1500.50, got %v", p.Trading.Caps.MaxPositionUSD)
	}
	if p.Trading.Caps.MaxDailyTurnoverUSD != 7500 {
		t.Errorf("MaxDailyTurnoverUSD: want 7500, got %v", p.Trading.Caps.MaxDailyTurnoverUSD)
	}
	if p.Trading.Caps.MaxOrdersPerHour != 12 {
		t.Errorf("MaxOrdersPerHour: want 12, got %d", p.Trading.Caps.MaxOrdersPerHour)
	}
	if p.Trading.Caps.MaxOrdersPerMinute != 3 {
		t.Errorf("MaxOrdersPerMinute: want 3, got %d", p.Trading.Caps.MaxOrdersPerMinute)
	}
	if p.Trading.Caps.DrawdownCircuitBreakerPct != 7.5 {
		t.Errorf("DrawdownCircuitBreakerPct: want 7.5, got %v", p.Trading.Caps.DrawdownCircuitBreakerPct)
	}
}

// TestProjectPermissions tests permission configuration.
func TestProjectPermissions(t *testing.T) {
	yaml := `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
permissions:
  secrets:
    - "DB_PASSWORD"
    - "API_KEY"
  allowedTools:
    - "file_read"
    - "file_write"
    - "exec"
    - "http_request"
`
	tmpDir, err := os.MkdirTemp("", "project-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	projectsDir := filepath.Join(tmpDir, "projects")
	if err := os.Mkdir(projectsDir, 0755); err != nil {
		t.Fatalf("failed to create projects dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(projectsDir, "test.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("failed to write project: %v", err)
	}

	projects, err := LoadProjects(tmpDir)
	if err != nil {
		t.Fatalf("LoadProjects failed: %v", err)
	}

	project := projects["test-project"]
	if project == nil {
		t.Fatal("expected test-project")
	}

	if len(project.Permissions.Secrets) != 2 {
		t.Errorf("expected 2 secrets, got %d", len(project.Permissions.Secrets))
	}
	if len(project.Permissions.AllowedTools) != 4 {
		t.Errorf("expected 4 allowed tools, got %d", len(project.Permissions.AllowedTools))
	}
}

// TestLoadMultipleProjects tests loading multiple projects.
func TestLoadMultipleProjects(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "project-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	projectsDir := filepath.Join(tmpDir, "projects")
	if err := os.Mkdir(projectsDir, 0755); err != nil {
		t.Fatalf("failed to create projects dir: %v", err)
	}

	// Create multiple project files
	projects := []struct {
		filename string
		content  string
	}{
		{
			"project-a.yaml",
			`projectId: "project-a"
swarmId: "swarm-a"
defaultWorkflowId: "test-workflow"
displayName: "Project A"
`,
		},
		{
			"project-b.yaml",
			`projectId: "project-b"
swarmId: "swarm-b"
defaultWorkflowId: "test-workflow"
displayName: "Project B"
`,
		},
		{
			"project-c.yaml",
			`projectId: "project-c"
swarmId: "swarm-a"
defaultWorkflowId: "test-workflow"
displayName: "Project C"
`,
		},
	}

	for _, p := range projects {
		if err := os.WriteFile(filepath.Join(projectsDir, p.filename), []byte(p.content), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", p.filename, err)
		}
	}

	loaded, err := LoadProjects(tmpDir)
	if err != nil {
		t.Fatalf("LoadProjects failed: %v", err)
	}

	if len(loaded) != 3 {
		t.Errorf("expected 3 projects, got %d", len(loaded))
	}

	for _, id := range []string{"project-a", "project-b", "project-c"} {
		if loaded[id] == nil {
			t.Errorf("expected project %s to exist", id)
		}
	}
}

// TestProjectDuplicates tests duplicate project ID detection.
func TestProjectDuplicates(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "project-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	projectsDir := filepath.Join(tmpDir, "projects")
	if err := os.Mkdir(projectsDir, 0755); err != nil {
		t.Fatalf("failed to create projects dir: %v", err)
	}

	// Create two projects with same ID
	project := `projectId: "duplicate"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`
	if err := os.WriteFile(filepath.Join(projectsDir, "proj1.yaml"), []byte(project), 0644); err != nil {
		t.Fatalf("failed to write proj1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectsDir, "proj2.yaml"), []byte(project), 0644); err != nil {
		t.Fatalf("failed to write proj2: %v", err)
	}

	_, err = LoadProjects(tmpDir)
	if err == nil {
		t.Error("expected error for duplicate project IDs")
	}
}

// TestLoadProjectsEmptyDir tests loading from empty directory.
func TestLoadProjectsEmptyDir(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "project-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	projectsDir := filepath.Join(tmpDir, "projects")
	if err := os.Mkdir(projectsDir, 0755); err != nil {
		t.Fatalf("failed to create projects dir: %v", err)
	}

	projects, err := LoadProjects(tmpDir)
	if err != nil {
		t.Fatalf("LoadProjects failed: %v", err)
	}

	if len(projects) != 0 {
		t.Errorf("expected 0 projects, got %d", len(projects))
	}
}

// TestProjectGitHubApp_Validate — every cross-field rule on the
// github_app block: required-when-any-set, deny-by-default repo
// allowlist, all-or-nothing outbound credentials.
func TestProjectGitHubApp_Validate(t *testing.T) {
	base := func() Project {
		return Project{
			ID:                "p",
			SwarmID:           "s",
			DefaultWorkflowID: "w",
		}
	}

	cases := []struct {
		name      string
		mutate    func(*Project)
		wantField string // empty → expect success
	}{
		{
			name:   "zero-value disables block (success)",
			mutate: func(p *Project) {},
		},
		{
			name: "inbound-only minimal (success)",
			mutate: func(p *Project) {
				p.GitHubApp.WebhookSecretEnv = "GH_SECRET"
				p.GitHubApp.RepoAllowlist = []string{"acme/api"}
			},
		},
		{
			name: "fully-configured outbound (success)",
			mutate: func(p *Project) {
				p.GitHubApp.WebhookSecretEnv = "GH_SECRET"
				p.GitHubApp.RepoAllowlist = []string{"acme/api"}
				p.GitHubApp.AppID = 1
				p.GitHubApp.PrivateKeyPath = "/path/to/key.pem"
				p.GitHubApp.InstallationID = 99
			},
		},
		{
			name: "secret set but no repo allowlist",
			mutate: func(p *Project) {
				p.GitHubApp.WebhookSecretEnv = "GH_SECRET"
			},
			wantField: "github_app.repo_allowlist",
		},
		{
			name: "repo allowlist set but no secret",
			mutate: func(p *Project) {
				p.GitHubApp.RepoAllowlist = []string{"acme/api"}
			},
			wantField: "github_app.webhook_secret_env",
		},
		{
			name: "secret blank string treated as unset",
			mutate: func(p *Project) {
				p.GitHubApp.WebhookSecretEnv = "   "
				p.GitHubApp.RepoAllowlist = []string{"acme/api"}
			},
			wantField: "github_app.webhook_secret_env",
		},
		{
			name: "outbound partial: AppID without key/installation",
			mutate: func(p *Project) {
				p.GitHubApp.WebhookSecretEnv = "GH_SECRET"
				p.GitHubApp.RepoAllowlist = []string{"acme/api"}
				p.GitHubApp.AppID = 1
			},
			wantField: "github_app",
		},
		{
			name: "outbound partial: key path without app id / installation",
			mutate: func(p *Project) {
				p.GitHubApp.WebhookSecretEnv = "GH_SECRET"
				p.GitHubApp.RepoAllowlist = []string{"acme/api"}
				p.GitHubApp.PrivateKeyPath = "/etc/key.pem"
			},
			wantField: "github_app",
		},
		{
			name: "outbound partial: installation id without app id / key",
			mutate: func(p *Project) {
				p.GitHubApp.WebhookSecretEnv = "GH_SECRET"
				p.GitHubApp.RepoAllowlist = []string{"acme/api"}
				p.GitHubApp.InstallationID = 99
			},
			wantField: "github_app",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base()
			c.mutate(&p)
			err := p.Validate("project.yaml")
			if c.wantField == "" {
				if err != nil {
					t.Errorf("Validate: %v (want nil)", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate returned nil, want error on %q", c.wantField)
			}
			verr, ok := err.(ProjectValidationError)
			if !ok {
				t.Fatalf("Validate err = %T %v, want ProjectValidationError", err, err)
			}
			if verr.Field != c.wantField {
				t.Errorf("Validate field = %q, want %q (err=%v)", verr.Field, c.wantField, err)
			}
		})
	}
}

// TestProjectGitHubApp_Enabled — Enabled() returns true only when
// inbound is wired (secret + repo allowlist).
func TestProjectGitHubApp_Enabled(t *testing.T) {
	cases := []struct {
		name string
		g    ProjectGitHubApp
		want bool
	}{
		{"zero value disables", ProjectGitHubApp{}, false},
		{"secret only", ProjectGitHubApp{WebhookSecretEnv: "X"}, false},
		{"repos only", ProjectGitHubApp{RepoAllowlist: []string{"a/b"}}, false},
		{"both set enables", ProjectGitHubApp{WebhookSecretEnv: "X", RepoAllowlist: []string{"a/b"}}, true},
		{"blank secret string", ProjectGitHubApp{WebhookSecretEnv: "  ", RepoAllowlist: []string{"a/b"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.g.Enabled(); got != c.want {
				t.Errorf("Enabled() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestProjectEmail_Validate — every cross-field rule on the email
// block: required-when-any-set, all-or-nothing SMTP outbound trio.
func TestProjectEmail_Validate(t *testing.T) {
	base := func() Project {
		return Project{
			ID:                "p",
			SwarmID:           "s",
			DefaultWorkflowID: "w",
		}
	}

	cases := []struct {
		name      string
		mutate    func(*Project)
		wantField string
	}{
		{
			name:   "zero-value disables block (success)",
			mutate: func(p *Project) {},
		},
		{
			name: "inbound-only minimal (success)",
			mutate: func(p *Project) {
				p.Email.IMAPHost = "imap.test"
				p.Email.IMAPUsername = "u"
				p.Email.IMAPPasswordEnv = "IMAP_PASS"
			},
		},
		{
			name: "fully configured (success)",
			mutate: func(p *Project) {
				p.Email.IMAPHost = "imap.test"
				p.Email.IMAPUsername = "u"
				p.Email.IMAPPasswordEnv = "IMAP_PASS"
				p.Email.SMTPHost = "smtp.test"
				p.Email.SMTPUsername = "u"
				p.Email.SMTPPasswordEnv = "SMTP_PASS"
				p.Email.FromAddress = "u@test"
			},
		},
		{
			name: "imap host missing when other fields set",
			mutate: func(p *Project) {
				p.Email.IMAPUsername = "u"
				p.Email.IMAPPasswordEnv = "IMAP_PASS"
			},
			wantField: "email.imap_host",
		},
		{
			name: "imap username missing",
			mutate: func(p *Project) {
				p.Email.IMAPHost = "imap.test"
				p.Email.IMAPPasswordEnv = "IMAP_PASS"
			},
			wantField: "email.imap_username",
		},
		{
			name: "imap password env missing",
			mutate: func(p *Project) {
				p.Email.IMAPHost = "imap.test"
				p.Email.IMAPUsername = "u"
			},
			wantField: "email.imap_password_env",
		},
		{
			name: "smtp partial: host without rest",
			mutate: func(p *Project) {
				p.Email.IMAPHost = "imap.test"
				p.Email.IMAPUsername = "u"
				p.Email.IMAPPasswordEnv = "IMAP_PASS"
				p.Email.SMTPHost = "smtp.test"
			},
			wantField: "email",
		},
		{
			name: "smtp partial: from_address without rest",
			mutate: func(p *Project) {
				p.Email.IMAPHost = "imap.test"
				p.Email.IMAPUsername = "u"
				p.Email.IMAPPasswordEnv = "IMAP_PASS"
				p.Email.FromAddress = "u@test"
			},
			wantField: "email",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base()
			c.mutate(&p)
			err := p.Validate("project.yaml")
			if c.wantField == "" {
				if err != nil {
					t.Errorf("Validate: %v (want nil)", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate returned nil, want error on %q", c.wantField)
			}
			verr, ok := err.(ProjectValidationError)
			if !ok {
				t.Fatalf("Validate err = %T %v, want ProjectValidationError", err, err)
			}
			if verr.Field != c.wantField {
				t.Errorf("Validate field = %q, want %q (err=%v)", verr.Field, c.wantField, err)
			}
		})
	}
}

// TestProjectMCP_ToolRateLimits_Validate — rate-limit hardening sub-item 3:
// per-MCP-tool throttles MUST reject negative rps/burst at boot. A zero
// value is the documented "disabled" sentinel and stays legal so operators
// can comment-out an entry without deleting the key.
func TestProjectMCP_ToolRateLimits_Validate(t *testing.T) {
	base := func() Project {
		return Project{
			ID:                "p",
			SwarmID:           "s",
			DefaultWorkflowID: "w",
		}
	}

	cases := []struct {
		name      string
		mutate    func(*Project)
		wantField string // empty → expect success
	}{
		{
			name:   "empty map (success)",
			mutate: func(p *Project) {},
		},
		{
			name: "valid positive entry (success)",
			mutate: func(p *Project) {
				p.MCP.ToolRateLimits = map[string]ToolRateLimitSpec{
					"broker.place_order": {RPS: 1, Burst: 3},
				}
			},
		},
		{
			name: "zero values legal (treated as disabled at runtime)",
			mutate: func(p *Project) {
				p.MCP.ToolRateLimits = map[string]ToolRateLimitSpec{
					"x": {RPS: 0, Burst: 0},
				}
			},
		},
		{
			name: "negative rps rejected",
			mutate: func(p *Project) {
				p.MCP.ToolRateLimits = map[string]ToolRateLimitSpec{
					"x": {RPS: -1, Burst: 3},
				}
			},
			wantField: `mcp.toolRateLimits["x"].rps`,
		},
		{
			name: "negative burst rejected",
			mutate: func(p *Project) {
				p.MCP.ToolRateLimits = map[string]ToolRateLimitSpec{
					"y": {RPS: 1, Burst: -1},
				}
			},
			wantField: `mcp.toolRateLimits["y"].burst`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base()
			c.mutate(&p)
			err := p.Validate("x.yaml")
			if c.wantField == "" {
				if err != nil {
					t.Fatalf("Validate() unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want field %q", c.wantField)
			}
			pe, ok := err.(ProjectValidationError)
			if !ok {
				t.Fatalf("Validate() = %v (%T), want ProjectValidationError", err, err)
			}
			if pe.Field != c.wantField {
				t.Errorf("Validate() field = %q, want %q", pe.Field, c.wantField)
			}
		})
	}
}

// TestProjectEmail_Enabled — Enabled() returns true only when the
// inbound minimum (IMAP host + username + password env) is set.
func TestProjectEmail_Enabled(t *testing.T) {
	cases := []struct {
		name string
		e    ProjectEmail
		want bool
	}{
		{"zero value disables", ProjectEmail{}, false},
		{"host only", ProjectEmail{IMAPHost: "h"}, false},
		{"host + username", ProjectEmail{IMAPHost: "h", IMAPUsername: "u"}, false},
		{"all three set enables", ProjectEmail{IMAPHost: "h", IMAPUsername: "u", IMAPPasswordEnv: "P"}, true},
		{"blank host string", ProjectEmail{IMAPHost: "  ", IMAPUsername: "u", IMAPPasswordEnv: "P"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.e.Enabled(); got != c.want {
				t.Errorf("Enabled() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestToolRateLimitSpec_Enabled — strict gate matches the keybucket
// bypass contract: only RPS>0 AND Burst>0 actually enforce.
func TestToolRateLimitSpec_Enabled(t *testing.T) {
	cases := []struct {
		name string
		spec ToolRateLimitSpec
		want bool
	}{
		{"both positive", ToolRateLimitSpec{RPS: 1, Burst: 1}, true},
		{"zero rps", ToolRateLimitSpec{RPS: 0, Burst: 5}, false},
		{"zero burst", ToolRateLimitSpec{RPS: 5, Burst: 0}, false},
		{"negative rps", ToolRateLimitSpec{RPS: -1, Burst: 1}, false},
		{"zero value", ToolRateLimitSpec{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.spec.Enabled(); got != c.want {
				t.Errorf("Enabled() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestProjectEmail_AttachmentFieldsPreserved — round-trip the new
// slice-2 attachment fields through the struct to lock down the
// YAML tag wiring. No validation rules apply (zero size = no cap;
// empty dir = use daemon default) so just verify they survive
// construction.
func TestProjectEmail_AttachmentFieldsPreserved(t *testing.T) {
	e := ProjectEmail{
		IMAPHost:               "h",
		IMAPUsername:           "u",
		IMAPPasswordEnv:        "P",
		AttachmentSizeCapBytes: 25 * 1024 * 1024,
		AttachmentStoreDir:     "/var/data/email",
	}
	if e.AttachmentSizeCapBytes != 25*1024*1024 {
		t.Errorf("AttachmentSizeCapBytes = %d", e.AttachmentSizeCapBytes)
	}
	if e.AttachmentStoreDir != "/var/data/email" {
		t.Errorf("AttachmentStoreDir = %q", e.AttachmentStoreDir)
	}
	// Enabled() still trips on the inbound minimum; attachment fields
	// don't gate it (operator may want inbound without attachment
	// ingestion).
	if !e.Enabled() {
		t.Errorf("Enabled() should be true with attachment fields + inbound minimum")
	}
}

// TestProjectSlack_Enabled — Enabled() returns true only when both
// SigningSecretEnv and TeamID are set; outbound (BotTokenEnv) is
// optional and doesn't gate the boot check.
func TestProjectSlack_Enabled(t *testing.T) {
	cases := []struct {
		name string
		s    ProjectSlack
		want bool
	}{
		{"zero value disables", ProjectSlack{}, false},
		{"secret only", ProjectSlack{SigningSecretEnv: "X"}, false},
		{"team only", ProjectSlack{TeamID: "T1"}, false},
		{"both set enables", ProjectSlack{SigningSecretEnv: "X", TeamID: "T1"}, true},
		{"blank secret", ProjectSlack{SigningSecretEnv: " ", TeamID: "T1"}, false},
		{"blank team", ProjectSlack{SigningSecretEnv: "X", TeamID: " "}, false},
		{"bot token doesn't gate", ProjectSlack{SigningSecretEnv: "X", TeamID: "T1", BotTokenEnv: ""}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.Enabled(); got != c.want {
				t.Errorf("Enabled() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestProjectSlack_AllowlistsPreserved — round-trip the per-Slack
// allowlist fields through the struct to lock down the YAML tags.
func TestProjectSlack_AllowlistsPreserved(t *testing.T) {
	s := ProjectSlack{
		TeamID:           "T_X",
		SigningSecretEnv: "S",
		BotTokenEnv:      "B",
		ChannelAllowlist: []string{"C_a", "C_b"},
		SenderAllowlist:  []string{"U_x", "U_y"},
		PostMessageRPS:   5,
		PostMessageBurst: 10,
	}
	if len(s.ChannelAllowlist) != 2 {
		t.Errorf("ChannelAllowlist len = %d, want 2", len(s.ChannelAllowlist))
	}
	if len(s.SenderAllowlist) != 2 {
		t.Errorf("SenderAllowlist len = %d, want 2", len(s.SenderAllowlist))
	}
	if s.PostMessageRPS != 5 || s.PostMessageBurst != 10 {
		t.Errorf("rate-limit knobs not preserved: rps=%d burst=%d", s.PostMessageRPS, s.PostMessageBurst)
	}
}

// TestProjectVoice_Enabled — voice block is wired when either STT or
// TTS has a non-empty provider; both empty is the zero-value disabled
// state.
func TestProjectVoice_Enabled(t *testing.T) {
	cases := []struct {
		name string
		v    ProjectVoice
		want bool
	}{
		{"zero value disables", ProjectVoice{}, false},
		{"stt-only enables", ProjectVoice{STT: ProjectVoiceSTT{Provider: "whisper-local"}}, true},
		{"tts-only enables", ProjectVoice{TTS: ProjectVoiceTTS{Provider: "piper"}}, true},
		{"both enables", ProjectVoice{
			STT: ProjectVoiceSTT{Provider: "whisper-local"},
			TTS: ProjectVoiceTTS{Provider: "piper"},
		}, true},
		{"whitespace-only providers disable", ProjectVoice{
			STT: ProjectVoiceSTT{Provider: "  "},
			TTS: ProjectVoiceTTS{Provider: " \t"},
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.v.Enabled(); got != c.want {
				t.Errorf("Enabled() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestProjectVoice_FieldsPreserved — round-trip every documented
// voice field through the struct to lock down the YAML tags.
func TestProjectVoice_FieldsPreserved(t *testing.T) {
	v := ProjectVoice{
		STT: ProjectVoiceSTT{
			Provider:     "whisper-local",
			Model:        "base",
			BinaryPath:   "/usr/local/bin/whisper",
			FFmpegPath:   "/usr/bin/ffmpeg",
			LanguageHint: "en-US",
		},
		TTS: ProjectVoiceTTS{
			Provider:     "piper",
			Voice:        "en_US-amy-medium",
			BinaryPath:   "/usr/local/bin/piper",
			FFmpegPath:   "/usr/bin/ffmpeg",
			Speed:        1.1,
			MaxTextRunes: 800,
		},
	}
	if v.STT.Provider != "whisper-local" || v.STT.Model != "base" {
		t.Errorf("STT fields not preserved: %+v", v.STT)
	}
	if v.TTS.Provider != "piper" || v.TTS.Voice != "en_US-amy-medium" {
		t.Errorf("TTS fields not preserved: %+v", v.TTS)
	}
	if v.TTS.Speed != 1.1 || v.TTS.MaxTextRunes != 800 {
		t.Errorf("TTS tuning knobs not preserved: speed=%v max=%d", v.TTS.Speed, v.TTS.MaxTextRunes)
	}
}

// TestLoadProjectsNoDir tests loading when projects directory doesn't exist.
func TestLoadProjectsNoDir(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "project-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Don't create projects directory
	projects, err := LoadProjects(tmpDir)
	if err != nil {
		t.Fatalf("LoadProjects failed: %v", err)
	}

	if len(projects) != 0 {
		t.Errorf("expected 0 projects, got %d", len(projects))
	}
}

// TestProjectGitEnabled verifies that the Project.Git.Enabled opt-in field is
// correctly unmarshalled from YAML and defaults to false when the block is absent.
func TestProjectGitEnabled(t *testing.T) {
	t.Run("enabled true when block set", func(t *testing.T) {
		tmpDir := t.TempDir()
		projectsDir := filepath.Join(tmpDir, "projects")
		if err := os.MkdirAll(projectsDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		yml := `projectId: "git-enabled-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
git:
  enabled: true
`
		if err := os.WriteFile(filepath.Join(projectsDir, "git-enabled-project.yaml"), []byte(yml), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		projects, err := LoadProjects(tmpDir)
		if err != nil {
			t.Fatalf("LoadProjects: %v", err)
		}
		p, ok := projects["git-enabled-project"]
		if !ok {
			t.Fatalf("project git-enabled-project not found in map")
		}
		if !p.Git.Enabled {
			t.Errorf("expected Git.Enabled == true, got false")
		}
	})

	t.Run("disabled by default when block absent", func(t *testing.T) {
		tmpDir := t.TempDir()
		projectsDir := filepath.Join(tmpDir, "projects")
		if err := os.MkdirAll(projectsDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		yml := `projectId: "no-git-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`
		if err := os.WriteFile(filepath.Join(projectsDir, "no-git-project.yaml"), []byte(yml), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		projects, err := LoadProjects(tmpDir)
		if err != nil {
			t.Fatalf("LoadProjects: %v", err)
		}
		p, ok := projects["no-git-project"]
		if !ok {
			t.Fatalf("project no-git-project not found in map")
		}
		if p.Git.Enabled {
			t.Errorf("expected Git.Enabled == false (default), got true")
		}
	})
}
