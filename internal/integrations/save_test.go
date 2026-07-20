package integrations

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/featuredoctor"
)

// fakeProber returns a canned ProbeResult regardless of the candidate,
// letting tests drive Save through every Outcome without a network call.
type fakeProber struct {
	kind   string
	result ProbeResult
	calls  int
}

func (p *fakeProber) Kind() string { return p.kind }
func (p *fakeProber) Probe(_ context.Context, _ CandidateConfig) ProbeResult {
	p.calls++
	return p.result
}

// fakeReloader lets tests force a reload failure without a real daemon.
type fakeReloader struct {
	err   error
	calls int
}

func (r *fakeReloader) Reload(_ context.Context) error {
	r.calls++
	return r.err
}

// fakeReloadStatusChecker lets tests drive the post-reload poll deterministically.
type fakeReloadStatusChecker struct {
	statuses []config.ReloadStatus // returned in order, last one repeats
	calls    int
}

func (c *fakeReloadStatusChecker) Status() config.ReloadStatus {
	i := c.calls
	if i >= len(c.statuses) {
		i = len(c.statuses) - 1
	}
	c.calls++
	return c.statuses[i]
}

// fakeConfigWriter implements featuredoctor.ConfigWriter entirely in
// memory so failure-injection tests don't need a real filesystem.
type fakeConfigWriter struct {
	content []byte
	backup  []byte

	backupErr, readErr, writeErr, restoreErr, validateErr error

	restored     bool
	writtenCount int
}

func (f *fakeConfigWriter) Read() ([]byte, error) { return f.content, f.readErr }
func (f *fakeConfigWriter) Write(b []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writtenCount++
	f.content = b
	return nil
}
func (f *fakeConfigWriter) Backup() (string, error) {
	if f.backupErr != nil {
		return "", f.backupErr
	}
	f.backup = append([]byte(nil), f.content...)
	return "fake-backup-token", nil
}
func (f *fakeConfigWriter) Restore(_ string) error {
	if f.restoreErr != nil {
		return f.restoreErr
	}
	f.restored = true
	f.content = append([]byte(nil), f.backup...)
	return nil
}
func (f *fakeConfigWriter) Validate() error    { return f.validateErr }
func (f *fakeConfigWriter) ConfigPath() string { return "/fake/integrations-config.yaml" }

// minimalDaemonConfig is a config.yaml fixture that satisfies
// config.Config.Validate() (sqlite driver sidesteps the postgres
// host/port/name/user requirement) so writer.Validate() (real
// FileConfigWriter path) genuinely passes in end-to-end tests.
const minimalDaemonConfig = `server:
  address: ":8080"
database:
  driver: sqlite
  path: /tmp/vornik-integrations-test.db
api:
  auth_enabled: false
telegram:
  enabled: false
mcp:
  servers: []
`

// newTestConfigDir builds a temp config dir with config.yaml + an empty
// secrets/ dir, returning the dir path.
func newTestConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(minimalDaemonConfig), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatalf("mkdir secrets: %v", err)
	}
	return dir
}

func telegramTestKind(prober Prober) IntegrationKind {
	return IntegrationKind{
		ID:     "telegram",
		Scope:  ScopeDaemon,
		Fields: []CredentialField{{Key: "bot_token", Secret: true, EnvName: "TELEGRAM_BOT_TOKEN", Required: true}},
		Prober: prober,
	}
}

func okProbe(kind string) ProbeResult {
	return ProbeResult{Kind: kind, OK: true, Outcome: OutcomeOK, Summary: "ok"}
}
func failProbe(kind string) ProbeResult {
	return ProbeResult{Kind: kind, OK: false, Outcome: OutcomeFail, Summary: "nope"}
}

func adminCaller() Caller { return Caller{IsAdmin: true} }

// --- Step 1: probe-refusal ---

func TestSave_RefusesWhenProbeFails(t *testing.T) {
	dir := newTestConfigDir(t)
	before, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	prober := &fakeProber{kind: "telegram", result: failProbe("telegram")}
	kind := telegramTestKind(prober)
	target, _ := SaveTargetForKind("telegram")
	cand := CandidateConfig{Kind: "telegram", Values: map[string]string{"bot_token": "some-token-value-1234567890"}}

	res, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Save returned error for a probe-refusal, want nil error: %v", err)
	}
	if res.Saved {
		t.Error("Saved = true, want false on probe failure")
	}
	if res.Probe.Outcome != OutcomeFail {
		t.Errorf("Probe.Outcome = %v, want OutcomeFail", res.Probe.Outcome)
	}

	after, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("config.yaml was modified despite a failing probe")
	}
	secretPath := filepath.Join(dir, "secrets", "telegram.env")
	if _, err := os.Stat(secretPath); !os.IsNotExist(err) {
		t.Error("secret file was written despite a failing probe")
	}
}

