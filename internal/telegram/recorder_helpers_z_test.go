// Helper carried over from the coverage-follow-up Z merge — the
// original dispatch_and_notify_test.go was dropped during merge
// because its test functions collided with notify_effective_cost_test.go,
// but other Z test files (handle_message_commands_test.go,
// route_reply_extra_test.go, etc.) still rely on this constructor.
package telegram

import (
	"testing"

	"vornik.io/vornik/internal/chat"
)

// newBotWithRecorder builds a Bot whose outgoing /sendMessage hits
// telegramRecorder (defined in notify_fill_test.go) so callers can
// introspect what would have been sent without touching the real
// Telegram API.
func newBotWithRecorder(t *testing.T, cfg BotConfig, opts ...BotOption) (*Bot, *telegramRecorder) {
	t.Helper()
	rec := newTelegramRecorder(t)
	chatClient := chat.NewClient("https://example.com", "k", "m")
	allOpts := append([]BotOption{WithHTTPClient(rec.server.Client())}, opts...)
	bot, err := NewBot(cfg, chatClient, allOpts...)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = rec.server.URL
	return bot, rec
}

// newFakeAutonomyMgr is the Z-side convenience constructor; main's
// fakeAutonomyMgr (defined in handlers_autopilot_test.go) lazily
// inits the `enabled` map inside EnableProject, so an explicit
// constructor is optional — but Z's tests call this name.
func newFakeAutonomyMgr() *fakeAutonomyMgr {
	return &fakeAutonomyMgr{enabled: map[string]bool{}}
}
