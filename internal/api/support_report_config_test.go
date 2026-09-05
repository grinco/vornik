package api

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// newTestDetector — the builder's own tests moved to internal/supportbundle
// with the builder; the wiring tests below stayed here, and still need a real
// detector to assert the Server hands one over.
func newTestDetector(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatalf("detector: %v", err)
	}
	return d
}

// TestRedactedConfigYAML confirms field-name redaction runs and the
// output is YAML-shaped. The Telegram BotToken field marshals to a key
// that redactSecrets catches (bottoken token).
func TestRedactedConfigYAML(t *testing.T) {
	cfg := &config.Config{
		Telegram: config.TelegramConfig{Enabled: true, BotToken: "1234567:super-secret-bot-token"},
	}
	yml, err := redactedConfigYAML(cfg)
	if err != nil {
		t.Fatalf("redactedConfigYAML: %v", err)
	}
	if strings.Contains(yml, "super-secret-bot-token") {
		t.Errorf("bot token leaked into config snapshot:\n%s", yml)
	}
	// Output should parse as something YAML-ish (contains a colon-keyed map).
	if !strings.Contains(yml, ":") {
		t.Errorf("config snapshot doesn't look like YAML:\n%s", yml)
	}
}

// TestNewBundleBuilderWiresConfig confirms the Server builder wires the
// redacted config snapshot.
func TestNewBundleBuilderWiresConfig(t *testing.T) {
	s := NewServer(WithConfig(&config.Config{
		Admin: config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-x"}},
	}))
	b := s.newBundleBuilder()
	if b.ConfigYAML == "" {
		t.Error("expected config snapshot wired into builder")
	}
}

// TestNewBundleBuilderFullWiring sets every support-relevant Server
// field directly (same-package test) so newBundleBuilder's per-repo
// branches all fire, and asserts the builder received them.
func TestNewBundleBuilderFullWiring(t *testing.T) {
	det := newTestDetector(t)
	s := &Server{
		secretsDetector:       det,
		taskRepo:              supportTaskRepoStub{task: &persistence.Task{ID: "t1"}},
		executionRepo:         nil, // left nil intentionally elsewhere; set below via fakes won't satisfy full iface
		stepOutcomeRepo:       nil,
		toolAuditRepo:         nil,
		llmUsageRepo:          nil,
		taskMessageRepo:       nil,
		adminAuditRepo:        nil,
		artifactRepo:          nil,
		artifactOpener:        stubOpener{data: []byte("x")},
		supportDoctor:         fakeDoctorBuilder{},
		supportHealth:         fakeHealthBuilder{},
		supportMetrics:        fakeMetricsBuilder{},
		supportJudgeRepo:      fakeJudgeBuilder{},
		supportPostMortemRepo: fakePMBuilder{},
		config:                &config.Config{Telegram: config.TelegramConfig{BotToken: "x"}},
	}
	b := s.newBundleBuilder()
	if b.Detector == nil || b.Opener == nil || b.Doctor == nil || b.Health == nil ||
		b.Metrics == nil || b.Repos.JudgeVerdct == nil || b.Repos.PostMortem == nil {
		t.Fatalf("newBundleBuilder did not wire all collectors: %+v", b)
	}
	if b.ConfigYAML == "" {
		t.Error("config snapshot not wired")
	}
	if b.Repos.Tasks == nil {
		t.Error("task repo not wired")
	}
}

type fakeDoctorBuilder struct{}

func (fakeDoctorBuilder) Run(context.Context) (any, error) { return map[string]string{"ok": "1"}, nil }

type fakeHealthBuilder struct{}

func (fakeHealthBuilder) Snapshot(context.Context) (any, error) {
	return map[string]string{"ok": "1"}, nil
}

type fakeMetricsBuilder struct{}

func (fakeMetricsBuilder) Snapshot(context.Context) (string, error) { return "# m\n", nil }

type fakeJudgeBuilder struct{}

func (fakeJudgeBuilder) GetByTask(context.Context, string) (*persistence.TaskJudgeVerdict, error) {
	return nil, persistence.ErrNotFound
}

type fakePMBuilder struct{}

func (fakePMBuilder) Get(context.Context, string) (*persistence.TaskPostMortem, error) {
	return nil, persistence.ErrNotFound
}
