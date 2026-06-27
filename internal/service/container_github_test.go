package service

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// writePEM emits a freshly generated 2048-bit RSA private key to
// the given path in PKCS#1 PEM form. Returns the path back.
func writePEM(t *testing.T, path string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write pem: %v", err)
	}
	return path
}

// inboundOnlyProject returns a Project value (not pointer) with the
// minimum github_app fields wired for inbound-only operation.
func inboundOnlyProject(id string) *registry.Project {
	return &registry.Project{
		ID:                id,
		SwarmID:           "s",
		DefaultWorkflowID: "w",
		GitHubApp: registry.ProjectGitHubApp{
			WebhookSecretEnv: "GH_TEST_SECRET",
			RepoAllowlist:    []string{"acme/api"},
		},
	}
}

// TestBuildGitHubChannel_NoProjectsEnabled — empty input returns
// (nil, nil, nil); caller skips mounting the route.
func TestBuildGitHubChannel_NoProjectsEnabled(t *testing.T) {
	ch, ps, err := buildGitHubChannel(nil)
	if err != nil {
		t.Fatalf("buildGitHubChannel(nil): %v", err)
	}
	if ch != nil || ps != nil {
		t.Errorf("expected (nil, nil), got channel=%v projects=%v", ch, ps)
	}
}

// TestBuildGitHubChannel_AllDisabled — projects exist but none
// has github_app.Enabled() = true. Same as empty.
func TestBuildGitHubChannel_AllDisabled(t *testing.T) {
	ps := []*registry.Project{
		{ID: "p1", SwarmID: "s", DefaultWorkflowID: "w"},
		{ID: "p2", SwarmID: "s", DefaultWorkflowID: "w"},
	}
	ch, enabled, err := buildGitHubChannel(ps)
	if err != nil {
		t.Fatalf("buildGitHubChannel: %v", err)
	}
	if ch != nil || enabled != nil {
		t.Errorf("expected (nil, nil), got channel=%v enabled=%v", ch, enabled)
	}
}

// TestBuildGitHubChannel_InboundOnly_Constructs — happy path:
// single enabled project produces a working channel.
func TestBuildGitHubChannel_InboundOnly_Constructs(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	ps := []*registry.Project{inboundOnlyProject("p-1")}
	ch, enabled, err := buildGitHubChannel(ps)
	if err != nil {
		t.Fatalf("buildGitHubChannel: %v", err)
	}
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}
	if len(enabled) != 1 || enabled[0].ID != "p-1" {
		t.Errorf("enabled = %v, want one project [p-1]", enabled)
	}
	if ch.Name() != "github-app" {
		t.Errorf("Name() = %q", ch.Name())
	}
}

// TestBuildGitHubChannel_MissingSecretEnv — env var unset → boot fails.
func TestBuildGitHubChannel_MissingSecretEnv(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "")
	ps := []*registry.Project{inboundOnlyProject("p-1")}
	_, _, err := buildGitHubChannel(ps)
	if err == nil || !strings.Contains(err.Error(), "webhook_secret_env") {
		t.Errorf("err = %v, want missing-env failure", err)
	}
}

// TestBuildGitHubChannel_BadPrivateKeyPath — outbound configured
// with a missing PEM aborts boot.
func TestBuildGitHubChannel_BadPrivateKeyPath(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	p := inboundOnlyProject("p-1")
	p.GitHubApp.AppID = 12345
	p.GitHubApp.InstallationID = 99
	p.GitHubApp.PrivateKeyPath = "/nonexistent/key.pem"
	_, _, err := buildGitHubChannel([]*registry.Project{p})
	if err == nil || !strings.Contains(err.Error(), "private_key_path") {
		t.Errorf("err = %v, want PEM read failure", err)
	}
}

// TestBuildGitHubChannel_OutboundFullyConfigured — every outbound
// field set + a parseable PEM constructs cleanly with PrivateKey
// loaded.
func TestBuildGitHubChannel_OutboundFullyConfigured(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	pemPath := writePEM(t, filepath.Join(t.TempDir(), "key.pem"))
	p := inboundOnlyProject("p-1")
	p.GitHubApp.AppID = 12345
	p.GitHubApp.InstallationID = 99
	p.GitHubApp.PrivateKeyPath = pemPath
	ch, _, err := buildGitHubChannel([]*registry.Project{p})
	if err != nil {
		t.Fatalf("buildGitHubChannel: %v", err)
	}
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}
}