// --- Happy path: telegram (scalar, daemon) ---

func TestSave_TelegramWritesPlaceholderAndSecret(t *testing.T) {
	dir := newTestConfigDir(t)
	prober := &fakeProber{kind: "telegram", result: okProbe("telegram")}
	kind := telegramTestKind(prober)
	target, ok := SaveTargetForKind("telegram")
	if !ok {
		t.Fatal("SaveTargetForKind(telegram) = false, want true")
	}
	cand := CandidateConfig{Kind: "telegram", Values: map[string]string{"bot_token": "1234567890:AAtheRealSecretTokenValue"}}

	res, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !res.Saved {
		t.Fatal("Saved = false, want true")
	}
	if res.Probe.Outcome != OutcomeOK {
		t.Errorf("Probe.Outcome = %v, want OutcomeOK", res.Probe.Outcome)
	}
	if prober.calls != 1 {
		t.Errorf("Prober.Probe called %d times, want exactly 1 (step 6 reuses step 1's result)", prober.calls)
	}

	written, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := config.GetYAMLString(written, "telegram.bot_token"); got != "${TELEGRAM_BOT_TOKEN}" {
		t.Errorf("telegram.bot_token = %q, want ${TELEGRAM_BOT_TOKEN}", got)
	}

	secretBytes, err := os.ReadFile(filepath.Join(dir, "secrets", "telegram.env"))
	if err != nil {
		t.Fatalf("read secret file: %v", err)
	}
	if !strings.Contains(string(secretBytes), "TELEGRAM_BOT_TOKEN=1234567890:AAtheRealSecretTokenValue") {
		t.Errorf("secret file = %q, want it to contain the literal token", string(secretBytes))
	}
}

// --- Secret-literal boundary (bug simulation, design §5.4/§8) ---

func TestSave_SecretLiteralBoundary_AbortsBeforeWrite(t *testing.T) {
	dir := newTestConfigDir(t)
	prober := &fakeProber{kind: "telegram", result: okProbe("telegram")}
	// Bug simulation: a field mis-classified as non-secret whose value
	// looks exactly like a bare secret literal.
	kind := IntegrationKind{
		ID:     "telegram",
		Scope:  ScopeDaemon,
		Fields: []CredentialField{{Key: "bot_token", Secret: false}},
		Prober: prober,
	}
	target := SaveTarget{
		Scope:      ScopeDaemon,
		ConfigFile: daemonConfigFile,
		ScalarKeys: map[string]string{"bot_token": "telegram.bot_token"},
	}
	cand := CandidateConfig{Kind: "telegram", Values: map[string]string{"bot_token": "aBareLiteralTokenNoPlaceholder"}}

	before, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))

	_, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
	if err == nil {
		t.Fatal("Save returned nil error, want a secret-literal boundary rejection")
	}
	if !strings.Contains(err.Error(), "secret literal") {
		t.Errorf("error = %q, want it to name the secret-literal boundary", err.Error())
	}

	after, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if string(after) != string(before) {
		t.Error("config.yaml was modified despite the boundary rejection")
	}
}

// TestSave_SecretLiteralBoundary_EnvShapedSecretNotDeclaredCaught is the
// review-20260709-cc3e finding-2 regression: before the fix, any value
// shaped like a POSIX env-var name ([A-Z_][A-Z0-9_]*) was exempt from the
// secret-literal boundary regardless of whether it was actually one of this
// kind's declared EnvNames — so a real env-shaped secret (e.g. a token that
// happens to look like "GITHUB_TOKEN_ABC123DEF456") would slip past the
// guard on a mis-classified field. It must still be caught.
func TestSave_SecretLiteralBoundary_EnvShapedSecretNotDeclaredCaught(t *testing.T) {
	dir := newTestConfigDir(t)
	prober := &fakeProber{kind: "telegram", result: okProbe("telegram")}
	// Bug simulation: a field mis-classified as non-secret whose value is
	// an env-var-shaped, but undeclared, string that is really a secret.
	kind := IntegrationKind{
		ID:     "telegram",
		Scope:  ScopeDaemon,
		Fields: []CredentialField{{Key: "bot_token", Secret: false}},
		Prober: prober,
	}
	target := SaveTarget{
		Scope:      ScopeDaemon,
		ConfigFile: daemonConfigFile,
		ScalarKeys: map[string]string{"bot_token": "telegram.bot_token"},
	}
	cand := CandidateConfig{Kind: "telegram", Values: map[string]string{"bot_token": "GITHUB_TOKEN_ABC123DEF456"}}

	before, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))

	_, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
	if err == nil {
		t.Fatal("Save returned nil error, want a secret-literal boundary rejection for an env-shaped-but-undeclared value")
	}
	if !strings.Contains(err.Error(), "secret literal") {
		t.Errorf("error = %q, want it to name the secret-literal boundary", err.Error())
	}

	after, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if string(after) != string(before) {
		t.Error("config.yaml was modified despite the boundary rejection")
	}
}

