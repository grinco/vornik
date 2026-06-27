// Package telegram: focused tests for the bot's option setters that
// the larger test rigs don't currently exercise (WithIntentJudgeRepository,
// WithMemoryCorrector, WithRescheduler, ActiveChatCount nil-safe).
package telegram

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

// stubIntentJudgeRepo implements persistence.IntentVerdictRepository
// via interface embedding — methods we don't override panic, but the
// option setter never calls them.
type stubIntentJudgeRepo struct {
	persistence.IntentVerdictRepository
}

// stubMemoryCorrector is the smallest dispatcher.MemoryCorrector
// implementation; we only need a typed sentinel for option-wire
// confirmation, not a working corrector.
type stubMemoryCorrector struct{}

func (s *stubMemoryCorrector) RefuteByClaim(_ context.Context, _, _ string, _ int) ([]memory.RefutedChunk, error) {
	return nil, nil
}

func (s *stubMemoryCorrector) InsertCorrection(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}

type stubRescheduler struct{ called bool }

func (s *stubRescheduler) Wake() { s.called = true }

func makeBotForOptions(t *testing.T) *Bot {
	t.Helper()
	chatClient := chat.NewClient("http://nope.invalid", "k", "m")
	bot, err := NewBot(BotConfig{Token: "x"}, chatClient)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	return bot
}

func TestWithIntentJudgeRepository(t *testing.T) {
	bot := makeBotForOptions(t)
	repo := &stubIntentJudgeRepo{}
	opt := WithIntentJudgeRepository(repo, "qwen-classifier")
	opt(bot)
	if bot.intentJudgeRepo != repo {
		t.Errorf("intentJudgeRepo not set")
	}
	if bot.intentJudgeModel != "qwen-classifier" {
		t.Errorf("intentJudgeModel: got %q, want qwen-classifier", bot.intentJudgeModel)
	}
}

func TestWithIntentJudgeRepository_EmptyModel(t *testing.T) {
	bot := makeBotForOptions(t)
	// Empty model is the documented "use chat router default" form.
	opt := WithIntentJudgeRepository(&stubIntentJudgeRepo{}, "")
	opt(bot)
	if bot.intentJudgeModel != "" {
		t.Errorf("empty model: got %q, want empty", bot.intentJudgeModel)
	}
}

func TestWithMemoryCorrector(t *testing.T) {
	bot := makeBotForOptions(t)
	c := &stubMemoryCorrector{}
	opt := WithMemoryCorrector(c)
	opt(bot)
	if bot.memoryCorrector == nil {
		t.Errorf("memoryCorrector not set")
	}
}

func TestWithRescheduler(t *testing.T) {
	bot := makeBotForOptions(t)
	r := &stubRescheduler{}
	opt := WithRescheduler(r)
	opt(bot)
	if bot.rescheduler != r {
		t.Errorf("rescheduler not set")
	}
}

func TestActiveChatCount_NilBot(t *testing.T) {
	// Documented contract: ActiveChatCount must be nil-safe so the
	// dashboard handler can call it without a nil check.
	var b *Bot
	if got := b.ActiveChatCount(); got != 0 {
		t.Errorf("nil bot: got %d, want 0", got)
	}
}

func TestActiveChatCount_TracksConversations(t *testing.T) {
	bot := makeBotForOptions(t)
	if got := bot.ActiveChatCount(); got != 0 {
		t.Errorf("fresh bot: got %d, want 0", got)
	}
	// Inject conversations directly so we don't depend on the full
	// HandleMessage path. chat.Conversation is the shared per-chat
	// state struct the dispatcher reuses across all channels.
	bot.mu.Lock()
	bot.conversations[1] = &chat.Conversation{}
	bot.conversations[2] = &chat.Conversation{}
	bot.mu.Unlock()
	if got := bot.ActiveChatCount(); got != 2 {
		t.Errorf("after seeding: got %d, want 2", got)
	}
}
