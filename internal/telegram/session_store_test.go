// Tests for telegramSessionStore — the dispatcher.SessionStore
// implementation that backs slice-2 of the ConversationChannel
// migration. The store wraps an existing *Bot so the legacy
// in-memory conversations + activeProjects maps stay the
// authoritative state during the migration window.
//
// Pure unit tests — no network, no DB. Bot is constructed via
// newBareTestBot (defined in middleware_test.go) so each case
// gets fresh state.

package telegram

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/registry"
)

// msg returns a minimal valid ChannelMessage with the given
// chatID and userID encoded as decimal strings.
func msg(chatID, userID int64, text string) conversation.ChannelMessage {
	return conversation.ChannelMessage{
		Source:    "telegram",
		SessionID: strconv.FormatInt(chatID, 10),
		SpeakerID: strconv.FormatInt(userID, 10),
		Text:      text,
	}
}

// TestTelegramSessionStore_Load_BadSessionIDErrors — a non-numeric
// SessionID can't be a Telegram chat_id; surface the parsing
// error rather than silently coercing to 0 (which would route
// every malformed inbound to chat 0).
func TestTelegramSessionStore_Load_BadSessionIDErrors(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	store := NewSessionStore(bot, nil)
	_, err := store.Load(context.Background(), conversation.ChannelMessage{
		SessionID: "not-a-number",
		SpeakerID: "42",
	})
	if err == nil {
		t.Fatal("expected error on non-numeric SessionID")
	}
}

// TestTelegramSessionStore_Load_BadSpeakerIDErrors — same defence
// applied to the SpeakerID.
func TestTelegramSessionStore_Load_BadSpeakerIDErrors(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	store := NewSessionStore(bot, nil)
	_, err := store.Load(context.Background(), conversation.ChannelMessage{
		SessionID: "100",
		SpeakerID: "not-a-number",
	})
	if err == nil {
		t.Fatal("expected error on non-numeric SpeakerID")
	}
}

// TestTelegramSessionStore_Load_UnknownSpeaker — when an
// allowlist is configured and the speaker is not on it, Load
// returns conversation.ErrSpeakerUnknown without burning LLM
// budget. Mirrors the GitHub channel's contract.
func TestTelegramSessionStore_Load_UnknownSpeaker(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{
		Token: "t",
		AllowedUsers: map[int64]UserAccess{
			42: {Allowed: true},
		},
	})
	store := NewSessionStore(bot, nil)
	_, err := store.Load(context.Background(), msg(100, 999, "hi"))
	if !errors.Is(err, conversation.ErrSpeakerUnknown) {
		t.Fatalf("expected ErrSpeakerUnknown, got %v", err)
	}
}

// TestTelegramSessionStore_Load_AllowedSpeakerPasses — speaker
// on the allowlist gets a populated Session back; ChatID matches
// the parsed SessionID.
func TestTelegramSessionStore_Load_AllowedSpeakerPasses(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{
		Token: "t",
		AllowedUsers: map[int64]UserAccess{
			42: {Allowed: true},
		},
	})
	store := NewSessionStore(bot, nil)
	sess, err := store.Load(context.Background(), msg(100, 42, "hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess.ChatID != 100 {
		t.Errorf("ChatID = %d, want 100", sess.ChatID)
	}
}

// TestTelegramSessionStore_Load_NoAllowlistDevMode — when no
// allowlist is configured every speaker passes through (dev
// mode), matching Bot.IsAllowed's semantics. Belt-and-suspenders
// against accidental gating in dev deployments.
func TestTelegramSessionStore_Load_NoAllowlistDevMode(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	store := NewSessionStore(bot, nil)
	_, err := store.Load(context.Background(), msg(100, 42, "hi"))
	if err != nil {
		t.Fatalf("dev mode load failed: %v", err)
	}
}