// --- Scope enforcement (design §6) ---

func TestSave_ScopeEnforcement_DaemonRequiresAdmin(t *testing.T) {
	dir := newTestConfigDir(t)
	prober := &fakeProber{kind: "telegram", result: okProbe("telegram")}
	kind := telegramTestKind(prober)
	target, _ := SaveTargetForKind("telegram")
	cand := CandidateConfig{Kind: "telegram", Values: map[string]string{"bot_token": "x"}}

	_, err := Save(context.Background(), kind, target, cand, Caller{IsAdmin: false}, SaveDeps{ConfigDir: dir})
	if err == nil {
		t.Fatal("Save returned nil error, want ErrForbidden for a non-admin daemon-scope save")
	}
	if !strings.Contains(err.Error(), "not authorized") {
		t.Errorf("error = %q, want ErrForbidden-shaped message", err.Error())
	}
	if prober.calls != 0 {
		t.Error("Prober.Probe was called despite the scope check failing — scope must gate BEFORE probing")
	}
}

func TestSave_ScopeEnforcement_ProjectRequiresMatchingScope(t *testing.T) {
	dir := newTestConfigDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "projects", "proj-a.yaml"), []byte("projectId: proj-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prober := &fakeProber{kind: "synthetic", result: okProbe("synthetic")}
	kind := IntegrationKind{
		ID:     "synthetic",
		Scope:  ScopeProject,
		Fields: []CredentialField{{Key: "note", Secret: false}},
		Prober: prober,
	}
	target := SaveTarget{
		Scope:      ScopeProject,
		ConfigFile: projectConfigFile,
		ScalarKeys: map[string]string{"note": "note"},
	}
	cand := CandidateConfig{Kind: "synthetic", ProjectID: "proj-b", Values: map[string]string{"note": "hi"}}

	// A fake writer isolates this test from the real config-write pipeline
	// (covered by the daemon-scope happy-path tests and the real
	// email/slack/github_app save tests below) — its purpose is only the
	// scope check.
	deps := SaveDeps{
		ConfigDir: dir,
		NewWriter: func(_ string) featuredoctor.ConfigWriter {
			return &fakeConfigWriter{content: []byte("projectId: proj-a\n")}
		},
	}

	// Caller scoped to proj-a only, targeting proj-b: forbidden.
	_, err := Save(context.Background(), kind, target, cand, Caller{ScopedProjectIDs: []string{"proj-a"}}, deps)
	if err == nil {
		t.Fatal("Save returned nil error, want ErrForbidden for an out-of-scope project")
	}

	// Caller scoped to proj-a targeting proj-a: allowed.
	candA := CandidateConfig{Kind: "synthetic", ProjectID: "proj-a", Values: map[string]string{"note": "hi"}}
	res, err := Save(context.Background(), kind, target, candA, Caller{ScopedProjectIDs: []string{"proj-a"}}, deps)
	if err != nil {
		t.Fatalf("Save for in-scope project: %v", err)
	}
	if !res.Saved {
		t.Error("Saved = false, want true for an in-scope project save")
	}
}

// --- Backup/Restore on each failure point (design §7/§8) ---

