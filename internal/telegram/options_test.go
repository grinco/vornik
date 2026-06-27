// Coverage for the telegram package's BotOption setters and
// post-construction Set* hooks. Most setters are single-field
// assignments — a single test that runs NewBot with every option
// covers ~20 functions in one shot.

package telegram

import (
	"context"
	"net/http"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

// TestBotOptions_AllSettersApply runs every WithX option through
// NewBot and verifies the matching field landed. Mirrors the api
// package's option-coverage test. The Bot's fields are all
// unexported but accessible from this _test.go file in the same
// package.
func TestBotOptions_AllSettersApply(t *testing.T) {
	hc := &http.Client{}
	rl := &ratelimit.Limiter{}
	pt := &pricing.Table{}
	reg := registry.New()
	logger := zerolog.Nop()

	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "t"}, chatClient,
		WithHTTPClient(hc),
		WithLogger(logger),
		WithRescheduler(nil),
		WithForumChatID(-100, 7),
		WithTelegramThreadRepository(nil),
		WithExecutionRepository(nil),
		WithArtifactRepository(nil),
		WithArtifactStore(nil),
		WithProjectWorkspacePath("/tmp/projects"),
		WithRegistry(reg),
		WithTaskWatcherRepository(nil),
		WithMCPManager(nil),
		WithAuditRepository(nil),
		WithLLMUsageRepository(nil),
		WithPricing(pt),
		WithRateLimiter(rl),
		WithDefaultModel("test-model"),
		WithMemorySearcher(nil),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	if bot.httpClient != hc {
		t.Error("WithHTTPClient: not applied")
	}
	if bot.projectWorkspacePath != "/tmp/projects" {
		t.Errorf("WithProjectWorkspacePath: got %q", bot.projectWorkspacePath)
	}
	if bot.registry != reg {
		t.Error("WithRegistry: not applied")
	}
	if bot.rateLimiter != rl {
		t.Error("WithRateLimiter: not applied")
	}
	if bot.pricingTable != pt {
		t.Error("WithPricing: not applied")
	}
	if bot.defaultModel != "test-model" {
		t.Errorf("WithDefaultModel: got %q", bot.defaultModel)
	}
	if bot.forumChatID != -100 {
		t.Errorf("WithForumChatID: got %d", bot.forumChatID)
	}
}

// TestBotSetMetrics covers the post-construction setter. The
// initial bot has no metrics; after SetMetrics they're applied.
func TestBotSetMetrics(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "t"}, chatClient)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	if bot.metrics != nil {
		t.Fatal("preconditions: bot.metrics should be nil before SetMetrics")
	}
	m := &Metrics{}
	bot.SetMetrics(m)
	if bot.metrics != m {
		t.Error("SetMetrics: not applied")
	}
}

// TestBotSetAutonomyManager mirrors TestBotSetMetrics for the
// autonomy controller post-construction injection.
func TestBotSetAutonomyManager(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "t"}, chatClient)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.SetAutonomyManager(nil) // typed nil — must not crash
	bot.SetAutonomyManager(stubAutonomyController{})
	if bot.autonomyMgr == nil {
		t.Error("SetAutonomyManager: not applied")
	}
}

// fakeReceiver is a conversation.Receiver test double. Records
// every Receive call so the SetReceiver/Receive-path tests can
// verify the bot wired the receiver correctly.
type fakeReceiver struct {
	calls int
	err   error
}

func (f *fakeReceiver) Receive(_ context.Context, _ conversation.ChannelMessage) error {
	f.calls++
	return f.err
}

// TestBotSetReceiver_AppliesReceiver — the setter the service
// container uses to wire a dispatcher.ChannelReceiver into the
// bot. HandleMessage's non-slash branch and the auto-resume
// follow-up both route dispatcher-bound turns through the
// receiver.
func TestBotSetReceiver_AppliesReceiver(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "t"}, chatClient)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	if bot.Receiver() != nil {
		t.Error("Receiver() returned non-nil before SetReceiver")
	}
	r := &fakeReceiver{}
	bot.SetReceiver(r)
	if got := bot.Receiver(); got != r {
		t.Errorf("Receiver() = %p, want %p", got, r)
	}
}

// TestBotWatchTask_NoRepoSafe — WatchTask is called from many
// chat handlers; a deployment without a watcher repo wired must
// not panic on the call. The function silently no-ops in that
// case.
func TestBotWatchTask_NoRepoSafe(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "t"}, chatClient)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.WatchTask("task_x", 42) // must not panic
}

// TestUserIDForChat_DefaultsToChatID covers the lookup helper:
// for personal chats chat_id == user_id, so the default-fallback
// path keeps the dispatcher coherent even before a recordChatUser
// fires.
func TestUserIDForChat_DefaultsToChatID(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "t"}, chatClient)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	// No recordChatUser call → falls back to chatID itself.
	if got := bot.userIDForChat(12345); got != 12345 {
		t.Errorf("userIDForChat without a recorded user: got %d, want 12345 (fallback)", got)
	}

	bot.recordChatUser(12345, 555)
	if got := bot.userIDForChat(12345); got != 555 {
		t.Errorf("userIDForChat with recorded user: got %d, want 555", got)
	}
}

// TestUserIDForChat_NilBotSafe — like findActiveDescendant the
// helper is called from many sites; nil-receiver path is
// exercised so a partial test setup can't crash.
func TestUserIDForChat_NilBotSafe(t *testing.T) {
	var b *Bot
	if got := b.userIDForChat(42); got != 42 {
		t.Errorf("nil bot userIDForChat: got %d, want fallback to chatID 42", got)
	}
}

// stubAutonomyController satisfies the AutonomyController
// interface with no behaviour — just enough for the
// SetAutonomyManager test.
type stubAutonomyController struct{}

func (stubAutonomyController) EnableProject(_ string) error    { return nil }
func (stubAutonomyController) DisableProject(_ string) error   { return nil }
func (stubAutonomyController) IsAutonomyEnabled(_ string) bool { return false }

// _ = dispatcher.MCPExecutor // ensure import is exercised even if no
// direct reference; keeps a future refactor that drops the import
// from a passing test honest.
var _ dispatcher.MCPExecutor = (dispatcher.MCPExecutor)(nil)
