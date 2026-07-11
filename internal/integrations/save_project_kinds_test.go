package integrations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/registry"
)

// This file tests task 5.2b's write path for the three project-scope kinds
// (email, github_app, slack): the real catalog Fields (reconciled against
// internal/registry/project.go), the real ProjectConfigWriter's
// project-schema Validate(), and the GitHub App file-secret placement mode.
// Every test here uses the REAL kind from Registry() with only the Prober
// swapped for a fake (no network calls) — proving the actual catalog
// Fields, not a hand-built stand-in, drive a working save.

// newTestConfigDirWithProject builds a temp config dir (config.yaml +
// secrets/, via newTestConfigDir) plus a minimal valid projects/<id>.yaml.
func newTestConfigDirWithProject(t *testing.T) string {
	t.Helper()
	const projectID = "proj-a"
	dir := newTestConfigDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("projectId: %s\nswarmId: test-swarm\ndefaultWorkflowId: test-workflow\n", projectID)
	if err := os.WriteFile(filepath.Join(dir, "projects", projectID+".yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// realKindWithFakeProber returns the REAL catalog kind (Fields exactly as
// shipped) with its Prober swapped for a fake that returns result — so
// these tests exercise the real Fields/save-target wiring without a
// network call.
func realKindWithFakeProber(t *testing.T, id string, result ProbeResult) IntegrationKind {
	t.Helper()
	kind := findKind(t, id)
	kind.Prober = &fakeProber{kind: id, result: result}
	return kind
}

func loadOneProject(t *testing.T, dir, projectID string) *registry.Project {
	t.Helper()
	projects, err := registry.LoadProjects(dir)
	if err != nil {
		t.Fatalf("LoadProjects: %v", err)
	}
	p, ok := projects[projectID]
	if !ok {
		t.Fatalf("project %q not found after save; loaded: %v", projectID, projects)
	}
	return p
}

// --- Email ---

func emailCandidateValues() map[string]string {
	return map[string]string{
		"imap_host":         "imap.example.com",
		"imap_port":         "993",
		"imap_username":     "user@example.com",
		"imap_password_env": "super-secret-imap-password-1234",
		"smtp_host":         "smtp.example.com",
		"smtp_port":         "587",
		"smtp_username":     "user@example.com",
		"smtp_password_env": "super-secret-smtp-password-5678",
		"from_address":      "user@example.com",
	}
}

func TestSave_Email_HappyPath(t *testing.T) {
	dir := newTestConfigDirWithProject(t)
	kind := realKindWithFakeProber(t, "email", okProbe("email"))
	target, ok := SaveTargetForKind("email")
	if !ok {
		t.Fatal("SaveTargetForKind(email) = false")
	}
	cand := CandidateConfig{Kind: "email", ProjectID: "proj-a", Values: emailCandidateValues()}

	res, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !res.Saved {
		t.Fatal("Saved = false, want true")
	}

	proj := loadOneProject(t, dir, "proj-a")
	if proj.Email.IMAPHost != "imap.example.com" || proj.Email.IMAPUsername != "user@example.com" {
		t.Errorf("Email = %+v, want the candidate's IMAP host/username", proj.Email)
	}
	if proj.Email.IMAPPort != 993 || proj.Email.SMTPPort != 587 {
		t.Errorf("Email ports = imap:%d smtp:%d, want 993/587 (Int field conversion)", proj.Email.IMAPPort, proj.Email.SMTPPort)
	}
	wantIMAPEnv := projectScopedEnvName("EMAIL_IMAP_PASSWORD", "proj-a")
	wantSMTPEnv := projectScopedEnvName("EMAIL_SMTP_PASSWORD", "proj-a")
	if proj.Email.IMAPPasswordEnv != wantIMAPEnv {
		t.Errorf("IMAPPasswordEnv = %q, want %q", proj.Email.IMAPPasswordEnv, wantIMAPEnv)
	}
	if proj.Email.SMTPPasswordEnv != wantSMTPEnv {
		t.Errorf("SMTPPasswordEnv = %q, want %q", proj.Email.SMTPPasswordEnv, wantSMTPEnv)
	}
	if !proj.Email.Enabled() {
		t.Error("Email.Enabled() = false, want true — the saved fields must satisfy the channel's own activation gate")
	}

	// Secret-never-inline boundary: the literal passwords must never
	// appear in project.yaml, only in the secrets file.
	rawYAML, err := os.ReadFile(filepath.Join(dir, "projects", "proj-a.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawYAML), "super-secret-imap-password-1234") || strings.Contains(string(rawYAML), "super-secret-smtp-password-5678") {
		t.Fatalf("project.yaml leaked a literal secret: %s", rawYAML)
	}
	secrets, err := os.ReadFile(filepath.Join(dir, "secrets", projectSecretsFile))
	if err != nil {
		t.Fatalf("read project secrets file: %v", err)
	}
	if !strings.Contains(string(secrets), wantIMAPEnv+"=super-secret-imap-password-1234") {
		t.Errorf("secrets file missing IMAP password entry: %s", secrets)
	}
	if !strings.Contains(string(secrets), wantSMTPEnv+"=super-secret-smtp-password-5678") {
		t.Errorf("secrets file missing SMTP password entry: %s", secrets)
	}
}

// TestSave_Email_ProjectAwareValidate_RollsBackMalformedWrite is the
// end-to-end proof for task 5.2b step 2 (5.2 review finding 3): using the
// REAL default writer (no NewWriter override), a candidate that would
// violate registry.Project's cross-field rule (SMTP fields set but
// from_address empty — all-or-nothing) must be rejected at Validate() and
// the project.yaml file restored to its pre-save content, not left
// half-patched.
func TestSave_Email_ProjectAwareValidate_RollsBackMalformedWrite(t *testing.T) {
	dir := newTestConfigDirWithProject(t)
	before, err := os.ReadFile(filepath.Join(dir, "projects", "proj-a.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	kind := realKindWithFakeProber(t, "email", okProbe("email"))
	target, _ := SaveTargetForKind("email")
	values := emailCandidateValues()
	values["from_address"] = "" // breaks the SMTP all-or-nothing rule
	cand := CandidateConfig{Kind: "email", ProjectID: "proj-a", Values: values}

	_, err = Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
	if err == nil {
		t.Fatal("Save() = nil error, want a project-schema validation rejection")
	}
	if !strings.Contains(err.Error(), "validate") {
		t.Errorf("error = %v, want it to name the validate step", err)
	}

	after, err := os.ReadFile(filepath.Join(dir, "projects", "proj-a.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("project.yaml was left modified after a failed validate:\nbefore=%s\nafter=%s", before, after)
	}
}

// --- Slack ---

func TestSave_Slack_HappyPath(t *testing.T) {
	dir := newTestConfigDirWithProject(t)
	kind := realKindWithFakeProber(t, "slack", okProbe("slack"))
	target, ok := SaveTargetForKind("slack")
	if !ok {
		t.Fatal("SaveTargetForKind(slack) = false")
	}
	cand := CandidateConfig{Kind: "slack", ProjectID: "proj-a", Values: map[string]string{
		"team_id":            "T12345",
		"bot_token_env":      "xoxb-fake-bot-token-value",
		"signing_secret_env": "fake-signing-secret-value",
	}}

	res, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !res.Saved {
		t.Fatal("Saved = false, want true")
	}

	proj := loadOneProject(t, dir, "proj-a")
	if proj.Slack.TeamID != "T12345" {
		t.Errorf("Slack.TeamID = %q, want T12345", proj.Slack.TeamID)
	}
	wantBotEnv := projectScopedEnvName("SLACK_BOT_TOKEN", "proj-a")
	wantSigningEnv := projectScopedEnvName("SLACK_SIGNING_SECRET", "proj-a")
	if proj.Slack.BotTokenEnv != wantBotEnv {
		t.Errorf("Slack.BotTokenEnv = %q, want %q", proj.Slack.BotTokenEnv, wantBotEnv)
	}
	if proj.Slack.SigningSecretEnv != wantSigningEnv {
		t.Errorf("Slack.SigningSecretEnv = %q, want %q", proj.Slack.SigningSecretEnv, wantSigningEnv)
	}
	if !proj.Slack.Enabled() {
		t.Error("Slack.Enabled() = false, want true")
	}

	rawYAML, _ := os.ReadFile(filepath.Join(dir, "projects", "proj-a.yaml"))
	if strings.Contains(string(rawYAML), "xoxb-fake-bot-token-value") || strings.Contains(string(rawYAML), "fake-signing-secret-value") {
		t.Fatalf("project.yaml leaked a literal secret: %s", rawYAML)
	}
	secrets, err := os.ReadFile(filepath.Join(dir, "secrets", projectSecretsFile))
	if err != nil {
		t.Fatalf("read project secrets file: %v", err)
	}
	if !strings.Contains(string(secrets), wantBotEnv+"=xoxb-fake-bot-token-value") {
		t.Errorf("secrets file missing bot token entry: %s", secrets)
	}
	if !strings.Contains(string(secrets), wantSigningEnv+"=fake-signing-secret-value") {
		t.Errorf("secrets file missing signing secret entry: %s", secrets)
	}
}

// --- GitHub App (file-secret placement mode) ---

const testGitHubAppPEM = `-----BEGIN RSA PRIVATE KEY-----
MIIBOgIBAAJBAK8DZ7m1v8m6c8s1n1s1n1s1n1s1n1s1n1s1n1s1n1s1n1s1n1s
FAKEFAKEFAKEnotarealkeyfortestingonlyFAKEFAKEFAKEnotarealkeyFAKE
-----END RSA PRIVATE KEY-----
`

func githubAppCandidateValues() map[string]string {
	return map[string]string{
		"app_id":             "123456",
		"installation_id":    "789012",
		"private_key_path":   testGitHubAppPEM,
		"webhook_secret_env": "fake-webhook-secret-value",
		"repo_allowlist":     "myorg/repo1, myorg/repo2",
	}
}

func TestSave_GitHubApp_HappyPath_FileSecretPlacement(t *testing.T) {
	dir := newTestConfigDirWithProject(t)
	kind := realKindWithFakeProber(t, "github_app", okProbe("github_app"))
	target, ok := SaveTargetForKind("github_app")
	if !ok {
		t.Fatal("SaveTargetForKind(github_app) = false")
	}
	cand := CandidateConfig{Kind: "github_app", ProjectID: "proj-a", Values: githubAppCandidateValues()}

	res, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !res.Saved {
		t.Fatal("Saved = false, want true")
	}

	proj := loadOneProject(t, dir, "proj-a")
	if proj.GitHubApp.AppID != 123456 || proj.GitHubApp.InstallationID != 789012 {
		t.Errorf("GitHubApp app/installation ID = %d/%d, want 123456/789012", proj.GitHubApp.AppID, proj.GitHubApp.InstallationID)
	}
	if len(proj.GitHubApp.RepoAllowlist) != 2 || proj.GitHubApp.RepoAllowlist[0] != "myorg/repo1" || proj.GitHubApp.RepoAllowlist[1] != "myorg/repo2" {
		t.Errorf("RepoAllowlist = %v, want [myorg/repo1 myorg/repo2] (List field conversion)", proj.GitHubApp.RepoAllowlist)
	}
	wantWebhookEnv := projectScopedEnvName("GITHUB_APP_WEBHOOK_SECRET", "proj-a")
	if proj.GitHubApp.WebhookSecretEnv != wantWebhookEnv {
		t.Errorf("WebhookSecretEnv = %q, want %q", proj.GitHubApp.WebhookSecretEnv, wantWebhookEnv)
	}
	if !proj.GitHubApp.Enabled() {
		t.Error("GitHubApp.Enabled() = false, want true")
	}

	// File-secret placement: config carries a PATH, not the PEM.
	wantPath := filepath.Join(dir, "secrets", "github-app-proj-a.pem")
	if proj.GitHubApp.PrivateKeyPath != wantPath {
		t.Errorf("PrivateKeyPath = %q, want %q", proj.GitHubApp.PrivateKeyPath, wantPath)
	}
	pemBytes, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read private key file: %v", err)
	}
	if string(pemBytes) != testGitHubAppPEM {
		t.Errorf("private key file content = %q, want the literal candidate PEM", pemBytes)
	}
	info, err := os.Stat(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("private key file mode = %o, want 0600", perm)
	}

	// Secret-never-inline boundary: neither the PEM nor the webhook secret
	// literal ever appears in project.yaml.
	rawYAML, _ := os.ReadFile(filepath.Join(dir, "projects", "proj-a.yaml"))
	if strings.Contains(string(rawYAML), "FAKEFAKEFAKE") || strings.Contains(string(rawYAML), "BEGIN RSA PRIVATE KEY") {
		t.Fatalf("project.yaml leaked the private key: %s", rawYAML)
	}
	if strings.Contains(string(rawYAML), "fake-webhook-secret-value") {
		t.Fatalf("project.yaml leaked the webhook secret: %s", rawYAML)
	}
}

// TestSave_GitHubApp_RollbackDoesNotLeavePEMReferencedButOrphaned mirrors
// the telegram orphaned-secret test: if the config write fails AFTER the
// PEM file was already placed, the file is inert (nothing references it)
// and a clean re-save overwrites it — never merged/duplicated.
func TestSave_GitHubApp_SecondSaveOverwritesPEMFile(t *testing.T) {
	dir := newTestConfigDirWithProject(t)
	kind := realKindWithFakeProber(t, "github_app", okProbe("github_app"))
	target, _ := SaveTargetForKind("github_app")

	first := githubAppCandidateValues()
	cand1 := CandidateConfig{Kind: "github_app", ProjectID: "proj-a", Values: first}
	if _, err := Save(context.Background(), kind, target, cand1, adminCaller(), SaveDeps{ConfigDir: dir}); err != nil {
		t.Fatalf("first save: %v", err)
	}

	second := githubAppCandidateValues()
	second["private_key_path"] = strings.ReplaceAll(testGitHubAppPEM, "FAKE", "SECOND")
	cand2 := CandidateConfig{Kind: "github_app", ProjectID: "proj-a", Values: second}
	res, err := Save(context.Background(), kind, target, cand2, adminCaller(), SaveDeps{ConfigDir: dir})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if !res.Saved {
		t.Fatal("second save: Saved = false")
	}

	pemBytes, err := os.ReadFile(filepath.Join(dir, "secrets", "github-app-proj-a.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pemBytes), "SECOND") {
		t.Errorf("private key file = %q, want the SECOND save's content to have won", pemBytes)
	}
}

// TestSave_GitHubApp_ConfigWriteFailureRollsBackPlacedPEM is the regression
// test for review-20260709-9160 finding 2: placeSecretFile writes the PEM
// (inside splitAndPlaceFields) BEFORE Save patches/validates the config, so
// a failure at the config-write step (Validate here) must not leave the PEM
// file orphaned on disk — Save rolls it back, mirroring its existing
// Backup/Restore posture for the config file itself. This is deliberately
// different from a Secret (non-SecretFile) field's env-line placement,
// which stays orphaned-but-inert by design (see SaveError's doc): a shared
// *.env file can't be selectively unwound without risking other kinds'
// entries, but a dedicated per-save PEM file can be removed outright.
func TestSave_GitHubApp_ConfigWriteFailureRollsBackPlacedPEM(t *testing.T) {
	dir := newTestConfigDirWithProject(t)
	kind := realKindWithFakeProber(t, "github_app", okProbe("github_app"))
	target, _ := SaveTargetForKind("github_app")
	cand := CandidateConfig{Kind: "github_app", ProjectID: "proj-a", Values: githubAppCandidateValues()}

	deps := SaveDeps{
		ConfigDir: dir,
		NewWriter: func(_ string) featuredoctor.ConfigWriter {
			return &fakeConfigWriter{content: []byte("projectId: proj-a\n"), validateErr: errors.New("bad project schema")}
		},
	}
	_, err := Save(context.Background(), kind, target, cand, adminCaller(), deps)
	if err == nil {
		t.Fatal("Save() = nil error, want a rejection from the fake Validate failure")
	}

	pemPath := filepath.Join(dir, "secrets", "github-app-proj-a.pem")
	if _, statErr := os.Stat(pemPath); !os.IsNotExist(statErr) {
		t.Errorf("PEM file at %q still exists after a config-write failure — it must be rolled back, not left orphaned", pemPath)
	}
}

// --- Path-confinement (security-audit finding F-1) ---

func TestSave_RejectsTraversalProjectID(t *testing.T) {
	dir := newTestConfigDir(t) // no "projects" subdir needed — must fail before any path is touched
	prober := &fakeProber{kind: "email", result: okProbe("email")}
	kind := realKindWithFakeProber(t, "email", okProbe("email"))
	kind.Prober = prober
	target, _ := SaveTargetForKind("email")
	cand := CandidateConfig{Kind: "email", ProjectID: "../../etc/x", Values: emailCandidateValues()}

	_, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
	if err == nil {
		t.Fatal("Save() = nil error, want a rejection for a traversal-shaped project ID")
	}
	if !strings.Contains(err.Error(), "project ID") {
		t.Errorf("error = %v, want it to name the project ID problem", err)
	}
	// review-20260709-9160 finding 3: assert the rejection came from the
	// path-safety gate specifically (via the sentinel), not merely that
	// *some* error surfaced — an incidental error earlier/later in the call
	// chain with unrelated wording would otherwise satisfy the weaker
	// strings.Contains check above without proving the gate actually fired.
	if !errors.Is(err, ErrInvalidProjectID) {
		t.Errorf("error = %v, want errors.Is(err, ErrInvalidProjectID)", err)
	}
	if prober.calls != 0 {
		t.Error("Prober.Probe was called despite the malformed project ID — the path-safety gate must run before probing")
	}
	// Nothing must have been written outside (or even inside) configDir.
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(dir), "etc")); !os.IsNotExist(statErr) {
		t.Error("a file/dir was created outside configDir — traversal was not fully blocked")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == "projects" {
			t.Error("a 'projects' directory was created despite the rejected save")
		}
	}
}

// TestGitHubAppPrivateKeyPath_RejectsTraversalProjectID is the unit-level
// proof for review-20260709-9160 finding 1: githubAppPrivateKeyPath must
// validate projectID BEFORE it is interpolated into the "github-app-<id>.pem"
// filename, not after. Calling the helper directly (rather than only through
// Save, which already validates cand.ProjectID before splitAndPlaceFields
// ever runs) isolates the helper's own ordering, so a future regression that
// re-introduces "build the slug, then validate" would fail here even if some
// other caller happened to pre-validate.
func TestGitHubAppPrivateKeyPath_RejectsTraversalProjectID(t *testing.T) {
	dir := t.TempDir()
	path, err := githubAppPrivateKeyPath(dir, "../../etc/x")
	if err == nil {
		t.Fatalf("githubAppPrivateKeyPath(...) = %q, nil, want a rejection for a traversal-shaped project ID", path)
	}
	if !errors.Is(err, ErrInvalidProjectID) {
		t.Errorf("error = %v, want errors.Is(err, ErrInvalidProjectID)", err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty on rejection", path)
	}
	if strings.Contains(path, "..") {
		t.Errorf("returned path %q contains a traversal segment — a malformed project ID must never produce a file whose name/path escapes the secrets dir", path)
	}
}

func TestSave_RejectsEmptyProjectIDForProjectScope(t *testing.T) {
	dir := newTestConfigDir(t)
	kind := realKindWithFakeProber(t, "email", okProbe("email"))
	target, _ := SaveTargetForKind("email")
	cand := CandidateConfig{Kind: "email", ProjectID: "", Values: emailCandidateValues()}

	_, err := Save(context.Background(), kind, target, cand, Caller{IsAdmin: true}, SaveDeps{ConfigDir: dir})
	if err == nil {
		t.Fatal("Save() = nil error, want a rejection for an empty project ID on a project-scope kind")
	}
}

// --- SaveDeps.newWriter scope-awareness (task 5.2b step 2) ---

func TestSaveDeps_NewWriter_ScopeAware(t *testing.T) {
	deps := SaveDeps{}

	daemonWriter := deps.newWriter("/tmp/does-not-need-to-exist.yaml", ScopeDaemon)
	if _, ok := daemonWriter.(*featuredoctor.FileConfigWriter); !ok {
		t.Errorf("daemon-scope default writer = %T, want *featuredoctor.FileConfigWriter", daemonWriter)
	}

	projectWriter := deps.newWriter("/tmp/does-not-need-to-exist.yaml", ScopeProject)
	if _, ok := projectWriter.(*ProjectConfigWriter); !ok {
		t.Errorf("project-scope default writer = %T, want *ProjectConfigWriter", projectWriter)
	}

	fake := &fakeConfigWriter{}
	deps.NewWriter = func(_ string) featuredoctor.ConfigWriter { return fake }
	if got := deps.newWriter("/tmp/x.yaml", ScopeProject); got != fake {
		t.Error("an explicit NewWriter override must win regardless of scope")
	}
}