// TestTelegramSessionStore_Load_ActiveProjectPropagates — the
// chat's pinned project (set via /project on the Bot) lands on
// Session.ActiveProject so the dispatcher's system prompt
// reflects the operator's pick.
func TestTelegramSessionStore_Load_ActiveProjectPropagates(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	bot.setActiveProject(100, "demo-project")
	store := NewSessionStore(bot, nil)
	sess, err := store.Load(context.Background(), msg(100, 42, "hi"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if sess.ActiveProject != "demo-project" {
		t.Errorf("ActiveProject = %q, want demo-project", sess.ActiveProject)
	}
}

// TestTelegramSessionStore_Load_HistoryFromConversation — Load
// reads the chat's existing conversation history into
// Session.History so multi-turn context survives the
// dispatcher hop.
func TestTelegramSessionStore_Load_HistoryFromConversation(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	conv := bot.getConversation(100)
	conv.AddMessage(chat.Message{Role: "user", Content: "first"})
	conv.AddMessage(chat.Message{Role: "assistant", Content: "reply"})

	store := NewSessionStore(bot, nil)
	sess, err := store.Load(context.Background(), msg(100, 42, "second"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(sess.History) != 2 {
		t.Fatalf("History len = %d, want 2", len(sess.History))
	}
	if sess.History[0].Content != "first" || sess.History[1].Content != "reply" {
		t.Errorf("unexpected History contents: %+v", sess.History)
	}
}

// TestTelegramSessionStore_Load_AllowedProjectsForUser — when an
// allowlist with project scoping is configured, Session.AllowedProjects
// reflects the user's scope so the dispatcher's tool surface gets
// gated correctly.
func TestTelegramSessionStore_Load_AllowedProjectsForUser(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{
		Token: "t",
		AllowedUsers: map[int64]UserAccess{
			42: {Allowed: true, Projects: []string{"alpha", "beta"}},
		},
	})
	store := NewSessionStore(bot, nil)
	sess, err := store.Load(context.Background(), msg(100, 42, "hi"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(sess.AllowedProjects) != 2 {
		t.Fatalf("AllowedProjects len = %d, want 2: %v", len(sess.AllowedProjects), sess.AllowedProjects)
	}
}

// TestTelegramSessionStore_Load_BotIsFileSender — Session.FileSender
// is the bot itself so the dispatcher's send_artifact tool can
// reach back into Telegram. Without it, attachments fail at the
// tool layer with no clear signal.
func TestTelegramSessionStore_Load_BotIsFileSender(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	store := NewSessionStore(bot, nil)
	sess, err := store.Load(context.Background(), msg(100, 42, "hi"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if sess.FileSender == nil {
		t.Fatal("FileSender unset; dispatcher's send_artifact would silently noop")
	}
}

// TestTelegramSessionStore_Load_ContextTierEstimated — Session
// carries the context-budget tier so the dispatcher can flip
// tool deferral on at DEGRADING / POOR. The estimate uses the
// chat's existing conversation token count + the bot's
// MaxHistoryTokens budget.
func TestTelegramSessionStore_Load_ContextTierEstimated(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{
		Token:            "t",
		MaxHistoryTokens: 1000,
	})
	conv := bot.getConversation(100)
	// chars/4 estimator — push tokens above the DEGRADING threshold.
	conv.AddMessage(chat.Message{Role: "user", Content: string(make([]byte, 4000))})

	store := NewSessionStore(bot, nil)
	sess, err := store.Load(context.Background(), msg(100, 42, "hi"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if sess.ContextTier != chat.TierPoor {
		t.Errorf("ContextTier = %v, want POOR (1000 tokens ≥ 90%% of 1000 budget)", sess.ContextTier)
	}
}

// TestTelegramSessionStore_Load_ContextTierIncludesInboundTurn
// preserves the legacy path's behaviour: the current Telegram
// message participates in the context-tier estimate before the
// dispatcher decides whether to defer tool loading.
func TestTelegramSessionStore_Load_ContextTierIncludesInboundTurn(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{
		Token:            "t",
		MaxHistoryTokens: 1000,
	})

	store := NewSessionStore(bot, nil)
	sess, err := store.Load(context.Background(), msg(100, 42, string(make([]byte, 4000))))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if sess.ContextTier != chat.TierPoor {
		t.Errorf("ContextTier = %v, want POOR from current message tokens", sess.ContextTier)
	}
}

// TestTelegramSessionStore_Append_SetsActiveProject — when the
// dispatcher's reply carries a NewProject (the model selected
// switch_project mid-turn) the store flips the chat's pin so the
// next turn lands in the new project's context.
func TestTelegramSessionStore_Append_SetsActiveProject(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	store := NewSessionStore(bot, nil)
	err := store.Append(context.Background(), msg(100, 42, ""), dispatcher.Result{
		NewProject: "switched-to",
		Messages: []chat.Message{
			{Role: "user", Content: "x"},
			{Role: "assistant", Content: "ok"},
		},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if got := bot.getActiveProject(100); got != "switched-to" {
		t.Errorf("active project = %q, want switched-to", got)
	}
}

// TestTelegramSessionStore_Append_ReplacesConversation — the
// dispatcher returns the full post-turn message slice; the
// store replaces the chat's conversation with it so the next
// Load sees the up-to-date history (matches the GitHub store's
// contract).
func TestTelegramSessionStore_Append_ReplacesConversation(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	// Pre-seed something the Append should overwrite.
	bot.getConversation(100).AddMessage(chat.Message{Role: "user", Content: "old"})

	store := NewSessionStore(bot, nil)
	err := store.Append(context.Background(), msg(100, 42, ""), dispatcher.Result{
		Messages: []chat.Message{
			{Role: "user", Content: "new"},
			{Role: "assistant", Content: "reply"},
		},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := bot.getConversation(100).GetMessages()
	if len(got) != 2 || got[0].Content != "new" || got[1].Content != "reply" {
		t.Errorf("conversation = %+v, want new + reply", got)
	}
}

// TestTelegramSessionStore_Append_EmptyMessagesIsNoop — a
// dispatcher error or early-return path may produce an empty
// Messages slice; the store must NOT wipe the chat's history
// in that case (mirrors githubSessionStore's defensive guard).
func TestTelegramSessionStore_Append_EmptyMessagesIsNoop(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	bot.getConversation(100).AddMessage(chat.Message{Role: "user", Content: "preserved"})

	store := NewSessionStore(bot, nil)
	err := store.Append(context.Background(), msg(100, 42, ""), dispatcher.Result{})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := bot.getConversation(100).GetMessages()
	if len(got) != 1 || got[0].Content != "preserved" {
		t.Errorf("history wiped: %+v", got)
	}
}

// TestTelegramSessionStore_Load_LeadPromptResolvedFromRegistry —
// when a registry is supplied and the active project has a lead
// role with a system prompt configured, Session.LeadSystemPrompt
// is the assembled prompt (project + swarm + role text). Without
// a registry the field stays empty (no harm — dispatcher renders
// its default prompt).
func TestTelegramSessionStore_Load_LeadPromptResolvedFromRegistry(t *testing.T) {
	reg := registry.New()
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	bot.registry = reg
	bot.setActiveProject(100, "any-project")

	store := NewSessionStore(bot, reg)
	// Even with a registry but no projects, we should get a clean
	// load (no error) with an empty LeadSystemPrompt — the dispatcher
	// falls back to its default prompt.
	sess, err := store.Load(context.Background(), msg(100, 42, "hi"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if sess.LeadSystemPrompt != "" {
		t.Errorf("LeadSystemPrompt = %q, want empty (no project in registry)", sess.LeadSystemPrompt)
	}
}

// Compile-time guard: the store satisfies the dispatcher contract.
var _ dispatcher.SessionStore = (*SessionStore)(nil)
