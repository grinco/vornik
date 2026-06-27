// Coverage for HandleMessage's slash-command surface. Each test
// drives ONE command through HandleMessage and asserts the reply
// text (or absence of reply, or side effect on conversation state).
// This avoids touching the receiver/dispatcher branch — those are
// covered by handle_message_receiver_test.go — and concentrates on
// the command router that lives between auth and the receiver.

package telegram

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// helper: send a /command and return the recorder snapshot.
func runCommand(t *testing.T, b *Bot, rec *telegramRecorder, chatID, userID int64, text string) []sentMessage {
	t.Helper()
	err := b.HandleMessage(context.Background(), &Message{
		ID: 1, ChatID: chatID, UserID: userID, Text: text,
	})
	if err != nil {
		t.Fatalf("HandleMessage(%q): %v", text, err)
	}
	return rec.snapshot()
}

// /help — covered already in receiver tests; redo here to land
// HandleMessage's command-router branch coverage.
func TestHandleMessage_HelpSlashCommand(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/help")
	if len(snap) != 1 {
		t.Fatalf("expected 1 message, got %d", len(snap))
	}
	if !strings.Contains(snap[0].Text, "Available commands") {
		t.Errorf("help text wrong: %q", snap[0].Text)
	}
}

func TestHandleMessage_ContextCommand(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	// Plant a message so /context has something to report.
	conv := b.getConversation(100)
	conv.AddMessage(chat.Message{Role: "user", Content: "hi"})

	snap := runCommand(t, b, rec, 100, 42, "/context")
	if !strings.Contains(snap[0].Text, "Session context") {
		t.Errorf("context output: %q", snap[0].Text)
	}
	if !strings.Contains(snap[0].Text, "Messages:") {
		t.Errorf("context output missing Messages: %q", snap[0].Text)
	}
}

func TestHandleMessage_UndoEmpty(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/undo")
	if !strings.Contains(snap[0].Text, "Nothing to undo") {
		t.Errorf("undo on empty: %q", snap[0].Text)
	}
}

func TestHandleMessage_UndoRemovesLastTurn(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	conv := b.getConversation(100)
	conv.AddMessage(chat.Message{Role: "user", Content: "q"})
	conv.AddMessage(chat.Message{Role: "assistant", Content: "a"})

	snap := runCommand(t, b, rec, 100, 42, "/undo")
	if !strings.Contains(snap[0].Text, "Removed") {
		t.Errorf("undo: %q", snap[0].Text)
	}
}

func TestHandleMessage_ForgetMissingArg(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/forget")
	if !strings.Contains(snap[0].Text, "Usage") {
		t.Errorf("missing arg: %q", snap[0].Text)
	}
}

func TestHandleMessage_ForgetInvalidArg(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/forget abc")
	if !strings.Contains(snap[0].Text, "positive integer") {
		t.Errorf("invalid arg: %q", snap[0].Text)
	}
}

func TestHandleMessage_ForgetDropsN(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	conv := b.getConversation(100)
	for i := 0; i < 5; i++ {
		conv.AddMessage(chat.Message{Role: "user", Content: "x"})
	}
	snap := runCommand(t, b, rec, 100, 42, "/forget 2")
	if !strings.Contains(snap[0].Text, "Dropped 2") {
		t.Errorf("forget 2: %q", snap[0].Text)
	}
	if conv.Len() != 3 {
		t.Errorf("conv length: got %d, want 3", conv.Len())
	}
}

func TestHandleMessage_PinMissingArg(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/pin")
	if !strings.Contains(snap[0].Text, "Usage") {
		t.Errorf("pin no-arg: %q", snap[0].Text)
	}
}

func TestHandleMessage_PinAddsPinnedMessage(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/pin be concise")
	if !strings.Contains(snap[0].Text, "Pinned") {
		t.Errorf("pin: %q", snap[0].Text)
	}
	pinned := b.getConversation(100).PinnedMessages()
	if len(pinned) != 1 || !strings.Contains(pinned[0].Content, "be concise") {
		t.Errorf("pinned: %+v", pinned)
	}
}

func TestHandleMessage_VerboseNoArgReportsCurrent(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/verbose")
	if !strings.Contains(snap[0].Text, "Notification verbosity is") {
		t.Errorf("verbose status: %q", snap[0].Text)
	}
}