func TestSave_BackupRestoreOnEachFailurePoint(t *testing.T) {
	baseContent := []byte("telegram:\n  enabled: false\n")

	newDeps := func(t *testing.T, w *fakeConfigWriter) SaveDeps {
		t.Helper()
		// Secrets placement (step 3) precedes the config Backup/Write
		// (step 4) that these subtests fake — give it a real, writable
		// secrets dir so only the config-writer step is under test.
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
			t.Fatal(err)
		}
		return SaveDeps{
			ConfigDir: dir,
			NewWriter: func(_ string) featuredoctor.ConfigWriter { return w },
		}
	}
	kind := telegramTestKind(&fakeProber{kind: "telegram", result: okProbe("telegram")})
	target, _ := SaveTargetForKind("telegram")
	cand := CandidateConfig{Kind: "telegram", Values: map[string]string{"bot_token": "a-literal-token-value-123456"}}

	t.Run("SetYAMLKey error", func(t *testing.T) {
		// A pre-existing scalar at "telegram" (not a mapping) makes
		// SetYAMLKey("telegram.bot_token", ...) fail for real.
		w := &fakeConfigWriter{content: []byte("telegram: not-a-map\n")}
		_, err := Save(context.Background(), kind, target, cand, adminCaller(), newDeps(t, w))
		if err == nil {
			t.Fatal("want an error from a bad SetYAMLKey path")
		}
		if !w.restored {
			t.Error("writer.Restore was not called")
		}
		if w.writtenCount != 0 {
			t.Error("writer.Write was called despite the patch step failing")
		}
	})

	t.Run("Write error", func(t *testing.T) {
		w := &fakeConfigWriter{content: baseContent, writeErr: os.ErrPermission}
		_, err := Save(context.Background(), kind, target, cand, adminCaller(), newDeps(t, w))
		if err == nil {
			t.Fatal("want an error from Write failing")
		}
		if !w.restored {
			t.Error("writer.Restore was not called after a Write failure")
		}
	})

	t.Run("Validate error", func(t *testing.T) {
		w := &fakeConfigWriter{content: baseContent, validateErr: os.ErrInvalid}
		_, err := Save(context.Background(), kind, target, cand, adminCaller(), newDeps(t, w))
		if err == nil {
			t.Fatal("want an error from Validate failing")
		}
		if !w.restored {
			t.Error("writer.Restore was not called after a Validate failure")
		}
	})

	t.Run("reload reject", func(t *testing.T) {
		w := &fakeConfigWriter{content: baseContent}
		deps := newDeps(t, w)
		deps.Reloader = &fakeReloader{err: context.DeadlineExceeded}
		_, err := Save(context.Background(), kind, target, cand, adminCaller(), deps)
		if err == nil {
			t.Fatal("want an error from a reload rejection")
		}
		if !w.restored {
			t.Error("writer.Restore was not called after a reload failure")
		}
	})

	t.Run("reload status poll timeout", func(t *testing.T) {
		w := &fakeConfigWriter{content: baseContent}
		deps := newDeps(t, w)
		deps.Reloader = &fakeReloader{}
		deps.ReloadDeadline = 30 * time.Millisecond
		deps.ReloadStatus = &fakeReloadStatusChecker{statuses: []config.ReloadStatus{{Blocked: true, BlockedReason: "busy"}}}
		_, err := Save(context.Background(), kind, target, cand, adminCaller(), deps)
		if err == nil {
			t.Fatal("want an error from a reload-status poll timeout")
		}
		if !w.restored {
			t.Error("writer.Restore was not called after a reload-status timeout")
		}
	})

	t.Run("backup error leaves nothing written", func(t *testing.T) {
		w := &fakeConfigWriter{content: baseContent, backupErr: os.ErrPermission}
		_, err := Save(context.Background(), kind, target, cand, adminCaller(), newDeps(t, w))
		if err == nil {
			t.Fatal("want an error from Backup failing")
		}
		if w.writtenCount != 0 {
			t.Error("writer.Write was called despite Backup failing")
		}
	})

	t.Run("reload status poll eventually succeeds", func(t *testing.T) {
		w := &fakeConfigWriter{content: baseContent}
		deps := newDeps(t, w)
		deps.Reloader = &fakeReloader{}
		deps.ReloadStatus = &fakeReloadStatusChecker{statuses: []config.ReloadStatus{
			{Blocked: true, BlockedReason: "busy"},
			// A genuine success sets LastReload — the poll now requires that
			// positive completion signal, not merely !Blocked.
			{Blocked: false, LastReload: time.Now()},
		}}
		res, err := Save(context.Background(), kind, target, cand, adminCaller(), deps)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if !res.Saved {
			t.Error("Saved = false, want true once the poll observes a completed reload")
		}
		if w.restored {
			t.Error("writer.Restore was called despite an eventual reload success")
		}
	})
}

// --- Orphaned secret is inert; clean re-save overwrites it (design §7) ---

