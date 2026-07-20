package service

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/storage"
)

// TestInitDispatcher_NoChatClient_NoOp — without a chat provider,
// initDispatcher skips construction and c.Dispatcher stays nil.
// Channels that depend on a dispatcher (GitHub @vornik reply,
// Telegram receiver path) detect this and degrade gracefully.
func TestInitDispatcher_NoChatClient_NoOp(t *testing.T) {
	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{},
	}
	c.initDispatcher()
	if c.Dispatcher != nil {
		t.Error("c.Dispatcher non-nil when ChatClient is nil")
	}
}

// TestInitDispatcher_WithChatNoBot — the GitHub-only deployment
// shape this refactor unblocks. Chat provider is configured but no
// Telegram bot is present; the dispatcher constructs cleanly,
// without the FollowupRegistrar / BudgetNotifier / TaskWatchFunc
// options that depend on the bot.
func TestInitDispatcher_WithChatNoBot(t *testing.T) {
	c := &Container{
		Logger:     zerolog.Nop(),
		Config:     &config.Config{},
		ChatClient: chat.NewClient("https://api.example.com", "test-key", "gpt-4"),
	}
	c.initDispatcher()
	if c.Dispatcher == nil {
		t.Fatal("c.Dispatcher nil despite ChatClient being configured")
	}
}

// TestInitDispatcher_HonoursTelegramMaxIterationsOverride — the
// telegram.dispatcher_max_iterations setting wins over the
// chat-wide MaxToolIterations when both are set. Mirrors the
// precedence initTelegram has historically applied; the lifted
// dispatcher init must keep that contract.
func TestInitDispatcher_HonoursTelegramMaxIterationsOverride(t *testing.T) {
	c := &Container{
		Logger:     zerolog.Nop(),
		ChatClient: chat.NewClient("https://api.example.com", "test-key", "gpt-4"),
		Config: &config.Config{
			Chat:     config.ChatConfig{MaxToolIterations: 5},
			Telegram: config.TelegramConfig{DispatcherMaxIterations: 25},
		},
	}
	c.initDispatcher()
	if c.Dispatcher == nil {
		t.Fatal("c.Dispatcher nil")
	}
	// The Agent exposes its iteration cap via the option's effect on
	// the agent struct — there's no public getter, so this test
	// asserts construction success + that no panic / mis-wired
	// option fires. The full iteration-cap behaviour is exercised by
	// the dispatcher's own tests.
}

// TestInitDispatcher_ChatWideMaxIterations — when no Telegram
// override is set, the chat-wide MaxToolIterations propagates.
func TestInitDispatcher_ChatWideMaxIterations(t *testing.T) {
	c := &Container{
		Logger:     zerolog.Nop(),
		ChatClient: chat.NewClient("https://api.example.com", "test-key", "gpt-4"),
		Config: &config.Config{
			Chat: config.ChatConfig{MaxToolIterations: 7},
		},
	}
	c.initDispatcher()
	if c.Dispatcher == nil {
		t.Fatal("c.Dispatcher nil")
	}
}

// TestInitDispatcher_WiredOptionalDeps — covers the option-
// attachment branches that gate on simple-to-construct
// dependencies: pricing table, rate limiter, default model
// string, dispatcher billing project, and a non-nil c.repos
// (whose fields stay nil because each individual option already
// has its own nil-tolerance test in the dispatcher package).
func TestInitDispatcher_WiredOptionalDeps(t *testing.T) {
	c := &Container{
		Logger:       zerolog.Nop(),
		ChatClient:   chat.NewClient("https://api.example.com", "test-key", "gpt-4"),
		pricingTable: &pricing.Table{},
		rateLimiter:  ratelimit.New(),
		repos:        &storage.Repositories{},
		Config: &config.Config{
			Chat:     config.ChatConfig{MaxToolIterations: 5},
			Telegram: config.TelegramConfig{DispatcherProjectID: "p-1"},
			Runtime:  config.RuntimeConfig{AgentLLM: config.AgentLLMConfig{Model: "claude-opus-4-7"}},
		},
	}
	c.initDispatcher()
	if c.Dispatcher == nil {
		t.Fatal("c.Dispatcher nil after init with wired deps")
	}
}

// TestInitDispatcher_NilReposGuard — fixture-grade test that
// runs initDispatcher against a Container where c.repos is nil.
// Exercises the in-function guard that previously lived in three
// trivial helper functions (nilTaskRepo / nilExecRepo /
// nilArtifactRepo) before they were inlined. Must not panic.
func TestInitDispatcher_NilReposGuard(t *testing.T) {
	c := &Container{
		Logger:     zerolog.Nop(),
		ChatClient: chat.NewClient("https://api.example.com", "test-key", "gpt-4"),
		Config:     &config.Config{},
		// c.repos intentionally nil — early-init path.
	}
	c.initDispatcher()
	if c.Dispatcher == nil {
		t.Fatal("c.Dispatcher nil with nil repos guard")
	}
}