// TestBuildGitHubChannel_MultipleEnabled_BuildsMultiInstall —
// two enabled projects with distinct installation_ids produce
// one channel routing across both. Replaces the legacy
// "first-wins" behaviour with the multi-installation flow.
func TestBuildGitHubChannel_MultipleEnabled_BuildsMultiInstall(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	pemPath := writePEM(t, filepath.Join(t.TempDir(), "key.pem"))
	p1 := inboundOnlyProject("alpha")
	p1.GitHubApp.AppID = 1
	p1.GitHubApp.InstallationID = 100
	p1.GitHubApp.PrivateKeyPath = pemPath
	p2 := inboundOnlyProject("beta")
	p2.GitHubApp.RepoAllowlist = []string{"other/repo"}
	p2.GitHubApp.AppID = 2
	p2.GitHubApp.InstallationID = 200
	p2.GitHubApp.PrivateKeyPath = pemPath
	ch, enabled, err := buildGitHubChannel([]*registry.Project{p1, p2})
	if err != nil {
		t.Fatalf("buildGitHubChannel: %v", err)
	}
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}
	if len(enabled) != 2 {
		t.Fatalf("enabled = %d, want 2", len(enabled))
	}
	if enabled[0].ID != "alpha" || enabled[1].ID != "beta" {
		t.Errorf("enabled IDs = [%q, %q], want [alpha, beta] (preserves input order)",
			enabled[0].ID, enabled[1].ID)
	}
}

// TestBuildGitHubChannel_MultipleEnabled_DuplicateInstallationID —
// two enabled projects sharing the same installation_id is a
// config bug; New surfaces it at boot.
func TestBuildGitHubChannel_MultipleEnabled_DuplicateInstallationID(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	pemPath := writePEM(t, filepath.Join(t.TempDir(), "key.pem"))
	p1 := inboundOnlyProject("alpha")
	p1.GitHubApp.AppID = 1
	p1.GitHubApp.InstallationID = 100
	p1.GitHubApp.PrivateKeyPath = pemPath
	p2 := inboundOnlyProject("beta")
	p2.GitHubApp.RepoAllowlist = []string{"other/repo"}
	p2.GitHubApp.AppID = 2
	p2.GitHubApp.InstallationID = 100 // collision
	p2.GitHubApp.PrivateKeyPath = pemPath
	_, _, err := buildGitHubChannel([]*registry.Project{p1, p2})
	if err == nil || !strings.Contains(err.Error(), "duplicate installation_id") {
		t.Errorf("err = %v, want duplicate-installation_id failure", err)
	}
}

// TestBuildGitHubChannel_MultipleEnabled_SecretMismatch — two
// enabled projects with different webhook secrets is a misconfig:
// the channel mounts one HTTP route, which can only verify one
// secret. Surface at boot.
func TestBuildGitHubChannel_MultipleEnabled_SecretMismatch(t *testing.T) {
	t.Setenv("GH_SECRET_A", "secretA")
	t.Setenv("GH_SECRET_B", "secretB")
	pemPath := writePEM(t, filepath.Join(t.TempDir(), "key.pem"))
	p1 := inboundOnlyProject("alpha")
	p1.GitHubApp.WebhookSecretEnv = "GH_SECRET_A"
	p1.GitHubApp.AppID = 1
	p1.GitHubApp.InstallationID = 100
	p1.GitHubApp.PrivateKeyPath = pemPath
	p2 := inboundOnlyProject("beta")
	p2.GitHubApp.WebhookSecretEnv = "GH_SECRET_B"
	p2.GitHubApp.RepoAllowlist = []string{"other/repo"}
	p2.GitHubApp.AppID = 2
	p2.GitHubApp.InstallationID = 200
	p2.GitHubApp.PrivateKeyPath = pemPath
	_, _, err := buildGitHubChannel([]*registry.Project{p1, p2})
	if err == nil || !strings.Contains(err.Error(), "WebhookSecret") {
		t.Errorf("err = %v, want WebhookSecret-mismatch failure", err)
	}
}