func TestSave_RestoredConfigLeavesOrphanedSecretInert_ThenCleanResaveOverwrites(t *testing.T) {
	dir := newTestConfigDir(t)
	// Corrupt config.yaml so the first save's SetYAMLKey step fails
	// AFTER the secret has already been placed (step 3 precedes step 4).
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("telegram: not-a-map\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kind := telegramTestKind(&fakeProber{kind: "telegram", result: okProbe("telegram")})
	target, _ := SaveTargetForKind("telegram")
	cand := CandidateConfig{Kind: "telegram", Values: map[string]string{"bot_token": "orphaned-secret-value-111"}}

	_, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
	if err == nil {
		t.Fatal("want the first save to fail (corrupted config.yaml)")
	}
	restoredConfig, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if string(restoredConfig) != "telegram: not-a-map\n" {
		t.Fatalf("config.yaml = %q, want it restored to the original corrupt-but-unchanged content", restoredConfig)
	}
	orphaned, err := os.ReadFile(filepath.Join(dir, "secrets", "telegram.env"))
	if err != nil {
		t.Fatalf("expected the secret to have been written (orphaned but inert): %v", err)
	}
	if !strings.Contains(string(orphaned), "orphaned-secret-value-111") {
		t.Errorf("orphaned secret file = %q, want the first save's value", orphaned)
	}

	// Fix config.yaml (simulating an operator/re-save) and save again with
	// a fresh value — the orphaned secret must be cleanly overwritten, not
	// merged/duplicated.
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(minimalDaemonConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cand2 := CandidateConfig{Kind: "telegram", Values: map[string]string{"bot_token": "fresh-secret-value-222"}}
	res, err := Save(context.Background(), kind, target, cand2, adminCaller(), SaveDeps{ConfigDir: dir})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if !res.Saved {
		t.Fatal("second save: Saved = false, want true")
	}
	final, err := os.ReadFile(filepath.Join(dir, "secrets", "telegram.env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(final), "orphaned-secret-value-111") {
		t.Errorf("secret file still contains the orphaned value: %q", final)
	}
	if !strings.Contains(string(final), "fresh-secret-value-222") {
		t.Errorf("secret file = %q, want the fresh value", final)
	}
}

// --- Daemon vs project secret-store routing (design §5.4 step 3) ---

func TestSave_DaemonVsProjectSecretRouting(t *testing.T) {
	dir := newTestConfigDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "projects", "proj-a.yaml"), []byte("projectId: proj-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Daemon-scope secret: telegram, real save target.
	tgKind := telegramTestKind(&fakeProber{kind: "telegram", result: okProbe("telegram")})
	tgTarget, _ := SaveTargetForKind("telegram")
	tgCand := CandidateConfig{Kind: "telegram", Values: map[string]string{"bot_token": "daemon-scope-secret-value"}}
	if _, err := Save(context.Background(), tgKind, tgTarget, tgCand, adminCaller(), SaveDeps{ConfigDir: dir}); err != nil {
		t.Fatalf("daemon save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "telegram.env")); err != nil {
		t.Errorf("daemon secret file missing: %v", err)
	}

	// Project-scope secret: synthetic kind exercising the shared machinery's
	// project routing. The base EnvName is derived into a per-project
	// suffixed name by splitAndPlaceFields (task 5.2b: two projects sharing
	// the one project-secrets.env file must not collide on the same bare
	// env-var name) — see projectScopedEnvName.
	envNameBase := "TEST_PROJECT_ROUTING_SECRET"
	envName := projectScopedEnvName(envNameBase, "proj-a")
	t.Cleanup(func() { _ = os.Unsetenv(envName) })
	projKind := IntegrationKind{
		ID:     "synthetic",
		Scope:  ScopeProject,
		Fields: []CredentialField{{Key: "token", Secret: true, EnvName: envNameBase}},
		Prober: &fakeProber{kind: "synthetic", result: okProbe("synthetic")},
	}
	projTarget := SaveTarget{
		Scope:       ScopeProject,
		ConfigFile:  projectConfigFile,
		ScalarKeys:  map[string]string{"token": "token"},
		SecretValue: func(name string) string { return name },
	}
	projCand := CandidateConfig{Kind: "synthetic", ProjectID: "proj-a", Values: map[string]string{"token": "project-scope-secret-value"}}
	// Fake writer for the project half only — this test's assertions are
	// about secret-store routing, not the config-write pipeline (that's
	// covered by the real email/slack/github_app save tests below, which
	// exercise ProjectConfigWriter's project-schema Validate() for real).
	projDeps := SaveDeps{
		ConfigDir: dir,
		NewWriter: func(_ string) featuredoctor.ConfigWriter {
			return &fakeConfigWriter{content: []byte("projectId: proj-a\n")}
		},
	}
	res, err := Save(context.Background(), projKind, projTarget, projCand, adminCaller(), projDeps)
	if err != nil {
		t.Fatalf("project save: %v", err)
	}
	if !res.Saved {
		t.Fatal("project save: Saved = false")
	}
	projSecret, err := os.ReadFile(filepath.Join(dir, "secrets", projectSecretsFile))
	if err != nil {
		t.Fatalf("project secrets file missing: %v", err)
	}
	if !strings.Contains(string(projSecret), envName+"=project-scope-secret-value") {
		t.Errorf("project secrets file = %q, want %s=project-scope-secret-value", projSecret, envName)
	}
	// projectdoctor.EnvSecrets.Set also os.Setenv's — config resolves live.
	if got := os.Getenv(envName); got != "project-scope-secret-value" {
		t.Errorf("os.Getenv(%s) = %q, want it live via os.Setenv", envName, got)
	}
	// The daemon secret file must NOT contain the project secret, and
	// vice versa — routing must not cross-contaminate the two files.
	if strings.Contains(string(projSecret), "daemon-scope-secret-value") {
		t.Error("project secrets file unexpectedly contains the daemon secret")
	}
}

// --- SaveTargetForKind: supported vs documented-gap kinds ---

func TestSaveTargetForKind_AllFourKindsSupported(t *testing.T) {
	// Task 5.2b reconciled and wired the three project-scope kinds
	// (email, github_app, slack) alongside 5.2's telegram — all four
	// catalog kinds have a write-path target.
	for _, id := range []string{"telegram", "email", "github_app", "slack"} {
		target, ok := SaveTargetForKind(id)
		if !ok {
			t.Errorf("SaveTargetForKind(%q) = false, want true", id)
			continue
		}
		if target.ConfigFile == nil {
			t.Errorf("SaveTargetForKind(%q).ConfigFile is nil", id)
		}
	}
	// Regression (2026-07-10 MCP-kind removal): the hub must not regrow a
	// write path to mcp.servers — the control-plane hub's ledger-gated MCP
	// tab is the canonical write surface (see catalog.go's Registry doc).
	if _, ok := SaveTargetForKind("mcp"); ok {
		t.Error("SaveTargetForKind(\"mcp\") = true — the Integrations Hub must not own an mcp.servers write path")
	}
}

func TestSaveTargetForKind_UnknownKindNotSupported(t *testing.T) {
	if _, ok := SaveTargetForKind("does-not-exist"); ok {
		t.Error("SaveTargetForKind(unknown) = true, want false")
	}
}

// --- hasSecretLiteral boundary function, direct unit tests ---

// TestHasSecretLiteral covers the review-20260709-cc3e finding-2 tightening:
// the env-var-name exemption is now a whitelist against allowedEnvNames
// (the calling kind's own declared Secret-field EnvNames), not a bare
// pattern match — an env-shaped value is only exempt when it's actually one
// of the declared names.
func TestHasSecretLiteral(t *testing.T) {
	cases := []struct {
		name            string
		in              string
		allowedEnvNames []string
		want            bool
	}{
		{"empty", "", nil, false},
		{"placeholder", "${TELEGRAM_BOT_TOKEN}", nil, false},
		{"env var name (short, not declared)", "TELEGRAM_BOT_TOKEN", nil, false}, // 18 chars < 24, caught by length not exemption
		{"bare long token", "aVeryLongBareSecretTokenValue1234", nil, true},
		{"short token", "short", nil, false},
		{"contains space", "this has spaces in it long enough", nil, false},
		{"looks like a path", "/some/long/path/that/is/not/a/secret", nil, false},
		{"looks like a host:port", "internal.example.com:8080-not-a-secret", nil, false},
		{
			"long env-var-shaped value NOT a declared EnvName is STILL caught (finding 2 regression)",
			"GITHUB_APP_WEBHOOK_SECRET_ENV_NAME", nil, true,
		},
		{
			"long env-var-shaped value that IS a declared EnvName is exempted",
			"GITHUB_APP_WEBHOOK_SECRET_ENV_NAME", []string{"GITHUB_APP_WEBHOOK_SECRET_ENV_NAME"}, false,
		},
		{
			"env-shaped real secret example from finding 2, not declared for this kind",
			"GITHUB_TOKEN_ABC123DEF456", nil, true,
		},
		{
			"env-shaped real secret example from finding 2, not declared even though OTHER names are",
			"GITHUB_TOKEN_ABC123DEF456", []string{"TELEGRAM_BOT_TOKEN"}, true,
		},
		{"lowercase long token still caught", "githubappwebhooksecretenvnamebutsecret", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasSecretLiteral(tc.in, tc.allowedEnvNames); got != tc.want {
				t.Errorf("hasSecretLiteral(%q, %v) = %v, want %v", tc.in, tc.allowedEnvNames, got, tc.want)
			}
		})
	}
}

// --- Caller.Authorized, direct unit tests ---

func TestCaller_Authorized(t *testing.T) {
	cases := []struct {
		name      string
		caller    Caller
		scope     Scope
		projectID string
		want      bool
	}{
		{"admin daemon", Caller{IsAdmin: true}, ScopeDaemon, "", true},
		{"admin project any", Caller{IsAdmin: true}, ScopeProject, "any-project", true},
		{"non-admin daemon", Caller{IsAdmin: false}, ScopeDaemon, "", false},
		{"non-admin project in scope", Caller{ScopedProjectIDs: []string{"proj-a"}}, ScopeProject, "proj-a", true},
		{"non-admin project out of scope", Caller{ScopedProjectIDs: []string{"proj-a"}}, ScopeProject, "proj-b", false},
		{"non-admin project no scope", Caller{}, ScopeProject, "proj-a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.caller.Authorized(tc.scope, tc.projectID); got != tc.want {
				t.Errorf("Authorized(%v, %q) = %v, want %v", tc.scope, tc.projectID, got, tc.want)
			}
		})
	}
}

// --- SaveError ---

func TestSaveError_ErrorAndUnwrap(t *testing.T) {
	cause := os.ErrPermission

	restored := &SaveError{Step: "write", Cause: cause, Restored: true}
	if !strings.Contains(restored.Error(), "restored from backup") {
		t.Errorf("Error() = %q, want it to mention restoration", restored.Error())
	}
	if !errors.Is(restored, os.ErrPermission) {
		t.Error("errors.Is(restored, os.ErrPermission) = false, want true (Unwrap must expose Cause)")
	}

	notRestored := &SaveError{Step: "write", Cause: cause, Restored: false}
	if !strings.Contains(notRestored.Error(), "RESTORE ALSO FAILED") {
		t.Errorf("Error() = %q, want it to flag the failed restore", notRestored.Error())
	}
}

// --- Defensive guards (should never trigger via SaveTargetForKind's real
// entries, but Save must fail loudly rather than silently misbehave if a
// caller builds an inconsistent kind/target pair by hand). ---

func TestSave_DefensiveGuards(t *testing.T) {
	dir := newTestConfigDir(t)

	t.Run("target scope mismatch", func(t *testing.T) {
		kind := telegramTestKind(&fakeProber{kind: "telegram", result: okProbe("telegram")})
		mismatched := SaveTarget{Scope: ScopeProject, ConfigFile: projectConfigFile}
		_, err := Save(context.Background(), kind, mismatched, CandidateConfig{}, adminCaller(), SaveDeps{ConfigDir: dir})
		if err == nil || !strings.Contains(err.Error(), "does not match kind") {
			t.Fatalf("err = %v, want a scope-mismatch error", err)
		}
	})

	t.Run("nil prober", func(t *testing.T) {
		kind := telegramTestKind(nil)
		target, _ := SaveTargetForKind("telegram")
		_, err := Save(context.Background(), kind, target, CandidateConfig{}, adminCaller(), SaveDeps{ConfigDir: dir})
		if err == nil || !strings.Contains(err.Error(), "no Prober") {
			t.Fatalf("err = %v, want a nil-Prober error", err)
		}
	})

	t.Run("secret field missing EnvName", func(t *testing.T) {
		kind := IntegrationKind{
			ID:     "telegram",
			Scope:  ScopeDaemon,
			Fields: []CredentialField{{Key: "bot_token", Secret: true}},
			Prober: &fakeProber{kind: "telegram", result: okProbe("telegram")},
		}
		target, _ := SaveTargetForKind("telegram")
		cand := CandidateConfig{Values: map[string]string{"bot_token": "x"}}
		_, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
		if err == nil || !strings.Contains(err.Error(), "no EnvName") {
			t.Fatalf("err = %v, want a missing-EnvName error", err)
		}
	})

	t.Run("secret field but target has no SecretValue", func(t *testing.T) {
		kind := telegramTestKind(&fakeProber{kind: "telegram", result: okProbe("telegram")})
		target := SaveTarget{Scope: ScopeDaemon, ConfigFile: daemonConfigFile, ScalarKeys: map[string]string{"bot_token": "telegram.bot_token"}}
		cand := CandidateConfig{Values: map[string]string{"bot_token": "x"}}
		_, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
		if err == nil || !strings.Contains(err.Error(), "no SecretValue") {
			t.Fatalf("err = %v, want a no-SecretValue error", err)
		}
	})

	t.Run("SecretValue itself misbehaves and returns a literal", func(t *testing.T) {
		kind := telegramTestKind(&fakeProber{kind: "telegram", result: okProbe("telegram")})
		target := SaveTarget{
			Scope:       ScopeDaemon,
			ConfigFile:  daemonConfigFile,
			ScalarKeys:  map[string]string{"bot_token": "telegram.bot_token"},
			SecretValue: func(_ string) string { return "aBareLiteralNotAPlaceholderAtAll" },
		}
		cand := CandidateConfig{Values: map[string]string{"bot_token": "x"}}
		_, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
		if err == nil || !strings.Contains(err.Error(), "bug in SecretValue") {
			t.Fatalf("err = %v, want a SecretValue-bug error", err)
		}
	})

	// review-20260709-cc3e finding 2 regression: SecretValue returning an
	// env-var-shaped string is only exempt when it equals THIS field's own
	// declared EnvName ("TELEGRAM_BOT_TOKEN" here) — a broken SecretValue
	// that returns some OTHER env-shaped name must still be caught, not
	// waved through by pattern alone.
	t.Run("SecretValue returns an env-shaped value that is not this kind's declared EnvName", func(t *testing.T) {
		kind := telegramTestKind(&fakeProber{kind: "telegram", result: okProbe("telegram")})
		target := SaveTarget{
			Scope:       ScopeDaemon,
			ConfigFile:  daemonConfigFile,
			ScalarKeys:  map[string]string{"bot_token": "telegram.bot_token"},
			SecretValue: func(_ string) string { return "SOME_OTHER_UNDECLARED_ENV_NAME" },
		}
		cand := CandidateConfig{Values: map[string]string{"bot_token": "x"}}
		_, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
		if err == nil || !strings.Contains(err.Error(), "bug in SecretValue") {
			t.Fatalf("err = %v, want a SecretValue-bug error", err)
		}
	})

	t.Run("scalar field has no ScalarKeys entry", func(t *testing.T) {
		kind := IntegrationKind{
			ID:    "telegram",
			Scope: ScopeDaemon,
			Fields: []CredentialField{
				{Key: "bot_token", Required: true}, // deliberately non-secret so it routes to ScalarKeys
			},
			Prober: &fakeProber{kind: "telegram", result: okProbe("telegram")},
		}
		target := SaveTarget{Scope: ScopeDaemon, ConfigFile: daemonConfigFile, ScalarKeys: map[string]string{}}
		cand := CandidateConfig{Values: map[string]string{"bot_token": "x"}}
		_, err := Save(context.Background(), kind, target, cand, adminCaller(), SaveDeps{ConfigDir: dir})
		if err == nil || !strings.Contains(err.Error(), "no ScalarKeys entry") {
			t.Fatalf("err = %v, want a missing-ScalarKeys error", err)
		}
	})
}

// --- pollReloadStatus context cancellation ---

func TestPollReloadStatus_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checker := &fakeReloadStatusChecker{statuses: []config.ReloadStatus{{Blocked: true}}}
	err := pollReloadStatus(ctx, checker, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestPollReloadStatus_ZeroValueDoesNotFalseSucceed is the review-20260716-b1ab
// regression: a zero-value ReloadStatus (reload not reflected / stale checker)
// must NOT be treated as success merely because Blocked/HasErrors are false —
// success requires positive LastReload evidence.
func TestPollReloadStatus_ZeroValueDoesNotFalseSucceed(t *testing.T) {
	checker := &fakeReloadStatusChecker{statuses: []config.ReloadStatus{{}}}
	err := pollReloadStatus(context.Background(), checker, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not report completion") {
		t.Fatalf("err = %v, want a completion-timeout error (no false success)", err)
	}
}

// TestPollReloadStatus_PendingActivationDoesNotSucceed: a reload staged behind
// in-flight tasks (PendingActivation) isn't active yet — the poll must not
// report success even with LastReload set.
func TestPollReloadStatus_PendingActivationDoesNotSucceed(t *testing.T) {
	checker := &fakeReloadStatusChecker{statuses: []config.ReloadStatus{
		{LastReload: time.Now(), PendingActivation: true},
	}}
	err := pollReloadStatus(context.Background(), checker, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "activation is still pending") {
		t.Fatalf("err = %v, want an activation-pending error", err)
	}
}

// TestPollReloadStatus_CompletedSucceeds: a completed, activated reload
// (LastReload set; not blocked/errored/pending) succeeds.
func TestPollReloadStatus_CompletedSucceeds(t *testing.T) {
	checker := &fakeReloadStatusChecker{statuses: []config.ReloadStatus{{LastReload: time.Now()}}}
	if err := pollReloadStatus(context.Background(), checker, time.Second); err != nil {
		t.Fatalf("completed reload should succeed: %v", err)
	}
}