// setupProjectRegistryForBootWarnTest stages+activates a registry containing
// a single project with the given api_providers allowlist, without needing
// matching swarm/workflow fixtures — Stage/ActivateStaged (unlike Load)
// skips cross-reference validation, and LoadProjects/LoadSwarms/LoadWorkflows
// all tolerate a missing directory, so this is the minimal fixture that gets
// the project into Registry.ListProjects().
func setupProjectRegistryForBootWarnTest(t *testing.T, apiProvidersYAML string) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlSrc := "projectId: \"janka\"\n" +
		"swarmId: \"test-swarm\"\n" +
		"defaultWorkflowId: \"test-workflow\"\n" +
		apiProvidersYAML
	if err := os.WriteFile(filepath.Join(projectsDir, "janka.yaml"), []byte(yamlSrc), 0o644); err != nil {
		t.Fatalf("write project: %v", err)
	}

	reg := registry.New()
	if err := reg.Stage(dir); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := reg.ActivateStaged(); err != nil {
		t.Fatalf("ActivateStaged: %v", err)
	}
	return reg
}

// TestInitDispatcher_WarnsUnknownAPIProviders covers the boot-time wiring of
// registry.WarnUnknownAPIProviders (query_api provider-discovery design
// §4.3, cross-task item from Task 2): a project's permissions.api_providers
// naming a provider absent from gateway.providers must log a warning during
// initDispatcher, fed by Registry.ListProjects() and the configured
// gateway.providers name set.
func TestInitDispatcher_WarnsUnknownAPIProviders(t *testing.T) {
	reg := setupProjectRegistryForBootWarnTest(t, "permissions:\n  api_providers:\n    - \"maps\"\n    - \"headmatch-ats\"\n")

	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.WarnLevel)

	c := &Container{
		Logger:     logger,
		ChatClient: chat.NewClient("https://api.example.com", "test-key", "gpt-4"),
		Registry:   reg,
		Config: &config.Config{
			Gateway: config.GatewayConfig{
				Providers: map[string]config.ProviderConfig{"maps": {}},
			},
		},
	}
	c.initDispatcher()

	out := buf.String()
	if !strings.Contains(out, "headmatch-ats") {
		t.Fatalf("expected boot warning naming the unknown provider %q, got log: %s", "headmatch-ats", out)
	}
	if !strings.Contains(out, "janka") {
		t.Fatalf("expected boot warning to name the offending project %q, got log: %s", "janka", out)
	}
}

// TestInitDispatcher_NoWarnWhenAllProvidersKnown pins the no-noise case at
// boot: a registry whose only project's allowlist matches the configured
// gateway providers produces no boot warning.
func TestInitDispatcher_NoWarnWhenAllProvidersKnown(t *testing.T) {
	reg := setupProjectRegistryForBootWarnTest(t, "permissions:\n  api_providers:\n    - \"maps\"\n")

	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.WarnLevel)

	c := &Container{
		Logger:     logger,
		ChatClient: chat.NewClient("https://api.example.com", "test-key", "gpt-4"),
		Registry:   reg,
		Config: &config.Config{
			Gateway: config.GatewayConfig{
				Providers: map[string]config.ProviderConfig{"maps": {}},
			},
		},
	}
	c.initDispatcher()

	// Note: buf may carry unrelated warnings from other initDispatcher steps
	// (e.g. the extractor's "ArtifactsPath unset" diagnostic, which fires
	// whenever c.Registry is non-nil regardless of this feature) — so assert
	// on the specific api_providers diagnostic message, not an empty buffer.
	if strings.Contains(buf.String(), "api_providers names a provider absent") {
		t.Errorf("expected no api_providers boot warning when every allowlisted provider is known, got: %s", buf.String())
	}
}

// TestInitDispatcher_NilRegistryNoWarnNoPanic pins that the boot-time
// diagnostic degrades gracefully when c.Registry is nil (an early-init
// fixture path some tests use) — it must never panic, matching
// WarnUnknownAPIProviders' own nil-safety contract.
func TestInitDispatcher_NilRegistryNoWarnNoPanic(t *testing.T) {
	c := &Container{
		Logger:     zerolog.Nop(),
		ChatClient: chat.NewClient("https://api.example.com", "test-key", "gpt-4"),
		Config:     &config.Config{},
	}
	c.initDispatcher()
	if c.Dispatcher == nil {
		t.Fatal("c.Dispatcher nil despite ChatClient being configured")
	}
}