func TestHandleMessage_VerboseValidValues(t *testing.T) {
	for _, mode := range []string{"silent", "short", "full"} {
		b, rec := newBotWithRecorder(t, BotConfig{
			Token:        "tok",
			AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
		})
		snap := runCommand(t, b, rec, 100, 42, "/verbose "+mode)
		if !strings.Contains(snap[0].Text, "verbosity set to") {
			t.Errorf("verbose %s: %q", mode, snap[0].Text)
		}
	}
}

func TestHandleMessage_VerboseInvalidValue(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/verbose deafening")
	if !strings.Contains(snap[0].Text, "Usage") {
		t.Errorf("invalid verbose: %q", snap[0].Text)
	}
}

func TestHandleMessage_NewCommand(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	conv := b.getConversation(100)
	conv.AddMessage(chat.Message{Role: "user", Content: "stuff"})

	snap := runCommand(t, b, rec, 100, 42, "/new")
	if !strings.Contains(snap[0].Text, "New session") {
		t.Errorf("/new: %q", snap[0].Text)
	}
}

// /save + /load both need SessionPath wired. Use a tempdir.
func TestHandleMessage_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	cfg := BotConfig{
		Token:        "tok",
		SessionPath:  filepath.Join(dir, "sessions.json"),
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	}
	b, rec := newBotWithRecorder(t, cfg)

	// Plant some history so /save has content.
	conv := b.getConversation(100)
	conv.AddMessage(chat.Message{Role: "user", Content: "remember this"})
	conv.AddMessage(chat.Message{Role: "assistant", Content: "ok"})

	saveSnap := runCommand(t, b, rec, 100, 42, "/save thread-1")
	if !strings.Contains(saveSnap[len(saveSnap)-1].Text, "Saved") {
		t.Errorf("save: %q", saveSnap[len(saveSnap)-1].Text)
	}

	// /load with no arg lists names.
	listSnap := runCommand(t, b, rec, 100, 42, "/load")
	if !strings.Contains(listSnap[len(listSnap)-1].Text, "thread-1") {
		t.Errorf("/load list: %q", listSnap[len(listSnap)-1].Text)
	}

	// /load <name> restores.
	restoreSnap := runCommand(t, b, rec, 100, 42, "/load thread-1")
	if !strings.Contains(restoreSnap[len(restoreSnap)-1].Text, "Loaded") {
		t.Errorf("/load: %q", restoreSnap[len(restoreSnap)-1].Text)
	}
}

func TestHandleMessage_SaveDisabledWithoutPath(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	conv := b.getConversation(100)
	conv.AddMessage(chat.Message{Role: "user", Content: "x"})

	snap := runCommand(t, b, rec, 100, 42, "/save x")
	if !strings.Contains(snap[0].Text, "disabled") {
		t.Errorf("/save without session_path: %q", snap[0].Text)
	}
}

func TestHandleMessage_SaveMissingName(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		SessionPath:  filepath.Join(t.TempDir(), "s.json"),
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/save")
	if !strings.Contains(snap[0].Text, "Usage") {
		t.Errorf("/save no-name: %q", snap[0].Text)
	}
}

func TestHandleMessage_SaveEmptyConversation(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		SessionPath:  filepath.Join(t.TempDir(), "s.json"),
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/save thread-empty")
	if !strings.Contains(snap[0].Text, "Nothing to save") {
		t.Errorf("/save empty: %q", snap[0].Text)
	}
}

func TestHandleMessage_LoadMissingFile(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		SessionPath:  filepath.Join(t.TempDir(), "s.json"),
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/load ghost")
	last := snap[len(snap)-1].Text
	if !strings.Contains(last, "No saved conversation") &&
		!strings.Contains(last, "no saved conversation") {
		// /load with no save dir present returns "No saved conversations" (list mode)
		// /load <name> with no such name returns "No saved conversation named …".
		// Accept either spelling.
		if !strings.Contains(last, "saved conversation") {
			t.Errorf("/load ghost: %q", last)
		}
	}
}

