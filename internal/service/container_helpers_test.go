package service

// Coverage for pure helpers extracted during the 2026-05-16 split:
// buildSecretsDetector, resolveRegistryConfigDir, hasRegistryLayout,
// toSet, buildObservabilityConfig, logJudgeStartupSummary, and
// postMortemAdapter.Generate. The init* / collect* methods that need
// a live database / network listener are exercised through
// integration tests, not here.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
)

// ---------- buildSecretsDetector ----------

func TestBuildSecretsDetector_DefaultsCompile(t *testing.T) {
	d, actions, err := buildSecretsDetector(config.SecretsConfig{})
	if err != nil {
		t.Fatalf("buildSecretsDetector(zero) error: %v", err)
	}
	if d == nil {
		t.Fatal("detector is nil")
	}
	if actions == nil {
		t.Fatal("actions map is nil")
	}
	if len(actions) != 0 {
		t.Errorf("actions = %v, want empty (no checkpoints set)", actions)
	}
}

func TestBuildSecretsDetector_DisablesNamedPattern(t *testing.T) {
	// Pick any default-pattern name. The list is stable enough that
	// at least one entry exists; we just verify the option doesn't
	// fail the build and produces a detector.
	cfg := config.SecretsConfig{
		Patterns: config.SecretsPatternsConfig{
			Disable: []string{"openai_api_key"}, // safe even if absent
		},
	}
	_, _, err := buildSecretsDetector(cfg)
	if err != nil {
		t.Fatalf("disabling a default pattern failed: %v", err)
	}
}

func TestBuildSecretsDetector_AppendsCustomPattern(t *testing.T) {
	cfg := config.SecretsConfig{
		Patterns: config.SecretsPatternsConfig{
			Custom: []config.SecretsPatternConfig{
				{Name: "test_token", Regex: `tok-[a-z]+`, Description: "test"},
			},
		},
	}
	d, _, err := buildSecretsDetector(cfg)
	if err != nil {
		t.Fatalf("appending custom pattern failed: %v", err)
	}
	if d == nil {
		t.Fatal("detector nil after custom append")
	}
}

func TestBuildSecretsDetector_BadRegexErrors(t *testing.T) {
	cfg := config.SecretsConfig{
		Patterns: config.SecretsPatternsConfig{
			Custom: []config.SecretsPatternConfig{
				{Name: "broken", Regex: "[unterminated"},
			},
		},
	}
	_, _, err := buildSecretsDetector(cfg)
	if err == nil {
		t.Fatal("expected error from invalid regex, got nil")
	}
}

func TestBuildSecretsDetector_ValidCheckpointActionsKept(t *testing.T) {
	cfg := config.SecretsConfig{
		Checkpoints: map[string]string{
			"result_json":  "detect",
			"unknown_chan": "wibble", // invalid action — silently dropped
		},
	}
	_, actions, err := buildSecretsDetector(cfg)
	if err != nil {
		t.Fatalf("buildSecretsDetector: %v", err)
	}
	if _, ok := actions["result_json"]; !ok {
		t.Error("valid action 'detect' missing from actions map")
	}
	if _, ok := actions["unknown_chan"]; ok {
		t.Error("invalid action string 'wibble' should have been dropped")
	}
}

// ---------- resolveRegistryConfigDir + hasRegistryLayout ----------

func TestResolveRegistryConfigDir_EnvOverrideWins(t *testing.T) {
	dir := makeRegistryLayout(t)
	t.Setenv("VORNIK_CONFIGS_DIR", dir)
	got := resolveRegistryConfigDir("/nonexistent/path/config.yaml")
	if got != dir {
		t.Errorf("got %q, want %q (env override should win)", got, dir)
	}
}

func TestResolveRegistryConfigDir_EnvSetButLayoutMissing_FallsThrough(t *testing.T) {
	t.Setenv("VORNIK_CONFIGS_DIR", "/this/does/not/exist/anywhere")
	// No fallback layout available, no configPath: should yield "".
	got := resolveRegistryConfigDir("")
	if got != "" {
		t.Errorf("got %q, want empty (env points nowhere, no fallback)", got)
	}
}

