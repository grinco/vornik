package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/retention"
)

func TestNormalizeFirewallMode(t *testing.T) {
	cases := map[string]memoryfirewall.EnforcementMode{
		"enforce":  memoryfirewall.EnforcementEnforce,
		"ENFORCE":  memoryfirewall.EnforcementEnforce, // case-insensitive
		"  off  ":  memoryfirewall.EnforcementOff,     // trimmed
		"advisory": memoryfirewall.EnforcementAdvisory,
		"":         memoryfirewall.EnforcementAdvisory, // default
		"nonsense": memoryfirewall.EnforcementAdvisory, // unknown → default
	}
	for in, want := range cases {
		if got := normalizeFirewallMode(in); got != want {
			t.Errorf("normalizeFirewallMode(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestEnvOrEmptyAndEnvOr(t *testing.T) {
	if got := envOrEmpty(""); got != "" {
		t.Errorf("envOrEmpty(empty key) = %q, want empty", got)
	}
	t.Setenv("VORNIK_TEST_HELPER_VAR", "hello")
	if got := envOrEmpty("VORNIK_TEST_HELPER_VAR"); got != "hello" {
		t.Errorf("envOrEmpty(set) = %q, want hello", got)
	}
	if got := envOr("VORNIK_TEST_HELPER_VAR", "dflt"); got != "hello" {
		t.Errorf("envOr(set) = %q, want hello", got)
	}
	if got := envOr("VORNIK_TEST_UNSET_VAR_XYZ", "dflt"); got != "dflt" {
		t.Errorf("envOr(unset) = %q, want dflt", got)
	}
}

func TestMemoryFirewallMode(t *testing.T) {
	if got := (*Container)(nil).memoryFirewallMode(); got != memoryfirewall.EnforcementAdvisory {
		t.Errorf("nil container = %v, want Advisory", got)
	}
	c := &Container{}
	// No env → advisory default.
	t.Setenv("VORNIK_MEMORY_FIREWALL_MODE", "")
	if got := c.memoryFirewallMode(); got != memoryfirewall.EnforcementAdvisory {
		t.Errorf("no env = %v, want Advisory", got)
	}
	// Env override → enforce.
	t.Setenv("VORNIK_MEMORY_FIREWALL_MODE", "enforce")
	if got := c.memoryFirewallMode(); got != memoryfirewall.EnforcementEnforce {
		t.Errorf("env=enforce = %v, want Enforce", got)
	}
}

func TestMemoryFirewallModeForProject_NilPaths(t *testing.T) {
	// nil container / nil registry / empty id all return (Off, false).
	if _, ok := (*Container)(nil).memoryFirewallModeForProject("p"); ok {
		t.Error("nil container must return ok=false")
	}
	c := &Container{} // Registry nil
	if _, ok := c.memoryFirewallModeForProject("p"); ok {
		t.Error("nil registry must return ok=false")
	}
	if _, ok := c.memoryFirewallModeForProject(""); ok {
		t.Error("empty project id must return ok=false")
	}
}

func TestInstinctAutoApplyGetters(t *testing.T) {
	if instinctAutoApplyMinConfidence(nil) != 0 {
		t.Error("nil cfg min_confidence must be 0")
	}
	if len(instinctAutoApplyAllowedClasses(nil)) != 0 {
		t.Error("nil cfg allowed_classes must be empty")
	}
	cfg := &config.Config{}
	cfg.Instinct.Consumers.AutoApply.MinConfidence = 0.7
	cfg.Instinct.Consumers.AutoApply.AllowedErrorClasses = []string{"context_timeout", "parse_invalid_json"}
	if got := instinctAutoApplyMinConfidence(cfg); got != 0.7 {
		t.Errorf("min_confidence = %v, want 0.7", got)
	}
	if got := instinctAutoApplyAllowedClasses(cfg); len(got) != 2 || got[0] != "context_timeout" {
		t.Errorf("allowed_classes = %v, want [context_timeout parse_invalid_json]", got)
	}
}

func TestEmailAttachmentDir(t *testing.T) {
	if got := (*Container)(nil).emailAttachmentDir(); got != "" {
		t.Errorf("nil container = %q, want empty", got)
	}
	// ArtifactsPath wins.
	c := &Container{Config: &config.Config{}}
	c.Config.Storage.ArtifactsPath = "/data/artifacts"
	if got := c.emailAttachmentDir(); got != filepath.Join("/data/artifacts", "email-inbound") {
		t.Errorf("ArtifactsPath = %q", got)
	}
	// No ArtifactsPath → VORNIK_DATA_DIR.
	c2 := &Container{Config: &config.Config{}}
	t.Setenv("VORNIK_DATA_DIR", "/var/lib/vornik")
	if got := c2.emailAttachmentDir(); got != filepath.Join("/var/lib/vornik", "email-attachments") {
		t.Errorf("VORNIK_DATA_DIR = %q", got)
	}
	// Neither → tmp.
	c3 := &Container{Config: &config.Config{}}
	t.Setenv("VORNIK_DATA_DIR", "")
	if got := c3.emailAttachmentDir(); got != filepath.Join(os.TempDir(), "vornik-email-attachments") {
		t.Errorf("tmp default = %q", got)
	}
}

func TestDaemonHolderID(t *testing.T) {
	c := &Container{}
	id := c.daemonHolderID()
	if id == "" || strings.Count(id, ":") != 2 {
		t.Errorf("holder id = %q, want host:pid:nonce", id)
	}
	if again := c.daemonHolderID(); again != id {
		t.Errorf("holder id not cached: %q != %q", again, id)
	}
}

func TestIsGitRepo(t *testing.T) {
	if isGitRepo("") {
		t.Error("empty dir must be false")
	}
	nonGitDir := t.TempDir()
	if isGitRepo(nonGitDir) {
		t.Logf("temp dir %q is inside a git work tree; using root fallback for negative case", nonGitDir)
	} else {
		t.Logf("fresh temp dir %q correctly detected as non-git", nonGitDir)
	}
	if isGitRepo("/") {
		t.Error("root dir without .git must be false")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isGitRepo(dir) {
		t.Error("dir with .git must be true")
	}
	// A nested subdir of a git repo is also inside the work tree.
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if !isGitRepo(sub) {
		t.Error("subdir of a git repo must be true")
	}
}

// fakeSteeringSink records NotifySteeringRequired calls.
type fakeSteeringSink struct{ calls int }

func (f *fakeSteeringSink) NotifySteeringRequired(context.Context, *persistence.Task, string) {
	f.calls++
}

func TestSteeringMux_FansOut(t *testing.T) {
	// nil mux is a no-op (no panic).
	(*steeringMux)(nil).NotifySteeringRequired(context.Background(), nil, "AWAITING_INPUT")

	a, b := &fakeSteeringSink{}, &fakeSteeringSink{}
	mux := &steeringMux{sinks: []executor.SteeringNotifier{a, b}}
	mux.NotifySteeringRequired(context.Background(), &persistence.Task{ID: "t1"}, "AWAITING_INPUT")
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("fan-out: a=%d b=%d, want 1/1", a.calls, b.calls)
	}
}

func TestCombinedSteeringNotifier_NoneWired(t *testing.T) {
	// A bare container has no steering/operator/a2a sinks → nil.
	c := &Container{Logger: zerolog.Nop()}
	if n := c.combinedSteeringNotifier(); n != nil {
		t.Errorf("no sinks wired must yield nil, got %T", n)
	}
}

func TestNewRetentionPreviewAdapter_NilSweeper(t *testing.T) {
	if a := newRetentionPreviewAdapter(nil, config.RetentionConfig{}, "", nil); a != nil {
		t.Error("nil sweeper must yield a nil adapter")
	}
}

func TestResolvePolicy_DefaultsWhenNoRegistry(t *testing.T) {
	defaults := config.RetentionConfig{
		TaskLLMUsageDays: 90, ToolAuditDays: 30, TasksDays: 60,
		ExecutionsDays: 60, ArtifactsDays: 60, TaskMessagesDays: 7, MemoryChunksDays: 14,
	}
	a := newRetentionPreviewAdapter(retention.New(nil, zerolog.Nop()), defaults, "/art/root", nil).(*retentionPreviewAdapter)
	pol := a.resolvePolicy("proj-x")
	if pol.ProjectID != "proj-x" {
		t.Errorf("ProjectID = %q", pol.ProjectID)
	}
	// With no registry, the policy mirrors the daemon defaults verbatim.
	if pol.TaskLLMUsageDays != 90 || pol.TasksDays != 60 || pol.MemoryChunksDays != 14 {
		t.Errorf("policy did not inherit defaults: %+v", pol)
	}
	if pol.ArtifactsRoot != "/art/root" {
		t.Errorf("ArtifactsRoot = %q, want /art/root", pol.ArtifactsRoot)
	}
}

func TestAdaptChannels(t *testing.T) {
	if got := adaptChannels(nil); got != nil {
		t.Errorf("nil channels = %v, want nil", got)
	}
	if got := adaptChannels([]*email.Channel{}); got != nil {
		t.Errorf("empty channels = %v, want nil", got)
	}
	// A nil entry is preserved as a (nil) slot — the loop's nil-skip branch.
	got := adaptChannels([]*email.Channel{nil})
	if len(got) != 1 || got[0] != nil {
		t.Errorf("nil-entry channel: got %v (len %d), want [nil]", got, len(got))
	}
}
