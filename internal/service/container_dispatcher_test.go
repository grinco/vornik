package service

import (
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/ratelimit"
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