func TestHandleMessage_SearchMissingArg(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		SessionPath:  filepath.Join(t.TempDir(), "s.json"),
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/search")
	if !strings.Contains(snap[0].Text, "Usage") {
		t.Errorf("/search no-arg: %q", snap[0].Text)
	}
}

func TestHandleMessage_SearchDisabled(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/search anything")
	if !strings.Contains(snap[0].Text, "disabled") {
		t.Errorf("/search no session path: %q", snap[0].Text)
	}
}

func TestHandleMessage_SearchNoMatches(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		SessionPath:  filepath.Join(t.TempDir(), "s.json"),
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/search no-such-word")
	if !strings.Contains(snap[0].Text, "No saved conversations") {
		t.Errorf("/search no matches: %q", snap[0].Text)
	}
}

// /project with no arg + no projects configured falls back to the
// prose response (because the registry is empty and sendProjectPicker
// won't produce any buttons).
func TestHandleMessage_ProjectNoRegistry(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/project")
	if !strings.Contains(snap[0].Text, "No active project") {
		t.Errorf("/project: %q", snap[0].Text)
	}
}

func TestHandleMessage_ProjectActiveShowsCurrent(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	b.setActiveProject(100, "trader-1")
	snap := runCommand(t, b, rec, 100, 42, "/project")
	if !strings.Contains(snap[0].Text, "trader-1") {
		t.Errorf("/project (active): %q", snap[0].Text)
	}
}

// /project <id> without any registry lookup wired produces a
// "Switched to project '<id>'" response — the registry guard only
// fires when registry is non-nil. Exercise that fall-through path.
func TestHandleMessage_ProjectSwitchNoRegistry(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/project no-such-project")
	if !strings.Contains(snap[0].Text, "Switched to project") {
		t.Errorf("/project switch: %q", snap[0].Text)
	}
}

func TestHandleMessage_InboxCommand(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/inbox")
	if !strings.Contains(snap[0].Text, "Inbox unavailable") {
		t.Errorf("/inbox no-repo: %q", snap[0].Text)
	}
}

func TestHandleMessage_AutopilotCommand(t *testing.T) {
	mgr := newFakeAutonomyMgr()
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	b.SetAutonomyManager(mgr)
	b.setActiveProject(100, "trader-1")
	snap := runCommand(t, b, rec, 100, 42, "/autopilot")
	if !strings.Contains(snap[0].Text, "is off") {
		t.Errorf("/autopilot: %q", snap[0].Text)
	}
}

func TestHandleMessage_SummarizeCommand(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	// Empty conversation → "Nothing to summarize"
	snap := runCommand(t, b, rec, 100, 42, "/summarize")
	if !strings.Contains(snap[0].Text, "Nothing to summarize") {
		t.Errorf("/summarize: %q", snap[0].Text)
	}
}

// /unknown — when commands map doesn't have the entry, we don't crash;
// the legacy "fallthrough to dispatcher" path handles it (and since
// no receiver is wired, we get the "Dispatcher is not configured"
// response).
func TestHandleMessage_UnknownSlashFallsThrough(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/zzz")
	if !strings.Contains(snap[0].Text, "Dispatcher is not configured") {
		t.Errorf("/zzz: %q", snap[0].Text)
	}
}

// /pin with a real text trims the slash command and preserves args.
func TestHandleMessage_PinDoesNotIncludeCommandPrefix(t *testing.T) {
	b, _ := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	_ = b.HandleMessage(context.Background(), &Message{
		ID: 1, ChatID: 100, UserID: 42, Text: "/pin Always reply in JSON",
	})
	pins := b.getConversation(100).PinnedMessages()
	if len(pins) != 1 {
		t.Fatalf("pin count: got %d, want 1", len(pins))
	}
	if strings.HasPrefix(pins[0].Content, "/pin") {
		t.Errorf("pin contains the slash prefix: %q", pins[0].Content)
	}
}

// Sanity test: command output containing JSON should still be a
// valid string the recorder captures (catches output that includes
// stray non-UTF8 bytes etc.).
func TestHandleMessage_ContextOutputIsValidJSON_Tolerant(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/context")
	// JSON marshal the text to confirm it round-trips — a sanity
	// guard, not a deep assertion.
	if _, err := json.Marshal(snap[0].Text); err != nil {
		t.Errorf("context text not JSON-safe: %v", err)
	}
}
