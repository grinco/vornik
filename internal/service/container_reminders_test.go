package service

import (
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/telegram"
)

// TestContainerChannelResolver_ResolvesTelegramWhenBotWired is the
// regression sentinel for the silent-cast bug that left every
// Telegram reminder stuck in status=firing with last_error
// "channel telegram not configured". A bare any(bot).(Channel)
// assertion returned ok=false because *telegram.Bot is not the
// Channel adapter; *telegram.Channel is. Always wrap with
// telegram.NewChannel(bot).
func TestContainerChannelResolver_ResolvesTelegramWhenBotWired(t *testing.T) {
	bot, err := telegram.NewBot(
		telegram.BotConfig{Token: "t"},
		chat.NewClient("https://api.example.com", "test-key", "gpt-4"),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	c := &Container{
		Logger:      zerolog.Nop(),
		Config:      &config.Config{},
		TelegramBot: bot,
	}
	cr := &containerChannelResolver{c: c}
	got := cr.ResolveChannel("telegram")
	if got == nil {
		t.Fatal("ResolveChannel(\"telegram\") returned nil; reminders runner would record \"channel not configured\"")
	}
	if got.Name() != "telegram" {
		t.Errorf("Channel.Name() = %q, want \"telegram\"", got.Name())
	}
}

// TestContainerChannelResolver_ReturnsNilWhenBotMissing keeps the
// degraded-deployment behaviour: no bot wired → resolver returns
// nil → runner marks the row errored rather than panicking.
func TestContainerChannelResolver_ReturnsNilWhenBotMissing(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	cr := &containerChannelResolver{c: c}
	if got := cr.ResolveChannel("telegram"); got != nil {
		t.Errorf("ResolveChannel(\"telegram\") = %v, want nil with no bot wired", got)
	}
}

// TestContainerChannelResolver_UnknownChannelReturnsNil — the
// resolver is permissive: unknown channel names yield nil rather
// than a panic, so a typo in a stored reminder row doesn't crash
// the runner.
func TestContainerChannelResolver_UnknownChannelReturnsNil(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	cr := &containerChannelResolver{c: c}
	if got := cr.ResolveChannel("smoke-signal"); got != nil {
		t.Errorf("ResolveChannel(\"smoke-signal\") = %v, want nil", got)
	}
}

// TestContainerChannelResolver_NilReceiverSafe — defensive: a nil
// resolver or nil container never panics; callers can hold a
// pointer that hasn't been initialised yet.
func TestContainerChannelResolver_NilReceiverSafe(t *testing.T) {
	var cr *containerChannelResolver
	if got := cr.ResolveChannel("telegram"); got != nil {
		t.Errorf("nil receiver should return nil, got %v", got)
	}
	cr2 := &containerChannelResolver{}
	if got := cr2.ResolveChannel("telegram"); got != nil {
		t.Errorf("nil container should return nil, got %v", got)
	}
}

// TestInitReminders_DisabledOnNonWorker: the reminders heartbeat is a
// leader-elected background worker. On a ui/webhook node it must not start —
// otherwise its lease_due poll runs ungated (the webhook node's elector is
// nil), which on a SQLite backend logs an error every 30s (incident
// 2026-06-12).
func TestInitReminders_DisabledOnNonWorker(t *testing.T) {
	cfg := &config.Config{
		Node:     config.NodeConfig{Profile: "webhook"}, // RunWorkers=false
		Database: config.DatabaseConfig{Driver: "postgres"},
	}
	c := &Container{Config: cfg, Logger: zerolog.Nop(), repos: &storage.Repositories{}}
	if r := c.initReminders(); r != nil {
		t.Error("reminders runner must be nil on a non-worker node")
	}
}

// TestInitReminders_DisabledOnSqlite: reminders require Postgres — the SQLite
// repository is an explicit "unsupported" stub whose every method errors. The
// runner must not start on SQLite even on a worker node, rather than polling a
// stub repo that fails on every tick.
func TestInitReminders_DisabledOnSqlite(t *testing.T) {
	cfg := &config.Config{
		Node:     config.NodeConfig{Profile: "worker"}, // RunWorkers=true
		Database: config.DatabaseConfig{Driver: "sqlite"},
	}
	c := &Container{Config: cfg, Logger: zerolog.Nop(), repos: &storage.Repositories{}}
	if r := c.initReminders(); r != nil {
		t.Error("reminders runner must be nil on a sqlite backend (Postgres required)")
	}
}
