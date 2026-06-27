// Package telegram: tests for saveConversations — the periodic
// persistence of bot.conversations to disk.
package telegram

import (
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/chat"
)

func TestSaveConversations_NoSessionPath_NoOp(t *testing.T) {
	bot, _, cleanup := makeAutopilotBot(t)
	defer cleanup()
	// SessionPath empty → early return; nothing to assert beyond
	// "doesn't panic / doesn't write".
	bot.saveConversations()
}

func TestSaveConversations_WritesToDisk(t *testing.T) {
	bot, _, cleanup := makeAutopilotBot(t)
	defer cleanup()
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sessions.json")
	bot.config.SessionPath = sessionPath
	bot.mu.Lock()
	bot.conversations[1] = &chat.Conversation{}
	bot.conversations[2] = &chat.Conversation{}
	bot.mu.Unlock()
	bot.saveConversations()
	info, err := os.Stat(sessionPath)
	if err != nil {
		t.Fatalf("expected session file written; got %v", err)
	}
	if info.Size() == 0 {
		t.Errorf("session file is empty")
	}
}

func TestSaveConversations_BadPath_LogsError(t *testing.T) {
	bot, _, cleanup := makeAutopilotBot(t)
	defer cleanup()
	// Path under a missing parent directory triggers the error
	// branch. We only assert no-panic + no test failure.
	bot.config.SessionPath = "/proc/no/such/directory/sessions.json"
	bot.saveConversations()
}
