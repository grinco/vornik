package service

import (
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/telegram"
)

// TestWireTelegramReceiver_NilBot_NoOp — without a TelegramBot,
// wireTelegramReceiver returns nil and the container stays in a
// degraded but functional state (GitHub-only deployments).
func TestWireTelegramReceiver_NilBot_NoOp(t *testing.T) {
	c := &Container{
		Logger:     zerolog.Nop(),
		ChatClient: chat.NewClient("https://api.example.com", "test-key", "gpt-4"),
		Config:     &config.Config{},
	}
	c.initDispatcher()
	if got := c.wireTelegramReceiver(); got != nil {
		t.Errorf("wireTelegramReceiver with nil TelegramBot = %v, want nil", got)
	}
}

// TestWireTelegramReceiver_NilDispatcher_NoOp — same defensive
// guard for the other dependency. Without a dispatcher there's
// nothing for the receiver to invoke.
func TestWireTelegramReceiver_NilDispatcher_NoOp(t *testing.T) {
	bot, err := telegram.NewBot(telegram.BotConfig{Token: "t"},
		chat.NewClient("https://api.example.com", "test-key", "gpt-4"))
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	c := &Container{
		Logger:      zerolog.Nop(),
		Config:      &config.Config{},
		TelegramBot: bot,
		// Dispatcher intentionally nil.
	}
	if got := c.wireTelegramReceiver(); got != nil {
		t.Errorf("wireTelegramReceiver with nil Dispatcher = %v, want nil", got)
	}
	if bot.Receiver() != nil {
		t.Error("bot.Receiver() should remain nil when wiring skipped")
	}
}

// TestWireTelegramReceiver_AttachesReceiverToBot — happy path:
// with both bot and dispatcher in place, the wiring produces a
// non-nil ChannelReceiver AND attaches it to the bot so
// HandleMessage's non-slash branch routes through the new
// pipeline.
func TestWireTelegramReceiver_AttachesReceiverToBot(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := telegram.NewBot(telegram.BotConfig{Token: "t"}, chatClient)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	c := &Container{
		Logger:      zerolog.Nop(),
		ChatClient:  chatClient,
		Config:      &config.Config{},
		TelegramBot: bot,
	}
	c.initDispatcher()
	if c.Dispatcher == nil {
		t.Fatal("initDispatcher did not produce a Dispatcher")
	}
	// initDispatcher already calls wireTelegramReceiver; verify
	// the receiver landed on the bot.
	if bot.Receiver() == nil {
		t.Fatal("bot.Receiver() is nil — wiring did not attach")
	}
}