// TestResolveGitHubAppConfig_RejectsPartialOutbound — AppID +
// InstallationID set without PrivateKeyPath is rejected as a
// defensive belt-and-braces check (registry validation already
// catches this, but resolve should never trust its inputs).
func TestResolveGitHubAppConfig_RejectsPartialOutbound(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	_, err := resolveGitHubAppConfig(registry.ProjectGitHubApp{
		WebhookSecretEnv: "GH_TEST_SECRET",
		RepoAllowlist:    []string{"acme/api"},
		AppID:            12345,
		InstallationID:   99,
	})
	if err == nil || !strings.Contains(err.Error(), "private_key_path") {
		t.Errorf("err = %v, want partial-outbound failure", err)
	}
}

// TestResolveGitHubAppConfig_MalformedPEM — surfaces a parse error.
func TestResolveGitHubAppConfig_MalformedPEM(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	pemPath := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(pemPath, []byte("-----BEGIN RSA PRIVATE KEY-----\nbad\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatalf("write bad pem: %v", err)
	}
	_, err := resolveGitHubAppConfig(registry.ProjectGitHubApp{
		WebhookSecretEnv: "GH_TEST_SECRET",
		RepoAllowlist:    []string{"acme/api"},
		AppID:            12345,
		InstallationID:   99,
		PrivateKeyPath:   pemPath,
	})
	if err == nil {
		t.Error("expected error on malformed PEM")
	}
}

// TestResolveGitHubAppConfig_OutboundReturnsParsedKey — inbound +
// outbound wired produces a Config with PrivateKey populated.
func TestResolveGitHubAppConfig_OutboundReturnsParsedKey(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	pemPath := writePEM(t, filepath.Join(t.TempDir(), "ok.pem"))
	cfg, err := resolveGitHubAppConfig(registry.ProjectGitHubApp{
		AppID:            12345,
		InstallationID:   99,
		PrivateKeyPath:   pemPath,
		WebhookSecretEnv: "GH_TEST_SECRET",
		RepoAllowlist:    []string{"acme/api"},
	})
	if err != nil {
		t.Fatalf("resolveGitHubAppConfig: %v", err)
	}
	if cfg.PrivateKey == nil {
		t.Error("PrivateKey nil after resolve")
	}
	if cfg.WebhookSecret != "shhh" {
		t.Errorf("WebhookSecret = %q, want shhh", cfg.WebhookSecret)
	}
}

// TestResolveGitHubAppConfig_BlankSecretWhitespaceRejected —
// whitespace-only env var counts as unset (same as
// strings.TrimSpace).
func TestResolveGitHubAppConfig_BlankSecretWhitespaceRejected(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "   ")
	_, err := resolveGitHubAppConfig(registry.ProjectGitHubApp{
		WebhookSecretEnv: "GH_TEST_SECRET",
		RepoAllowlist:    []string{"acme/api"},
	})
	if err == nil {
		t.Error("expected error on whitespace-only secret")
	}
}

// TestResolveGitHubAppConfig_InboundOnly — when no outbound fields
// are set, the resolved Config has nil PrivateKey but is otherwise
// well-formed.
func TestResolveGitHubAppConfig_InboundOnly(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	cfg, err := resolveGitHubAppConfig(registry.ProjectGitHubApp{
		WebhookSecretEnv: "GH_TEST_SECRET",
		RepoAllowlist:    []string{"acme/api"},
		TaskLabels:       []string{"vornik-task"},
		PRReviewLabels:   []string{"needs-review"},
		SenderAllowlist:  []string{"vadim"},
	})
	if err != nil {
		t.Fatalf("resolveGitHubAppConfig: %v", err)
	}
	if cfg.PrivateKey != nil {
		t.Error("PrivateKey set on inbound-only config")
	}
	if len(cfg.TaskLabels) != 1 || cfg.TaskLabels[0] != "vornik-task" {
		t.Errorf("TaskLabels = %v", cfg.TaskLabels)
	}
}