func TestResolveRegistryConfigDir_PrefersConfigsSiblingOfConfigFile(t *testing.T) {
	t.Setenv("VORNIK_CONFIGS_DIR", "")
	base := t.TempDir()
	configFile := filepath.Join(base, "vornik.yaml")
	if err := os.WriteFile(configFile, []byte(""), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	configsDir := filepath.Join(base, "configs")
	mkdirRegistry(t, configsDir)

	got := resolveRegistryConfigDir(configFile)
	if got != configsDir {
		t.Errorf("got %q, want %q (configs/ sibling should win)", got, configsDir)
	}
}

func TestHasRegistryLayout_AllThreeDirsRequired(t *testing.T) {
	dir := t.TempDir()
	if hasRegistryLayout(dir) {
		t.Error("empty dir should not satisfy layout")
	}
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if hasRegistryLayout(dir) {
		t.Error("only projects/ present — should not satisfy layout")
	}
	if err := os.MkdirAll(filepath.Join(dir, "swarms"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if hasRegistryLayout(dir) {
		t.Error("projects+swarms only — should not satisfy layout")
	}
	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !hasRegistryLayout(dir) {
		t.Error("all three present — should satisfy layout")
	}
}

func TestHasRegistryLayout_FilesNotDirsAreRejected(t *testing.T) {
	dir := t.TempDir()
	// Create projects as a file rather than a dir.
	if err := os.WriteFile(filepath.Join(dir, "projects"), []byte(""), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "swarms"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if hasRegistryLayout(dir) {
		t.Error("projects as file (not dir) — layout should fail")
	}
}

// makeRegistryLayout creates a temp dir with the projects/swarms/
// workflows layout that hasRegistryLayout requires.
func makeRegistryLayout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mkdirRegistry(t, dir)
	return dir
}

func mkdirRegistry(t *testing.T, dir string) {
	t.Helper()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
}

// ---------- toSet ----------

func TestToSet(t *testing.T) {
	got := toSet([]string{"a", "b", "a", "c"})
	if len(got) != 3 {
		t.Errorf("len=%d, want 3", len(got))
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if got2 := toSet(nil); len(got2) != 0 {
		t.Errorf("toSet(nil) = %v, want empty", got2)
	}
}

// ---------- registryProjectIDsAdapter ----------

func TestRegistryProjectIDsAdapter_NilSafe(t *testing.T) {
	var a *registryProjectIDsAdapter
	if got := a.ListProjectIDs(); got != nil {
		t.Errorf("nil adapter should return nil, got %v", got)
	}
	a = &registryProjectIDsAdapter{registry: nil}
	if got := a.ListProjectIDs(); got != nil {
		t.Errorf("nil internal registry should return nil, got %v", got)
	}
}

// ---------- buildObservabilityConfig ----------

func TestBuildObservabilityConfig_NilReturnsZero(t *testing.T) {
	got := buildObservabilityConfig(nil)
	if got.MetricsAddr != "" || got.TracingEnabled || got.TracingEndpoint != "" {
		t.Errorf("nil cfg should yield zero observability.Config, got %+v", got)
	}
}

func TestBuildObservabilityConfig_MapsFields(t *testing.T) {
	cfg := &config.Config{
		Metrics: config.MetricsConfig{Enabled: true, Addr: ":9999"},
		Tracing: config.TracingConfig{Enabled: true, Endpoint: "otel:4317"},
	}
	got := buildObservabilityConfig(cfg)
	if got.MetricsAddr != ":9999" {
		t.Errorf("MetricsAddr = %q", got.MetricsAddr)
	}
	if !got.TracingEnabled {
		t.Error("TracingEnabled lost")
	}
	if got.TracingEndpoint != "otel:4317" {
		t.Errorf("TracingEndpoint = %q", got.TracingEndpoint)
	}
}

func TestBuildObservabilityConfig_MetricsDisabledDropsAddr(t *testing.T) {
	cfg := &config.Config{
		Metrics: config.MetricsConfig{Enabled: false, Addr: ":9999"},
	}
	got := buildObservabilityConfig(cfg)
	if got.MetricsAddr != "" {
		t.Errorf("disabled metrics should clear addr, got %q", got.MetricsAddr)
	}
}

// ---------- logJudgeStartupSummary ----------
//
// The function is log-only; we exercise the three branches and
// verify no panic on nil receiver / missing registry.

func TestLogJudgeStartupSummary_NilReceiverIsSafe(t *testing.T) {
	var c *Container
	c.logJudgeStartupSummary() // must not panic
}

func TestLogJudgeStartupSummary_NotWired_Branch(t *testing.T) {
	c := &Container{
		Logger:           zerolog.Nop(),
		judgeRunnerWired: false,
	}
	c.logJudgeStartupSummary() // hits the "NOT wired" branch
}

func TestLogJudgeStartupSummary_WiredZeroProjects_Branch(t *testing.T) {
	c := &Container{
		Logger:           zerolog.Nop(),
		judgeRunnerWired: true,
		Registry:         registry.New(),
	}
	c.logJudgeStartupSummary() // hits the "0 projects opt in" branch
}

// ---------- postMortemAdapter.Generate ----------

func TestPostMortemAdapter_NilReceiverErrors(t *testing.T) {
	var a *postMortemAdapter
	_, err := a.Generate(context.Background(), "task-1", false)
	if err == nil {
		t.Fatal("nil receiver should error")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error message changed: %v", err)
	}
}

func TestPostMortemAdapter_NilExplainerErrors(t *testing.T) {
	a := &postMortemAdapter{e: nil}
	_, err := a.Generate(context.Background(), "task-1", false)
	if err == nil {
		t.Fatal("nil explainer should error")
	}
}

// Note: the happy-path "explainer wired, delegate succeeds" branch is
// covered by integration tests that hit a real explainer + DB. Unit-
// testing it here would require a full persistence.TaskRepository
// stub (15+ methods) for no added safety guarantee.
